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
	"time"

	"github.com/microbus-io/errors"
	"github.com/microbus-io/testarossa"
)

// These tests measure wall-clock time, so they deliberately do not call t.Parallel(): Go runs
// non-parallel tests one at a time, which keeps a busy sibling test from inflating the numbers.

// The delay is set to zero, so no simulated latency can leak into the assertion.
func TestRTT_OffByDefault(t *testing.T) {
	assert := testarossa.For(t)

	db := &DB{}
	assert.Equal(time.Duration(0), db.rtt(), "the simulation is off until SimulateRTT asks for it")

	db.SimulateRTT(50 * time.Millisecond)
	assert.Equal(50*time.Millisecond, db.rtt())

	// A negative duration is treated as off rather than wrapping into an enormous pause.
	db.SimulateRTT(-time.Second)
	assert.Equal(time.Duration(0), db.rtt())
}

// Every operation sequel shadows pays the simulated round trip, and setting the delay back to zero
// removes it again.
func TestRTT_DelaysWireOperations(t *testing.T) {
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	db, err := Open("sqlite", dsn)
	assert.NoError(err)
	defer db.Close()

	_, err = db.Exec("CREATE TABLE rtt_ops (k INT)")
	assert.NoError(err)

	// Prepared ahead of the timed section, so the entries below time only the execution. A DB-prepared
	// Stmt reads the pool's current delay at each execution, so both phases of the loop apply to it.
	stmtIns, err := db.Prepare("INSERT INTO rtt_ops (k) VALUES (?)")
	assert.NoError(err)
	defer stmtIns.Close()
	stmtSel, err := db.Prepare("SELECT COUNT(*) FROM rtt_ops")
	assert.NoError(err)
	defer stmtSel.Close()

	ctx := context.Background()
	var tx *Tx
	ops := []struct {
		name string
		run  func()
	}{
		{"Exec", func() {
			_, err := db.Exec("INSERT INTO rtt_ops (k) VALUES (1)")
			assert.NoError(err)
		}},
		{"ExecContext", func() {
			_, err := db.ExecContext(ctx, "INSERT INTO rtt_ops (k) VALUES (?)", 2)
			assert.NoError(err)
		}},
		{"Query", func() {
			rows, err := db.Query("SELECT k FROM rtt_ops")
			assert.NoError(err)
			assert.NoError(rows.Close())
		}},
		{"QueryContext", func() {
			rows, err := db.QueryContext(ctx, "SELECT k FROM rtt_ops")
			assert.NoError(err)
			assert.NoError(rows.Close())
		}},
		{"QueryRow", func() {
			var n int
			assert.NoError(db.QueryRow("SELECT COUNT(*) FROM rtt_ops").Scan(&n))
		}},
		{"QueryRowContext", func() {
			var n int
			assert.NoError(db.QueryRowContext(ctx, "SELECT COUNT(*) FROM rtt_ops").Scan(&n))
		}},
		{"Prepare", func() {
			stmt, err := db.Prepare("SELECT k FROM rtt_ops WHERE k=?")
			assert.NoError(err)
			assert.NoError(stmt.Close())
		}},
		{"Stmt.Exec", func() {
			_, err := stmtIns.ExecContext(ctx, 9)
			assert.NoError(err)
		}},
		{"Stmt.QueryRow", func() {
			var n int
			assert.NoError(stmtSel.QueryRow().Scan(&n))
		}},
		{"Ping", func() {
			assert.NoError(db.Ping())
		}},
		{"PingContext", func() {
			assert.NoError(db.PingContext(ctx))
		}},
		// Begin/Commit/Rollback are round trips of their own, so each is charged separately. The
		// transaction opened by one entry is closed by the next.
		{"Begin", func() {
			var err error
			tx, err = db.Begin()
			assert.NoError(err)
		}},
		{"Rollback", func() {
			assert.NoError(tx.Rollback())
		}},
		{"BeginTx", func() {
			var err error
			tx, err = db.BeginTx(ctx, nil)
			assert.NoError(err)
		}},
		{"Commit", func() {
			assert.NoError(tx.Commit())
		}},
	}

	const rtt = 50 * time.Millisecond
	db.SimulateRTT(rtt)
	for _, op := range ops {
		t0 := time.Now()
		op.run()
		elapsed := time.Since(t0)
		assert.True(elapsed >= rtt, "%s must pay the simulated round trip, took %v", op.name, elapsed)
	}

	db.SimulateRTT(0)
	for _, op := range ops {
		t0 := time.Now()
		op.run()
		elapsed := time.Since(t0)
		assert.True(elapsed < rtt, "%s must be free again once the simulation is off, took %v", op.name, elapsed)
	}
}

