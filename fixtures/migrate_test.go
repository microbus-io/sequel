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
	"sync"
	"testing"
	"testing/fstest"

	"github.com/microbus-io/sequel"
	"github.com/microbus-io/sequel/testdata"
	"github.com/microbus-io/testarossa"
	"go.opentelemetry.io/otel/codes"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
)

// TestMigrate_AutoCreate runs the full migration set against the configured provider and verifies the
// resulting data, exercising CreateTestingDatabase, Migrate, virtual-function expansion and placeholder
// conforming end to end on a real engine.
func TestMigrate_AutoCreate(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	assert.NoError(db.Migrate(t.Name(), testdata.FS))

	var count int
	assert.NoError(db.QueryRow("SELECT COUNT(id) FROM foo").Scan(&count))
	assert.Equal(4, count)

	// ? placeholders are conformed to the driver's dialect automatically by the query shadow methods.
	var id int
	assert.NoError(db.QueryRow("SELECT id FROM foo WHERE id=?", 1).Scan(&id))
	assert.Equal(1, id)

	// 10.insert.sql sorts before 2.alter-table.sql by filename but must run after it (it uses the column
	// 2 adds). Its row proves migrations ran in numeric, not lexicographic, order.
	var updated int
	assert.NoError(db.QueryRow("SELECT updated FROM foo WHERE id=?", 10).Scan(&updated))
	assert.Equal(1, updated)
}

// TestMigrate_Idempotent verifies that applying the same migration set twice is a no-op the second time:
// already-completed statements are skipped, no error surfaces, and no rows are duplicated.
func TestMigrate_Idempotent(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	assert.NoError(db.Migrate(t.Name(), testdata.FS))

	var first int
	assert.NoError(db.QueryRow("SELECT COUNT(id) FROM foo").Scan(&first))
	assert.Equal(4, first)

	// Re-running must not re-apply CREATE/INSERT statements (which would error or duplicate rows).
	assert.NoError(db.Migrate(t.Name(), testdata.FS))

	var second int
	assert.NoError(db.QueryRow("SELECT COUNT(id) FROM foo").Scan(&second))
	assert.Equal(4, second)
}

// TestMigrate_ConcurrentFirstRun starts several Migrates at once on an empty database, as a fleet rolled out
// together onto a new database does. They race to create the sequel_migrations table before any migration lease
// exists, and every one must succeed.
func TestMigrate_ConcurrentFirstRun(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	testingDSN, err := sequel.CreateTestingDatabase("", "", t.Name())
	if !assert.NoError(err) {
		return
	}
	// A pool each, so each Migrate runs in its own session, as separate replicas do.
	dbs := make([]*sequel.DB, 8)
	for i := range dbs {
		dbs[i], err = sequel.Open("", testingDSN)
		if !assert.NoError(err) {
			return
		}
		defer dbs[i].Close()
		assert.NoError(dbs[i].Ping()) // Connect up front, so the Migrates start together
	}

	start := make(chan struct{})
	errs := make([]error, len(dbs))
	var wg sync.WaitGroup
	for i, db := range dbs {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			errs[i] = db.Migrate(t.Name(), testdata.FS)
		}()
	}
	close(start)
	wg.Wait()
	for _, err := range errs {
		assert.NoError(err)
	}

	var count int
	assert.NoError(dbs[0].QueryRow("SELECT COUNT(id) FROM foo").Scan(&count))
	assert.Equal(4, count)
}

// TestMigrate_ExistingRowIsNotAnError runs a migration whose row is already registered, as every replica but the first
// finds it on a rolling deploy, and checks that no statement reports an error. SQL Server once registered rows with a
// plain INSERT and ignored the duplicate-key error, which put an error span on every such run.
func TestMigrate_ExistingRowIsNotAnError(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	// Makes the migrations table
	if !assert.NoError(db.Migrate(t.Name(), fstest.MapFS{})) {
		return
	}
	_, err := db.Exec("INSERT INTO sequel_migrations (seq_name, seq_num) VALUES (?, ?)", t.Name(), 1)
	if !assert.NoError(err) {
		return
	}
	recorder := tracetest.NewSpanRecorder()
	db.SetTracerProvider(sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(recorder)))
	defer db.SetTracerProvider(nil)

	fs := fstest.MapFS{"1.create.sql": {Data: []byte("CREATE TABLE registered (id INT)")}}
	if !assert.NoError(db.Migrate(t.Name(), fs)) {
		return
	}
	for _, span := range recorder.Ended() {
		assert.NotEqual(codes.Error, span.Status().Code, "%s reported an error: %s", span.Name(), span.Status().Description)
	}
}

