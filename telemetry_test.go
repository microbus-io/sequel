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
	"bytes"
	"context"
	"database/sql"
	"log/slog"
	"maps"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/microbus-io/errors"
	"github.com/microbus-io/sequel/testdata"
	"github.com/microbus-io/testarossa"
	"go.opentelemetry.io/otel/codes"
	sdkmetric "go.opentelemetry.io/otel/sdk/metric"
	"go.opentelemetry.io/otel/sdk/metric/metricdata"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

func TestTelemetry_ParseOperation(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	cases := []struct {
		query string
		op    string
		table string
	}{
		{"SELECT * FROM users WHERE id=?", "SELECT", "users"},
		{"  select id from foo", "SELECT", "foo"},
		{"INSERT INTO users (name) VALUES (?)", "INSERT", "users"},
		{"UPDATE accounts SET balance=? WHERE id=?", "UPDATE", "accounts"},
		{"DELETE FROM sessions WHERE expired=1", "DELETE", "sessions"},
		{"SELECT * FROM `my-table`", "SELECT", "my-table"},
		{"SELECT * FROM [dbo].[Orders]", "SELECT", "dbo].[Orders"}, // bracket-trim is best-effort; schema kept
		// Ambiguous: table omitted on purpose.
		{"SELECT a.* FROM a JOIN b ON a.id=b.id", "SELECT", ""},
		{"SELECT * FROM t1, t2 WHERE t1.id=t2.id", "SELECT", ""},
		{"SELECT * FROM (SELECT 1) sub", "SELECT", ""},
		{"WITH cte AS (SELECT 1) SELECT * FROM cte", "WITH", ""},
		{"", "", ""},
		{"COMMIT;", "COMMIT", ""}, // a single-word statement may carry its terminator
		{"UPSERT INTO kv (k, v) VALUES (?, ?)", "UPSERT", "kv"}, // CockroachDB
		// Verbs outside the seed are learned on first use and report verbatim, so the seed never has to
		// enumerate five dialects — there is no allowlist to be missing from.
		{"OPTIMIZE TABLE t1", "OPTIMIZE", ""},   // MySQL
		{"DBCC CHECKDB", "DBCC", ""},            // SQL Server
		{"LISTEN channel", "LISTEN", ""},        // PostgreSQL
		{"FROBNICATE gizmos", "FROBNICATE", ""}, // nothing engine-specific about it: verb-shaped is enough
		// A token that is not verb-shaped is bucketed as OTHER and never learned, so it cannot consume a
		// slot: db.operation.name is a metric attribute and must stay low-cardinality even when an
		// application interpolates uncontrolled input into its SQL.
		{"'; DROP TABLE users; --", "OTHER", ""},
		{"/*comment*/SELECT * FROM t", "OTHER", ""},
		{strings.Repeat("X", 40) + " 1", "OTHER", ""}, // beyond the length bound
		{"123 GO", "OTHER", ""},                       // must start with a letter
	}
	for _, c := range cases {
		op, table := parseOperation(c.query)
		assert.Equal(c.op, op, "op for %q", c.query)
		assert.Equal(c.table, table, "table for %q", c.query)
	}
}

// The learned verb set is bounded: past the cap, further verbs report as OTHER rather than minting
// unbounded metric attribute values. Runs against its own instance, never the package-level set — filling
// a shared set would change the labels every other test observes.
func TestTelemetry_OperationLabelCap(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	labels := newOperationLabels([]string{"SELECT"})
	assert.Equal("SELECT", labels.label("SELECT"), "a seeded verb needs no learning")

	// Fill the remaining slots. Each is verb-shaped, so each is learned and reported verbatim.
	for i := range operationLabelCap - 1 {
		token := "V" + strconv.Itoa(i)
		assert.Equal(token, labels.label(token), "%s is within the cap", token)
	}
	assert.False(labels.capExceeded.Load(), "filling the cap exactly is not exceeding it")

	// The cap is now full: a new verb is bucketed.
	assert.Equal("OTHER", labels.label("OVERFLOW"))
	assert.True(labels.capExceeded.Load(), "the operator-facing flag is set once the cap starts bucketing")
	// Verbs already learned — and the seed above all — keep reporting verbatim afterward.
	assert.Equal("SELECT", labels.label("SELECT"))
	assert.Equal("V0", labels.label("V0"))
}

