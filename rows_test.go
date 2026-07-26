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
	"testing"

	"github.com/microbus-io/testarossa"
)

// NextResultSet is latched like Next, and like Next it must not false-latch: running out of result sets
// is a nil rows.Err(), so a Transact that advances past the last result set still commits.
func TestRows_NextResultSetDoesNotFalseLatch(t *testing.T) {
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	db, err := Open("sqlite", dsn)
	assert.NoError(err)
	defer db.Close()
	_, err = db.Exec("CREATE TABLE rows_nrs (k INT)")
	assert.NoError(err)

	ctx := context.Background()
	err = db.Transact(ctx, func(tx *Tx) error {
		rows, qerr := tx.QueryContext(ctx, "SELECT k FROM rows_nrs")
		if qerr != nil {
			return qerr
		}
		defer rows.Close()
		for rows.Next() {
		}
		assert.False(rows.NextResultSet(), "a single-result-set query has no next result set")
		if _, xerr := tx.ExecContext(ctx, "INSERT INTO rows_nrs (k) VALUES (1)"); xerr != nil {
			return xerr
		}
		return nil
	})
	assert.NoError(err, "running out of result sets is not an error and must not doom the transaction")

	var n int
	assert.NoError(db.QueryRow("SELECT COUNT(*) FROM rows_nrs").Scan(&n))
	assert.Equal(1, n)
}
