/*
Copyright (c) 2025-2026 Microbus LLC and various contributors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sequel

import (
	"context"
	"database/sql"
	"log/slog"
	"maps"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/microbus-io/errors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
)

// instrumentationName is the OpenTelemetry instrumentation scope for the tracer and meter sequel obtains
// from the caller-supplied providers.
const instrumentationName = "github.com/microbus-io/sequel"

/*
telemetry holds the observability wiring derived from the caller-supplied providers and is referenced by
a *DB (and handed to each *Tx). It is intentionally immutable once stored: the setters on *DB build a new
telemetry value under lock and swap it in atomically, so the hot path reads a single atomic pointer and
never takes a lock. A nil *telemetry is the zero-overhead path — every method below is nil-safe so that a
DB without any provider does no extra work beyond one nil check.

Spans follow OpenTelemetry database semantic conventions (db.system.name, db.operation.name,
db.collection.name); metrics carry the sequel_ prefix. Spans deliberately omit the statement text: the
operation keyword and table, combined with the caller's own (function-named) parent span, identify the
statement without per-span text volume. The full parameterized statement is available instead in the
per-query Debug log, which the caller's logger level gates.
*/
type telemetry struct {
	tracer trace.Tracer
	meter  metric.Meter
	logger *slog.Logger

	// Instruments are created when a MeterProvider is set; each is nil-checked at use so a tracer-only or
	// logger-only configuration works.
	queryDuration       metric.Float64Histogram
	transactionDuration metric.Float64Histogram
	lockContention      metric.Int64Counter
	migrationRuns       metric.Int64Counter
	poolRegistration    metric.Registration
}

// newDefaultTelemetry builds the telemetry a freshly opened DB starts with: the process-wide OpenTelemetry
// tracer and meter providers (otel.GetTracerProvider / otel.GetMeterProvider) and a discard logger. Those
// global providers are delegating no-ops until the application installs real ones, so an app that configures
// OpenTelemetry globally gets sequel spans and metrics with no extra call, while an app that configures
// nothing pays only OTEL's no-op cost. The per-DB setters override any of the three; passing them nil
// reverts to these same defaults.
func newDefaultTelemetry(db *DB) *telemetry {
	t := &telemetry{
		tracer: otel.GetTracerProvider().Tracer(instrumentationName),
		meter:  otel.GetMeterProvider().Meter(instrumentationName),
		logger: slog.New(slog.DiscardHandler),
	}
	t.initInstruments(db)
	return t
}

// clone returns a shallow copy so a setter can mutate one field and swap the pointer without disturbing a
// telemetry value that concurrent operations may still be reading.
func (t *telemetry) clone() *telemetry {
	nt := &telemetry{}
	if t != nil {
		*nt = *t
	}
	return nt
}

// enabled reports whether telemetry is present. Since Open seeds every *DB with the global OTEL providers
// and a discard logger (and the setters never clear them to nil), this is a plain nil-pointer guard, not a
// live fast path: an unconfigured *DB still funnels through the instrumentation, where OTEL's no-op tracer
// and meter short-circuit the cost. The check remains only to stay safe on a zero-value *telemetry.
func (t *telemetry) enabled() bool {
	return t != nil
}