// TestMigrate_KeepsDashesInStrings runs a migration whose string literal holds "--", followed by a real comment. The
// comment must go and the literal must arrive whole.
func TestMigrate_KeepsDashesInStrings(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	fs := fstest.MapFS{"1.dashes.sql": {Data: []byte(
		"-- a table\nCREATE TABLE dashes (v VARCHAR(16));\n" +
			"INSERT INTO dashes (v) -- a row\nVALUES ('a--b');\n")}}
	if !assert.NoError(db.Migrate(t.Name(), fs)) {
		return
	}
	var v string
	assert.NoError(db.QueryRow("SELECT v FROM dashes").Scan(&v))
	assert.Equal("a--b", v)
}

// TestMigrate_SemicolonsAndDirectivesInStrings runs a migration whose string literals hold a line-ending ";" and a
// "-- DRIVER:" marker. Neither is SQL: the first must not end its statement, and the second must not skip it.
func TestMigrate_SemicolonsAndDirectivesInStrings(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	fs := fstest.MapFS{"1.literals.sql": {Data: []byte(
		"CREATE TABLE literals (id INT, v VARCHAR(64));\n" +
			"INSERT INTO literals (id, v) VALUES (1, 'first;\nsecond');\n" +
			"INSERT INTO literals (id, v) VALUES (2, '-- DRIVER: none');\n")}}
	if !assert.NoError(db.Migrate(t.Name(), fs)) {
		return
	}
	var v string
	assert.NoError(db.QueryRow("SELECT v FROM literals WHERE id=1").Scan(&v))
	assert.Equal("first;\nsecond", v)
	assert.NoError(db.QueryRow("SELECT v FROM literals WHERE id=2").Scan(&v))
	assert.Equal("-- DRIVER: none", v)
}

// migratesInTransaction reports whether a migration file of more than one statement runs in one transaction on db.
func migratesInTransaction(db *sequel.DB) bool {
	switch db.DriverName() {
	case "pgx", "mssql", "sqlite":
		return true
	}
	return false
}

// migrationCompleted reports whether the migration of a sequence is marked complete.
func migrationCompleted(t *testing.T, db *sequel.DB, sequenceName string, seqNum int) bool {
	t.Helper()
	var completed bool
	err := db.QueryRow("SELECT completed FROM sequel_migrations WHERE seq_name=? AND seq_num=?", sequenceName, seqNum).Scan(&completed)
	testarossa.For(t).NoError(err)
	return completed
}

// tableRows returns the number of rows of a table, or -1 if there is no such table.
func tableRows(db *sequel.DB, table string) int {
	var n int
	if db.QueryRow("SELECT COUNT(*) FROM "+table).Scan(&n) != nil {
		return -1
	}
	return n
}

// A file of more than one statement whose later statement fails leaves nothing applied on PostgreSQL, SQL Server and
// SQLite, and is not marked complete, so the next Migrate runs it whole. On MySQL, MariaDB and CockroachDB the
// statements before the failure stay applied, as before.
func TestMigrate_AFailedFileLeavesNothingBehind(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	err := db.Migrate(t.Name(), fstest.MapFS{"1.whole.sql": {Data: []byte(
		"CREATE TABLE whole_a (id INT);\nINSERT INTO whole_a (id) VALUES (1);\nINSERT INTO whole_missing (id) VALUES (1);\n")}})
	if assert.Error(err) {
		assert.Contains(err.Error(), "1.whole.sql")
	}
	assert.False(migrationCompleted(t, db, t.Name(), 1))
	if !migratesInTransaction(db) {
		assert.Equal(1, tableRows(db, "whole_a"), "the statements before the failure stay applied")
		return
	}
	assert.Equal(-1, tableRows(db, "whole_a"), "the statements before the failure were left applied")

	assert.NoError(db.Migrate(t.Name(), fstest.MapFS{"1.whole.sql": {Data: []byte(
		"CREATE TABLE whole_a (id INT);\nINSERT INTO whole_a (id) VALUES (1);\nINSERT INTO whole_a (id) VALUES (2);\n")}}))
	assert.True(migrationCompleted(t, db, t.Name(), 1))
	assert.Equal(2, tableRows(db, "whole_a"))
}

