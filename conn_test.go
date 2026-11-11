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
	"database/sql/driver"
	"testing"

	"github.com/microbus-io/errors"
	"github.com/microbus-io/testarossa"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestConn_ClosedReturnsErrConnDone: every call on a closed Conn returns the bare sql.ErrConnDone, statements
// prepared on it included, and so does a second Close.
func TestConn_ClosedReturnsErrConnDone(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newSQLiteDB(t)
	_, err := db.Exec("CREATE TABLE conn_closed (id INTEGER PRIMARY KEY AUTOINCREMENT, v INT)")
	assert.NoError(err)
	conn, err := db.Conn(ctx)
	assert.Must(assert.NoError(err))
	stmt, err := conn.Prepare("SELECT COUNT(*) FROM conn_closed WHERE v=?")
	assert.Must(assert.NoError(err))
	assert.NoError(conn.Close())

	isDone := func(op string, err error) {
		t.Helper()
		assert.True(err == sql.ErrConnDone, "%s: got %v", op, err) //nolint:errorlint // the identity comparison is the contract under test
	}
	_, err = conn.Exec("INSERT INTO conn_closed (v) VALUES (?)", 1)
	isDone("Exec", err)
	_, err = conn.QueryContext(ctx, "SELECT v FROM conn_closed")
	isDone("QueryContext", err)
	var n int
	isDone("QueryRow", conn.QueryRow("SELECT COUNT(*) FROM conn_closed").Scan(&n))
	_, err = conn.PrepareContext(ctx, "SELECT v FROM conn_closed")
	isDone("PrepareContext", err)
	isDone("Stmt.QueryRow", stmt.QueryRow(1).Scan(&n))
	_, err = stmt.Exec(1)
	isDone("Stmt.Exec", err)
	_, err = conn.InsertReturnID(ctx, "id", "INSERT INTO conn_closed (v) VALUES (?)", 1)
	isDone("InsertReturnID", err)
	isDone("PingContext", conn.PingContext(ctx))
	isDone("Transact", conn.Transact(ctx, func(tx *Tx) error { return nil }))
	isDone("second Close", conn.Close())
	assert.NoError(stmt.Close())

	assert.NoError(db.QueryRow("SELECT COUNT(*) FROM conn_closed").Scan(&n))
	assert.Zero(n, "a call on the closed Conn reached the database")
}

// TestConn_BadConnectionIsLost: a driver.ErrBadConn means the driver found the connection gone and sent nothing, so
// the Conn reports sql.ErrConnDone for it and for every later call, or the context's error when that has ended. Other
// errors leave the Conn usable.
func TestConn_BadConnectionIsLost(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newSQLiteDB(t)
	conn, err := db.Conn(ctx)
	assert.Must(assert.NoError(err))
	defer conn.Close()

	other := errors.New("some failure")
	assert.True(errors.Is(conn.observe(ctx, other), other))
	_, err = conn.ExecContext(ctx, "SELECT 1")
	assert.NoError(err)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	err = conn.observe(cancelled, errors.Trace(driver.ErrBadConn))
	assert.True(errors.Is(err, context.Canceled), "got %v", err)
	_, err = conn.ExecContext(ctx, "SELECT 1")
	assert.True(err == sql.ErrConnDone, "got %v", err) //nolint:errorlint // the identity comparison is the contract under test
	assert.NoError(conn.Close(), "closing a lost Conn")
}

