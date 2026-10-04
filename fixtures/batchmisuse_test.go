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
	"database/sql/driver"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/microbus-io/sequel"
	"github.com/microbus-io/testarossa"
)

// withTimeout fails the test instead of hanging the suite when body does not return in time - the shape of a
// deadlock regression.
func withTimeout(t *testing.T, d time.Duration, body func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		body()
	}()
	select {
	case <-done:
	case <-time.After(d):
		t.Fatalf("did not return within %v", d)
	}
}

// TestBatch_StatementWhileOpen: between Send and Close the transaction belongs to the batch. A statement issued on
// it meanwhile - plain, or another batch - fails rather than deadlocking or interleaving, on every driver, and the
// transaction rolls back.
func TestBatch_StatementWhileOpen(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_nested (id INT)")
	assert.NoError(err)
	_, err = db.Exec("INSERT INTO batch_nested (id) VALUES (1)")
	assert.NoError(err)

	var nestedErr, nestedBatchErr error
	withTimeout(t, 20*time.Second, func() {
		err = db.Transact(ctx, func(tx *sequel.Tx) error {
			b := tx.Batch()
			b.Queue("INSERT INTO batch_nested (id) VALUES (2)")
			b.Queue("SELECT id FROM batch_nested")
			res := b.Send(ctx)
			defer res.Close()
			if _, err := res.Exec(); err != nil {
				return err
			}
			rows, err := res.Query()
			if err != nil {
				return err
			}
			rows.Next()
			_, nestedErr = tx.ExecContext(ctx, "INSERT INTO batch_nested (id) VALUES (3)")
			inner := tx.Batch()
			inner.Queue("INSERT INTO batch_nested (id) VALUES (4)")
			nestedBatchErr = inner.Send(ctx).Close()
			return res.Close()
		})
	})
	assert.Error(nestedErr)
	assert.Error(nestedBatchErr)
	assert.Error(err, "the refused statement dooms the transaction")
	assert.Equal(1, countRows(t, db, "batch_nested"))
}

// TestBatch_PanicWhileReading: a panic between reads reaches the caller, the transaction rolls back, and the pool is
// left healthy.
func TestBatch_PanicWhileReading(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_panic (id INT)")
	assert.NoError(err)

	var recovered any
	withTimeout(t, 20*time.Second, func() {
		defer func() { recovered = recover() }()
		_ = db.Transact(ctx, func(tx *sequel.Tx) error {
			b := tx.Batch()
			b.Queue("INSERT INTO batch_panic (id) VALUES (1)")
			b.Queue("SELECT id FROM batch_panic")
			res := b.Send(ctx)
			defer res.Close()
			_, _ = res.Exec()
			panic("boom")
		})
	})
	assert.Equal("boom", recovered)
	assert.Equal(0, countRows(t, db, "batch_panic"))
	assert.Equal(0, db.Stats().InUse, "the connection went back to the pool")
}

// panicValuer panics when the driver converts it, which in a batch sent in one round trip happens while the
// connection is borrowed.
type panicValuer struct{}

func (panicValuer) Value() (driver.Value, error) { panic("valuer") }

// TestBatch_PanicInArgument: a panic converting an argument reaches the caller instead of hanging, and the pool is
// left healthy. Sent in one round trip, the conversion runs while the driver connection is borrowed.
func TestBatch_PanicInArgument(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_valuer (name VARCHAR(64))")
	assert.NoError(err)

	var recovered any
	withTimeout(t, 20*time.Second, func() {
		defer func() { recovered = recover() }()
		_ = db.Transact(ctx, func(tx *sequel.Tx) error {
			b := tx.Batch()
			b.Queue("INSERT INTO batch_valuer (name) VALUES (?)", panicValuer{})
			return b.Send(ctx).Close()
		})
	})
	assert.Equal("valuer", recovered)
	assert.Equal(0, countRows(t, db, "batch_valuer"))
	assert.Equal(0, db.Stats().InUse, "the connection went back to the pool")
}