// initInstruments creates the sequel_ metric instruments and registers the connection-pool gauge callback
// against the current meter. It is called under db.mutex when a MeterProvider is set. Instrument
// construction errors are ignored: the OTEL API returns a working no-op instrument alongside the error, so
// a failed instrument simply records nothing rather than breaking queries.
func (t *telemetry) initInstruments(db *DB) {
	m := t.meter
	t.queryDuration, _ = m.Float64Histogram(
		"sequel_query_duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of database queries in seconds"),
	)
	t.transactionDuration, _ = m.Float64Histogram(
		"sequel_transaction_duration",
		metric.WithUnit("s"),
		metric.WithDescription("Duration of Transact calls in seconds, including retries"),
	)
	// Counter instrument names carry no _total suffix; the Prometheus exporter appends it at scrape time.
	t.lockContention, _ = m.Int64Counter(
		"sequel_lock_contention",
		metric.WithDescription("Count of operations that failed on lock contention or deadlock"),
	)
	t.migrationRuns, _ = m.Int64Counter(
		"sequel_migration_runs",
		metric.WithDescription("Count of schema migrations executed (excludes already-completed ones)"),
	)

	openConns, _ := m.Int64ObservableGauge("sequel_pool_open_connections",
		metric.WithDescription("Open connections in the pool (in use plus idle)"))
	inUseConns, _ := m.Int64ObservableGauge("sequel_pool_in_use_connections",
		metric.WithDescription("Connections currently in use"))
	idleConns, _ := m.Int64ObservableGauge("sequel_pool_idle_connections",
		metric.WithDescription("Idle connections in the pool"))
	waitCount, _ := m.Int64ObservableCounter("sequel_pool_waits",
		metric.WithDescription("Total number of connections waited for"))
	waitDuration, _ := m.Float64ObservableCounter("sequel_pool_wait_duration_seconds",
		metric.WithUnit("s"),
		metric.WithDescription("Total time blocked waiting for a connection, in seconds"))

	dbAttr := attribute.String("database", databaseAttr(db.driverName, db.dataSourceName))
	reg, err := m.RegisterCallback(
		func(_ context.Context, o metric.Observer) error {
			stats, ok := db.snapshotStats()
			if !ok {
				return nil
			}
			o.ObserveInt64(openConns, int64(stats.OpenConnections), metric.WithAttributes(dbAttr))
			o.ObserveInt64(inUseConns, int64(stats.InUse), metric.WithAttributes(dbAttr))
			o.ObserveInt64(idleConns, int64(stats.Idle), metric.WithAttributes(dbAttr))
			o.ObserveInt64(waitCount, stats.WaitCount, metric.WithAttributes(dbAttr))
			o.ObserveFloat64(waitDuration, stats.WaitDuration.Seconds(), metric.WithAttributes(dbAttr))
			return nil
		},
		openConns, inUseConns, idleConns, waitCount, waitDuration,
	)
	if err == nil {
		t.poolRegistration = reg
	}
}

// clearInstruments drops all instruments and unregisters the pool callback, used when a nil MeterProvider
// is set to disable metrics.
func (t *telemetry) clearInstruments() {
	if t.poolRegistration != nil {
		_ = t.poolRegistration.Unregister()
	}
	t.queryDuration = nil
	t.transactionDuration = nil
	t.lockContention = nil
	t.migrationRuns = nil
	t.poolRegistration = nil
}

/*
begin starts instrumentation for a single query-shaped operation and returns a (possibly new) context
carrying the span plus a finish func to call with the operation's error. query is the executed (unpacked)
SQL; its leading keyword and, when unambiguous, its table become the db.operation.name / db.collection.name
attributes and the span name. The finish func records duration, classifies lock contention, sets the span
status, and — when the logger is enabled at Debug level — emits a Debug log. Safe to call on a nil
*telemetry (no-op).
*/
func (t *telemetry) begin(ctx context.Context, driver, query string) (context.Context, func(err error)) {
	return t.beginAt(ctx, driver, query, time.Now())
}

