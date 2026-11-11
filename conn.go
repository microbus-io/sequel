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
	"sync"
	"sync/atomic"
	"time"

	"github.com/microbus-io/errors"
)

/*
Conn is one connection taken from a [DB]'s pool and held until Close. Statements and transactions run on it one
after another without waiting for the pool between them, which is what it is for: a sequence of statements under a
busy pool waits for a connection once rather than once per statement.

Everything run on a Conn gets the same treatment as on the DB: virtual functions are expanded and placeholders
conformed, operations are instrumented, and the simulated round-trip delay ([DB.SimulateRTT]) applies. A Conn
satisfies [Executor], so code written against Executor runs on it unchanged. [Conn.Transact] runs its transaction
on this connection, with the same retries as [DB.Transact].

Close returns the connection to the pool as it is. Session state left on it - a session lock, a temporary table, a
session variable - outlives Close on PostgreSQL, CockroachDB, MySQL and MariaDB, where the next user of the pooled
connection inherits it; SQL Server resets the session when the connection is next used. Leave none. Close also
closes the statements prepared on the Conn.

A Conn never replaces its connection. Once the connection is lost, every later call returns [sql.ErrConnDone],
and the caller takes a new Conn from the DB. A call that finds the connection already gone returns
sql.ErrConnDone too, and one that loses it returns the error it met. A call whose context has already ended returns
the context's error and leaves the Conn usable. A transaction run by [Conn.Transact] whose context ends before it
commits ends the Conn, because the transaction may still be rolling back on the connection when Transact returns,
and on SQL Server the driver discards the connection after it.

While [Conn.Transact] runs, every other call on the Conn fails: statements inside the transaction go through its
[Tx]. Use a Conn from one goroutine at a time, and read a query's rows to the end or close them before running the
next statement.
*/
type Conn struct {
	conn *sql.Conn
	db   *DB
	// lost is set once the connection is known to be unusable. Every later call returns sql.ErrConnDone without
	// reaching database/sql, which matters after a transaction whose context ended: database/sql may still be rolling
	// it back on another goroutine, and may close the Conn when that rollback finishes.
	lost   atomic.Bool
	closed atomic.Bool
	// transacting is set while Transact runs. A statement on the Conn then would run inside the transaction without
	// its error recorded, and a BEGIN would commit it on MySQL or have its COMMIT commit it on PostgreSQL.
	transacting atomic.Bool
	// xactAbortOn is set on SQL Server once a transaction has turned XACT_ABORT on for the session. The next statement
	// outside a transaction turns it off first, and the next transaction need not turn it on.
	xactAbortOn atomic.Bool
	// stmts are the open statements prepared on the Conn, which Close closes. database/sql does not: one closed after
	// the connection went back to the pool would act on a connection someone else may be using.
	stmtsMu sync.Mutex
	stmts   map[*Stmt]struct{}
}

// Conn shadows sql.DB.Conn: it takes one connection from the pool and returns it as a [Conn], on which statements
// and transactions run with sequel's handling. The connection is held until [Conn.Close], so every Conn must be
// closed. The raw connection, which bypasses sequel entirely, remains reachable as db.DB.Conn.
func (db *DB) Conn(ctx context.Context) (*Conn, error) {
	conn, err := db.DB.Conn(ctx)
	if err != nil {
		return nil, traceErr(err)
	}
	return &Conn{conn: conn, db: db}, nil
}

// errConnTransacting is what a call on a Conn returns while its Transact runs.
var errConnTransacting = errors.New("a transaction is running on this connection: use its Tx")

// check reports why the Conn cannot take a call: it is lost, its Transact is running, or the context has ended. A
// statement with an ended context must not reach the driver: pgx reports it as driver.ErrBadConn, and database/sql
// then closes the connection.
func (c *Conn) check(ctx context.Context) error {
	if c.lost.Load() {
		return sql.ErrConnDone
	}
	if c.transacting.Load() {
		return errors.Trace(errConnTransacting)
	}
	if err := ctx.Err(); err != nil {
		return errors.Trace(err)
	}
	return nil
}

// ready is check for a statement, which on SQL Server first turns off the XACT_ABORT an earlier transaction left on.
func (c *Conn) ready(ctx context.Context) error {
	if err := c.check(ctx); err != nil {
		return err
	}
	if c.xactAbortOn.Load() {
		if err := setXactAbort(ctx, c.conn, c.db.rtt(), false); err != nil {
			return c.observe(ctx, traceErr(err))
		}
		c.xactAbortOn.Store(false)
	}
	return nil
}

