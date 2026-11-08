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
	"database/sql"
	"testing"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/microbus-io/errors"
	"github.com/microbus-io/testarossa"
	mssql "github.com/microsoft/go-mssqldb"
)

// The database/sql sentinels pass through untouched: they are routine control flow, and callers are
// entitled to compare them with == exactly as they would against database/sql. Everything else is wrapped
// with a stack trace (see the companion test), which is why the exemption must be pinned.
func TestErrors_SentinelsPassThroughUnwrapped(t *testing.T) {
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	db, err := Open("sqlite", dsn)
	assert.NoError(err)
	defer db.Close()
	_, err = db.Exec("CREATE TABLE err_sentinel (k INT)")
	assert.NoError(err)

	var k int
	err = db.QueryRow("SELECT k FROM err_sentinel").Scan(&k)
	assert.True(err == sql.ErrNoRows, "ErrNoRows must remain ==-comparable, got %v", err) //nolint:errorlint // the identity comparison is the contract under test

	tx, err := db.Begin()
	assert.NoError(err)
	assert.NoError(tx.Rollback())
	assert.True(tx.Rollback() == sql.ErrTxDone, "ErrTxDone must remain ==-comparable") //nolint:errorlint // ditto
	assert.True(tx.Commit() == sql.ErrTxDone)                                          //nolint:errorlint // ditto
}

// Every non-sentinel operation error is wrapped with a stack trace at the API boundary, with the raw
// driver error intact underneath, so errors.Is/errors.As (and IsLockContentionError) see through the
// wrapper while the trace records where the failure surfaced.
func TestErrors_OperationErrorsCarryStackTrace(t *testing.T) {
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	db, err := Open("sqlite", dsn)
	assert.NoError(err)
	defer db.Close()

	_, err = db.Exec("SELECT no_such_column FROM no_such_table")
	assert.Error(err)
	var traced *errors.TracedError
	assert.True(errors.As(err, &traced), "operation errors carry a stack trace, got %T", err)
	assert.Contains(err.Error(), "no_such_table", "the wrapper must not alter the error message")
}

func TestErrors_IsLockContentionError(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	// Driver-native typed errors classified by code (the primary path).
	assert.True(IsLockContentionError(&pgconn.PgError{Code: "40P01"}))  // PG deadlock
	assert.True(IsLockContentionError(&pgconn.PgError{Code: "40001"}))  // PG/CRDB serialization
	assert.False(IsLockContentionError(&pgconn.PgError{Code: "23505"})) // PG unique violation
	// A typed error is classified by its code alone, whatever its message says
	assert.False(IsLockContentionError(&mysql.MySQLError{Number: 1062, Message: "Duplicate entry 'restart transaction' for key 'PRIMARY'"}))
	assert.False(IsLockContentionError(&pgconn.PgError{Code: "23505", Message: `duplicate key value: (note)=(deadlock detected)`}))
	assert.False(IsLockContentionError(mssql.Error{Number: 2627, Message: "duplicate key value is (database is locked)"}))
	assert.True(IsLockContentionError(&mysql.MySQLError{Number: 1213}))  // MySQL deadlock
	assert.True(IsLockContentionError(&mysql.MySQLError{Number: 1205}))  // MySQL lock wait timeout
	assert.False(IsLockContentionError(&mysql.MySQLError{Number: 1062})) // MySQL duplicate entry
	assert.True(IsLockContentionError(mssql.Error{Number: 1205}))        // SQL Server deadlock
	assert.True(IsLockContentionError(mssql.Error{Number: 1222}))        // SQL Server lock timeout
	assert.False(IsLockContentionError(mssql.Error{Number: 2627}))       // SQL Server PK violation
	// Typed errors are also found through a wrapped chain.
	assert.True(IsLockContentionError(errors.Join(errors.New("op failed"), &pgconn.PgError{Code: "40P01"})))

	// Substring fallback (drivers whose typed error isn't in the chain, e.g. text-only messages).
	retryable := []string{
		"SQLITE_BUSY: database is locked",                            // SQLite busy
		"database table is locked: database is deadlocked (6)",       // SQLite shared-cache (SQLITE_LOCKED)
		"SQLITE_LOCKED: database table is locked",                    // SQLite locked
		"Error 1213: Deadlock found when trying to get lock",         // MySQL
		"Error 1205: Lock wait timeout exceeded",                     // MySQL
		"ERROR: deadlock detected (SQLSTATE 40P01)",                  // PostgreSQL
		"mssql: Transaction was chosen as the deadlock victim",       // SQL Server 1205
		"mssql: Lock request time out period exceeded",               // SQL Server 1222
		"restart transaction: TransactionRetryWithProtoRefreshError", // CockroachDB
	}
	for _, msg := range retryable {
		assert.True(IsLockContentionError(errors.New(msg)), "should be retryable: %s", msg)
	}

	notRetryable := []string{
		"",
		"syntax error near ')'",
		"UNIQUE constraint failed: users.email",
		"connection refused",
		"no such table: users",
	}
	for _, msg := range notRetryable {
		if msg == "" {
			assert.False(IsLockContentionError(nil))
			continue
		}
		assert.False(IsLockContentionError(errors.New(msg)), "should not be retryable: %s", msg)
	}
}

