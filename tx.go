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
	"sync/atomic"
	"time"
)

// Tx is an in-progress database transaction that shadows sql.Tx methods
// to apply virtual function expansion and placeholder conforming.
//
// When created by [DB.Transact], a Tx records the first statement error and short-circuits subsequent
// statements (returning that error without touching the database). This guarantees a transaction cannot
// commit partial state when a caller forgets to check a statement's error, and it surfaces a deadlock
// (rather than masking it as a later "COMMIT has no corresponding BEGIN" on some drivers) so Transact
// can retry. A Tx obtained from [DB.BeginTx] does not do this — its statement methods behave exactly
// like the underlying sql.Tx.
type Tx struct {
	*sql.Tx
	driverName string
	autoErr    bool          // set by Transact: record first statement error and short-circuit thereafter
	err        error         // first recorded statement error (autoErr mode only)
	rtt        time.Duration // simulated round-trip delay, captured when the transaction began
	// done is set only by a Commit/Rollback that returned nil; see finalize. Atomic to match sql.Tx, whose
	// own finalization is a CAS on an atomic.Bool so that concurrent Commit/Rollback is race-free.
	done atomic.Bool
	// ctx is the context the transaction began with, kept so Commit/Rollback — which take none of their own
	// — can parent their spans to the transaction. sql.Tx stores its context for the same reason.
	ctx context.Context
	t   *telemetry // observability snapshot taken when the transaction began (may be nil)
}

// recordErr remembers the first statement error in Transact (autoErr) mode and returns err unchanged,
// so callers that do check the error see identical behavior.
func (tx *Tx) recordErr(err error) error {
	if err != nil && tx.autoErr && tx.err == nil {
		tx.err = err
	}
	return err
}

// shortCircuit reports the recorded error when the transaction has already failed in autoErr mode.
func (tx *Tx) shortCircuit() error {
	if tx.autoErr {
		return tx.err
	}
	return nil
}

// Err returns the first statement error recorded in Transact mode, or nil. Always nil for a Tx obtained
// from BeginTx.
func (tx *Tx) Err() error {
	return tx.err
}

// Exec shadows sql.Tx.Exec and conforms arg placeholders for the driver.
func (tx *Tx) Exec(query string, args ...any) (sql.Result, error) {
	if err := tx.shortCircuit(); err != nil {
		return nil, err
	}
	res, err := instrumentExec(tx.t, tx.rtt, context.Background(), tx.driverName, query,
		func(_ context.Context, q string) (sql.Result, error) {
			return tx.Tx.Exec(q, args...)
		})
	return res, tx.recordErr(err)
}

// ExecContext shadows sql.Tx.ExecContext and conforms arg placeholders for the driver.
func (tx *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if err := tx.shortCircuit(); err != nil {
		return nil, err
	}
	res, err := instrumentExec(tx.t, tx.rtt, ctx, tx.driverName, query,
		func(ctx context.Context, q string) (sql.Result, error) {
			return tx.Tx.ExecContext(ctx, q, args...)
		})
	return res, tx.recordErr(err)
}

// Query shadows sql.Tx.Query and conforms arg placeholders for the driver. It returns a [Rows], which
// embeds *sql.Rows so existing rows.Next()/rows.Scan()/rows.Err() call sites are unchanged. In Transact
// (autoErr) mode the returned Rows latches a mid-iteration Scan or streaming error into the transaction,
// so a closure that forgets rows.Err() cannot commit state read from a truncated result set.
func (tx *Tx) Query(query string, args ...any) (*Rows, error) {
	if err := tx.shortCircuit(); err != nil {
		return nil, err
	}
	rows, err := instrumentExec(tx.t, tx.rtt, context.Background(), tx.driverName, query,
		func(_ context.Context, q string) (*sql.Rows, error) {
			return tx.Tx.Query(q, args...)
		})
	if err != nil {
		return nil, tx.recordErr(err)
	}
	return &Rows{Rows: rows, recordErr: tx.recordErr}, nil
}

// QueryContext shadows sql.Tx.QueryContext and conforms arg placeholders for the driver. It returns a
// [Rows] (see Query) that latches row-iteration errors into the transaction in Transact (autoErr) mode.
func (tx *Tx) QueryContext(ctx context.Context, query string, args ...any) (*Rows, error) {
	if err := tx.shortCircuit(); err != nil {
		return nil, err
	}
	rows, err := instrumentExec(tx.t, tx.rtt, ctx, tx.driverName, query,
		func(ctx context.Context, q string) (*sql.Rows, error) {
			return tx.Tx.QueryContext(ctx, q, args...)
		})
	if err != nil {
		return nil, tx.recordErr(err)
	}
	return &Rows{Rows: rows, recordErr: tx.recordErr}, nil
}

// QueryRow shadows sql.Tx.QueryRow and conforms arg placeholders for the driver. It returns a [Row], which
// embeds *sql.Row so existing QueryRow(...).Scan(...) call sites are unchanged. In Transact (autoErr) mode
// the Row latches its Scan/Err error into the transaction (except sql.ErrNoRows — see [Row]).
func (tx *Tx) QueryRow(query string, args ...any) *Row {
	if err := tx.shortCircuit(); err != nil {
		return &Row{shortErr: err}
	}
	return instrumentQueryRow(tx.t, tx.rtt, context.Background(), tx.driverName, query, tx.recordErr,
		func(_ context.Context, q string) *sql.Row {
			return tx.Tx.QueryRow(q, args...)
		})
}

