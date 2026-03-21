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
type Tx struct {
	*sql.Tx
	driverName string
}

// Exec shadows sql.Tx.Exec and conforms arg placeholders for the driver.
func (tx *Tx) Exec(query string, args ...any) (sql.Result, error) {
	query, err := tx.UnpackQuery(query)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return tx.Tx.Exec(query, args...)
}

// ExecContext shadows sql.Tx.ExecContext and conforms arg placeholders for the driver.
func (tx *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	query, err := tx.UnpackQuery(query)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return tx.Tx.ExecContext(ctx, query, args...)
}

// Query shadows sql.Tx.Query and conforms arg placeholders for the driver.
func (tx *Tx) Query(query string, args ...any) (*sql.Rows, error) {
	query, err := tx.UnpackQuery(query)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return tx.Tx.Query(query, args...)
}

// QueryContext shadows sql.Tx.QueryContext and conforms arg placeholders for the driver.
func (tx *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	query, err := tx.UnpackQuery(query)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return tx.Tx.QueryContext(ctx, query, args...)
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
	query, err := tx.UnpackQuery(query)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return tx.Tx.Prepare(query)
}

// PrepareContext shadows sql.Tx.PrepareContext and conforms arg placeholders for the driver.
func (tx *Tx) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	query, err := tx.UnpackQuery(query)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return tx.Tx.PrepareContext(ctx, query)
}

// InsertReturnID executes an INSERT statement and returns the auto-generated ID for the named ID column.
func (tx *Tx) InsertReturnID(ctx context.Context, idColumn string, stmt string, args ...any) (int64, error) {
	return insertReturnID(ctx, tx, tx.driverName, idColumn, stmt, args...)
}

// DriverName is the name of the driver: "mysql", "pgx", "mssql" or "sqlite".
func (tx *Tx) DriverName() string {
	return tx.driverName
}

// UnpackQuery expands virtual functions (e.g. NOW_UTC(), REGEXP_TEXT_SEARCH()) into
// driver-specific SQL expressions, and conforms arg placeholders
// to the syntax expected by the driver (e.g. ? to $1, $2 for PostgreSQL).
func (tx *Tx) UnpackQuery(query string) (string, error) {
	return unpackQuery(tx.driverName, query)
}