// TestBatch_SendAfterRollback: a batch sent after its transaction ended fails like any statement on a finished
// transaction, rather than running on its own and committing.
func TestBatch_SendAfterRollback(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_after (id INT)")
	assert.NoError(err)

	var batchErr error
	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		assert.NoError(tx.Rollback())
		b := tx.Batch()
		b.Queue("INSERT INTO batch_after (id) VALUES (1)")
		batchErr = b.Send(ctx).Close()
		return nil
	})
	assert.Error(batchErr)
	assert.Error(err)
	assert.Equal(0, countRows(t, db, "batch_after"), "nothing ran outside the transaction")
}

// TestBatch_Misuse: every misuse of a batch inside Transact fails the transaction rather than letting it commit with
// statements silently missing or unread.
func TestBatch_Misuse(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	cases := map[string]func(t *testing.T, tx *sequel.Tx){
		"queued_after_send": func(t *testing.T, tx *sequel.Tx) {
			b := tx.Batch()
			b.Queue("INSERT INTO batch_misuse (id) VALUES (1)")
			res := b.Send(ctx)
			b.Queue("INSERT INTO batch_misuse (id) VALUES (2)")
			testarossa.For(t).Error(res.Close(), "a statement queued after Send fails the batch")
		},
		"sent_twice": func(t *testing.T, tx *sequel.Tx) {
			b := tx.Batch()
			b.Queue("INSERT INTO batch_misuse (id) VALUES (1)")
			_ = b.Send(ctx).Close()
			testarossa.For(t).Error(b.Send(ctx).Close())
		},
		"never_sent": func(t *testing.T, tx *sequel.Tx) {
			b := tx.Batch()
			b.Queue("INSERT INTO batch_misuse (id) VALUES (1)")
			tx.ExecContext(ctx, "INSERT INTO batch_misuse (id) VALUES (2)") //nolint:errcheck
		},
		"never_closed": func(t *testing.T, tx *sequel.Tx) {
			b := tx.Batch()
			b.Queue("INSERT INTO batch_misuse (id) VALUES (1)")
			_, _ = b.Send(ctx).Exec()
		},
		"named_argument": func(t *testing.T, tx *sequel.Tx) {
			b := tx.Batch()
			b.Queue("INSERT INTO batch_misuse (id) VALUES (1)")
			b.Queue("INSERT INTO batch_misuse (id) VALUES (@id)", sql.Named("id", 2))
			_ = b.Send(ctx).Close()
		},
		"raw_bytes": func(t *testing.T, tx *sequel.Tx) {
			b := tx.Batch()
			b.Queue("INSERT INTO batch_misuse (id) VALUES (1)")
			b.Queue("SELECT id FROM batch_misuse")
			res := b.Send(ctx)
			_, _ = res.Exec()
			var raw sql.RawBytes
			_ = res.QueryRow().Scan(&raw)
			_ = res.Close()
		},
		"ignored_scan_error": func(t *testing.T, tx *sequel.Tx) {
			b := tx.Batch()
			b.Queue("INSERT INTO batch_misuse (id) VALUES (1)")
			b.Queue("SELECT id FROM batch_misuse")
			res := b.Send(ctx)
			_, _ = res.Exec()
			rows, _ := res.Query()
			for rows.Next() {
				var a, b int
				_ = rows.Scan(&a, &b) // one column, two destinations; the error is dropped
			}
			_ = res.Close()
		},
	}
	for name, misuse := range cases {
		t.Run(name, func(t *testing.T) {
			assert := testarossa.For(t)
			db := newTestDB(t)
			_, err := db.Exec("CREATE TABLE batch_misuse (id INT)")
			assert.NoError(err)
			err = db.Transact(ctx, func(tx *sequel.Tx) error {
				misuse(t, tx)
				return nil // the closure reports success; the misuse must still fail the transaction
			})
			assert.Error(err)
			assert.Equal(0, countRows(t, db, "batch_misuse"))
		})
	}
}

