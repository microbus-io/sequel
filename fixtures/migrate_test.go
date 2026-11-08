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
