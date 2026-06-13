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
	"errors"
	"testing"

	mssql "github.com/denisenkom/go-mssqldb"
	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/microbus-io/testarossa"
)

func TestDB_IsLockContentionError(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	// Driver-native typed errors classified by code (the primary path).
	assert.True(IsLockContentionError(&pgconn.PgError{Code: "40P01"}))                  // PG deadlock
	assert.True(IsLockContentionError(&pgconn.PgError{Code: "40001"}))                  // PG/CRDB serialization
	assert.False(IsLockContentionError(&pgconn.PgError{Code: "23505"}))                 // PG unique violation
	assert.True(IsLockContentionError(&mysql.MySQLError{Number: 1213}))                 // MySQL deadlock
	assert.True(IsLockContentionError(&mysql.MySQLError{Number: 1205}))                 // MySQL lock wait timeout
	assert.False(IsLockContentionError(&mysql.MySQLError{Number: 1062}))                // MySQL duplicate entry
	assert.True(IsLockContentionError(mssql.Error{Number: 1205}))                       // SQL Server deadlock
	assert.True(IsLockContentionError(mssql.Error{Number: 1222}))                       // SQL Server lock timeout
	assert.False(IsLockContentionError(mssql.Error{Number: 2627}))                      // SQL Server PK violation
	// Typed errors are also found through a wrapped chain.
	assert.True(IsLockContentionError(errors.Join(errors.New("op failed"), &pgconn.PgError{Code: "40P01"})))

	// Substring fallback (drivers whose typed error isn't in the chain, e.g. text-only messages).
	retryable := []string{
		"SQLITE_BUSY: database is locked",                          // SQLite busy
		"database table is locked: database is deadlocked (6)",     // SQLite shared-cache (SQLITE_LOCKED)
		"SQLITE_LOCKED: database table is locked",                  // SQLite locked
		"Error 1213: Deadlock found when trying to get lock",       // MySQL
		"Error 1205: Lock wait timeout exceeded",                   // MySQL
		"ERROR: deadlock detected (SQLSTATE 40P01)",                // PostgreSQL
		"mssql: Transaction was chosen as the deadlock victim",     // SQL Server 1205
		"mssql: Lock request time out period exceeded",             // SQL Server 1222
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