// beginAt is begin with an explicit start instant, for an operation whose instrumentation can only be
// decided after it has run (see instrumentTxEnd). Back-dating keeps the span covering the whole operation.
func (t *telemetry) beginAt(ctx context.Context, driver, query string, start time.Time) (context.Context, func(err error)) {
	if !t.enabled() {
		return ctx, func(error) {}
	}
	op, table := parseOperation(query)
	t.logOperationCapOnce(ctx)

	var span trace.Span
	if t.tracer != nil {
		spanName := op
		if table != "" {
			spanName = op + " " + table
		}
		attrs := []attribute.KeyValue{
			attribute.String("db.system.name", driver),
			attribute.String("db.operation.name", op),
		}
		if table != "" {
			attrs = append(attrs, attribute.String("db.collection.name", table))
		}
		ctx, span = t.tracer.Start(ctx, spanName, trace.WithSpanKind(trace.SpanKindClient),
			trace.WithTimestamp(start), trace.WithAttributes(attrs...))
	}

	return ctx, func(err error) {
		dur := time.Since(start).Seconds()
		status := "ok"
		if err != nil {
			status = "error"
			if t.lockContention != nil && IsLockContentionError(err) {
				t.lockContention.Add(ctx, 1, metric.WithAttributes(
					attribute.String("db.system.name", driver),
					attribute.String("db.operation.name", op),
				))
			}
		}
		if t.queryDuration != nil {
			t.queryDuration.Record(ctx, dur, metric.WithAttributes(
				attribute.String("db.system.name", driver),
				attribute.String("db.operation.name", op),
				attribute.String("status", status),
			))
		}
		if span != nil {
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			}
			span.End()
		}
		if t.logger != nil && t.logger.Enabled(ctx, slog.LevelDebug) {
			args := []any{"driver", driver, "operation", op, "query", query, "duration_s", dur}
			if err != nil {
				// Logged at Debug, not Error: the library returns every error to the caller, who logs it.
				t.logger.DebugContext(ctx, "sequel query failed", append(args, "error", err.Error())...)
			} else {
				t.logger.DebugContext(ctx, "sequel query", args...)
			}
		}
	}
}

// beginTransact starts instrumentation for a whole Transact call (covering all retry attempts) and returns
// a context carrying the span plus a finish func taking the final attempt count and error. Safe on a nil
// *telemetry (no-op).
func (t *telemetry) beginTransact(ctx context.Context, driver string) (context.Context, func(attempts int, err error)) {
	if !t.enabled() {
		return ctx, func(int, error) {}
	}
	start := time.Now()
	var span trace.Span
	if t.tracer != nil {
		ctx, span = t.tracer.Start(ctx, "transact", trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(attribute.String("db.system.name", driver)))
	}
	return ctx, func(attempts int, err error) {
		dur := time.Since(start).Seconds()
		outcome := "committed"
		if err != nil {
			outcome = "rolledback"
		}
		if t.transactionDuration != nil {
			t.transactionDuration.Record(ctx, dur, metric.WithAttributes(
				attribute.String("db.system.name", driver),
				attribute.String("outcome", outcome),
			))
		}
		if span != nil {
			span.SetAttributes(attribute.Int("db.transaction.attempts", attempts))
			if err != nil {
				span.RecordError(err)
				span.SetStatus(codes.Error, err.Error())
			}
			span.End()
		}
	}
}

// beginMigrate starts a span around a whole Migrate call and returns a context plus a finish func. Safe on
// a nil *telemetry (no-op).
func (t *telemetry) beginMigrate(ctx context.Context, driver, sequence string) (context.Context, func(err error)) {
	if !t.enabled() {
		return ctx, func(error) {}
	}
	var span trace.Span
	if t.tracer != nil {
		ctx, span = t.tracer.Start(ctx, "migrate "+sequence, trace.WithSpanKind(trace.SpanKindClient),
			trace.WithAttributes(
				attribute.String("db.system.name", driver),
				attribute.String("db.migration.sequence", sequence),
			))
	}
	return ctx, func(err error) {
		if span == nil {
			return
		}
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		span.End()
	}
}

// logMigrationAttempt logs (at Info) that a specific migration is about to run, regardless of its eventual
// outcome. Skipped/already-completed migrations never reach this point.
func (t *telemetry) logMigrationAttempt(ctx context.Context, driver, sequence, file string) {
	if t == nil || t.logger == nil {
		return
	}
	t.logger.InfoContext(ctx, "running database migration",
		"driver", driver, "sequence", sequence, "file", file)
}

