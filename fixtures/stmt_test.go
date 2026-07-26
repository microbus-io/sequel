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

// TestTransact_RollsBackOnIgnoredPreparedStmtError proves that a prepared statement is not an escape
// hatch from the no-partial-commit guarantee: a Transact closure that executes through a Stmt and ignores
// its error cannot commit, on any engine. This matters most on MySQL and SQLite, which do not abort a
// transaction server-side on a statement error — there, only the latch stands between an ignored error
// and a half-committed transaction.
func TestTransact_RollsBackOnIgnoredPreparedStmtError(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE stmt_prep (k INT PRIMARY KEY)")
	assert.NoError(err)

	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		stmt, perr := tx.PrepareContext(ctx, "INSERT INTO stmt_prep (k) VALUES (?)")
		if perr != nil {
			return perr
		}
		defer stmt.Close()
		stmt.ExecContext(ctx, 1) //nolint:errcheck
		stmt.ExecContext(ctx, 1) //nolint:errcheck // duplicate PK: fails, and is deliberately ignored
		// Short-circuited by the recorded error; would otherwise succeed and be committed below.
		stmt.ExecContext(ctx, 2) //nolint:errcheck
		return nil               // ignore everything
	})
	assert.Error(err, "the ignored Stmt error surfaces even though the closure returned nil")

	var n int
	assert.NoError(db.QueryRow("SELECT COUNT(*) FROM stmt_prep").Scan(&n))
	assert.Equal(0, n, "no partial work commits when a prepared statement's error is ignored")
}