// TestBatch_ReadsOutOfRange: reading past the last statement, or after Close, is an error; it is not a statement's
// failure, so the transaction still commits.
func TestBatch_ReadsOutOfRange(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_range (id INT)")
	assert.NoError(err)

	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		b := tx.Batch()
		b.Queue("INSERT INTO batch_range (id) VALUES (1)")
		res := b.Send(ctx)
		if _, err := res.Exec(); err != nil {
			return err
		}
		_, pastEnd := res.Exec()
		assert.Error(pastEnd)
		if err := res.Close(); err != nil {
			return err
		}
		_, afterClose := res.Exec()
		assert.Error(afterClose)
		assert.NoError(res.Close(), "Close is safe to call again")
		return nil
	})
	assert.NoError(err)
	assert.Equal(1, countRows(t, db, "batch_range"))
}

// TestBatch_MixedWithStatements: a transaction interleaves plain statements and several batches, each seeing the
// effects of what came before.
func TestBatch_MixedWithStatements(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_mixed (id INT)")
	assert.NoError(err)

	var afterFirst, afterSecond int
	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO batch_mixed (id) VALUES (1)"); err != nil {
			return err
		}
		b := tx.Batch()
		b.Queue("INSERT INTO batch_mixed (id) VALUES (2)")
		b.Queue("SELECT COUNT(*) FROM batch_mixed")
		res := b.Send(ctx)
		_, _ = res.Exec()
		_ = res.QueryRow().Scan(&afterFirst)
		if err := res.Close(); err != nil {
			return err
		}
		if _, err := tx.ExecContext(ctx, "INSERT INTO batch_mixed (id) VALUES (3)"); err != nil {
			return err
		}
		b = tx.Batch()
		b.Queue("INSERT INTO batch_mixed (id) VALUES (4)")
		b.Queue("SELECT COUNT(*) FROM batch_mixed")
		res = b.Send(ctx)
		_, _ = res.Exec()
		_ = res.QueryRow().Scan(&afterSecond)
		return res.Close()
	})
	assert.NoError(err)
	assert.Equal(2, afterFirst)
	assert.Equal(4, afterSecond)
	assert.Equal(4, countRows(t, db, "batch_mixed"))
}

// valuer is an argument converted by driver.Valuer.
type valuer string

func (v valuer) Value() (driver.Value, error) { return "v:" + string(v), nil }

// TestBatch_ArgumentTypes: the argument shapes database/sql accepts - a driver.Valuer, a sql.Null, a nil - bind
// the same way in a batch.
func TestBatch_ArgumentTypes(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_args (id INT, name VARCHAR(64))")
	assert.NoError(err)

	var name sql.NullString
	var nulls int
	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		b := tx.Batch()
		b.Queue("INSERT INTO batch_args (id, name) VALUES (?, ?)", 1, valuer("x"))
		b.Queue("INSERT INTO batch_args (id, name) VALUES (?, ?)", 2, sql.NullString{})
		b.Queue("INSERT INTO batch_args (id, name) VALUES (?, ?)", 3, nil)
		b.Queue("SELECT name FROM batch_args WHERE id=?", 1)
		b.Queue("SELECT COUNT(*) FROM batch_args WHERE name IS NULL")
		res := b.Send(ctx)
		defer res.Close()
		for range 3 {
			if _, err := res.Exec(); err != nil {
				return err
			}
		}
		if err := res.QueryRow().Scan(&name); err != nil {
			return err
		}
		if err := res.QueryRow().Scan(&nulls); err != nil {
			return err
		}
		return res.Close()
	})
	assert.NoError(err)
	assert.Equal("v:x", name.String)
	assert.Equal(2, nulls)
}