// recordMigration increments sequel_migration_runs for a migration that actually executed, tagged
// with its ok/error status.
func (t *telemetry) recordMigration(ctx context.Context, driver, sequence string, err error) {
	if t == nil || t.migrationRuns == nil {
		return
	}
	status := "ok"
	if err != nil {
		status = "error"
	}
	t.migrationRuns.Add(ctx, 1, metric.WithAttributes(
		attribute.String("db.system.name", driver),
		attribute.String("db.migration.sequence", sequence),
		attribute.String("status", status),
	))
}

// parseOperation extracts the SQL operation keyword and, only when it can do so unambiguously, the primary
// table from a statement. The operation is always the leading keyword. The table is returned only for a
// single-target statement: it is omitted for joins, multi-table FROM lists, and subqueries, so a non-empty
// table is trustworthy (and keeps span-name cardinality bounded) rather than a guess.
// operationOther is the bucket for a statement verb that is not reported verbatim, either because it is
// not shaped like a verb or because the label cap has been reached.
const operationOther = "OTHER"

// operationLabelCap bounds how many distinct verbs may ever be reported. A curated codebase uses on the
// order of a dozen, so the cap is unreachable in normal operation; it exists so that an application which
// interpolates uncontrolled input into its SQL cannot mint unbounded metric attribute values or span names.
const operationLabelCap = 128

// seedOperations are the verbs reported verbatim from the very first statement, without being learned.
// Seeding matters for more than convenience: a seeded verb is deterministic (its label never depends on
// what the process happened to see first) and cannot be crowded out of the learned set, so the labels an
// ordinary application depends on survive even while something else is filling the cap with junk. The verbs
// participating in table extraction below are seeded for the same reason — db.collection.name and the span
// name should not vary with history either. Rarer verbs are deliberately absent: the learner picks them up
// on first use, which is what keeps this list from needing to enumerate five dialects.
var seedOperations = []string{
	"SELECT", "INSERT", "UPDATE", "DELETE", "REPLACE", "MERGE", "UPSERT", "WITH",
	"CREATE", "DROP", "ALTER", "TRUNCATE",
	"BEGIN", "COMMIT", "ROLLBACK", "SAVEPOINT",
	"SET", "PRAGMA", "USE",
	"SHOW", "EXPLAIN", "ANALYZE",
	"CALL", "EXEC",
}

// verbPattern is the shape a token must have to be learned as a verb. It is what stops a hostile leading
// token from consuming the cap: only plausible-verb-shaped tokens are ever added, so arbitrary bytes,
// injected SQL fragments and comment prefixes are bucketed as OTHER without occupying a slot. It also
// bounds the length of any label sequel can emit, since the token is spliced into metric attributes and
// span names.
var verbPattern = regexp.MustCompile(`^[A-Z][A-Z0-9_]{0,31}$`)

// operationLabels is the bounded set of verbs reported as db.operation.name. Reads are a single atomic load
// of an immutable snapshot, so the hot path never locks; a first sighting clones-then-swaps under the mutex.
// This is the same copy-on-write discipline as the virtual function registry, for the same reason.
//
// It is package-level rather than per-DB because the quantity being bounded — how many distinct label
// values this process exports — is a property of the process, not of one pool. That does mean a pool
// issuing unusual statements consumes slots shared with every other pool; acceptable, since the seed
// protects the labels that matter and the cap is far above any curated workload.
type operationLabels struct {
	mutex       sync.Mutex // serializes writers so concurrent first sightings don't lose updates
	known       atomic.Pointer[map[string]bool]
	capExceeded atomic.Bool
}

// newOperationLabels returns a set pre-seeded with the given verbs.
func newOperationLabels(seed []string) *operationLabels {
	o := &operationLabels{}
	m := make(map[string]bool, len(seed))
	for _, v := range seed {
		m[v] = true
	}
	o.known.Store(&m)
	return o
}

var defaultOperationLabels = newOperationLabels(seedOperations)

