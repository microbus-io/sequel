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
	"strconv"
	"testing"
	"time"

	"github.com/microbus-io/sequel"
	"github.com/microbus-io/testarossa"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// pipelines reports whether a batch on db is sent in one round trip: a DB batch, or a Tx batch inside Transact.
func pipelines(db *sequel.DB) bool {
	return db.DriverName() == "pgx" || db.DriverName() == "cockroachdb"
}

// countRows returns the number of rows in table.
func countRows(t *testing.T, db *sequel.DB, table string) int {
	t.Helper()
	var n int
	testarossa.For(t).NoError(db.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&n))
	return n
}

// TestBatch_ResultsInOrder reads every kind of result from one batch, in queue order. Later statements see the
// effects of earlier ones, because they run in order inside the transaction.
func TestBatch_ResultsInOrder(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_order (id INT, name VARCHAR(64), note VARCHAR(64))")
	assert.NoError(err)

	var count int
	var name string
	var note sql.NullString
	var ids []int
	var updated int64
	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		b := tx.Batch()
		b.Queue("INSERT INTO batch_order (id, name) VALUES (?, ?)", 1, "one")
		b.Queue("INSERT INTO batch_order (id, name) VALUES (?, ?)", 2, "two")
		b.Queue("INSERT INTO batch_order (id, name) VALUES (?, ?)", 3, "three")
		b.Queue("UPDATE batch_order SET name=? WHERE id>=?", "big", 2)
		b.Queue("SELECT COUNT(*) FROM batch_order")
		b.Queue("SELECT name, note FROM batch_order WHERE id=?", 1)
		b.Queue("SELECT id FROM batch_order ORDER BY id")
		assert.Equal(7, b.Len())
		res := b.Send(ctx)
		defer res.Close()
		for range 3 {
			if _, err := res.Exec(); err != nil {
				return err
			}
		}
		upd, err := res.Exec()
		if err != nil {
			return err
		}
		updated, _ = upd.RowsAffected()
		if err := res.QueryRow().Scan(&count); err != nil {
			return err
		}
		if err := res.QueryRow().Scan(&name, &note); err != nil {
			return err
		}
		rows, err := res.Query()
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		if err := rows.Err(); err != nil {
			return err
		}
		return res.Close()
	})
	assert.NoError(err)
	assert.Equal(int64(2), updated)
	assert.Equal(3, count, "a query sees the inserts queued before it in the same batch")
	assert.Equal("one", name)
	assert.False(note.Valid, "a NULL scans into a sql.Null type")
	assert.Equal([]int{1, 2, 3}, ids)
}

// TestBatch_ParseFailure pins a statement rejected without running it - a missing table, a syntax error, a
// virtual function that does not expand - in the middle of a batch. The closure ignores every error, and Transact
// still rolls back. The failing statement and those after it report the error everywhere. The statement before
// it differs by driver, as documented: in one round trip the whole batch is prepared first, so it never ran and
// reports the error too; everywhere else it ran.
func TestBatch_ParseFailure(t *testing.T) {
	t.Parallel()
	ctx := context.Background()

	for name, bad := range map[string]string{
		"missing_table":    "INSERT INTO batch_missing (id) VALUES (1)",
		"syntax_error":     "INSERT INTO batch_parse (id) VALUES (",
		"virtual_function": "SELECT id FROM batch_parse ORDER BY id LIMIT_OFFSET(1)",
	} {
		t.Run(name, func(t *testing.T) {
			assert := testarossa.For(t)
			db := newTestDB(t)
			_, err := db.Exec("CREATE TABLE batch_parse (id INT)")
			assert.NoError(err)

			var beforeErr, failedErr, afterErr, closeErr error
			err = db.Transact(ctx, func(tx *sequel.Tx) error {
				b := tx.Batch()
				b.Queue("INSERT INTO batch_parse (id) VALUES (1)")
				b.Queue(bad)
				b.Queue("INSERT INTO batch_parse (id) VALUES (2)")
				res := b.Send(ctx)
				_, beforeErr = res.Exec()
				_, failedErr = res.Exec()
				_, afterErr = res.Exec()
				closeErr = res.Close()
				return nil // the closure reports success; the recorded error must still fail the transaction
			})
			assert.Error(err)
			assert.Error(failedErr)
			assert.Error(afterErr, "a statement queued after the failure reports the failure")
			assert.Error(closeErr)
			if pipelines(db) {
				assert.Error(beforeErr, "the whole batch is prepared before any of it runs")
			} else {
				assert.NoError(beforeErr)
			}
			assert.Equal(0, countRows(t, db, "batch_parse"))
		})
	}
}