// A hostile leading token cannot consume the cap: only verb-shaped tokens are ever learned, so an
// application interpolating input into its SQL can be noisy without displacing the labels it depends on.
func TestTelemetry_MalformedVerbsDoNotConsumeCap(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	labels := newOperationLabels([]string{"SELECT"})
	for i := range 1000 {
		assert.Equal("OTHER", labels.label("'; DROP DATABASE x"+strconv.Itoa(i)+"; --"))
	}
	assert.False(labels.capExceeded.Load(), "junk is filtered, which is not a cap event")
	assert.Equal(1, len(*labels.known.Load()), "nothing was learned, so the seed is untouched")
	assert.Equal("SELECT", labels.label("SELECT"))
	assert.Equal("ANALYZE", labels.label("ANALYZE"), "a real verb can still be learned afterward")
}

// The learned set is written from the hot path, so concurrent first sightings must be race-free and must
// not lose updates. Fails under -race if the copy-on-write discipline is broken.
func TestTelemetry_OperationLabelsConcurrent(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	labels := newOperationLabels([]string{"SELECT"})
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 20 {
				// Overlapping ranges, so goroutines race to learn the same tokens.
				labels.label("V" + strconv.Itoa((g*7+i)%40))
				labels.label("SELECT")
			}
		}()
	}
	wg.Wait()
	// Every distinct token was learned exactly once; none was lost to a racing clone-then-swap.
	assert.Equal(41, len(*labels.known.Load()), "40 learned verbs plus the seeded SELECT")
}

// newSQLiteDB opens an isolated in-memory SQLite database for a test.
func newSQLiteDB(t *testing.T) *DB {
	t.Helper()
	assert := testarossa.For(t)
	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	db, err := OpenSingleton("sqlite", dsn)
	assert.NoError(err)
	t.Cleanup(func() { db.Close() })
	return db
}

func TestTelemetry_QuerySpans(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	db := newSQLiteDB(t)
	db.SetTracerProvider(tp)
	assert.NoError(db.Migrate(t.Name(), testdata.FS))

	var count int
	assert.NoError(db.QueryRow("SELECT COUNT(id) FROM foo").Scan(&count))
	assert.Equal(4, count)

	// Find the span for the SELECT ... FROM foo query.
	var found *tracetest.SpanStub
	for _, s := range tracetest.SpanStubsFromReadOnlySpans(sr.Ended()) {
		if s.Name == "SELECT foo" {
			st := s
			found = &st
		}
	}
	if !assert.NotNil(found, "expected a 'SELECT foo' span") {
		return
	}
	attrs := map[string]string{}
	for _, kv := range found.Attributes {
		attrs[string(kv.Key)] = kv.Value.AsString()
	}
	assert.Equal("sqlite", attrs["db.system.name"])
	assert.Equal("SELECT", attrs["db.operation.name"])
	assert.Equal("foo", attrs["db.collection.name"])
	// Statement text is never attached to spans; operation + table + the caller's parent span identify it.
	_, hasText := attrs["db.query.text"]
	assert.False(hasText)
}

func TestTelemetry_StatementLogsGatedByLevel(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	// A Debug-level logger captures the per-query statement; an Info-level logger does not, and never
	// the span text — per-query logging is controlled by the logger's level, not a separate switch.
	run := func(name string, level slog.Level) string {
		var logBuf bytes.Buffer
		logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: level}))
		sr := tracetest.NewSpanRecorder()
		tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

		// Each run gets its own isolated database so both can apply the migration from scratch.
		dsn, err := CreateTestingDatabase("sqlite", "", t.Name()+"_"+name)
		assert.NoError(err)
		db, err := OpenSingleton("sqlite", dsn)
		assert.NoError(err)
		t.Cleanup(func() { db.Close() })
		db.SetTracerProvider(tp)
		db.SetLogger(logger)
		assert.NoError(db.Migrate(t.Name(), testdata.FS))

		var count int
		assert.NoError(db.QueryRow("SELECT COUNT(id) FROM foo").Scan(&count))

		// The statement text is never attached to a span, regardless of log level.
		for _, s := range tracetest.SpanStubsFromReadOnlySpans(sr.Ended()) {
			for _, kv := range s.Attributes {
				assert.NotEqual("db.query.text", string(kv.Key))
			}
		}
		return logBuf.String()
	}

	debugLogs := run("debugrun", slog.LevelDebug)
	// Migration attempts log at Info; queries log at Debug, carrying the parameterized statement.
	assert.Contains(debugLogs, "running database migration")
	assert.Contains(debugLogs, "sequel query")
	assert.Contains(debugLogs, "SELECT COUNT(id) FROM foo")

	infoLogs := run("inforun", slog.LevelInfo)
	// At Info the migration event still logs, but per-query Debug lines are suppressed by the logger.
	assert.Contains(infoLogs, "running database migration")
	assert.NotContains(infoLogs, "sequel query")
}