// TestBatch_ConcurrentTransactions: batched transactions running at once all commit and give every connection back.
func TestBatch_ConcurrentTransactions(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_concurrent (id INT)")
	assert.NoError(err)

	const workers = 8
	var wg sync.WaitGroup
	errs := make([]error, workers)
	for w := range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			errs[w] = db.Transact(ctx, func(tx *sequel.Tx) error {
				b := tx.Batch()
				for i := range 5 {
					b.Queue("INSERT INTO batch_concurrent (id) VALUES (?)", w*10+i)
				}
				return b.Send(ctx).Close()
			})
		}()
	}
	wg.Wait()
	for _, err := range errs {
		assert.NoError(err)
	}
	assert.Equal(workers*5, countRows(t, db, "batch_concurrent"))
	assert.Equal(0, db.Stats().InUse)
}

// TestBatch_LateRowError: a statement whose rows fail partway - a division by zero on the second row - fails the
// batch. PostgreSQL only: other engines return NULL for x/0.
func TestBatch_LateRowError(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	if !pipelines(db) {
		t.Skip("division by zero is not an error on this engine")
	}
	_, err := db.Exec("CREATE TABLE batch_late (id INT)")
	assert.NoError(err)
	_, err = db.Exec("INSERT INTO batch_late (id) VALUES (1), (2), (3)")
	assert.NoError(err)

	var readErr error
	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		b := tx.Batch()
		b.Queue("SELECT 1/(id-2) FROM batch_late ORDER BY id")
		res := b.Send(ctx)
		_, readErr = res.Query()
		return res.Close()
	})
	assert.Error(err)
	assert.Error(readErr)
}

// TestBatch_CancelledMidBatch: cancelling the context while a batch sent in one round trip waits on the database
// fails it and returns its connection to the pool. PostgreSQL only, for pg_sleep.
func TestBatch_CancelledMidBatch(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	if !pipelines(db) {
		t.Skip("pg_sleep is PostgreSQL's")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	withTimeout(t, 20*time.Second, func() {
		err := db.Transact(ctx, func(tx *sequel.Tx) error {
			b := tx.Batch()
			b.Queue("SELECT pg_sleep(5)")
			return b.Send(ctx).Close()
		})
		assert.True(errors.Is(err, context.DeadlineExceeded), "got %v", err)
	})
	assert.Equal(0, db.Stats().InUse)
	var one int
	assert.NoError(db.QueryRow("SELECT 1").Scan(&one), "the pool still serves queries")
}

// TestBatch_SupersededRow: a row read after a later result was read reports that, rather than an empty result a
// caller would take for "not found" - and dooms the transaction, since the caller read nothing it meant to.
func TestBatch_SupersededRow(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_superseded (id INT)")
	assert.NoError(err)
	_, err = db.Exec("INSERT INTO batch_superseded (id) VALUES (1)")
	assert.NoError(err)

	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		b := tx.Batch()
		b.Queue("SELECT id FROM batch_superseded")
		b.Queue("SELECT id FROM batch_superseded")
		res := b.Send(ctx)
		defer res.Close()
		first := res.QueryRow()
		second := res.QueryRow()
		var id int
		err := first.Scan(&id)
		assert.Error(err)
		assert.False(err == sql.ErrNoRows, "a superseded row is not an empty one")
		assert.NoError(second.Scan(&id))
		return res.Close()
	})
	assert.Error(err)
}

// TestBatch_ScanErrorEndsReads: in Transact, a scan error dooms the transaction, so the next read and Close report
// it, however the batch was sent.
func TestBatch_ScanErrorEndsReads(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_scanend (id INT)")
	assert.NoError(err)

	var scanErr, nextErr, closeErr error
	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		b := tx.Batch()
		b.Queue("INSERT INTO batch_scanend (id) VALUES (1)")
		b.Queue("SELECT id FROM batch_scanend")
		b.Queue("INSERT INTO batch_scanend (id) VALUES (2)")
		res := b.Send(ctx)
		_, _ = res.Exec()
		var a, c int
		scanErr = res.QueryRow().Scan(&a, &c)
		_, nextErr = res.Exec()
		closeErr = res.Close()
		return nil
	})
	assert.Error(scanErr)
	assert.Error(nextErr)
	assert.Error(closeErr)
	assert.Error(err)
	assert.Equal(0, countRows(t, db, "batch_scanend"))
}