// observe marks the connection lost when err says it is gone, and returns sql.ErrConnDone in its place, or the
// context's error when that has ended.
func (c *Conn) observe(ctx context.Context, err error) error {
	if err == nil || !connGone(err) {
		return err
	}
	c.lost.Store(true)
	if ctxErr := ctx.Err(); ctxErr != nil {
		return errors.Trace(ctxErr)
	}
	return sql.ErrConnDone
}

// connGone reports whether err says the connection is gone. driver.ErrBadConn is returned when the driver sent
// nothing, and database/sql closes a Conn whose driver returned it.
func connGone(err error) bool {
	return errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone)
}

// Exec runs a statement that returns no rows on the connection, like [DB.Exec].
func (c *Conn) Exec(query string, args ...any) (sql.Result, error) {
	return c.ExecContext(context.Background(), query, args...)
}

// ExecContext runs a statement that returns no rows on the connection, like [DB.ExecContext].
func (c *Conn) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if err := c.ready(ctx); err != nil {
		return nil, err
	}
	res, err := instrumentExec(c.db.telemetry.Load(), c.db.rtt(), ctx, lookupQuery(c.db.driverName, query),
		func(ctx context.Context, q string) (sql.Result, error) {
			return c.conn.ExecContext(ctx, q, args...)
		})
	return res, c.observe(ctx, err)
}

// Query runs a query on the connection, like [DB.Query].
func (c *Conn) Query(query string, args ...any) (*Rows, error) {
	return c.QueryContext(context.Background(), query, args...)
}

// QueryContext runs a query on the connection, like [DB.QueryContext]. Read the rows to the end or close them before
// running the next statement on the connection.
func (c *Conn) QueryContext(ctx context.Context, query string, args ...any) (*Rows, error) {
	if err := c.ready(ctx); err != nil {
		return nil, err
	}
	rows, err := instrumentExec(c.db.telemetry.Load(), c.db.rtt(), ctx, lookupQuery(c.db.driverName, query),
		func(ctx context.Context, q string) (*sql.Rows, error) {
			return c.conn.QueryContext(ctx, q, args...)
		})
	if err != nil {
		return nil, c.observe(ctx, err)
	}
	return &Rows{Rows: rows}, nil
}

// QueryRow runs a query expected to return at most one row on the connection, like [DB.QueryRow].
func (c *Conn) QueryRow(query string, args ...any) *Row {
	return c.QueryRowContext(context.Background(), query, args...)
}

// QueryRowContext runs a query expected to return at most one row on the connection, like [DB.QueryRowContext]. Scan
// the row before running the next statement on the connection.
func (c *Conn) QueryRowContext(ctx context.Context, query string, args ...any) *Row {
	if err := c.ready(ctx); err != nil {
		return &Row{shortErr: err}
	}
	row := instrumentQueryRow(c.db.telemetry.Load(), c.db.rtt(), ctx, lookupQuery(c.db.driverName, query), nil,
		func(ctx context.Context, q string) *sql.Row {
			return c.conn.QueryRowContext(ctx, q, args...)
		})
	row.conn, row.connCtx = c, ctx
	return row
}

// Prepare prepares a statement on the connection, like [DB.Prepare]. The returned [Stmt] runs on this connection only.
func (c *Conn) Prepare(query string) (*Stmt, error) {
	return c.PrepareContext(context.Background(), query)
}

// PrepareContext prepares a statement on the connection, like [DB.PrepareContext]. The returned [Stmt] runs on this
// connection only, fails like any call on the Conn once the Conn cannot take one, and is closed by [Conn.Close].
func (c *Conn) PrepareContext(ctx context.Context, query string) (*Stmt, error) {
	if err := c.ready(ctx); err != nil {
		return nil, err
	}
	pq := lookupQuery(c.db.driverName, query)
	sqlStmt, err := instrumentExec(c.db.telemetry.Load(), c.db.rtt(), ctx, pq,
		func(ctx context.Context, q string) (*sql.Stmt, error) {
			return c.conn.PrepareContext(ctx, q)
		})
	if err != nil {
		return nil, c.observe(ctx, err)
	}
	stmt := &Stmt{Stmt: sqlStmt, pq: pq, db: c.db, conn: c}
	c.stmtsMu.Lock()
	if c.stmts == nil {
		c.stmts = map[*Stmt]struct{}{}
	}
	c.stmts[stmt] = struct{}{}
	c.stmtsMu.Unlock()
	return stmt, nil
}