// TestConn_ErrConnDoneFromTheDriverIsBare: when database/sql closes the connection underneath a Conn, as it does after
// a driver.ErrBadConn, every path reports the bare sql.ErrConnDone and marks the Conn lost, the ones that run the
// statement through other methods or report at Scan included.
func TestConn_ErrConnDoneFromTheDriverIsBare(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newSQLiteDB(t)
	_, err := db.Exec("CREATE TABLE conn_bare (id INTEGER PRIMARY KEY AUTOINCREMENT, v INT)")
	assert.NoError(err)
	isDone := func(op string, err error) {
		t.Helper()
		assert.True(err == sql.ErrConnDone, "%s: got %v", op, err) //nolint:errorlint // the identity comparison is the contract under test
	}
	for _, op := range []string{"InsertReturnID", "QueryRow", "Stmt.QueryRow", "Stmt.Exec"} {
		conn, err := db.Conn(ctx)
		assert.Must(assert.NoError(err))
		stmt, err := conn.PrepareContext(ctx, "SELECT COUNT(*) FROM conn_bare WHERE v=?")
		assert.Must(assert.NoError(err))
		assert.NoError(conn.conn.Close()) // as database/sql does, without the Conn knowing
		var n int
		switch op {
		case "InsertReturnID":
			_, err = conn.InsertReturnID(ctx, "id", "INSERT INTO conn_bare (v) VALUES (?)", 1)
		case "QueryRow":
			err = conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM conn_bare").Scan(&n)
		case "Stmt.QueryRow":
			err = stmt.QueryRowContext(ctx, 1).Scan(&n)
		case "Stmt.Exec":
			_, err = stmt.ExecContext(ctx, 1)
		}
		isDone(op, err)
		assert.True(conn.lost.Load(), "%s did not mark the Conn lost", op)
		assert.NoError(conn.Close())
	}
}