// A file whose lock is lost before it commits leaves nothing applied where it runs in a transaction: the mark, which
// is written in the same transaction, finds the lock gone, and the whole file rolls back. Here the file itself hands
// its lock to another runner.
func TestMigrate_ALockLostBeforeCommitLeavesNothing(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	steal := "UPDATE sequel_migrations SET locked_by='another' WHERE seq_name='" + t.Name() + "'"
	err := db.Migrate(t.Name(), fstest.MapFS{"1.stolen.sql": {Data: []byte(
		"CREATE TABLE stolen_a (id INT);\n" + steal + ";\nCREATE TABLE stolen_b (id INT);\n")}})
	if assert.Error(err) {
		assert.Contains(err.Error(), "1.stolen.sql")
		assert.Contains(err.Error(), "lock was lost")
	}
	assert.False(migrationCompleted(t, db, t.Name(), 1))
	if !migratesInTransaction(db) {
		assert.Equal(0, tableRows(db, "stolen_b"), "the statements ran one at a time")
		return
	}
	assert.Equal(-1, tableRows(db, "stolen_a"), "the file was left applied")
	assert.Equal(-1, tableRows(db, "stolen_b"), "the file was left applied")

	// The rollback took the theft back with it, and the lock was released: the file runs at once
	assert.NoError(db.Migrate(t.Name(), fstest.MapFS{"1.stolen.sql": {Data: []byte(
		"CREATE TABLE stolen_a (id INT);\nCREATE TABLE stolen_b (id INT);\n")}}))
	assert.True(migrationCompleted(t, db, t.Name(), 1))
}

// A statement that refuses a transaction, such as PostgreSQL's CREATE INDEX CONCURRENTLY, runs in a file of its own,
// and fails beside another statement, which puts the file in a transaction.
func TestMigrate_AConcurrentIndexBuildStandsAlone(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	if db.DriverName() != "pgx" {
		t.Skip("CREATE INDEX CONCURRENTLY is PostgreSQL's")
	}
	fs := fstest.MapFS{
		"1.table.sql": {Data: []byte("CREATE TABLE concurrent (a INT, b INT)")},
		"2.index.sql": {Data: []byte("-- Alone in its file\nCREATE INDEX CONCURRENTLY concurrent_a ON concurrent (a);\n")},
	}
	assert.NoError(db.Migrate(t.Name(), fs))
	assert.True(migrationCompleted(t, db, t.Name(), 2))

	fs["3.pair.sql"] = &fstest.MapFile{Data: []byte(
		"CREATE INDEX CONCURRENTLY concurrent_b ON concurrent (b);\nCREATE TABLE concurrent_after (id INT);\n")}
	err := db.Migrate(t.Name(), fs)
	if assert.Error(err) {
		assert.Contains(err.Error(), "cannot run inside a transaction block")
	}
	assert.False(migrationCompleted(t, db, t.Name(), 3))
	assert.Equal(-1, tableRows(db, "concurrent_after"))
}

// The statements that decide whether a file runs in a transaction are the ones left to run on the driver: a file with
// one PostgreSQL statement and two SQLite ones runs the index build alone on PostgreSQL, which it could not do in a
// transaction, and both inserts in one transaction on SQLite, where the failure of the second takes back the first.
func TestMigrate_StatementCountIsPerDriver(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	err := db.Migrate(t.Name(), fstest.MapFS{
		"1.table.sql": {Data: []byte("CREATE TABLE per_driver (id INT)")},
		"2.mixed.sql": {Data: []byte(
			"-- DRIVER: pgx\nCREATE INDEX CONCURRENTLY per_driver_id ON per_driver (id);\n\n" +
				"-- DRIVER: sqlite\nINSERT INTO per_driver (id) VALUES (1);\n\n" +
				"-- DRIVER: sqlite\nINSERT INTO per_driver_missing (id) VALUES (2);\n")},
	})
	switch db.DriverName() {
	case "sqlite":
		assert.Error(err)
		assert.False(migrationCompleted(t, db, t.Name(), 2))
		assert.Equal(0, tableRows(db, "per_driver"), "the first insert was left applied")
	default:
		// The index build alone on PostgreSQL, and nothing at all elsewhere
		assert.NoError(err)
		assert.True(migrationCompleted(t, db, t.Name(), 2))
	}
}