func TestTelemetry_Metrics(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	db := newSQLiteDB(t)
	db.SetMeterProvider(mp)
	assert.NoError(db.Migrate(t.Name(), testdata.FS))

	var count int
	assert.NoError(db.QueryRow("SELECT COUNT(id) FROM foo").Scan(&count))
	assert.NoError(db.Transact(context.Background(), func(tx *Tx) error {
		_, err := tx.Exec("INSERT INTO foo (id, str) VALUES (?, ?)", 99, "z")
		return err
	}))

	var rm metricdata.ResourceMetrics
	assert.NoError(reader.Collect(context.Background(), &rm))

	names := map[string]bool{}
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			names[m.Name] = true
		}
	}
	for _, want := range []string{
		"sequel_query_duration",
		"sequel_transaction_duration",
		"sequel_migration_runs",
		"sequel_pool_open_connections",
		"sequel_pool_idle_connections",
	} {
		assert.True(names[want], "expected metric %s, got %v", want, names)
	}
}

func TestTelemetry_RowScanErrorCaptured(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	db := newSQLiteDB(t)
	db.SetTracerProvider(tp)
	assert.NoError(db.Migrate(t.Name(), testdata.FS))

	// Query against a non-existent table: the error surfaces at Scan, and the Row shadow must end the
	// span with an error status.
	var x int
	err := db.QueryRow("SELECT id FROM does_not_exist").Scan(&x)
	assert.Error(err)

	var found bool
	for _, s := range tracetest.SpanStubsFromReadOnlySpans(sr.Ended()) {
		if s.Name == "SELECT does_not_exist" {
			found = true
			assert.Equal(codes.Error, s.Status.Code)
		}
	}
	assert.True(found, "expected span for failed QueryRow")
}

// BEGIN, COMMIT and ROLLBACK are round trips, so each gets its own span, nested under the transaction.
func TestTelemetry_TransactionLifecycleSpans(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	db := newSQLiteDB(t)
	db.SetTracerProvider(tp)
	assert.NoError(db.Migrate(t.Name(), testdata.FS))

	ctx := context.Background()
	assert.NoError(db.Transact(ctx, func(tx *Tx) error {
		_, err := tx.Exec("INSERT INTO foo (id, str) VALUES (?, ?)", 101, "a")
		return err
	}))
	// A closure that fails rolls the transaction back, which is a round trip of its own.
	assert.Error(db.Transact(ctx, func(tx *Tx) error {
		return errors.New("closure failed")
	}))

	spans := tracetest.SpanStubsFromReadOnlySpans(sr.Ended())
	nameOfSpanID := map[string]string{}
	byName := map[string]tracetest.SpanStub{}
	for _, s := range spans {
		nameOfSpanID[s.SpanContext.SpanID().String()] = s.Name
		byName[s.Name] = s
	}

	for _, op := range []string{"BEGIN", "COMMIT", "ROLLBACK"} {
		span, ok := byName[op]
		if !assert.True(ok, "expected a %s span, got %v", op, slices.Sorted(maps.Keys(byName))) {
			continue
		}
		attrs := map[string]string{}
		for _, kv := range span.Attributes {
			attrs[string(kv.Key)] = kv.Value.AsString()
		}
		assert.Equal("sqlite", attrs["db.system.name"])
		assert.Equal(op, attrs["db.operation.name"], "%s carries its own operation name", op)
		// No table is involved, so the attribute is omitted rather than guessed at.
		_, hasTable := attrs["db.collection.name"]
		assert.False(hasTable)
		assert.Equal("transact", nameOfSpanID[span.Parent.SpanID().String()],
			"%s nests under its transaction rather than orphaning at the trace root", op)
	}

	// Each transaction begins and ends exactly once — the redundant Rollback that Transact skips after a
	// successful commit must not add a second.
	counts := map[string]int{}
	for _, s := range spans {
		counts[s.Name]++
	}
	assert.Equal(2, counts["BEGIN"], "both transactions begin")
	assert.Equal(1, counts["COMMIT"], "the successful transaction commits once")
	assert.Equal(1, counts["ROLLBACK"], "only the failed transaction rolls back")
}

