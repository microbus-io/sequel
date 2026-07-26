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
	"sync"
	"testing"

	"github.com/microbus-io/testarossa"
)

// A failed Commit must not mark the transaction finalized, or a later Rollback answers ErrTxDone where
// database/sql would really have rolled back.
//
// Asserts the flag, not the later Rollback's error, deliberately: once the context is cancelled
// database/sql's awaitDone goroutine races to finalize, so that error is nondeterministic for database/sql
// as much as for sequel. Do not "strengthen" this into a comparison against a raw *sql.Tx — that compares
// two independent races and flakes. The flag is what sequel controls.
func TestTx_FailedCommitDoesNotMarkTransactionFinalized(t *testing.T) {
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	db, err := Open("sqlite", dsn)
	assert.NoError(err)
	defer db.Close()
	_, err = db.Exec("CREATE TABLE tx_cancel (k INT)")
	assert.NoError(err)

	ctx, cancel := context.WithCancel(context.Background())
	tx, err := db.BeginTx(ctx, nil)
	assert.NoError(err)
	_, _ = tx.ExecContext(ctx, "INSERT INTO tx_cancel (k) VALUES (1)")
	cancel()

	assert.Error(tx.Commit(), "a commit on a cancelled context fails")
	assert.False(tx.done.Load(), "only a nil error proves the transaction was finalized; a failed commit must not shortcut a later rollback")
	_ = tx.Rollback()
}

// sql.Tx finalizes with a CAS on an atomic so concurrent Commit/Rollback is safe; the shadow must not
// reintroduce a race in front of it. Fails under -race if the finalization flag becomes a plain bool.
func TestTx_ConcurrentFinalizeIsRaceFree(t *testing.T) {
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	db, err := Open("sqlite", dsn)
	assert.NoError(err)
	defer db.Close()
	_, err = db.Exec("CREATE TABLE tx_race (k INT)")
	assert.NoError(err)

	for range 50 {
		tx, err := db.BeginTx(context.Background(), nil)
		if !assert.NoError(err) {
			return
		}
		var wg sync.WaitGroup
		wg.Add(2)
		go func() { defer wg.Done(); _ = tx.Commit() }()
		go func() { defer wg.Done(); _ = tx.Rollback() }()
		wg.Wait()
		// Exactly one of the two won; whichever it was, the transaction is over and a third call says so.
		assert.Equal(sql.ErrTxDone, tx.Rollback())
	}
}

// A successful Commit finalizes the transaction, so the deferred Rollback that idiomatically follows it
// reports ErrTxDone without a round trip.
func TestTx_DeferredRollbackAfterCommit(t *testing.T) {
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	db, err := Open("sqlite", dsn)
	assert.NoError(err)
	defer db.Close()
	_, err = db.Exec("CREATE TABLE tx_defer (k INT)")
	assert.NoError(err)

	tx, err := db.BeginTx(context.Background(), nil)
	assert.NoError(err)
	_, err = tx.Exec("INSERT INTO tx_defer (k) VALUES (1)")
	assert.NoError(err)
	assert.NoError(tx.Commit())
	assert.Equal(sql.ErrTxDone, tx.Rollback())
	assert.Equal(sql.ErrTxDone, tx.Commit())

	var n int
	assert.NoError(db.QueryRow("SELECT COUNT(*) FROM tx_defer").Scan(&n))
	assert.Equal(1, n, "the commit stands; the redundant rollback must not have undone it")
}
