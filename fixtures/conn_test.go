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
	"fmt"
	"strconv"
	"testing"
	"time"

	"github.com/microbus-io/sequel"
	"github.com/microbus-io/testarossa"
)

// identityColumn is an auto-generated integer primary key on each driver, for InsertReturnID.
func identityColumn(driverName string) string {
	switch driverName {
	case "mysql", "mariadb":
		return "id INT AUTO_INCREMENT PRIMARY KEY"
	case "pgx", "cockroachdb":
		return "id SERIAL PRIMARY KEY"
	case "mssql":
		return "id INT IDENTITY(1,1) PRIMARY KEY"
	default:
		return "id INTEGER PRIMARY KEY AUTOINCREMENT"
	}
}

// poolIsHeld asserts that the pool has no connection to give: its only one is held by a Conn.
func poolIsHeld(t *testing.T, db *sequel.DB) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	var one int
	err := db.QueryRowContext(ctx, "SELECT 1").Scan(&one)
	testarossa.For(t).True(errors.Is(err, context.DeadlineExceeded), "the pool served a statement: %v", err)
}

// TestConn_HoldsOneConnection: everything run on a Conn runs on its one connection, with sequel's handling. The pool
// has a single connection, so a statement or a transaction attempt that went to the pool instead would wait for it
// and time out.
func TestConn_HoldsOneConnection(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	timestamp := "TIMESTAMP(3)"
	switch db.DriverName() {
	case "mssql":
		timestamp = "DATETIME2(3)"
	case "mysql", "mariadb":
		timestamp = "DATETIME(3)"
	}
	_, err := db.Exec("CREATE TABLE conn_hold (" + identityColumn(db.DriverName()) + ", v INT, at_time " + timestamp + ")")
	assert.Must(assert.NoError(err))
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	assert.Must(assert.NoError(err))
	defer conn.Close()
	poolIsHeld(t, db)

	// Placeholders and virtual functions, through every statement method
	for v := 1; v <= 5; v++ {
		_, err = conn.ExecContext(ctx, "INSERT INTO conn_hold (v, at_time) VALUES (?, NOW_UTC())", v)
		assert.NoError(err)
	}
	id, err := conn.InsertReturnID(ctx, "id", "INSERT INTO conn_hold (v, at_time) VALUES (?, NOW_UTC())", 6)
	assert.NoError(err)
	assert.True(id > 0)
	rows, err := conn.QueryContext(ctx, "SELECT v FROM conn_hold WHERE v>? ORDER BY v LIMIT_OFFSET(?, ?)", 1, 2, 1)
	if assert.NoError(err) {
		got := []int{}
		for rows.Next() {
			var v int
			assert.NoError(rows.Scan(&v))
			got = append(got, v)
		}
		assert.NoError(rows.Err())
		assert.NoError(rows.Close())
		assert.Equal([]int{3, 4}, got)
	}
	var n int
	assert.NoError(conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM conn_hold WHERE at_time <= NOW_UTC()").Scan(&n))
	assert.Equal(6, n)
	stmt, err := conn.PrepareContext(ctx, "SELECT COUNT(*) FROM conn_hold WHERE v>=?")
	if assert.NoError(err) {
		assert.NoError(stmt.QueryRowContext(ctx, 4).Scan(&n))
		assert.Equal(3, n)
		assert.NoError(stmt.Close())
	}
	assert.NoError(conn.PingContext(ctx))

	// Code written against Executor runs on a Conn
	count := func(e sequel.Executor) int {
		var n int
		assert.NoError(e.QueryRowContext(ctx, "SELECT COUNT(*) FROM conn_hold WHERE v<=?", 2).Scan(&n))
		return n
	}
	assert.Equal(2, count(conn))

	// Transact runs on the held connection
	err = conn.Transact(ctx, func(tx *sequel.Tx) error {
		_, err := tx.ExecContext(ctx, "UPDATE conn_hold SET v=v+? WHERE v>?", 10, 5)
		if err != nil {
			return err
		}
		assert.Equal(2, count(tx))
		return nil
	})
	assert.NoError(err)
	assert.NoError(conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM conn_hold WHERE v=?", 16).Scan(&n))
	assert.Equal(1, n)
	poolIsHeld(t, db)

	// Closing returns the connection to the pool
	assert.NoError(conn.Close())
	assert.NoError(db.QueryRowContext(ctx, "SELECT COUNT(*) FROM conn_hold").Scan(&n))
	assert.Equal(6, n)
}

// TestConn_TransactKeepsTheConn: a transaction that fails or rolls back for any reason but a lost connection or an
// ended context leaves the Conn usable, and commits nothing.
func TestConn_TransactKeepsTheConn(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE conn_keep (id INT NOT NULL PRIMARY KEY)")
	assert.Must(assert.NoError(err))
	conn, err := db.Conn(ctx)
	assert.Must(assert.NoError(err))
	defer conn.Close()

	// The closure fails
	errClosure := errors.New("closure failed")
	err = conn.Transact(ctx, func(tx *sequel.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO conn_keep (id) VALUES (?)", 1)
		assert.NoError(err)
		return errClosure
	})
	assert.True(errors.Is(err, errClosure), "got %v", err)

	// A statement fails, and the closure ignores it
	err = conn.Transact(ctx, func(tx *sequel.Tx) error {
		_, _ = tx.ExecContext(ctx, "INSERT INTO conn_keep (id) VALUES (?)", 2)
		_, _ = tx.ExecContext(ctx, "INSERT INTO conn_absent (id) VALUES (?)", 3)
		return nil
	})
	assert.Error(err)
	assert.False(errors.Is(err, sql.ErrConnDone))

	var n int
	assert.NoError(conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM conn_keep").Scan(&n))
	assert.Zero(n, "a failed transaction committed")
	assert.NoError(conn.Transact(ctx, func(tx *sequel.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO conn_keep (id) VALUES (?)", 4)
		return err
	}))
	assert.NoError(conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM conn_keep").Scan(&n))
	assert.Equal(1, n)
}

// TestConn_TransactRetriesOnItsConnection: an attempt that loses to lock contention is retried on the same connection.
// The pool has a single connection, held by the Conn, so a retry that went to the pool would time out.
func TestConn_TransactRetriesOnItsConnection(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE conn_retry (id INT)")
	assert.Must(assert.NoError(err))
	db.SetMaxOpenConns(1)

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	conn, err := db.Conn(ctx)
	assert.Must(assert.NoError(err))
	defer conn.Close()

	attempts := 0
	err = conn.Transact(ctx, func(tx *sequel.Tx) error {
		attempts++
		if _, err := tx.ExecContext(ctx, "INSERT INTO conn_retry (id) VALUES (?)", attempts); err != nil {
			return err
		}
		if attempts < 3 {
			return errors.New("deadlock detected")
		}
		return nil
	})
	assert.NoError(err)
	assert.Equal(3, attempts)
	var ids []int
	rows, err := conn.QueryContext(ctx, "SELECT id FROM conn_retry")
	if assert.NoError(err) {
		for rows.Next() {
			var id int
			assert.NoError(rows.Scan(&id))
			ids = append(ids, id)
		}
		assert.NoError(rows.Close())
	}
	assert.Equal([]int{3}, ids, "only the last attempt commits")
}

// TestConn_EndedContextEndsTheConn: a transaction whose context ends before it commits ends its Conn, which then
// returns sql.ErrConnDone from every call, while closing it hands the pool a connection that serves the next
// statement. The pool has a single connection, so that statement runs on the connection the Conn held, or on its
// replacement if the driver discarded it. On SQL Server the driver discards it after a cancelled rollback, and that
// race is iterated as in TestTransact_EndedContextLeavesThePoolUsable.
func TestConn_EndedContextEndsTheConn(t *testing.T) {
	t.Parallel()

	sleeps := map[string]string{
		"mssql":       "WAITFOR DELAY '00:00:05'",
		"pgx":         "SELECT pg_sleep(5)",
		"cockroachdb": "SELECT pg_sleep(5)",
		"mysql":       "SELECT SLEEP(5)",
		"mariadb":     "SELECT SLEEP(5)",
	}
	for _, ending := range []string{"cancelled", "deadline"} {
		for _, state := range []string{"idle", "in flight"} {
			t.Run(ending+"/"+state, func(t *testing.T) {
				t.Parallel()
				assert := testarossa.For(t)

				db := newTestDB(t)
				if db.DriverName() == "mssql" && raceDetector {
					t.Skip("go-mssqldb races on a discarded connection's read buffer")
				}
				sleep, ok := sleeps[db.DriverName()]
				if state == "in flight" && !ok {
					t.Skip("no sleep statement on " + db.DriverName())
				}
				db.SetMaxOpenConns(1)
				iterations := 10
				if db.DriverName() == "mssql" {
					iterations = 100
				}

				withTimeout(t, 5*time.Minute, func() {
					for range iterations {
						conn, err := db.Conn(context.Background())
						if !assert.NoError(err) {
							return
						}
						var ctx context.Context
						var cancel context.CancelFunc
						end := func() { <-ctx.Done() }
						if ending == "cancelled" {
							ctx, cancel = context.WithCancel(context.Background())
							end = cancel
						} else {
							ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
						}
						err = conn.Transact(ctx, func(tx *sequel.Tx) error {
							if _, err := tx.ExecContext(ctx, "SELECT 1"); err != nil {
								return err
							}
							if state == "idle" {
								end()
								return ctx.Err()
							}
							if ending == "cancelled" {
								time.AfterFunc(10*time.Millisecond, end)
							}
							_, err := tx.ExecContext(ctx, sleep)
							return err
						})
						cancel()
						assert.Error(err)

						_, err = conn.ExecContext(context.Background(), "SELECT 1")
						assert.True(err == sql.ErrConnDone, "a statement after the ended transaction: %v", err) //nolint:errorlint // the identity comparison is the contract under test

						err = conn.Transact(context.Background(), func(tx *sequel.Tx) error { return nil })
						assert.True(err == sql.ErrConnDone, "a transaction after the ended one: %v", err) //nolint:errorlint // ditto
						assert.NoError(conn.Close())

						var one int
						assert.NoError(db.QueryRowContext(context.Background(), "SELECT 1").Scan(&one))
					}
				})
			})
		}
	}
}

// TestConn_LostConnectionIsNotReplaced: a Conn whose connection is killed by the server reports the loss, then
// returns sql.ErrConnDone from every call rather than taking another connection from the pool.
func TestConn_LostConnectionIsNotReplaced(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	var idQuery, kill string
	switch db.DriverName() {
	case "pgx":
		idQuery, kill = "SELECT pg_backend_pid()", "SELECT pg_terminate_backend(%d)"
	case "mysql", "mariadb":
		idQuery, kill = "SELECT CONNECTION_ID()", "KILL %d"
	case "mssql":
		idQuery, kill = "SELECT @@SPID", "KILL %d"
	default:
		t.Skip("no way to kill a session on " + db.DriverName())
	}
	conn, err := db.Conn(ctx)
	assert.Must(assert.NoError(err))
	defer conn.Close()
	var id int64
	assert.Must(assert.NoError(conn.QueryRowContext(ctx, idQuery).Scan(&id)))
	_, err = db.ExecContext(ctx, fmt.Sprintf(kill, id))
	assert.Must(assert.NoError(err))

	// The server ends the session asynchronously, so the connection may answer once more before it notices
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err = conn.ExecContext(ctx, "SELECT 1")
		if err != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.Must(assert.Error(err, "the killed connection kept answering"))
	assert.True(sequel.IsConnectionError(err), "a statement on a killed connection: %T %v", err, err)

	for i := range 3 {
		_, err = conn.ExecContext(ctx, "SELECT 1")
		assert.True(err == sql.ErrConnDone, "statement %d after the loss: %v", i, err) //nolint:errorlint // the identity comparison is the contract under test
	}
	err = conn.Transact(ctx, func(tx *sequel.Tx) error { return nil })
	assert.True(err == sql.ErrConnDone, "a transaction after the loss: %v", err) //nolint:errorlint // ditto
	assert.NoError(conn.Close())

	var one int
	assert.NoError(db.QueryRowContext(ctx, "SELECT 1").Scan(&one), "the pool replaces the lost connection")
}

// TestConn_BatchOneRoundTrip: a batch in a transaction run on a Conn pipelines on the pgx drivers, as under
// DB.Transact.
func TestConn_BatchOneRoundTrip(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE conn_batch (id INT)")
	assert.Must(assert.NoError(err))
	conn, err := db.Conn(ctx)
	assert.Must(assert.NoError(err))
	defer conn.Close()

	const statements = 10
	run := func() error {
		return conn.Transact(ctx, func(tx *sequel.Tx) error {
			b := tx.Batch()
			for i := range statements {
				b.Queue("INSERT INTO conn_batch (id) VALUES (" + strconv.Itoa(i) + ")")
			}
			return b.Send(ctx).Close()
		})
	}
	assert.NoError(run()) // Prepares the statements on the connection

	const rtt = 50 * time.Millisecond
	db.SimulateRTT(rtt)
	defer db.SimulateRTT(0)
	start := time.Now()
	assert.NoError(run())
	elapsed := time.Since(start)
	if pipelines(db) {
		assert.True(elapsed < 6*rtt, "the batch is one round trip; took %v", elapsed)
	} else {
		assert.True(elapsed >= statements*rtt, "every statement is a round trip; took %v", elapsed)
	}
	var n int
	assert.NoError(conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM conn_batch").Scan(&n))
	assert.Equal(2*statements, n)
}

// TestConn_EndedContextLeavesTheConnUsable: a statement whose context has already ended returns the context's error
// and leaves the Conn usable. pgx reports such a statement as a bad connection, and database/sql would discard it.
func TestConn_EndedContextLeavesTheConnUsable(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE conn_ended (v INT)")
	assert.Must(assert.NoError(err))
	conn, err := db.Conn(ctx)
	assert.Must(assert.NoError(err))
	defer conn.Close()

	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	var n int
	_, err = conn.ExecContext(cancelled, "INSERT INTO conn_ended (v) VALUES (?)", 1)
	assert.True(errors.Is(err, context.Canceled), "ExecContext: got %v", err)
	err = conn.QueryRowContext(cancelled, "SELECT COUNT(*) FROM conn_ended").Scan(&n)
	assert.True(errors.Is(err, context.Canceled), "QueryRowContext: got %v", err)
	err = conn.PingContext(cancelled)
	assert.True(errors.Is(err, context.Canceled), "PingContext: got %v", err)
	err = conn.Transact(cancelled, func(tx *sequel.Tx) error { return nil })
	assert.True(errors.Is(err, context.Canceled), "Transact: got %v", err)

	_, err = conn.ExecContext(ctx, "INSERT INTO conn_ended (v) VALUES (?)", 2)
	assert.NoError(err, "the Conn is usable after calls on an ended context")
	assert.NoError(conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM conn_ended").Scan(&n))
	assert.Equal(1, n)
}

// TestConn_NestedTransactIsRefused: a Transact inside a Transact on the same Conn fails, and the outer transaction
// rolls back. Run, it would begin a transaction on a connection already in one: MySQL commits the outer transaction
// at the inner BEGIN, and PostgreSQL commits it at the inner COMMIT.
func TestConn_NestedTransactIsRefused(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE conn_nested (v INT)")
	assert.Must(assert.NoError(err))
	conn, err := db.Conn(ctx)
	assert.Must(assert.NoError(err))
	defer conn.Close()

	errOuter := errors.New("the outer transaction fails")
	err = conn.Transact(ctx, func(tx *sequel.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO conn_nested (v) VALUES (?)", 1); err != nil {
			return err
		}
		err := conn.Transact(ctx, func(tx *sequel.Tx) error {
			_, err := tx.ExecContext(ctx, "INSERT INTO conn_nested (v) VALUES (?)", 2)
			return err
		})
		assert.Error(err, "a nested Transact ran")
		_, err = conn.ExecContext(ctx, "INSERT INTO conn_nested (v) VALUES (?)", 3)
		assert.Error(err, "a statement on the Conn ran inside its transaction")
		return errOuter
	})
	assert.True(errors.Is(err, errOuter), "got %v", err)
	var n int
	assert.NoError(conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM conn_nested").Scan(&n))
	assert.Zero(n, "the rolled-back transaction left rows")
}

// TestConn_XactAbortEndsWithTheTransaction: the XACT_ABORT that Transact turns on for SQL Server is off again for the
// Conn's next statement outside a transaction, and on in every transaction, the ones that skip setting it included.
func TestConn_XactAbortEndsWithTheTransaction(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	if db.DriverName() != "mssql" {
		t.Skip("XACT_ABORT is SQL Server's")
	}
	conn, err := db.Conn(ctx)
	assert.Must(assert.NoError(err))
	defer conn.Close()

	const xactAbort = "SELECT CAST(@@OPTIONS & 16384 AS INT)"
	var on int
	assert.NoError(conn.QueryRowContext(ctx, xactAbort).Scan(&on))
	assert.Zero(on, "XACT_ABORT is on before any transaction")
	for range 2 {
		for range 2 {
			assert.NoError(conn.Transact(ctx, func(tx *sequel.Tx) error {
				return tx.QueryRowContext(ctx, xactAbort).Scan(&on)
			}))
			assert.NotZero(on, "XACT_ABORT is off inside a transaction")
		}
		assert.NoError(conn.QueryRowContext(ctx, xactAbort).Scan(&on))
		assert.Zero(on, "XACT_ABORT is still on after the transaction")
	}

	// A batch that continues past a failed statement, as it does with XACT_ABORT off
	_, err = conn.ExecContext(ctx, "CREATE TABLE conn_xact (id INT PRIMARY KEY)")
	assert.NoError(err)
	assert.NoError(conn.Transact(ctx, func(tx *sequel.Tx) error {
		_, err := tx.ExecContext(ctx, "INSERT INTO conn_xact (id) VALUES (?)", 1)
		return err
	}))
	_, err = conn.ExecContext(ctx, "INSERT INTO conn_xact (id) VALUES (1); INSERT INTO conn_xact (id) VALUES (2)")
	assert.Error(err)
	var n int
	assert.NoError(conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM conn_xact").Scan(&n))
	assert.Equal(2, n, "the batch stopped at its first error")
}
