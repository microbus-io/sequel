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
	"os"
	"strings"
	"testing"

	"github.com/microbus-io/sequel"
	"github.com/microbus-io/testarossa"
)

// sharesTestingDatabase checks that every call returns a testing DSN that opens as the driver it names, all of them on
// one database: a table the first creates must survive the calls after it, which would otherwise drop and recreate
// the database under its open handle.
func sharesTestingDatabase(t *testing.T, id string, calls []testingDSNCall) {
	assert := testarossa.For(t)
	var first *sequel.DB
	for i, c := range calls {
		dsn, err := sequel.CreateTestingDatabase(c.driver, c.base, id)
		if !assert.NoError(err) {
			return
		}
		assert.True(strings.HasPrefix(dsn, c.prefix), "%s %q gave %q", c.driver, c.base, dsn)
		db, err := sequel.Open("", dsn)
		if !assert.NoError(err) {
			return
		}
		defer db.Close()
		assert.Equal(c.opensAs, db.DriverName(), "%s %q gave %q", c.driver, c.base, dsn)
		if i == 0 {
			first = db
			_, err = db.Exec("CREATE TABLE kept (id INT)")
			assert.NoError(err)
			continue
		}
		_, err = first.Exec("INSERT INTO kept (id) VALUES (?)", i)
		assert.NoError(err, "%s %q dropped the database", c.driver, c.base)
	}
}

type testingDSNCall struct {
	driver, base    string
	prefix, opensAs string
}

// TestDSN_CockroachDBTestingDSNNamesItsDriver checks that a CockroachDB testing DSN says it is CockroachDB, so that
// opening it without a driver name does not take it for PostgreSQL. A base spelled either way, asked for as
// CockroachDB or as PostgreSQL, reaches one testing database.
func TestDSN_CockroachDBTestingDSNNamesItsDriver(t *testing.T) {
	t.Parallel()

	base := os.Getenv("SEQUEL_TESTING_DSN")
	rest, ok := strings.CutPrefix(base, "cockroachdb://")
	if !ok {
		t.Skip("needs SEQUEL_TESTING_DSN in cockroachdb:// form")
	}
	sharesTestingDatabase(t, t.Name(), []testingDSNCall{
		{"cockroachdb", "postgres://" + rest, "cockroachdb://", "cockroachdb"},
		{"", "cockroachdb://" + rest, "cockroachdb://", "cockroachdb"},
		{"pgx", "postgres://" + rest, "postgres://", "pgx"},
	})
}

// TestDSN_MariaDBTestingDSNNamesItsDriver checks that a MariaDB testing DSN says it is MariaDB: the driver's own DSN
// format names no engine, and would be taken for MySQL when opened without a driver name. A named driver wins over the
// base's pseudo-scheme, a mysql:// base keeps its prefix, and every spelling of one base, asked for as MariaDB or as
// MySQL, reaches one testing database.
func TestDSN_MariaDBTestingDSNNamesItsDriver(t *testing.T) {
	t.Parallel()

	base := os.Getenv("SEQUEL_TESTING_DSN")
	native, ok := strings.CutPrefix(base, "mariadb://")
	if !ok {
		t.Skip("needs SEQUEL_TESTING_DSN in mariadb:// form")
	}
	sharesTestingDatabase(t, t.Name(), []testingDSNCall{
		{"mariadb", native, "mariadb://", "mariadb"},
		{"", base, "mariadb://", "mariadb"},
		{"mysql", native, native[:4], "mysql"},
		{"mysql", base, "mysql://", "mysql"},
		{"", "mysql://" + native, "mysql://", "mysql"},
	})
}

// Both logical PostgreSQL drivers must survive the provision -> Open/OpenSingleton flow with a keyword DSN.
func TestDSN_KeywordTestingDSNNamesItsDriver(t *testing.T) {
	t.Parallel()
	base := os.Getenv("SEQUEL_TESTING_DSN")
	if !strings.HasPrefix(base, "host=") {
		t.Skip("needs SEQUEL_TESTING_DSN in keyword form beginning with host=")
	}
	sharesTestingDatabase(t, t.Name(), []testingDSNCall{
		{"pgx", base, "pgx+dsn://", "pgx"},
		{"cockroachdb", base, "cockroachdb+dsn://", "cockroachdb"},
		{"", base, "pgx+dsn://", "pgx"},
	})
	assert := testarossa.For(t)
	for _, driver := range []string{"pgx", "cockroachdb"} {
		dsn, err := sequel.CreateTestingDatabase(driver, base, t.Name()+driver)
		if !assert.NoError(err) {
			continue
		}
		db, err := sequel.OpenSingleton("", dsn)
		if !assert.NoError(err) {
			continue
		}
		assert.Equal(driver, db.DriverName())
		assert.NoError(db.Ping())
		db.Close()
	}
}
