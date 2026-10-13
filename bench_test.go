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
	"database/sql/driver"
	"io"
	"sync"
	"testing"
)

// The benchmarks below measure sequel's own per-statement overhead: the statements go to benchDriver, a driver
// that answers every statement instantly with one row, so what varies between runs is sequel's work (unpacking,
// telemetry) plus a constant database/sql cost. The *DB is labeled with a real driver name so that the
// driver-specific paths (placeholder conforming on pgx) run as they would in production.

// benchQuery has a quoted literal, which sends conformPlaceholders down its byte-by-byte path on pgx, and a
// single table, so parseOperation extracts db.collection.name.
const benchQuery = "SELECT step_id FROM microbus_steps WHERE status='running' AND flow_id=? AND step_id>? LIMIT 1"

type benchDriver struct{}

func (benchDriver) Open(string) (driver.Conn, error) { return benchConn{}, nil }

type benchConn struct{}

func (benchConn) Prepare(string) (driver.Stmt, error) { return benchStmt{}, nil }
func (benchConn) Close() error                        { return nil }
func (benchConn) Begin() (driver.Tx, error)           { return benchTx{}, nil }
func (benchConn) BeginTx(context.Context, driver.TxOptions) (driver.Tx, error) {
	return benchTx{}, nil
}
func (benchConn) ExecContext(context.Context, string, []driver.NamedValue) (driver.Result, error) {
	return driver.RowsAffected(1), nil
}
func (benchConn) QueryContext(context.Context, string, []driver.NamedValue) (driver.Rows, error) {
	return &benchRows{}, nil
}

type benchStmt struct{}

func (benchStmt) Close() error                               { return nil }
func (benchStmt) NumInput() int                              { return -1 }
func (benchStmt) Exec([]driver.Value) (driver.Result, error) { return driver.RowsAffected(1), nil }
func (benchStmt) Query([]driver.Value) (driver.Rows, error)  { return &benchRows{}, nil }

type benchTx struct{}

func (benchTx) Commit() error   { return nil }
func (benchTx) Rollback() error { return nil }

type benchRows struct{ done bool }

func (*benchRows) Columns() []string { return []string{"step_id"} }
func (*benchRows) Close() error      { return nil }
func (r *benchRows) Next(dest []driver.Value) error {
	if r.done {
		return io.EOF
	}
	r.done = true
	dest[0] = int64(1)
	return nil
}

var registerBenchDriver sync.Once

// openBenchDB returns a *DB labeled as driverName whose statements run on benchDriver, with the default
// telemetry - the global OpenTelemetry providers, no-ops since nothing installs real ones here.
func openBenchDB(b *testing.B, driverName string) *DB {
	registerBenchDriver.Do(func() { sql.Register("sequel_bench", benchDriver{}) })
	sqlDB, err := sql.Open("sequel_bench", "")
	if err != nil {
		b.Fatal(err)
	}
	sqlDB.SetMaxIdleConns(256)
	db := &DB{DB: sqlDB, driverName: driverName, refCount: 1}
	db.telemetry.Store(newDefaultTelemetry(db))
	b.Cleanup(func() { sqlDB.Close() })
	return db
}

var benchDrivers = []string{"pgx", "mysql"}

func BenchmarkUnpackQuery(b *testing.B) {
	for _, driverName := range benchDrivers {
		b.Run(driverName, func(b *testing.B) {
			b.ReportAllocs()
			for b.Loop() {
				if _, err := unpackQuery(driverName, benchQuery); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkUnpackQuery_Parallel(b *testing.B) {
	for _, driverName := range benchDrivers {
		b.Run(driverName, func(b *testing.B) {
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				for pb.Next() {
					if _, err := unpackQuery(driverName, benchQuery); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}

func BenchmarkDB_ExecContext(b *testing.B) {
	for _, driverName := range benchDrivers {
		b.Run(driverName, func(b *testing.B) {
			db := openBenchDB(b, driverName)
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				if _, err := db.ExecContext(ctx, benchQuery, 1, 2); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkDB_QueryRowContext(b *testing.B) {
	for _, driverName := range benchDrivers {
		b.Run(driverName, func(b *testing.B) {
			db := openBenchDB(b, driverName)
			ctx := context.Background()
			b.ReportAllocs()
			var id int64
			for b.Loop() {
				if err := db.QueryRowContext(ctx, benchQuery, 1, 2).Scan(&id); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

func BenchmarkDB_QueryRowContext_Parallel(b *testing.B) {
	for _, driverName := range benchDrivers {
		b.Run(driverName, func(b *testing.B) {
			db := openBenchDB(b, driverName)
			ctx := context.Background()
			b.ReportAllocs()
			b.RunParallel(func(pb *testing.PB) {
				var id int64
				for pb.Next() {
					if err := db.QueryRowContext(ctx, benchQuery, 1, 2).Scan(&id); err != nil {
						b.Error(err)
						return
					}
				}
			})
		})
	}
}

// BenchmarkDB_Transact is one Transact with a QueryRow and an Exec: BEGIN, two statements and COMMIT.
func BenchmarkDB_Transact(b *testing.B) {
	for _, driverName := range benchDrivers {
		b.Run(driverName, func(b *testing.B) {
			db := openBenchDB(b, driverName)
			ctx := context.Background()
			b.ReportAllocs()
			for b.Loop() {
				err := db.Transact(ctx, func(tx *Tx) error {
					var id int64
					if err := tx.QueryRowContext(ctx, benchQuery, 1, 2).Scan(&id); err != nil {
						return err
					}
					_, err := tx.ExecContext(ctx, "UPDATE microbus_steps SET status='done' WHERE step_id=?", id)
					return err
				})
				if err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkStmt_QueryRowContext executes a statement prepared once on the DB.
func BenchmarkStmt_QueryRowContext(b *testing.B) {
	for _, driverName := range benchDrivers {
		b.Run(driverName, func(b *testing.B) {
			db := openBenchDB(b, driverName)
			ctx := context.Background()
			stmt, err := db.PrepareContext(ctx, benchQuery)
			if err != nil {
				b.Fatal(err)
			}
			defer stmt.Close()
			b.ReportAllocs()
			var id int64
			for b.Loop() {
				if err := stmt.QueryRowContext(ctx, 1, 2).Scan(&id); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkRawSQL_QueryRowContext is the same query on the bare *sql.DB, the database/sql and benchDriver cost
// that the sequel benchmarks include and sequel does not control.
func BenchmarkRawSQL_QueryRowContext(b *testing.B) {
	db := openBenchDB(b, "mysql")
	ctx := context.Background()
	b.ReportAllocs()
	var id int64
	for b.Loop() {
		if err := db.DB.QueryRowContext(ctx, benchQuery, 1, 2).Scan(&id); err != nil {
			b.Fatal(err)
		}
	}
}