// QueryRowContext shadows sql.Tx.QueryRowContext and conforms arg placeholders for the driver. It returns a
// [Row] (see QueryRow) that latches its error into the transaction in Transact (autoErr) mode.
func (tx *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *Row {
	if err := tx.shortCircuit(); err != nil {
		return &Row{shortErr: err}
	}
	return instrumentQueryRow(tx.t, tx.rtt, ctx, tx.driverName, query, tx.recordErr,
		func(ctx context.Context, q string) *sql.Row {
			return tx.Tx.QueryRowContext(ctx, q, args...)
		})
}

// Prepare shadows sql.Tx.Prepare and conforms arg placeholders for the driver. It returns a [Stmt] bound
// to this transaction: in Transact mode an execution error is recorded and short-circuits later
// statements, exactly as for a statement issued through the Tx directly.
func (tx *Tx) Prepare(query string) (*Stmt, error) {
	// sql.Tx.Prepare is defined as PrepareContext(context.Background(), ...), so this loses nothing and
	// keeps one instrumented path.
	return tx.PrepareContext(context.Background(), query)
}

// PrepareContext shadows sql.Tx.PrepareContext and conforms arg placeholders for the driver. It returns a
// [Stmt] bound to this transaction (see Prepare).
func (tx *Tx) PrepareContext(ctx context.Context, query string) (*Stmt, error) {
	if err := tx.shortCircuit(); err != nil {
		return nil, err
	}
	var unpacked string
	sqlStmt, err := instrumentExec(tx.t, tx.rtt, ctx, tx.driverName, query,
		func(ctx context.Context, q string) (*sql.Stmt, error) {
			unpacked = q
			return tx.Tx.PrepareContext(ctx, q)
		})
	if err != nil {
		return nil, tx.recordErr(err)
	}
	return &Stmt{Stmt: sqlStmt, query: unpacked, driverName: tx.driverName, tx: tx}, nil
}

// Stmt shadows sql.Tx.Stmt: it binds a statement prepared on the [DB] to this transaction. The returned
// [Stmt] is transaction-bound, so in Transact mode its execution errors are recorded and short-circuit
// later statements — a prepared statement is not an escape hatch from the no-partial-commit guarantee.
func (tx *Tx) Stmt(stmt *Stmt) *Stmt {
	return tx.StmtContext(context.Background(), stmt)
}

// StmtContext shadows sql.Tx.StmtContext (see Stmt).
func (tx *Tx) StmtContext(ctx context.Context, stmt *Stmt) *Stmt {
	// The statement text was unpacked for the same pool, so it carries over; only the binding changes.
	return &Stmt{Stmt: tx.Tx.StmtContext(ctx, stmt.Stmt), query: stmt.query, driverName: tx.driverName, tx: tx}
}

/*
Commit shadows sql.Tx.Commit so the COMMIT round trip is instrumented like any statement: it emits its own
span (nested under the transaction), records into sequel_query_duration as operation COMMIT, is classified
for sequel_lock_contention, and pays any simulated round-trip delay ([DB.SimulateRTT]). Classification
matters here because a serialization failure most often surfaces at commit on CockroachDB and on PostgreSQL
under SERIALIZABLE.

A call that answers [sql.ErrTxDone] never reaches the database, so it emits no span, records no duration,
and pays no delay. Return values are identical to sql.Tx throughout.

Behavior is otherwise unchanged, including in Transact mode: Transact decides whether to commit before
calling this, and a commit error is not recorded into [Tx.Err].
*/
func (tx *Tx) Commit() error {
	return tx.finalize("COMMIT", tx.Tx.Commit)
}

// Rollback shadows sql.Tx.Rollback for the same reasons as [Tx.Commit], and handles an already-finalized
// transaction the same way. A rollback is a round trip whether or not anything went right, so it is
// instrumented while the transaction unwinds after a failure too.
func (tx *Tx) Rollback() error {
	return tx.finalize("ROLLBACK", tx.Tx.Rollback)
}

// finalize runs Commit or Rollback, skipping the round trip, span and delay once the transaction is known
// to be over.
//
// Only a nil error may set done: sql.Tx.Commit reports a cancelled context from an early return that does
// *not* finalize, so marking done on any completed call would answer a later Rollback with ErrTxDone where
// database/sql would really have rolled back. Being conservative costs at most one extra ErrTxDone call,
// which is free.
func (tx *Tx) finalize(op string, run func() error) error {
	if tx.done.Load() {
		return sql.ErrTxDone
	}
	err := instrumentTxEnd(tx.t, tx.rtt, tx.ctx, tx.driverName, op, run)
	if err == nil {
		tx.done.Store(true)
	}
	// traceErr keeps ErrTxDone bare — whichever path answered it — so == comparisons keep working.
	return traceErr(err)
}

// InsertReturnID executes an INSERT statement and returns the auto-generated ID for the named ID column.
// idColumn must be a plain identifier matching [A-Za-z_][A-Za-z0-9_]* — it is spliced into the statement
// on some drivers, so quoted or exotic column names are rejected rather than escaped.
func (tx *Tx) InsertReturnID(ctx context.Context, idColumn string, stmt string, args ...any) (int64, error) {
	if err := tx.shortCircuit(); err != nil {
		return 0, err
	}
	id, err := insertReturnID(ctx, tx, tx.driverName, idColumn, stmt, args...)
	return id, tx.recordErr(err)
}

// DriverName is the name of the driver: "mysql", "pgx", "cockroachdb", "mssql" or "sqlite".
func (tx *Tx) DriverName() string {
	return tx.driverName
}

// UnpackQuery expands virtual functions (e.g. NOW_UTC(), REGEXP_TEXT_SEARCH()) into
// driver-specific SQL expressions, and conforms arg placeholders
// to the syntax expected by the driver (e.g. ? to $1, $2 for PostgreSQL).
func (tx *Tx) UnpackQuery(query string) (string, error) {
	return unpackQuery(tx.driverName, query)
}