// The delay is charged once per round trip, so a transaction pays for its BEGIN and COMMIT on top of
// every statement it runs — which is the whole point: it makes a chatty transaction visibly expensive.
func TestRTT_ChargedPerRoundTrip(t *testing.T) {
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	db, err := Open("sqlite", dsn)
	assert.NoError(err)
	defer db.Close()

	_, err = db.Exec("CREATE TABLE rtt_trips (k INT)")
	assert.NoError(err)

	const (
		rtt        = 40 * time.Millisecond
		statements = 3
		trips      = statements + 2 // BEGIN and COMMIT
	)
	db.SimulateRTT(rtt)

	ctx := context.Background()
	t0 := time.Now()
	err = db.Transact(ctx, func(tx *Tx) error {
		for i := range statements {
			if _, err := tx.ExecContext(ctx, "INSERT INTO rtt_trips (k) VALUES (?)", i); err != nil {
				return err
			}
		}
		return nil
	})
	elapsed := time.Since(t0)
	assert.NoError(err)
	assert.True(elapsed >= trips*rtt, "a %d-statement transaction pays %d round trips, took %v", statements, trips, elapsed)
	// A generous ceiling that still catches the delay being charged twice per operation.
	assert.True(elapsed < 2*trips*rtt, "the delay must be charged once per round trip, took %v", elapsed)

	db.SimulateRTT(0)
	var n int
	assert.NoError(db.QueryRow("SELECT COUNT(*) FROM rtt_trips").Scan(&n))
	assert.Equal(statements, n, "the transaction must still commit its work")
}

// A context whose deadline is shorter than the simulated round trip fails the operation with the
// context's error and never reaches the database — the outcome a real round trip that outlives its
// deadline produces.
func TestRTT_HonorsContextDeadline(t *testing.T) {
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	db, err := Open("sqlite", dsn)
	assert.NoError(err)
	defer db.Close()

	_, err = db.Exec("CREATE TABLE rtt_deadline (k INT)")
	assert.NoError(err)

	db.SimulateRTT(time.Minute) // far longer than any deadline below, so the pause is what expires
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
	defer cancel()

	_, err = db.ExecContext(ctx, "INSERT INTO rtt_deadline (k) VALUES (1)")
	assert.Error(err)
	assert.True(errors.Is(err, context.DeadlineExceeded), "the operation fails with the context's error, got %v", err)

	// QueryRow defers its error to Scan, where the expired context surfaces via the driver instead.
	var k int
	err = db.QueryRowContext(ctx, "SELECT k FROM rtt_deadline").Scan(&k)
	assert.Error(err)

	db.SimulateRTT(0)
	var n int
	assert.NoError(db.QueryRow("SELECT COUNT(*) FROM rtt_deadline").Scan(&n))
	assert.Equal(0, n, "a statement whose simulated round trip outran the deadline must not reach the database")
}

// A transaction captures the delay when it begins, so changing the setting mid-transaction does not
// leave it running at two different latencies.
func TestRTT_TransactionCapturesDelayAtBegin(t *testing.T) {
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	db, err := Open("sqlite", dsn)
	assert.NoError(err)
	defer db.Close()

	_, err = db.Exec("CREATE TABLE rtt_capture (k INT)")
	assert.NoError(err)

	tx, err := db.Begin()
	assert.NoError(err)
	db.SimulateRTT(time.Minute) // set after the transaction began

	t0 := time.Now()
	_, err = tx.Exec("INSERT INTO rtt_capture (k) VALUES (1)")
	assert.NoError(err)
	assert.NoError(tx.Rollback())
	elapsed := time.Since(t0)
	assert.True(elapsed < time.Second, "the transaction runs at the latency captured when it began, took %v", elapsed)
}
