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

package fixtures

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/microbus-io/sequel"
	"github.com/microbus-io/testarossa"
)

// TestTransact_RollsBackOnIgnoredQueryRowError proves the Row latch: a Transact closure that ignores a
// QueryRow(...).Scan error cannot commit the write it makes afterward. This is the single-row counterpart
// to TestTransact_RollsBackOnIgnoredScanError. The scan fails portably by scanning a non-numeric VARCHAR
// into an *int (database/sql's convertAssign rejects it on every driver).
func TestTransact_RollsBackOnIgnoredQueryRowError(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE row_scan (v VARCHAR(16))")
	assert.NoError(err)
	_, err = db.Exec("INSERT INTO row_scan (v) VALUES ('abc')") // non-numeric: Scan into *int fails
	assert.NoError(err)

	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		var n int
		tx.QueryRowContext(ctx, "SELECT v FROM row_scan").Scan(&n) //nolint:errcheck // deliberately ignored
		// The latched error must roll this back; it also short-circuits, so it never executes.
		tx.ExecContext(ctx, "INSERT INTO row_scan (v) VALUES ('written')") //nolint:errcheck
		return nil                                                         // ignore everything
	})
	// The recorded scan error surfaces even though the closure returned nil.
	assert.Error(err)

	// Only the original row survives; the closure's write did not commit.
	var n int
	assert.NoError(db.QueryRow("SELECT COUNT(*) FROM row_scan").Scan(&n))
	assert.Equal(1, n)
}

// TestTransact_CommitsOnErrNoRows pins the sql.ErrNoRows exemption. Unlike a Rows iteration - where an
// empty result set is simply Next() returning false, never an error - "no row" reaches a QueryRow caller
// AS an error, and handling it is routine control flow. Latching it would doom every transaction that
// legitimately defaults a missing row, so the exemption is load-bearing, not a nicety.
func TestTransact_CommitsOnErrNoRows(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE row_none (id INT, v INT)")
	assert.NoError(err)

	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		var v int
		serr := tx.QueryRowContext(ctx, "SELECT v FROM row_none WHERE id=?", 42).Scan(&v)
		if errors.Is(serr, sql.ErrNoRows) {
			v = 7 // absence is the expected case; default and carry on
		} else if serr != nil {
			return serr
		}
		_, ierr := tx.ExecContext(ctx, "INSERT INTO row_none (id, v) VALUES (?, ?)", 42, v)
		return ierr
	})
	assert.NoError(err)

	// The defaulted row committed: the ErrNoRows did not abort the transaction.
	var v int
	assert.NoError(db.QueryRow("SELECT v FROM row_none WHERE id=?", 42).Scan(&v))
	assert.Equal(7, v)
}

// TestTransact_QueryRowShortCircuits proves QueryRow honors the short-circuit guard every sibling
// statement method already had. After a statement fails, the transaction is doomed; a QueryRow that still
// went to the database would come back with a driver cascade error ("current transaction is aborted" on
// PostgreSQL) and mask the original. The Scan must instead report the FIRST error verbatim.
func TestTransact_QueryRowShortCircuits(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE row_short (v INT)")
	assert.NoError(err)

	var first, afterScan error
	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		// Fail a statement to doom the transaction.
		_, first = tx.ExecContext(ctx, "INSERT INTO no_such_table_row_short (v) VALUES (1)")
		var n int
		afterScan = tx.QueryRowContext(ctx, "SELECT COUNT(*) FROM row_short").Scan(&n)
		return nil // ignore both; the recorded error must still surface
	})
	assert.Error(err)
	assert.Error(first)
	// The short-circuited Scan reports the original error, not a driver cascade error.
	assert.Equal(first, afterScan)
}