// label maps a statement's leading token to the value reported as db.operation.name: the token itself if it
// is already known or can still be learned, otherwise OTHER.
func (o *operationLabels) label(token string) string {
	if (*o.known.Load())[token] {
		return token
	}
	if !verbPattern.MatchString(token) {
		// Not verb-shaped, so not learnable — and deliberately not a cap event: filtering junk is normal,
		// whereas exhausting the cap is worth telling an operator about.
		return operationOther
	}
	o.mutex.Lock()
	defer o.mutex.Unlock()
	// Re-read under the lock: another goroutine may have learned this token, or filled the last slot.
	known := *o.known.Load()
	if known[token] {
		return token
	}
	if len(known) >= operationLabelCap {
		o.capExceeded.Store(true)
		return operationOther
	}
	clone := maps.Clone(known)
	clone[token] = true
	o.known.Store(&clone)
	return token
}

// operationCapLogOnce keeps the cap warning to a single line per process.
var operationCapLogOnce sync.Once

// logOperationCapOnce tells the operator, once, that the verb label cap has been reached and further
// unrecognized verbs are being bucketed. This is the one thing about the cap that is not self-evident from
// the metrics: seeing OTHER does not by itself distinguish "an odd statement" from "labels are now being
// dropped". Logged at Info as a one-off lifecycle event, not per occurrence.
func (t *telemetry) logOperationCapOnce(ctx context.Context) {
	if t.logger == nil || !defaultOperationLabels.capExceeded.Load() {
		return
	}
	operationCapLogOnce.Do(func() {
		t.logger.InfoContext(ctx,
			"sequel reached its limit of distinct statement verbs; further unrecognized verbs report as "+operationOther,
			"cap", operationLabelCap)
	})
}

func parseOperation(query string) (op string, table string) {
	s := strings.TrimLeft(query, " \t\r\n")
	if s == "" {
		return "", ""
	}
	end := 0
	for end < len(s) && !isSQLSpace(s[end]) {
		end++
	}
	// A single-word statement may arrive with its terminator attached ("COMMIT;").
	op = defaultOperationLabels.label(strings.ToUpper(strings.TrimRight(s[:end], ";")))
	if op == operationOther {
		return op, ""
	}
	upper := strings.ToUpper(s)
	switch op {
	case "SELECT", "DELETE":
		table = tableAfter(s, upper, " FROM ")
	case "INSERT", "REPLACE", "UPSERT":
		table = tableAfter(s, upper, " INTO ")
	case "UPDATE", "MERGE":
		table = identAt(s, end)
	default:
		return op, ""
	}
	if strings.Contains(upper, " JOIN ") {
		// A join means more than one table is involved; the single-table attribute would be misleading.
		table = ""
	}
	return op, table
}

// tableAfter returns the identifier following the first occurrence of keyword (e.g. " FROM "), or "" if the
// keyword is absent or the following token is not a plain single table.
func tableAfter(s, upper, keyword string) string {
	i := strings.Index(upper, keyword)
	if i < 0 {
		return ""
	}
	return identAt(s, i+len(keyword))
}

// identAt reads the identifier starting at or after position start (skipping leading whitespace), returning
// "" for a subquery ('(') or a multi-table list (token immediately followed by a comma).
func identAt(s string, start int) string {
	for start < len(s) && isSQLSpace(s[start]) {
		start++
	}
	if start >= len(s) || s[start] == '(' {
		return ""
	}
	end := start
	for end < len(s) && !isIdentDelim(s[end]) {
		end++
	}
	tok := s[start:end]
	j := end
	for j < len(s) && isSQLSpace(s[j]) {
		j++
	}
	if j < len(s) && s[j] == ',' {
		return ""
	}
	return strings.Trim(tok, "`\"[]")
}

func isSQLSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n'
}

func isIdentDelim(c byte) bool {
	return isSQLSpace(c) || c == ',' || c == '(' || c == ')' || c == ';'
}