// TestBatch_UnreadFailureReportedByClose: a statement that failed is the batch's failure whether or not its result
// was read.
func TestBatch_UnreadFailureReportedByClose(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_unreadfail (id INT PRIMARY KEY)")
	assert.NoError(err)

	var closeErr error
	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		b := tx.Batch()
		b.Queue("INSERT INTO batch_unreadfail (id) VALUES (1)")
		b.Queue("INSERT INTO batch_unreadfail (id) VALUES (1)")
		closeErr = b.Send(ctx).Close()
		return nil
	})
	assert.Error(closeErr)
	assert.Error(err)
	assert.Equal(0, countRows(t, db, "batch_unreadfail"))
}

// TestBatch_BeginTxCommitGuard: in a BeginTx transaction, Commit refuses while a batch is unsent or its results are
// open, rather than committing without its statements.
func TestBatch_BeginTxCommitGuard(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_commitguard (id INT)")
	assert.NoError(err)

	tx, err := db.BeginTx(ctx, nil)
	assert.NoError(err)
	defer tx.Rollback()
	b := tx.Batch()
	b.Queue("INSERT INTO batch_commitguard (id) VALUES (1)")
	assert.Error(tx.Commit(), "a batch queued and never sent")
	res := b.Send(ctx)
	assert.Error(tx.Commit(), "a batch whose results are open")
	assert.NoError(res.Close())
	assert.NoError(tx.Commit())
	assert.Equal(1, countRows(t, db, "batch_commitguard"))
}

// TestBatch_StmtWhileOpen: binding a prepared statement to a transaction whose batch is open is refused like any
// statement, and the unbound statement is safe to close.
func TestBatch_StmtWhileOpen(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_stmt (id INT)")
	assert.NoError(err)
	stmt, err := db.Prepare("INSERT INTO batch_stmt (id) VALUES (?)")
	assert.NoError(err)
	defer stmt.Close()

	var stmtErr error
	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		b := tx.Batch()
		b.Queue("INSERT INTO batch_stmt (id) VALUES (1)")
		res := b.Send(ctx)
		bound := tx.StmtContext(ctx, stmt)
		_, stmtErr = bound.ExecContext(ctx, 2)
		assert.NoError(bound.Close())
		return res.Close()
	})
	assert.Error(stmtErr)
	assert.Error(err)
	assert.Equal(0, countRows(t, db, "batch_stmt"))

	// A statement refused while the batch was open stays refused, and rebinding it does not panic.
	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		b := tx.Batch()
		b.Queue("INSERT INTO batch_stmt (id) VALUES (1)")
		res := b.Send(ctx)
		refused := tx.StmtContext(ctx, stmt)
		if err := res.Close(); err != nil {
			return err
		}
		_, err := tx.StmtContext(ctx, refused).ExecContext(ctx, 2)
		return err
	})
	assert.Error(err)
}

// TestBatch_PanicInClose: a statement left unread whose argument panics when Close runs it. The panic reaches the
// caller and the results are still released, so the transaction is not left refusing every statement.
func TestBatch_PanicInClose(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_closepanic (name VARCHAR(64))")
	assert.NoError(err)

	tx, err := db.BeginTx(ctx, nil)
	assert.NoError(err)
	defer tx.Rollback()
	b := tx.Batch()
	b.Queue("INSERT INTO batch_closepanic (name) VALUES (?)", panicValuer{})
	res := b.Send(ctx)
	var recovered any
	withTimeout(t, 20*time.Second, func() {
		defer func() { recovered = recover() }()
		_ = res.Close()
	})
	assert.Equal("valuer", recovered)
	_, err = tx.ExecContext(ctx, "SELECT 1")
	assert.NoError(err, "the results were released despite the panic")
}