// TestBatch_RuntimeFailure is a failure found while running rather than while preparing - a duplicate key. The
// statement before it ran on every driver, the ones after it did not, and the transaction rolls back.
func TestBatch_RuntimeFailure(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_dup (id INT PRIMARY KEY)")
	assert.NoError(err)

	var beforeErr, dupErr, afterErr error
	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		b := tx.Batch()
		b.Queue("INSERT INTO batch_dup (id) VALUES (1)")
		b.Queue("INSERT INTO batch_dup (id) VALUES (1)")
		b.Queue("INSERT INTO batch_dup (id) VALUES (2)")
		res := b.Send(ctx)
		defer res.Close()
		_, beforeErr = res.Exec()
		_, dupErr = res.Exec()
		_, afterErr = res.Exec()
		return res.Close()
	})
	assert.Error(err)
	assert.NoError(beforeErr)
	assert.Error(dupErr)
	assert.Error(afterErr)
	assert.Equal(0, countRows(t, db, "batch_dup"))
}

// TestBatch_NoRowIsNotAFailure: a QueryRow that finds nothing returns a bare sql.ErrNoRows, and the batch - and the
// transaction - carry on.
func TestBatch_NoRowIsNotAFailure(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_norow (id INT)")
	assert.NoError(err)

	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		b := tx.Batch()
		b.Queue("SELECT id FROM batch_norow WHERE id=?", 42)
		b.Queue("INSERT INTO batch_norow (id) VALUES (1)")
		res := b.Send(ctx)
		defer res.Close()
		var id int
		assert.True(res.QueryRow().Scan(&id) == sql.ErrNoRows, "sql.ErrNoRows is returned bare, so it compares with ==")
		if _, err := res.Exec(); err != nil {
			return err
		}
		return res.Close()
	})
	assert.NoError(err)
	assert.Equal(1, countRows(t, db, "batch_norow"))
}

// TestBatch_ScanErrorRollsBack: a QueryRow whose destinations do not match its columns fails the transaction, even
// when the closure ignores the error.
func TestBatch_ScanErrorRollsBack(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_scan (id INT, name VARCHAR(64))")
	assert.NoError(err)

	var scanErr error
	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		b := tx.Batch()
		b.Queue("INSERT INTO batch_scan (id, name) VALUES (1, 'a')")
		b.Queue("SELECT id, name FROM batch_scan")
		res := b.Send(ctx)
		_, _ = res.Exec()
		var id int
		scanErr = res.QueryRow().Scan(&id) // two columns, one destination
		_ = res.Close()
		return nil
	})
	assert.Error(scanErr)
	assert.Error(err)
	assert.Equal(0, countRows(t, db, "batch_scan"))
}

// TestBatch_UnreadResults: Close runs the statements that were not read, a QueryRow takes the first of several rows,
// rows left open are closed by the next read, and an update that matches nothing affects no rows.
func TestBatch_UnreadResults(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_unread (id INT)")
	assert.NoError(err)

	var first int
	var none int64
	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		b := tx.Batch()
		b.Queue("INSERT INTO batch_unread (id) VALUES (1)")
		b.Queue("INSERT INTO batch_unread (id) VALUES (2)")
		b.Queue("SELECT id FROM batch_unread ORDER BY id")
		b.Queue("SELECT id FROM batch_unread ORDER BY id")
		b.Queue("UPDATE batch_unread SET id=0 WHERE id=?", 99)
		b.Queue("INSERT INTO batch_unread (id) VALUES (3)") // never read: Close runs it
		b.Queue("SELECT id FROM batch_unread")              // never read either
		res := b.Send(ctx)
		defer res.Close()
		for range 2 {
			if _, err := res.Exec(); err != nil {
				return err
			}
		}
		if err := res.QueryRow().Scan(&first); err != nil {
			return err
		}
		if _, err := res.Query(); err != nil { // left open; the next read closes it
			return err
		}
		upd, err := res.Exec()
		if err != nil {
			return err
		}
		none, _ = upd.RowsAffected()
		return res.Close()
	})
	assert.NoError(err)
	assert.Equal(1, first, "QueryRow scans the first row")
	assert.Equal(int64(0), none, "an update matching nothing affects no rows and is not an error")
	assert.Equal(3, countRows(t, db, "batch_unread"))
}

// TestBatch_ShortCircuitsDoomedTransaction: in a Transact whose earlier statement already failed, Send touches
// nothing and the reads and Close return the recorded error verbatim.
func TestBatch_ShortCircuitsDoomedTransaction(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_doomed (id INT)")
	assert.NoError(err)

	var firstErr, readErr, closeErr error
	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		_, firstErr = tx.ExecContext(ctx, "INSERT INTO batch_doomed_missing (id) VALUES (1)")
		b := tx.Batch()
		b.Queue("INSERT INTO batch_doomed (id) VALUES (1)")
		res := b.Send(ctx)
		_, readErr = res.Exec()
		closeErr = res.Close()
		return nil
	})
	assert.Error(firstErr)
	assert.Error(err)
	assert.Equal(firstErr, readErr, "the read returns the recorded error verbatim")
	assert.Equal(firstErr, closeErr)
}

