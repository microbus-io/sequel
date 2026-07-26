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

	"github.com/microbus-io/errors"
	"github.com/microbus-io/testarossa"
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