// instrumentExec wraps an Exec/Query/Prepare-shaped operation: it unpacks the query once, opens a span,
// pauses for the simulated round-trip time (see [DB.SimulateRTT]; normally zero), runs the operation, and
// finishes instrumentation with the result error. The operation error is wrapped by traceErr — the stack
// is attached around the raw error, so driver error types stay intact for IsLockContentionError and the
// database/sql sentinels pass through untouched.
func instrumentExec[T any](t *telemetry, rtt time.Duration, ctx context.Context, driver, query string, run func(ctx context.Context, unpacked string) (T, error)) (T, error) {
	var zero T
	unpacked, err := unpackQuery(driver, query)
	if err != nil {
		_, finish := t.begin(ctx, driver, query)
		finish(err)
		return zero, errors.Trace(err)
	}
	return instrumentUnpacked(t, rtt, ctx, driver, unpacked, run)
}

// instrumentUnpacked is instrumentExec for a statement that is already unpacked — an execution of a
// prepared [Stmt], whose text was expanded and conformed once at Prepare time.
//
// The pause sits inside the span rather than before it, so an operation's recorded duration includes the
// latency being simulated. That is the point of simulating it: the numbers a test reads should be the ones
// a caller would experience. Unpacking is local string work, not a round trip, so it stays outside.
func instrumentUnpacked[T any](t *telemetry, rtt time.Duration, ctx context.Context, driver, unpacked string, run func(ctx context.Context, unpacked string) (T, error)) (T, error) {
	ctx, finish := t.begin(ctx, driver, unpacked)
	if err := simulateRTT(ctx, rtt); err != nil {
		finish(err)
		var zero T
		return zero, errors.Trace(err)
	}
	res, err := run(ctx, unpacked)
	// finish sees the raw error: spans, metrics and the Debug log record what the driver reported, without
	// the wrapper.
	finish(err)
	return res, traceErr(err)
}

// instrumentQueryRow wraps QueryRow/QueryRowContext. Because database/sql defers the query error to Scan,
// the returned *Row carries the finish func and reports the operation's error (and duration ending) when
// the caller calls Scan or Err. An unpack error is swallowed here exactly as the original shadow methods do
// (the empty query then surfaces a driver error at Scan time).
func instrumentQueryRow(t *telemetry, rtt time.Duration, ctx context.Context, driver, query string, recordErr func(error) error, run func(ctx context.Context, unpacked string) *sql.Row) *Row {
	unpacked, _ := unpackQuery(driver, query)
	return instrumentQueryRowUnpacked(t, rtt, ctx, driver, unpacked, recordErr, run)
}

// instrumentQueryRowUnpacked is instrumentQueryRow for a statement that is already unpacked — an
// execution of a prepared [Stmt].
func instrumentQueryRowUnpacked(t *telemetry, rtt time.Duration, ctx context.Context, driver, unpacked string, recordErr func(error) error, run func(ctx context.Context, unpacked string) *sql.Row) *Row {
	ctx, finish := t.begin(ctx, driver, unpacked)
	// A context that expires during the simulated round trip needs no handling of its own here: the query
	// below is issued with that same context, so the driver surfaces the cancellation at Scan — which is
	// where a QueryRow error surfaces in any case.
	_ = simulateRTT(ctx, rtt)
	return &Row{Row: run(ctx, unpacked), finish: finish, recordErr: recordErr}
}

// instrumentTxOp wraps a transaction lifecycle operation — BEGIN, COMMIT, ROLLBACK. database/sql exposes
// these as methods rather than SQL, so there is no statement text to unpack and the operation keyword goes
// to beginAt as the "query".
func instrumentTxOp[T any](t *telemetry, rtt time.Duration, ctx context.Context, driver, op string, run func(ctx context.Context) (T, error)) (T, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	start := time.Now()
	// No handling for a context cancelled during the pause: database/sql checks the context itself in all
	// three calls, so running the call is what produces the authoritative error.
	_ = simulateRTT(ctx, rtt)
	res, err := run(ctx)
	if errors.Is(err, sql.ErrTxDone) {
		// Answered without touching the database, so there is no operation to report. Instrumenting it would
		// stamp an error span on every cancelled Transact, whose deferred rollback lands here.
		return res, err
	}
	_, finish := t.beginAt(ctx, driver, op, start)
	finish(err)
	return res, err
}