// TestBatch_CancelledContext: a batch sent with a context already cancelled fails, and the transaction rolls back.
func TestBatch_CancelledContext(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_cancel (id INT)")
	assert.NoError(err)

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		b := tx.Batch()
		b.Queue("INSERT INTO batch_cancel (id) VALUES (1)")
		res := b.Send(cancelled)
		_, _ = res.Exec()
		return res.Close()
	})
	assert.True(errors.Is(err, context.Canceled), "got %v", err)
	assert.Equal(0, countRows(t, db, "batch_cancel"))
}

// TestBatch_VirtualFunctionsAndPlaceholders: batched statements are unpacked like any other, so virtual functions
// expand and ? placeholders conform to the driver.
func TestBatch_VirtualFunctionsAndPlaceholders(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_vf (id INT)")
	assert.NoError(err)

	var ids []int
	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		b := tx.Batch()
		for i := 1; i <= 5; i++ {
			b.Queue("INSERT INTO batch_vf (id) VALUES (?)", i)
		}
		b.Queue("SELECT id FROM batch_vf WHERE id>=? ORDER BY id LIMIT_OFFSET(2, 1)", 1)
		res := b.Send(ctx)
		defer res.Close()
		for range 5 {
			if _, err := res.Exec(); err != nil {
				return err
			}
		}
		rows, err := res.Query()
		if err != nil {
			return err
		}
		for rows.Next() {
			var id int
			if err := rows.Scan(&id); err != nil {
				return err
			}
			ids = append(ids, id)
		}
		return res.Close()
	})
	assert.NoError(err)
	assert.Equal([]int{2, 3}, ids)
}

// TestBatch_Large sends a batch far wider than a typical transaction.
func TestBatch_Large(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_large (id INT)")
	assert.NoError(err)

	const width = 500
	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		b := tx.Batch()
		for i := range width {
			b.Queue("INSERT INTO batch_large (id) VALUES (?)", i)
		}
		return b.Send(ctx).Close()
	})
	assert.NoError(err)
	assert.Equal(width, countRows(t, db, "batch_large"))
}

// TestBatch_Telemetry: a batch sent in one round trip is one BATCH span carrying the batch size; otherwise a span
// per statement.
func TestBatch_Telemetry(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_spans (id INT)")
	assert.NoError(err)
	recorder := tracetest.NewSpanRecorder()
	db.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))

	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		b := tx.Batch()
		for i := range 3 {
			b.Queue("INSERT INTO batch_spans (id) VALUES (?)", i)
		}
		return b.Send(ctx).Close()
	})
	assert.NoError(err)

	var batches, inserts int
	var size int64
	for _, span := range recorder.Ended() {
		switch span.Name() {
		case "BATCH":
			batches++
			for _, kv := range span.Attributes() {
				if kv.Key == "db.operation.batch.size" {
					size = kv.Value.AsInt64()
				}
			}
		case "INSERT batch_spans":
			inserts++
		}
	}
	if pipelines(db) {
		assert.Equal(1, batches)
		assert.Equal(int64(3), size)
		assert.Equal(0, inserts)
	} else {
		assert.Equal(0, batches)
		assert.Equal(3, inserts)
	}
}

// TestBatch_BeginTx: a batch also works on a transaction from BeginTx, where each statement runs as it is read.
func TestBatch_BeginTx(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_begin (id INT)")
	assert.NoError(err)

	tx, err := db.BeginTx(ctx, nil)
	assert.NoError(err)
	defer tx.Rollback()
	b := tx.Batch()
	b.Queue("INSERT INTO batch_begin (id) VALUES (1)")
	b.Queue("INSERT INTO batch_begin (id) VALUES (2)")
	assert.NoError(b.Send(ctx).Close())
	assert.NoError(tx.Commit())
	assert.Equal(2, countRows(t, db, "batch_begin"))
}

// TestBatch_OneRoundTripOnPostgres measures a Tx batch with a simulated round trip: inside Transact on PostgreSQL
// the batch is one round trip; elsewhere every statement is its own. The statements are sent once beforehand so the
// measured run does not include preparing them.
func TestBatch_OneRoundTripOnPostgres(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE batch_rtt (id INT)")
	assert.NoError(err)

	const statements = 10
	run := func() error {
		return db.Transact(ctx, func(tx *sequel.Tx) error {
			b := tx.Batch()
			for i := range statements {
				b.Queue("INSERT INTO batch_rtt (id) VALUES (" + strconv.Itoa(i) + ")")
			}
			return b.Send(ctx).Close()
		})
	}
	assert.NoError(run())

	const rtt = 50 * time.Millisecond
	db.SimulateRTT(rtt)
	defer db.SimulateRTT(0)
	start := time.Now()
	assert.NoError(run())
	elapsed := time.Since(start)

	if pipelines(db) {
		// BEGIN, the batch, COMMIT.
		assert.True(elapsed < 6*rtt, "the batch is one round trip; took %v", elapsed)
	} else {
		// BEGIN, every statement, COMMIT.
		assert.True(elapsed >= statements*rtt, "every statement is a round trip; took %v", elapsed)
	}
}
