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
	"log/slog"
	"strings"
	"testing"

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
	}
	for _, c := range cases {
		op, table := parseOperation(c.query)
		assert.Equal(c.op, op, "op for %q", c.query)
		assert.Equal(c.table, table, "table for %q", c.query)
	}
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
	// Statement text is captured only in verbose mode; off by default.
	_, hasText := attrs["db.query.text"]
	assert.False(hasText)
}

func TestTelemetry_VerboseStatementAndLogs(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	var logBuf bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logBuf, &slog.HandlerOptions{Level: slog.LevelDebug}))

	db := newSQLiteDB(t)
	db.SetTracerProvider(tp)
	db.SetLogger(logger)
	db.SetVerbose(true)
	assert.NoError(db.Migrate(t.Name(), testdata.FS))

	var count int
	assert.NoError(db.QueryRow("SELECT COUNT(id) FROM foo").Scan(&count))

	// Span carries the parameterized statement text.
	var stmtText string
	for _, s := range tracetest.SpanStubsFromReadOnlySpans(sr.Ended()) {
		if s.Name == "SELECT foo" {
			for _, kv := range s.Attributes {
				if string(kv.Key) == "db.query.text" {
					stmtText = kv.Value.AsString()
				}
			}
		}
	}
	assert.Contains(stmtText, "SELECT COUNT(id) FROM foo")

	logs := logBuf.String()
	// Migration attempts logged at Info, queries at Debug in verbose mode.
	assert.Contains(logs, "running database migration")
	assert.Contains(logs, "sequel query")
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
		"sequel_migration_runs_total",
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

func TestTelemetry_NoProvidersIsNoOp(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newSQLiteDB(t)
	assert.Nil(db.telemetry.Load()) // nothing configured
	assert.NoError(db.Migrate(t.Name(), testdata.FS))

	var count int
	assert.NoError(db.QueryRow("SELECT COUNT(id) FROM foo").Scan(&count))
	assert.Equal(4, count)
	assert.Nil(db.telemetry.Load())
}

func TestTelemetry_DisableClearsInstruments(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	reader := sdkmetric.NewManualReader()
	mp := sdkmetric.NewMeterProvider(sdkmetric.WithReader(reader))

	db := newSQLiteDB(t)
	db.SetMeterProvider(mp)
	db.SetMeterProvider(nil) // disable

	t2 := db.telemetry.Load()
	assert.Nil(t2.queryDuration)
	assert.Nil(t2.poolRegistration)

	var rm metricdata.ResourceMetrics
	assert.NoError(reader.Collect(context.Background(), &rm))
	// After disabling, the pool callback is unregistered, so no sequel_ metrics remain.
	for _, sm := range rm.ScopeMetrics {
		for _, m := range sm.Metrics {
			assert.False(strings.HasPrefix(m.Name, "sequel_"), "unexpected metric %s after disable", m.Name)
		}
	}
}
