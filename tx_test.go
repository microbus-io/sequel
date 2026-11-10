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
	"errors"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/microbus-io/testarossa"
)

// A failed Commit must not mark the transaction finalized, or a later Rollback answers ErrTxDone where
// database/sql would really have rolled back.
//
// Asserts the flag, not the later Rollback's error, deliberately: once the context is cancelled
// database/sql's awaitDone goroutine races to finalize, so that error is nondeterministic for database/sql
// as much as for sequel. Do not "strengthen" this into a comparison against a raw *sql.Tx - that compares
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

// A Transact Tx used from several goroutines - statements failing and succeeding concurrently, and a watchdog rolling
// back - must be race-free, and the error it records must be one of the statements' errors. Run under -race.
func TestTx_ConcurrentUseIsRaceFree(t *testing.T) {
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	db, err := Open("sqlite", dsn)
	assert.NoError(err)
	defer db.Close()
	_, err = db.Exec("CREATE TABLE tx_concurrent (k INT)")
	assert.NoError(err)

	err = db.Transact(context.Background(), func(tx *Tx) error {
		var wg sync.WaitGroup
		for i := range 8 {
			wg.Add(1)
			go func() {
				defer wg.Done()
				if i%2 == 0 {
					_, _ = tx.Exec("INSERT INTO tx_concurrent (k) VALUES (?)", i)
				} else {
					_, _ = tx.Exec("INSERT INTO no_such_table (k) VALUES (?)", i)
				}
				_ = tx.Err()
			}()
		}
		wg.Add(1)
		go func() { defer wg.Done(); _ = tx.Rollback() }()
		wg.Wait()
		return nil
	})
	assert.Error(err)
}

// A SQL Server transaction's context reports a cancellation to the driver as a net.Error, which makes the driver
// discard the connection, while staying context.Canceled - in message and errors.Is - to everyone else, including the
// context database/sql derives from it for the transaction. A deadline and a live context are unchanged.
func TestTx_MSSQLContextReportsCancellationAsNetError(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	parent, cancel := context.WithCancel(context.Background())
	ctx := mssqlTxContext{parent}
	child, cancelChild := context.WithCancel(ctx) // as database/sql's beginDC derives the transaction's
	defer cancelChild()
	assert.NoError(ctx.Err())
	cancel()
	err := ctx.Err()
	var netErr net.Error
	assert.True(errors.As(err, &netErr), "the driver takes it for a network error")
	assert.True(errors.Is(err, context.Canceled))
	assert.False(errors.Is(err, context.DeadlineExceeded))
	assert.Equal(context.Canceled.Error(), err.Error())
	<-child.Done()
	assert.Equal(context.Canceled, child.Err(), "database/sql's own context reports the plain cancellation")
	assert.Equal(context.Canceled, plainCancellation(err), "sequel hands back the plain cancellation")
	assert.Nil(plainCancellation(nil))

	expired, cancelExpired := context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancelExpired()
	<-expired.Done()
	assert.Equal(context.DeadlineExceeded, mssqlTxContext{expired}.Err())
}