// TestConn_EndedContextLeavesTheConnUsable: a call whose context has already ended returns the context's error
// without reaching the driver - pgx would report it as a bad connection, and database/sql discard the connection.
func TestConn_EndedContextLeavesTheConnUsable(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newSQLiteDB(t)
	_, err := db.Exec("CREATE TABLE conn_ended (id INTEGER PRIMARY KEY AUTOINCREMENT, v INT)")
	assert.NoError(err)
	conn, err := db.Conn(ctx)
	assert.Must(assert.NoError(err))
	defer conn.Close()
	stmt, err := conn.PrepareContext(ctx, "SELECT COUNT(*) FROM conn_ended WHERE v=?")
	assert.Must(assert.NoError(err))

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	isCancelled := func(op string, err error) {
		t.Helper()
		assert.True(errors.Is(err, context.Canceled), "%s: got %v", op, err)
	}
	var n int
	_, err = conn.ExecContext(cancelled, "INSERT INTO conn_ended (v) VALUES (?)", 1)
	isCancelled("ExecContext", err)
	_, err = conn.QueryContext(cancelled, "SELECT v FROM conn_ended")
	isCancelled("QueryContext", err)
	isCancelled("QueryRowContext", conn.QueryRowContext(cancelled, "SELECT COUNT(*) FROM conn_ended").Scan(&n))
	_, err = conn.PrepareContext(cancelled, "SELECT v FROM conn_ended")
	isCancelled("PrepareContext", err)
	isCancelled("Stmt.QueryRowContext", stmt.QueryRowContext(cancelled, 1).Scan(&n))
	_, err = conn.InsertReturnID(cancelled, "id", "INSERT INTO conn_ended (v) VALUES (?)", 1)
	isCancelled("InsertReturnID", err)
	isCancelled("PingContext", conn.PingContext(cancelled))
	isCancelled("Transact", conn.Transact(cancelled, func(tx *Tx) error { return nil }))

	assert.False(conn.lost.Load())
	assert.NoError(conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM conn_ended").Scan(&n))
	assert.Zero(n)
}

// TestConn_RefusesCallsWhileTransacting: while Transact runs, every call on the Conn fails, a nested Transact
// included: on the same connection it would run inside the transaction, and a nested BEGIN commits the outer
// transaction on MySQL.
func TestConn_RefusesCallsWhileTransacting(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newSQLiteDB(t)
	_, err := db.Exec("CREATE TABLE conn_nested (v INT)")
	assert.NoError(err)
	conn, err := db.Conn(ctx)
	assert.Must(assert.NoError(err))
	defer conn.Close()
	stmt, err := conn.PrepareContext(ctx, "INSERT INTO conn_nested (v) VALUES (?)")
	assert.Must(assert.NoError(err))

	err = conn.Transact(ctx, func(tx *Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO conn_nested (v) VALUES (?)", 1)
		assert.NoError(err)
		_, err = conn.ExecContext(ctx, "INSERT INTO conn_nested (v) VALUES (?)", 2)
		assert.True(errors.Is(err, errConnTransacting), "ExecContext: got %v", err)
		_, err = stmt.ExecContext(ctx, 3)
		assert.True(errors.Is(err, errConnTransacting), "Stmt.ExecContext: got %v", err)
		err = conn.Transact(ctx, func(tx *Tx) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO conn_nested (v) VALUES (?)", 4)
			return err
		})
		assert.True(errors.Is(err, errConnTransacting), "Transact: got %v", err)
		return err
	})
	assert.True(errors.Is(err, errConnTransacting), "got %v", err)

	var n int
	assert.NoError(conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM conn_nested").Scan(&n))
	assert.Zero(n, "the refused transaction committed")
	assert.NoError(conn.Transact(ctx, func(tx *Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO conn_nested (v) VALUES (?)", 5)
		return err
	}))
	_, err = stmt.ExecContext(ctx, 6)
	assert.NoError(err)
	assert.NoError(conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM conn_nested").Scan(&n))
	assert.Equal(2, n)
}

// TestConn_TransactKeepsAnotherConnsLoss: a closure returning another Conn's sql.ErrConnDone does not end this Conn,
// and its error comes back as it was.
func TestConn_TransactKeepsAnotherConnsLoss(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newSQLiteDB(t)
	conn, err := db.Conn(ctx)
	assert.Must(assert.NoError(err))
	defer conn.Close()
	other, err := db.Conn(ctx)
	assert.Must(assert.NoError(err))
	assert.NoError(other.Close())

	err = conn.Transact(ctx, func(tx *Tx) error {
		_, err := other.ExecContext(ctx, "SELECT 1")
		return errors.New("the other connection: %w", err)
	})
	assert.True(errors.Is(err, sql.ErrConnDone), "got %v", err)
	assert.True(err != sql.ErrConnDone, "the closure's error was replaced") //nolint:errorlint // identity is the point
	assert.False(conn.lost.Load())
	_, err = conn.ExecContext(ctx, "SELECT 1")
	assert.NoError(err)
}

// TestConn_CloseClosesItsStatements: Close closes the statements prepared on the Conn before the connection goes back
// to the pool, where closing one later would act on a connection someone else may be using.
func TestConn_CloseClosesItsStatements(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newSQLiteDB(t)
	conn, err := db.Conn(ctx)
	assert.Must(assert.NoError(err))
	kept, err := conn.PrepareContext(ctx, "SELECT 1")
	assert.Must(assert.NoError(err))
	closed, err := conn.PrepareContext(ctx, "SELECT 2")
	assert.Must(assert.NoError(err))
	assert.NoError(closed.Close())
	assert.Equal(1, len(conn.stmts), "a closed statement is forgotten")

	assert.NoError(conn.Close())
	_, err = kept.Stmt.ExecContext(ctx) // below sequel, which would refuse it
	assert.Error(err)
	assert.Contains(err.Error(), "statement is closed")
	assert.NoError(kept.Close(), "closing it again is harmless")
}

// TestConn_Telemetry: statements and transactions on a Conn are instrumented like the DB's.
func TestConn_Telemetry(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))
	db := newSQLiteDB(t)
	db.SetTracerProvider(tp)
	_, err := db.Exec("CREATE TABLE conn_spans (v INT)")
	assert.NoError(err)

	conn, err := db.Conn(ctx)
	assert.Must(assert.NoError(err))
	defer conn.Close()
	_, err = conn.ExecContext(ctx, "INSERT INTO conn_spans (v) VALUES (?)", 1)
	assert.NoError(err)
	assert.NoError(conn.Transact(ctx, func(tx *Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE conn_spans SET v=?", 2)
		return err
	}))

	names := map[string]string{}
	parents := map[string]string{}
	for _, s := range tracetest.SpanStubsFromReadOnlySpans(sr.Ended()) {
		names[s.SpanContext.SpanID().String()] = s.Name
		parents[s.Name] = s.Parent.SpanID().String()
	}
	for _, name := range []string{"INSERT conn_spans", "transact", "BEGIN", "UPDATE conn_spans", "COMMIT"} {
		_, ok := parents[name]
		assert.True(ok, "no %s span", name)
	}
	for _, name := range []string{"BEGIN", "COMMIT"} {
		assert.Equal("transact", names[parents[name]], "%s nests under the transaction", name)
	}
}
