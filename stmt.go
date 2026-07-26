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
	"time"
)

/*
Stmt is a prepared statement that shadows sql.Stmt so that executing it stays on sequel's path: each
execution emits a span and a duration sample, is classified for lock contention, and pays the simulated
round-trip delay ([DB.SimulateRTT]). Inside a [DB.Transact] transaction, an execution error is recorded
into the transaction and subsequent statements short-circuit, exactly as for a statement issued through
[Tx] directly — a closure that ignores a prepared statement's error cannot commit partial work.

It embeds *sql.Stmt, so stmt.Exec(...)/stmt.Query(...)/stmt.Close() call sites are unchanged; only code
that explicitly stores the result of Prepare as *sql.Stmt needs adjustment (the same source-compat shape
as [Rows] and [Row]).

A Stmt prepared on a [DB] reads the pool's telemetry and simulated delay at each execution; a Stmt bound
to a [Tx] (from [Tx.Prepare] or [Tx.Stmt]) uses the snapshots the transaction captured when it began, so
one transaction runs at one consistent latency. Close is a passthrough: releasing the statement handle is
lifecycle, not caller-facing work.
*/
type Stmt struct {
	*sql.Stmt
	query      string // the unpacked (expanded and conformed) statement, for instrumentation
	driverName string
	db         *DB // for a DB-prepared Stmt: telemetry and simulated delay are read live per execution
	tx         *Tx // for a Tx-bound Stmt: latch, short-circuit and snapshots come from the transaction
}

// telemetry is the observability to instrument an execution with: the transaction's snapshot when bound
// to one, the pool's current value otherwise.
func (s *Stmt) telemetry() *telemetry {
	if s.tx != nil {
		return s.tx.t
	}
	return s.db.telemetry.Load()
}

// rttDelay is the simulated round-trip delay an execution pays: the transaction's captured value when
// bound to one, the pool's current value otherwise.
func (s *Stmt) rttDelay() time.Duration {
	if s.tx != nil {
		return s.tx.rtt
	}
	return s.db.rtt()
}

// shortCircuit mirrors Tx.shortCircuit for a transaction-bound statement, and is a no-op otherwise.
func (s *Stmt) shortCircuit() error {
	if s.tx != nil {
		return s.tx.shortCircuit()
	}
	return nil
}

// recordErr mirrors Tx.recordErr for a transaction-bound statement, and returns err unchanged otherwise.
func (s *Stmt) recordErr(err error) error {
	if s.tx != nil {
		return s.tx.recordErr(err)
	}
	return err
}

// rowsRecorder is the latch handed to a [Rows]/[Row] produced by this statement: the transaction's when
// bound to one, nil (pure passthrough) otherwise.
func (s *Stmt) rowsRecorder() func(error) error {
	if s.tx != nil {
		return s.tx.recordErr
	}
	return nil
}

// Exec shadows sql.Stmt.Exec.
func (s *Stmt) Exec(args ...any) (sql.Result, error) {
	return s.ExecContext(context.Background(), args...)
}

// ExecContext shadows sql.Stmt.ExecContext.
func (s *Stmt) ExecContext(ctx context.Context, args ...any) (sql.Result, error) {
	if err := s.shortCircuit(); err != nil {
		return nil, err
	}
	res, err := instrumentUnpacked(s.telemetry(), s.rttDelay(), ctx, s.driverName, s.query,
		func(ctx context.Context, _ string) (sql.Result, error) {
			return s.Stmt.ExecContext(ctx, args...)
		})
	return res, s.recordErr(err)
}

// Query shadows sql.Stmt.Query. It returns a [Rows], which embeds *sql.Rows so existing call sites are
// unchanged; inside a Transact transaction it latches row-read errors like any Tx query.
func (s *Stmt) Query(args ...any) (*Rows, error) {
	return s.QueryContext(context.Background(), args...)
}

// QueryContext shadows sql.Stmt.QueryContext. It returns a [Rows] (see Query).
func (s *Stmt) QueryContext(ctx context.Context, args ...any) (*Rows, error) {
	if err := s.shortCircuit(); err != nil {
		return nil, err
	}
	rows, err := instrumentUnpacked(s.telemetry(), s.rttDelay(), ctx, s.driverName, s.query,
		func(ctx context.Context, _ string) (*sql.Rows, error) {
			return s.Stmt.QueryContext(ctx, args...)
		})
	if err != nil {
		return nil, s.recordErr(err)
	}
	return &Rows{Rows: rows, recordErr: s.rowsRecorder()}, nil
}

// QueryRow shadows sql.Stmt.QueryRow. It returns a [Row], which embeds *sql.Row so existing
// QueryRow(...).Scan(...) call sites are unchanged.
func (s *Stmt) QueryRow(args ...any) *Row {
	return s.QueryRowContext(context.Background(), args...)
}

// QueryRowContext shadows sql.Stmt.QueryRowContext. It returns a [Row] (see QueryRow).
func (s *Stmt) QueryRowContext(ctx context.Context, args ...any) *Row {
	if err := s.shortCircuit(); err != nil {
		return &Row{shortErr: err}
	}
	return instrumentQueryRowUnpacked(s.telemetry(), s.rttDelay(), ctx, s.driverName, s.query, s.rowsRecorder(),
		func(ctx context.Context, _ string) *sql.Row {
			return s.Stmt.QueryRowContext(ctx, args...)
		})
}
