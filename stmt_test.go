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

// A prepared statement is not an escape hatch from the no-partial-commit guarantee: a Transact closure
// that ignores a Stmt execution error must not commit, and later statements must short-circuit.
func TestStmt_TransactLatchesIgnoredExecError(t *testing.T) {
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	db, err := Open("sqlite", dsn)
	assert.NoError(err)
	defer db.Close()
	_, err = db.Exec("CREATE TABLE stmt_latch (k INT PRIMARY KEY)")
	assert.NoError(err)

	ctx := context.Background()
	err = db.Transact(ctx, func(tx *Tx) error {
		stmt, perr := tx.PrepareContext(ctx, "INSERT INTO stmt_latch (k) VALUES (?)")
		if perr != nil {
			return perr
		}
		defer stmt.Close()
		stmt.ExecContext(ctx, 1) //nolint:errcheck
		stmt.ExecContext(ctx, 1) //nolint:errcheck // duplicate PK: fails, and is deliberately ignored
		// The recorded error must short-circuit this statement — it would otherwise succeed and commit.
		stmt.ExecContext(ctx, 2) //nolint:errcheck
		return nil               // ignore everything
	})
	assert.Error(err, "the ignored Stmt error surfaces even though the closure returned nil")

	var n int
	assert.NoError(db.QueryRow("SELECT COUNT(*) FROM stmt_latch").Scan(&n))
	assert.Equal(0, n, "no partial work commits when a prepared statement's error is ignored")
}

// Tx.Stmt binds a DB-prepared statement to the transaction, and the binding carries the latch: the same
// no-partial-commit guarantee as Tx.Prepare.
func TestStmt_TxStmtBindingLatches(t *testing.T) {
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	db, err := Open("sqlite", dsn)
	assert.NoError(err)
	defer db.Close()
	_, err = db.Exec("CREATE TABLE stmt_bind (k INT PRIMARY KEY)")
	assert.NoError(err)

	prepared, err := db.Prepare("INSERT INTO stmt_bind (k) VALUES (?)")
	assert.NoError(err)
	defer prepared.Close()

	ctx := context.Background()
	err = db.Transact(ctx, func(tx *Tx) error {
		stmt := tx.StmtContext(ctx, prepared)
		defer stmt.Close()
		stmt.ExecContext(ctx, 1) //nolint:errcheck
		stmt.ExecContext(ctx, 1) //nolint:errcheck // duplicate PK, ignored
		return nil
	})
	assert.Error(err)

	var n int
	assert.NoError(db.QueryRow("SELECT COUNT(*) FROM stmt_bind").Scan(&n))
	assert.Equal(0, n)

	// The DB-level statement is unaffected by the transaction's doom and remains usable.
	_, err = prepared.ExecContext(ctx, 7)
	assert.NoError(err)
	assert.NoError(db.QueryRow("SELECT COUNT(*) FROM stmt_bind").Scan(&n))
	assert.Equal(1, n)
}

// A healthy prepared statement must not trip the latch: prepare, execute, query and scan through Stmt in
// a Transact, and the transaction commits normally.
func TestStmt_HealthyUseCommits(t *testing.T) {
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	db, err := Open("sqlite", dsn)
	assert.NoError(err)
	defer db.Close()
	_, err = db.Exec("CREATE TABLE stmt_ok (k INT PRIMARY KEY)")
	assert.NoError(err)

	ctx := context.Background()
	sum := 0
	err = db.Transact(ctx, func(tx *Tx) error {
		ins, perr := tx.PrepareContext(ctx, "INSERT INTO stmt_ok (k) VALUES (?)")
		if perr != nil {
			return perr
		}
		defer ins.Close()
		for k := 1; k <= 3; k++ {
			if _, xerr := ins.ExecContext(ctx, k); xerr != nil {
				return xerr
			}
		}
		sel, perr := tx.PrepareContext(ctx, "SELECT k FROM stmt_ok ORDER BY k")
		if perr != nil {
			return perr
		}
		defer sel.Close()
		rows, qerr := sel.QueryContext(ctx)
		if qerr != nil {
			return qerr
		}
		defer rows.Close()
		for rows.Next() {
			var k int
			if serr := rows.Scan(&k); serr != nil {
				return serr
			}
			sum += k
		}
		return rows.Err()
	})
	assert.NoError(err)
	assert.Equal(6, sum)

	var n int
	assert.NoError(db.QueryRow("SELECT COUNT(*) FROM stmt_ok").Scan(&n))
	assert.Equal(3, n, "the healthy transaction committed")
}

// A Stmt.QueryRow on a doomed transaction short-circuits: the recorded first error surfaces from Scan
// without touching the database.
func TestStmt_QueryRowShortCircuits(t *testing.T) {
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	db, err := Open("sqlite", dsn)
	assert.NoError(err)
	defer db.Close()
	_, err = db.Exec("CREATE TABLE stmt_short (k INT PRIMARY KEY)")
	assert.NoError(err)

	ctx := context.Background()
	err = db.Transact(ctx, func(tx *Tx) error {
		stmt, perr := tx.PrepareContext(ctx, "SELECT COUNT(*) FROM stmt_short")
		if perr != nil {
			return perr
		}
		defer stmt.Close()
		tx.ExecContext(ctx, "INSERT INTO no_such_table (k) VALUES (1)") //nolint:errcheck // dooms the tx
		var n int
		serr := stmt.QueryRowContext(ctx).Scan(&n)
		assert.Error(serr)
		assert.Contains(serr.Error(), "no_such_table", "the first error surfaces, not a cascade")
		return nil
	})
	assert.Error(err)
}
