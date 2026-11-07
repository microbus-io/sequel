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