// A cancelled Transact reaches its deferred rollback after database/sql has already finalized the
// transaction, so that rollback answers ErrTxDone without touching the database and must report nothing.
// Otherwise every cancelled request stamps an error span for a failure that never happened.
func TestTelemetry_CancelledTransactEmitsNoRollbackError(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	db := newSQLiteDB(t)
	db.SetTracerProvider(tp)
	assert.NoError(db.Migrate(t.Name(), testdata.FS))

	// Racy by nature — database/sql finalizes from its own goroutine — so repeat enough to catch it.
	for i := range 100 {
		ctx, cancel := context.WithCancel(context.Background())
		_ = db.Transact(ctx, func(tx *Tx) error {
			_, _ = tx.ExecContext(ctx, "INSERT INTO foo (id, str) VALUES (?, ?)", 1000+i, "x")
			cancel()
			return context.Canceled
		})
		cancel()
	}

	for _, s := range tracetest.SpanStubsFromReadOnlySpans(sr.Ended()) {
		if s.Name == "ROLLBACK" {
			assert.NotEqual(sql.ErrTxDone.Error(), s.Status.Description,
				"a rollback that never reached the database must not be reported as a failed one")
		}
	}
}

// A finalized transaction reports ErrTxDone without emitting a second span — the `defer tx.Rollback()`
// idiom alongside a successful Commit must stay free of spurious telemetry.
func TestTelemetry_NoSpanForFinalizedTransaction(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	db := newSQLiteDB(t)
	db.SetTracerProvider(tp)
	assert.NoError(db.Migrate(t.Name(), testdata.FS))

	tx, err := db.BeginTx(context.Background(), nil)
	assert.NoError(err)
	_, err = tx.Exec("INSERT INTO foo (id, str) VALUES (?, ?)", 102, "b")
	assert.NoError(err)
	assert.NoError(tx.Commit())
	// The idiomatic deferred rollback, now redundant.
	assert.Equal(sql.ErrTxDone, tx.Rollback())

	for _, s := range tracetest.SpanStubsFromReadOnlySpans(sr.Ended()) {
		assert.NotEqual("ROLLBACK", s.Name, "a rollback that never reached the database must emit no span")
	}
}

func TestTelemetry_DefaultsToGlobalProviders(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	// A freshly opened DB starts with the global OTEL providers and a discard logger — non-nil, but a no-op
	// until the application installs real providers, so queries run normally without any explicit setup.
	db := newSQLiteDB(t)
	tl := db.telemetry.Load()
	assert.NotNil(tl)
	assert.NotNil(tl.tracer)
	assert.NotNil(tl.meter)
	assert.NotNil(tl.logger)
	assert.NoError(db.Migrate(t.Name(), testdata.FS))

	var count int
	assert.NoError(db.QueryRow("SELECT COUNT(id) FROM foo").Scan(&count))
	assert.Equal(4, count)
}

func TestTelemetry_SetMeterProviderNilRevertsToGlobal(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	db := newSQLiteDB(t)
	db.SetMeterProvider(mp)
	db.SetMeterProvider(nil) // revert to the global (no-op) provider

	var rm metricdata.ResourceMetrics
	assert.NoError(reader.Collect(context.Background(), &rm))
	// Reverting unregisters the pool callback from the real reader, so it no longer sees sequel_ metrics.
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			assert.False(strings.HasPrefix(m.Name, "sequel_"), "unexpected metric %s after revert", m.Name)
		}
	}
}
