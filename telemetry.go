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
	"strings"
	"time"

	"github.com/microbus-io/errors"
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
db.collection.name); metrics carry the sequel_ prefix. Statement text (db.query.text) is attached only in
verbose mode and never includes argument values — sequel always parameterizes args, so the captured text
holds only placeholders.
*/
type telemetry struct {
	tracer  trace.Tracer
	meter   metric.Meter
	logger  *slog.Logger
	verbose bool

	// Instruments are created when a MeterProvider is set; each is nil-checked at use so a tracer-only or
	// logger-only configuration works.
	queryDuration       metric.Float64Histogram
	transactionDuration metric.Float64Histogram
	lockContention      metric.Int64Counter
	migrationRuns       metric.Int64Counter
	poolRegistration    metric.Registration
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

// enabled reports whether any observability sink is configured. Used to skip work that only matters when
// something is listening.
func (t *telemetry) enabled() bool {
	return t != nil && (t.tracer != nil || t.meter != nil || t.logger != nil)
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
	t.lockContention, _ = m.Int64Counter(
		"sequel_lock_contention_total",
		metric.WithDescription("Count of operations that failed on lock contention or deadlock"),
	)
	t.migrationRuns, _ = m.Int64Counter(
		"sequel_migration_runs_total",
		metric.WithDescription("Count of schema migrations executed (excludes already-completed ones)"),
	)

	openConns, _ := m.Int64ObservableGauge("sequel_pool_open_connections",
		metric.WithDescription("Open connections in the pool (in use plus idle)"))
	inUseConns, _ := m.Int64ObservableGauge("sequel_pool_in_use_connections",
		metric.WithDescription("Connections currently in use"))
	idleConns, _ := m.Int64ObservableGauge("sequel_pool_idle_connections",
		metric.WithDescription("Idle connections in the pool"))
	waitCount, _ := m.Int64ObservableGauge("sequel_pool_wait_count",
		metric.WithDescription("Total number of connections waited for"))
	waitDuration, _ := m.Float64ObservableGauge("sequel_pool_wait_duration_seconds",
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
status, and — in verbose mode — emits a Debug log. Safe to call on a nil *telemetry (no-op).
*/
func (t *telemetry) begin(ctx context.Context, driver, query string) (context.Context, func(err error)) {
	if !t.enabled() {
		return ctx, func(error) {}
	}
	op, table := parseOperation(query)
	start := time.Now()

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
		if t.verbose {
			attrs = append(attrs, attribute.String("db.query.text", query))
		}
		ctx, span = t.tracer.Start(ctx, spanName, trace.WithSpanKind(trace.SpanKindClient), trace.WithAttributes(attrs...))
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
		if t.verbose && t.logger != nil {
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

// recordMigration increments sequel_migration_runs_total for a migration that actually executed, tagged
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
func parseOperation(query string) (op string, table string) {
	s := strings.TrimLeft(query, " \t\r\n")
	if s == "" {
		return "", ""
	}
	end := 0
	for end < len(s) && !isSQLSpace(s[end]) {
		end++
	}
	op = strings.ToUpper(s[:end])
	upper := strings.ToUpper(s)
	switch op {
	case "SELECT", "DELETE":
		table = tableAfter(s, upper, " FROM ")
	case "INSERT", "REPLACE":
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
// runs the operation, and finishes instrumentation with the result error. It preserves the existing error
// behavior — the unpack error is traced (matching the shadow methods), the operation error is returned
// verbatim so driver error types stay intact for IsLockContentionError.
func instrumentExec[T any](t *telemetry, ctx context.Context, driver, query string, run func(ctx context.Context, unpacked string) (T, error)) (T, error) {
	var zero T
	unpacked, err := unpackQuery(driver, query)
	if err != nil {
		_, finish := t.begin(ctx, driver, query)
		finish(err)
		return zero, errors.Trace(err)
	}
	ctx, finish := t.begin(ctx, driver, unpacked)
	res, err := run(ctx, unpacked)
	finish(err)
	return res, err
}

// instrumentQueryRow wraps QueryRow/QueryRowContext. Because database/sql defers the query error to Scan,
// the returned *Row carries the finish func and reports the operation's error (and duration ending) when
// the caller calls Scan or Err. An unpack error is swallowed here exactly as the original shadow methods do
// (the empty query then surfaces a driver error at Scan time).
func instrumentQueryRow(t *telemetry, ctx context.Context, driver, query string, run func(ctx context.Context, unpacked string) *sql.Row) *Row {
	unpacked, _ := unpackQuery(driver, query)
	ctx, finish := t.begin(ctx, driver, unpacked)
	return &Row{Row: run(ctx, unpacked), finish: finish}
}

/*
Row shadows *sql.Row so sequel can observe a single-row query. database/sql does not surface a QueryRow
error until Scan, so Row records the operation's duration, classifies lock contention, and ends the span
when the caller calls Scan (or Err). It embeds *sql.Row, so the common QueryRow(...).Scan(...) call site is
unchanged; only code that explicitly stores the result as *sql.Row needs adjustment.

As with *sql.Row, a Row whose Scan/Err is never called holds resources open — and here, leaves its span
unended. Call Scan (or Err) exactly as you would with *sql.Row.
*/
type Row struct {
	*sql.Row
	finish func(error)
	done   bool
}

// Scan shadows sql.Row.Scan and finishes instrumentation with the scan error.
func (r *Row) Scan(dest ...any) error {
	err := r.Row.Scan(dest...)
	r.complete(err)
	return err
}

// Err shadows sql.Row.Err and finishes instrumentation with the query error, so a caller that inspects Err
// instead of scanning still closes the span.
func (r *Row) Err() error {
	err := r.Row.Err()
	r.complete(err)
	return err
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