// instrumentTxEnd is instrumentTxOp for COMMIT and ROLLBACK, which return no value.
func instrumentTxEnd(t *telemetry, rtt time.Duration, ctx context.Context, driver, op string, run func() error) error {
	_, err := instrumentTxOp(t, rtt, ctx, driver, op,
		func(context.Context) (any, error) { return nil, run() })
	return err
}

/*
Row shadows *sql.Row so sequel can observe a single-row query and latch its error into a [DB.Transact]-managed
transaction. database/sql does not surface a QueryRow error until Scan, so Row records the operation's
duration, classifies lock contention, and ends the span when the caller calls Scan (or Err). It embeds
*sql.Row, so the common QueryRow(...).Scan(...) call site is unchanged; only code that explicitly stores the
result as *sql.Row needs adjustment.

In Transact (autoErr) mode Scan/Err also latch the error into the transaction, exactly as [Rows] does for a
streamed read, so a closure that ignores a QueryRow error cannot commit work built on a row it never read.
[sql.ErrNoRows] is deliberately exempt: unlike a Rows iteration, where an empty result set is simply Next
returning false, "no row" reaches a QueryRow caller as an error and is routine control flow
(`if err == sql.ErrNoRows { ...default... }`). Latching it would doom every transaction that legitimately
handles a missing row. Every other error — deadlock, type-conversion failure, connection drop — is latched.
Outside a Transact-managed Tx, recordErr is nil and no latching occurs.

As with *sql.Row, a Row whose Scan/Err is never called holds resources open — and here, leaves its span
unended. Call Scan (or Err) exactly as you would with *sql.Row.
*/
type Row struct {
	*sql.Row
	finish func(error)
	// recordErr latches an error into the owning autoErr Tx; nil for a *DB query or a non-Transact Tx.
	recordErr func(error) error
	// shortErr is the already-recorded transaction error for a Row returned by a short-circuited Tx
	// query. When set, the embedded *sql.Row is nil and Scan/Err report shortErr without touching the
	// database; no span was ever started, so finish is nil too.
	shortErr error
	done     bool
}

// latch records a non-nil error into the owning transaction, when there is one. sql.ErrNoRows is exempt —
// see the type doc.
func (r *Row) latch(err error) {
	if err != nil && !errors.Is(err, sql.ErrNoRows) && r.recordErr != nil {
		r.recordErr(err)
	}
}

// Scan shadows sql.Row.Scan, finishes instrumentation with the scan error, and latches it into the
// transaction (autoErr mode), so a closure that ignores the returned error cannot commit work built on a
// row it never read.
func (r *Row) Scan(dest ...any) error {
	if r.shortErr != nil {
		return r.shortErr
	}
	err := r.Row.Scan(dest...)
	r.complete(err)
	r.latch(err)
	return traceErr(err)
}

// Err shadows sql.Row.Err, finishes instrumentation with the query error, and latches it, so a caller that
// inspects Err instead of scanning still closes the span and aborts the transaction.
func (r *Row) Err() error {
	if r.shortErr != nil {
		return r.shortErr
	}
	err := r.Row.Err()
	r.complete(err)
	r.latch(err)
	return traceErr(err)
}

// complete fires the finish func once. A Row is not meant for concurrent use, so no synchronization.
func (r *Row) complete(err error) {
	if r.finish != nil && !r.done {
		r.done = true
		r.finish(err)
	}
}

// databaseAttr derives a low-cardinality, credential-free pool identifier for pool metrics: the database
// name when it can be parsed, falling back to the driver name. The raw DSN is never used — it carries
// credentials.
func databaseAttr(driver, dsn string) string {
	if name, err := databaseNameFromDataSourceName(driver, dsn); err == nil && name != "" {
		return name
	}
	return driver
}