// forget drops a closed statement from the ones Close closes.
func (c *Conn) forget(stmt *Stmt) {
	c.stmtsMu.Lock()
	delete(c.stmts, stmt)
	c.stmtsMu.Unlock()
}

// PingContext verifies that the connection is still alive, like [DB.PingContext].
func (c *Conn) PingContext(ctx context.Context) error {
	if err := c.check(ctx); err != nil {
		return err
	}
	if err := simulateRTT(ctx, c.db.rtt()); err != nil {
		return errors.Trace(err)
	}
	return c.observe(ctx, traceErr(c.conn.PingContext(ctx)))
}

// InsertReturnID executes an INSERT statement on the connection and returns the auto-generated ID for the named ID
// column, like [DB.InsertReturnID].
func (c *Conn) InsertReturnID(ctx context.Context, idColumn string, stmt string, args ...any) (int64, error) {
	if err := c.check(ctx); err != nil {
		return 0, err
	}
	id, err := insertReturnID(ctx, c, c.db.driverName, idColumn, stmt, args...)
	return id, c.observe(ctx, err)
}

/*
Transact runs fn inside a transaction on this connection, committing on success and rolling back on error, exactly
as [DB.Transact] does: the same retries on lock contention, the same recording of the first statement error, and
on SQL Server the same SET XACT_ABORT ON, which the Conn's next statement outside a transaction turns off again.
Every attempt runs on this connection, and while it runs, every other call on the Conn fails, Transact included.

If the connection is lost, Transact returns an error instead of retrying elsewhere, and the Conn is done: every
later call returns [sql.ErrConnDone]. So is a Conn whose transaction's context ends before it commits.
*/
func (c *Conn) Transact(ctx context.Context, fn func(tx *Tx) error) error {
	if err := c.check(ctx); err != nil {
		return err
	}
	if !c.transacting.CompareAndSwap(false, true) {
		return errors.Trace(errConnTransacting)
	}
	defer c.transacting.Store(false)
	err := c.db.transact(ctx, c, fn)
	if c.lost.Load() && connGone(err) {
		// The loss is this connection's own: transactOnce marks it only from the transaction's own errors
		return c.observe(ctx, err)
	}
	return err
}

// DriverName is the name of the driver: "mysql", "mariadb", "pgx", "cockroachdb", "mssql" or "sqlite".
func (c *Conn) DriverName() string {
	return c.db.driverName
}

// UnpackQuery expands virtual functions and conforms arg placeholders for the driver, like [DB.UnpackQuery].
func (c *Conn) UnpackQuery(query string) (string, error) {
	return unpackQuery(c.db.driverName, query)
}

// Close closes the statements prepared on the Conn and returns the connection to the pool, as it is: see [Conn] on
// session state. It waits for a transaction still rolling back on the connection. Closing a lost Conn returns nil;
// closing a Conn twice returns [sql.ErrConnDone].
func (c *Conn) Close() error {
	if !c.closed.CompareAndSwap(false, true) {
		return sql.ErrConnDone
	}
	c.lost.Store(true)
	c.stmtsMu.Lock()
	stmts := c.stmts
	c.stmts = nil
	c.stmtsMu.Unlock()
	for stmt := range stmts {
		_ = stmt.Stmt.Close()
	}
	err := c.conn.Close()
	if errors.Is(err, sql.ErrConnDone) {
		// database/sql closes a Conn itself once the driver reports the connection bad
		return nil
	}
	return traceErr(err)
}

// setXactAbort turns SQL Server's XACT_ABORT on or off for the session, charged a round trip like any statement.
func setXactAbort(ctx context.Context, execer interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}, rtt time.Duration, on bool) error {
	_ = simulateRTT(ctx, rtt) // an expired context is reported by the Exec
	stmt := "SET XACT_ABORT OFF"
	if on {
		stmt = "SET XACT_ABORT ON"
	}
	_, err := execer.ExecContext(ctx, stmt)
	return err
}
