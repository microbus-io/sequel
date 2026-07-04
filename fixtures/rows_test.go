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
	"testing"

	"github.com/microbus-io/sequel"
	"github.com/microbus-io/testarossa"
)

// TestTransact_RollsBackOnIgnoredScanError proves the Rows latch: a Transact closure that reads rows in a
// loop and IGNORES a mid-iteration Scan error cannot commit the write it makes afterward. This is the
// row-read counterpart to TestTransact_RollsBackAndSurfacesIgnoredError (which covers statement errors) -
// the gap that let a consumer commit state built from a truncated result set. The scan fails portably by
// scanning a non-numeric VARCHAR into an *int (database/sql's convertAssign rejects it on every driver).
// Proven with DML so the rollback holds on MySQL too.
func TestTransact_RollsBackOnIgnoredScanError(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE rows_scan (v VARCHAR(16))")
	assert.NoError(err)
	_, err = db.Exec("INSERT INTO rows_scan (v) VALUES ('abc')") // non-numeric: Scan into *int fails
	assert.NoError(err)

	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		rows, qerr := tx.QueryContext(ctx, "SELECT v FROM rows_scan")
		if qerr != nil {
			return qerr
		}
		for rows.Next() {
			var n int
			rows.Scan(&n) //nolint:errcheck // deliberately ignored: the latch must still abort the tx
		}
		rows.Close() //nolint:errcheck
		// Write a row the closure expects to persist; the latched scan error must roll it back. This
		// statement also short-circuits (tx already carries the recorded error), so it never executes.
		tx.ExecContext(ctx, "INSERT INTO rows_scan (v) VALUES ('written')") //nolint:errcheck
		return nil                                                          // ignore everything
	})
	// The recorded scan error surfaces even though the closure returned nil.
	assert.Error(err)

	// Only the original row survives; the closure's write did not commit.
	var n int
	assert.NoError(db.QueryRow("SELECT COUNT(*) FROM rows_scan").Scan(&n))
	assert.Equal(1, n)
}

// TestTransact_CommitsOnHealthyRowScan proves the latch does not fire spuriously: a closure that drains a
// result set fully and scans every row successfully (so the shadowed Next() latches a nil rows.Err() at
// end-of-iteration) still commits its write normally. Guards against the Rows wrapper aborting healthy
// transactions.
func TestTransact_CommitsOnHealthyRowScan(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE rows_ok (v INT)")
	assert.NoError(err)
	_, err = db.Exec("INSERT INTO rows_ok (v) VALUES (10)")
	assert.NoError(err)
	_, err = db.Exec("INSERT INTO rows_ok (v) VALUES (20)")
	assert.NoError(err)

	sum := 0
	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		rows, qerr := tx.QueryContext(ctx, "SELECT v FROM rows_ok ORDER BY v")
		if qerr != nil {
			return qerr
		}
		for rows.Next() {
			var v int
			if serr := rows.Scan(&v); serr != nil {
				rows.Close() //nolint:errcheck
				return serr
			}
			sum += v
		}
		if rerr := rows.Err(); rerr != nil {
			rows.Close() //nolint:errcheck
			return rerr
		}
		rows.Close() //nolint:errcheck
		_, ierr := tx.ExecContext(ctx, "INSERT INTO rows_ok (v) VALUES (?)", sum)
		return ierr
	})
	assert.NoError(err)
	assert.Equal(30, sum)

	// The computed row committed: three rows now (10, 20, and the 30 written from a clean read).
	var n, total int
	assert.NoError(db.QueryRow("SELECT COUNT(*), SUM(v) FROM rows_ok").Scan(&n, &total))
	assert.Equal(3, n)
	assert.Equal(60, total)
}