// TestBatch_ScanErrorThenClose: a scan error on the last statement is reported by Close, with no read in between,
// whichever way the batch was sent.
func TestBatch_ScanErrorThenClose(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_scanclose (id INT)")
	assert.NoError(err)

	var closeErr error
	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		b := tx.Batch()
		b.Queue("INSERT INTO batch_scanclose (id) VALUES (1)")
		b.Queue("SELECT id FROM batch_scanclose")
		res := b.Send(ctx)
		_, _ = res.Exec()
		var a, c int
		_ = res.QueryRow().Scan(&a, &c)
		closeErr = res.Close()
		return nil
	})
	assert.Error(closeErr)
	assert.Error(err)
	assert.Equal(0, countRows(t, db, "batch_scanclose"))
}

// TestBatch_SupersededRowsDoomTransaction: rows abandoned by a later read, then iterated with the error ignored,
// fail the transaction rather than committing on an empty read. Rows read to the end before the next read are not
// superseded, and the transaction commits.
func TestBatch_SupersededRowsDoomTransaction(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_abandon (id INT)")
	assert.NoError(err)
	_, err = db.Exec("INSERT INTO batch_abandon (id) VALUES (1), (2)")
	assert.NoError(err)

	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		b := tx.Batch()
		b.Queue("SELECT id FROM batch_abandon")
		b.Queue("INSERT INTO batch_abandon (id) VALUES (3)")
		res := b.Send(ctx)
		rows, _ := res.Query()
		_, _ = res.Exec() // supersedes rows before they were read
		for rows.Next() {
		}
		_ = res.Close()
		return nil
	})
	assert.Error(err, "an abandoned read fails the transaction")
	assert.Equal(2, countRows(t, db, "batch_abandon"))

	var seen int
	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		b := tx.Batch()
		b.Queue("SELECT id FROM batch_abandon")
		b.Queue("INSERT INTO batch_abandon (id) VALUES (3)")
		res := b.Send(ctx)
		defer res.Close()
		rows, err := res.Query()
		if err != nil {
			return err
		}
		for rows.Next() {
			seen++
		}
		if _, err := res.Exec(); err != nil {
			return err
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return res.Close()
	})
	assert.NoError(err, "rows read to the end are not superseded")
	assert.Equal(2, seen)
	assert.Equal(3, countRows(t, db, "batch_abandon"))
}

// TestBatch_ScanAfterLastRow: Scan after Next returned false is an error on every driver, not a re-read of the last
// row.
func TestBatch_ScanAfterLastRow(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_overread (id INT, name VARCHAR(64))")
	assert.NoError(err)
	_, err = db.Exec("INSERT INTO batch_overread (id, name) VALUES (1, 'a')")
	assert.NoError(err)

	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		b := tx.Batch()
		b.Queue("SELECT id, name FROM batch_overread")
		res := b.Send(ctx)
		defer res.Close()
		rows, err := res.Query()
		if err != nil {
			return err
		}
		cols, err := rows.Columns()
		assert.NoError(err)
		assert.Equal([]string{"id", "name"}, cols)
		for rows.Next() {
		}
		var id int
		var name string
		assert.Error(rows.Scan(&id, &name))
		return nil
	})
	assert.Error(err, "the failed scan dooms the transaction")
}

// TestBatch_ReadAfterRollback: ending the transaction closes its batch's results, so a later read is an error.
func TestBatch_ReadAfterRollback(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_afterrb (id INT)")
	assert.NoError(err)

	tx, err := db.BeginTx(ctx, nil)
	assert.NoError(err)
	b := tx.Batch()
	b.Queue("INSERT INTO batch_afterrb (id) VALUES (1)")
	b.Queue("SELECT id FROM batch_afterrb")
	res := b.Send(ctx)
	_, err = res.Exec()
	assert.NoError(err)
	assert.NoError(tx.Rollback())
	_, err = res.Query()
	assert.Error(err)
	assert.Equal(0, countRows(t, db, "batch_afterrb"))
}