func TestErrors_IsUndefinedTableError(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	// Driver-native typed errors classified by code.
	assert.True(IsUndefinedTableError(&pgconn.PgError{Code: "42P01"}))   // PG undefined table
	assert.False(IsUndefinedTableError(&pgconn.PgError{Code: "42703"}))  // PG undefined column
	assert.False(IsUndefinedTableError(&pgconn.PgError{Code: "40P01"}))  // PG deadlock
	assert.True(IsUndefinedTableError(&mysql.MySQLError{Number: 1146}))  // MySQL no such table
	assert.False(IsUndefinedTableError(&mysql.MySQLError{Number: 1054})) // MySQL unknown column
	assert.True(IsUndefinedTableError(mssql.Error{Number: 208}))         // SQL Server invalid object name
	assert.False(IsUndefinedTableError(mssql.Error{Number: 207}))        // SQL Server invalid column name
	// Typed errors are also found through a wrapped chain.
	assert.True(IsUndefinedTableError(errors.Join(errors.New("op failed"), &pgconn.PgError{Code: "42P01"})))
	// A typed error's code decides, whatever its message says.
	assert.False(IsUndefinedTableError(&pgconn.PgError{Code: "23505", Message: "no such table"}))
	// PostgreSQL and CockroachDB report a qualifier with no FROM-clause entry as 42P01 too, but no table is missing.
	assert.True(IsUndefinedTableError(&pgconn.PgError{Code: "42P01",
		Message: `relation "users" does not exist`, Routine: "parserOpenTable"}))
	assert.False(IsUndefinedTableError(&pgconn.PgError{Code: "42P01",
		Message: `missing FROM-clause entry for table "f"`, Routine: "errorMissingRTE"}))
	assert.False(IsUndefinedTableError(&pgconn.PgError{Code: "42P01",
		Message: `invalid reference to FROM-clause entry for table "users"`, Routine: "errorMissingRTE"}))
	assert.False(IsUndefinedTableError(&pgconn.PgError{Code: "42P01",
		Message: `fehlender Eintrag in FROM-Klausel für Tabelle »f«`, Routine: "errorMissingRTE"})) // Localized
	assert.False(IsUndefinedTableError(&pgconn.PgError{Code: "42P01",
		Message: `missing FROM-clause entry for table "f"`})) // No routine
	assert.False(IsUndefinedTableError(&pgconn.PgError{Code: "42P01",
		Message: `no data source matches prefix: f in this context`, Routine: "ResolveColumnItem"})) // CockroachDB
	assert.False(IsUndefinedTableError(&pgconn.PgError{Code: "42P01",
		Message: `no data source matches pattern: f.*`, Routine: "ResolveAllColumnsSelector"})) // CockroachDB
	assert.False(IsUndefinedTableError(&pgconn.PgError{Code: "42P01",
		Message: `no data source matches pattern: f.*`})) // No routine
	assert.True(IsUndefinedTableError(&pgconn.PgError{Code: "42P01",
		Message: `relation "nope" does not exist`, Routine: "NewUndefinedRelationError"})) // CockroachDB

	// Substring fallback.
	undefined := []string{
		"SQL logic error: no such table: users (1)",               // SQLite
		"Error 1146 (42S02): Table 'db.users' doesn't exist",      // MySQL, MariaDB
		`ERROR: relation "users" does not exist (SQLSTATE 42P01)`, // PostgreSQL
		"mssql: Invalid object name 'users'.",                     // SQL Server
	}
	for _, msg := range undefined {
		assert.True(IsUndefinedTableError(errors.New(msg)), "should be undefined table: %s", msg)
	}
	defined := []string{
		"syntax error near ')'",
		"connection refused",
		"SQL logic error: no such column: name (1)",
		"SQLITE_BUSY: database is locked",
		`ERROR: missing FROM-clause entry for table "f" (SQLSTATE 42P01)`, // PostgreSQL
		`ERROR: invalid reference to FROM-clause entry for table "users" (SQLSTATE 42P01)`, // PostgreSQL
		`ERROR: no data source matches prefix: f in this context (SQLSTATE 42P01)`,         // CockroachDB
		`ERROR: no data source matches pattern: f.* (SQLSTATE 42P01)`,                      // CockroachDB
	}
	for _, msg := range defined {
		assert.False(IsUndefinedTableError(errors.New(msg)), "should not be undefined table: %s", msg)
	}
	assert.False(IsUndefinedTableError(nil))
}
