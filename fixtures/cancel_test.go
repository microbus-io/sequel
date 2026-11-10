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
	"net"
	"testing"
	"time"

	"github.com/microbus-io/sequel"
	"github.com/microbus-io/testarossa"
)

// TestTransact_EndedContextLeavesThePoolUsable: a transaction whose context ends - cancelled or past its deadline,
// idle or with a statement in flight, run by Transact, by BeginTx and rolled back, or by BeginTx and abandoned -
// leaves the pool serving the next statement. go-mssqldb v1.11.2 pools a connection whose rollback on a cancelled
// context returned with the server's reply unread, and the next statement on it fails (microsoft/go-mssqldb#390).
// A pool of one connection puts the next statement on that connection, and the case is iterated because the
// failure is a race the driver mostly, but not always, loses.
func TestTransact_EndedContextLeavesThePoolUsable(t *testing.T) {
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
			for _, entry := range []string{"Transact", "BeginTx", "BeginTx abandoned"} {
				t.Run(ending+"/"+state+"/"+entry, func(t *testing.T) {
					t.Parallel()
					assert := testarossa.For(t)

					db := newTestDB(t)
					if db.DriverName() == "mssql" && raceDetector {
						// A driver race, not sequel's; CI runs this test on SQL Server without the race detector too
						t.Skip("go-mssqldb races on a discarded connection's read buffer")
					}
					sleep, ok := sleeps[db.DriverName()]
					if state == "in flight" && !ok {
						t.Skip("no sleep statement on " + db.DriverName())
					}
					db.SetMaxOpenConns(1)
					iterations := 20
					if db.DriverName() == "mssql" {
						iterations = 300
					}

					failures := 0
					var firstErr error
					withTimeout(t, 5*time.Minute, func() {
						for range iterations {
							var ctx context.Context
							var cancel context.CancelFunc
							end := func() { <-ctx.Done() }
							if ending == "cancelled" {
								ctx, cancel = context.WithCancel(context.Background())
								end = cancel
							} else {
								// Long enough for BEGIN to reconnect, when the previous iteration's connection was discarded
								ctx, cancel = context.WithTimeout(context.Background(), 100*time.Millisecond)
							}
							work := func(tx *sequel.Tx) error {
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
							}
							switch entry {
							case "Transact":
								err := db.Transact(ctx, work)
								if state == "idle" {
									// The caller's own error comes back: a cancellation is not reported as a deadline
									assert.True(errors.Is(err, ctx.Err()), "got %v, want %v", err, ctx.Err())
								} else {
									assert.Error(err)
								}
							case "BeginTx", "BeginTx abandoned":
								tx, err := db.BeginTx(ctx, nil)
								if err != nil {
									// A deadline may pass during BEGIN, on a loaded server
									assert.True(ending == "deadline" && errors.Is(err, context.DeadlineExceeded), "got %v", err)
									break
								}
								assert.Error(work(tx))
								if entry == "BeginTx" {
									_ = tx.Rollback()
								}
							}
							cancel()
							var one int
							if err := db.QueryRowContext(context.Background(), "SELECT 1").Scan(&one); err != nil {
								failures++
								if firstErr == nil {
									firstErr = err
								}
							}
						}
					})
					assert.Zero(failures, "%d of %d statements after the transaction failed, first: %v", failures, iterations, firstErr)
				})
			}
		}
	}
}

// TestTransact_CancellationIsNotANetworkError: the errors a cancelled transaction returns are the plain cancellation.
// On SQL Server the driver is handed a cancellation as a network error, so that it discards the connection, and no
// caller may see that as a network error.
func TestTransact_CancellationIsNotANetworkError(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	isCancellation := func(op string, err error) {
		t.Helper()
		var netErr net.Error
		assert.True(errors.Is(err, context.Canceled), "%s: got %v", op, err)
		assert.False(errors.As(err, &netErr), "%s: a network error: %v", op, err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := db.BeginTx(cancelled, nil)
	isCancellation("BEGIN", err)
	isCancellation("Transact", db.Transact(cancelled, func(tx *sequel.Tx) error { return nil }))

	for _, end := range []string{"COMMIT", "ROLLBACK"} {
		ctx, cancel := context.WithCancel(context.Background())
		tx, err := db.BeginTx(ctx, nil)
		assert.Must(assert.NoError(err))
		_, err = tx.ExecContext(ctx, "SELECT 1")
		assert.NoError(err)
		cancel()
		if end == "COMMIT" {
			err = tx.Commit()
		} else {
			err = tx.Rollback()
		}
		if err != nil && !errors.Is(err, sql.ErrTxDone) {
			// database/sql may have rolled back already, and a rollback may succeed despite the cancellation
			isCancellation(end, err)
		}
	}
}
