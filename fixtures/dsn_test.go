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

// TestDSN_CockroachDBTestingDSNNamesItsDriver checks that a CockroachDB testing DSN says it is CockroachDB, so that
// opening it without a driver name does not take it for PostgreSQL off the default port. It asks for the same
// testing database with a postgres:// base and an explicit driver, then with a cockroachdb:// base, which share a
// cached DSN.
func TestDSN_CockroachDBTestingDSNNamesItsDriver(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	base := os.Getenv("SEQUEL_TESTING_DSN")
	rest, ok := strings.CutPrefix(base, "cockroachdb://")
	if !ok {
		t.Skip("needs SEQUEL_TESTING_DSN in cockroachdb:// form")
	}
	for _, c := range []struct{ driver, base string }{
		{"cockroachdb", "postgres://" + rest},
		{"", "cockroachdb://" + rest},
	} {
		dsn, err := sequel.CreateTestingDatabase(c.driver, c.base, t.Name())
		if !assert.NoError(err) {
			return
		}
		assert.True(strings.HasPrefix(dsn, "cockroachdb://"), "%q gave %q", c.base, dsn)
		db, err := sequel.Open("", dsn)
		if assert.NoError(err) {
			assert.Equal("cockroachdb", db.DriverName())
			db.Close()
		}
	}
}

// TestDSN_MariaDBTestingDSNNamesItsDriver checks that a MariaDB testing DSN says it is MariaDB: the driver's own DSN
// format names no engine, and would be taken for MySQL when opened without a driver name. It asks for one testing
// database with a driver-format base and an explicit driver, then with a mariadb:// base, which must share it rather
// than drop and recreate it under the first one's open handle. A named driver wins over the base's scheme.
func TestDSN_MariaDBTestingDSNNamesItsDriver(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	base := os.Getenv("SEQUEL_TESTING_DSN")
	if !strings.HasPrefix(base, "mariadb://") {
		t.Skip("needs SEQUEL_TESTING_DSN in mariadb:// form")
	}
	driverFormat := strings.TrimPrefix(base, "mariadb://")

	first, err := sequel.CreateTestingDatabase("mariadb", driverFormat, t.Name())
	if !assert.NoError(err) {
		return
	}
	assert.True(strings.HasPrefix(first, "mariadb://"), "%q gave %q", driverFormat, first)
	db, err := sequel.Open("", first)
	if !assert.NoError(err) {
		return
	}
	defer db.Close()
	assert.Equal("mariadb", db.DriverName())
	_, err = db.Exec("CREATE TABLE kept (id INT)")
	assert.NoError(err)

	second, err := sequel.CreateTestingDatabase("", base, t.Name())
	if assert.NoError(err) {
		assert.Equal(first, second)
	}
	_, err = db.Exec("INSERT INTO kept (id) VALUES (1)")
	assert.NoError(err, "the second spelling dropped the database")

	asMySQL, err := sequel.CreateTestingDatabase("mysql", base, t.Name()+"_mysql")
	if assert.NoError(err) {
		assert.False(strings.HasPrefix(asMySQL, "mariadb://"), "%q gave %q", base, asMySQL)
		db, err := sequel.Open("", asMySQL)
		if assert.NoError(err) {
			assert.Equal("mysql", db.DriverName())
			db.Close()
		}
	}
}
