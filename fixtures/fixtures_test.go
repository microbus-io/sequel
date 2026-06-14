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

// Package fixtures holds sequel's integration tests: every test here provisions a real, isolated database
// and runs SQL against it, as opposed to the root package's unit tests, which only exercise pure logic
// (DSN parsing, placeholder conforming, virtual-function string expansion) with no server.
//
// The whole suite picks its database from the SEQUEL_TESTING_DSN environment variable, which sequel.CreateTestingDatabase
// reads itself. An unset or empty value runs against in-memory SQLite, which needs no server, so the suite is
// green out of the box. Setting SEQUEL_TESTING_DSN to a server's base DSN runs the exact same tests against that server
// instead, which is how CI exercises MySQL, PostgreSQL and SQL Server — one job per provider, each pointing the
// env var at its container. Because CreateTestingDatabase provisions an isolated, auto-dropped database per test
// off the base DSN, the DSN must connect with CREATE/DROP DATABASE privilege.
package fixtures

import (
	"strconv"
	"strings"
	"testing"

	"github.com/microbus-io/sequel"
	"github.com/microbus-io/testarossa"
)

// newTestDB provisions an isolated database for the calling test on the configured provider and opens it.
// Passing empty driver and DSN lets sequel select the provider from SEQUEL_TESTING_DSN (empty → in-memory SQLite)
// and infer the driver, so the test body never names a driver. The database is dropped and the pool closed
// when the test ends.
func newTestDB(t *testing.T) *sequel.DB {
	t.Helper()
	assert := testarossa.For(t)
	testingDSN, err := sequel.CreateTestingDatabase("", "", t.Name())
	assert.NoError(err)
	db, err := sequel.OpenSingleton("", testingDSN)
	assert.NoError(err)
	if !assert.NotNil(db) {
		t.FailNow()
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// queryFloat runs a single-value numeric query and returns the result as a float64. Numeric results cross
// drivers in inconsistent native types — DECIMAL as []byte on MySQL, numeric on pgx, BIGINT on SQL Server —
// so the value is scanned into a string (which database/sql produces for any of them) and parsed, keeping
// the virtual-function arithmetic tests driver-agnostic.
func queryFloat(t *testing.T, db *sequel.DB, query string, args ...any) float64 {
	t.Helper()
	assert := testarossa.For(t)
	var s string
	err := db.QueryRow(query, args...).Scan(&s)
	if !assert.NoError(err) {
		return 0
	}
	f, err := strconv.ParseFloat(strings.TrimSpace(s), 64)
	assert.NoError(err)
	return f
}
