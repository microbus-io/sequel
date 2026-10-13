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
)

// benchQuery has a quoted literal, which sends placeholder conforming down its slow path on PostgreSQL, and a
// single table, so the operation parser extracts db.collection.name.
const benchQuery = "SELECT step_id FROM bench_steps WHERE status='running' AND flow_id=? AND step_id>?"

// newBenchDB opens a testing database selected by SEQUEL_TESTING_DSN, with one row in bench_steps.
func newBenchDB(b *testing.B) *sequel.DB {
	b.Helper()
	testingDSN, err := sequel.CreateTestingDatabase("", "", b.Name())
	if err != nil {
		b.Fatal(err)
	}
	db, err := sequel.Open("", testingDSN)
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { db.Close() })
	ctx := context.Background()
	_, err = db.ExecContext(ctx, "CREATE TABLE bench_steps (step_id INT PRIMARY KEY, flow_id INT, status VARCHAR(16))")
	if err != nil {
		b.Fatal(err)
	}
	_, err = db.ExecContext(ctx, "INSERT INTO bench_steps (step_id, flow_id, status) VALUES (1, 1, 'running')")
	if err != nil {
		b.Fatal(err)
	}
	return db
}

func BenchmarkQueryRowContext(b *testing.B) {
	db := newBenchDB(b)
	ctx := context.Background()
	b.ReportAllocs()
	var id int64
	for b.Loop() {
		if err := db.QueryRowContext(ctx, benchQuery, 1, 0).Scan(&id); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkQueryRowContext_Parallel(b *testing.B) {
	db := newBenchDB(b)
	ctx := context.Background()
	b.ReportAllocs()
	b.RunParallel(func(pb *testing.PB) {
		var id int64
		for pb.Next() {
			if err := db.QueryRowContext(ctx, benchQuery, 1, 0).Scan(&id); err != nil {
				b.Error(err)
				return
			}
		}
	})
}

func BenchmarkTransact(b *testing.B) {
	db := newBenchDB(b)
	ctx := context.Background()
	b.ReportAllocs()
	for b.Loop() {
		err := db.Transact(ctx, func(tx *sequel.Tx) error {
			var id int64
			if err := tx.QueryRowContext(ctx, benchQuery, 1, 0).Scan(&id); err != nil {
				return err
			}
			_, err := tx.ExecContext(ctx, "UPDATE bench_steps SET status='running' WHERE step_id=?", id)
			return err
		})
		if err != nil {
			b.Fatal(err)
		}
	}
}
