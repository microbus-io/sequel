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

	"github.com/microbus-io/errors"
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
	autoErr    bool  // set by Transact: record first statement error and short-circuit thereafter
	err        error // first recorded statement error (autoErr mode only)
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
	query, err := tx.UnpackQuery(query)
	if err != nil {
		return nil, tx.recordErr(errors.Trace(err))
	}
	res, err := tx.Tx.Exec(query, args...)
	return res, tx.recordErr(err)
}

// ExecContext shadows sql.Tx.ExecContext and conforms arg placeholders for the driver.
func (tx *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if err := tx.shortCircuit(); err != nil {
		return nil, err
	}
	query, err := tx.UnpackQuery(query)
	if err != nil {
		return nil, tx.recordErr(errors.Trace(err))
	}
	res, err := tx.Tx.ExecContext(ctx, query, args...)
	return res, tx.recordErr(err)
}

// Query shadows sql.Tx.Query and conforms arg placeholders for the driver.
func (tx *Tx) Query(query string, args ...any) (*sql.Rows, error) {
	if err := tx.shortCircuit(); err != nil {
		return nil, err
	}
	query, err := tx.UnpackQuery(query)
	if err != nil {
		return nil, tx.recordErr(errors.Trace(err))
	}
	rows, err := tx.Tx.Query(query, args...)
	return rows, tx.recordErr(err)
}

// QueryContext shadows sql.Tx.QueryContext and conforms arg placeholders for the driver.
func (tx *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if err := tx.shortCircuit(); err != nil {
		return nil, err
	}
	query, err := tx.UnpackQuery(query)
	if err != nil {
		return nil, tx.recordErr(errors.Trace(err))
	}
	rows, err := tx.Tx.QueryContext(ctx, query, args...)
	return rows, tx.recordErr(err)
}

// QueryRow shadows sql.Tx.QueryRow and conforms arg placeholders for the driver.
func (tx *Tx) QueryRow(query string, args ...any) *sql.Row {
	query, _ = tx.UnpackQuery(query)
	return tx.Tx.QueryRow(query, args...)
}

// QueryRowContext shadows sql.Tx.QueryRowContext and conforms arg placeholders for the driver.
func (tx *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	query, _ = tx.UnpackQuery(query)
	return tx.Tx.QueryRowContext(ctx, query, args...)
}

// Prepare shadows sql.Tx.Prepare and conforms arg placeholders for the driver.
func (tx *Tx) Prepare(query string) (*sql.Stmt, error) {
	if err := tx.shortCircuit(); err != nil {
		return nil, err
	}
	query, err := tx.UnpackQuery(query)
	if err != nil {
		return nil, tx.recordErr(errors.Trace(err))
	}
	stmt, err := tx.Tx.Prepare(query)
	return stmt, tx.recordErr(err)
}

// PrepareContext shadows sql.Tx.PrepareContext and conforms arg placeholders for the driver.
func (tx *Tx) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	if err := tx.shortCircuit(); err != nil {
		return nil, err
	}
	query, err := tx.UnpackQuery(query)
	if err != nil {
		return nil, tx.recordErr(errors.Trace(err))
	}
	stmt, err := tx.Tx.PrepareContext(ctx, query)
	return stmt, tx.recordErr(err)
}

// InsertReturnID executes an INSERT statement and returns the auto-generated ID for the named ID column.
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
