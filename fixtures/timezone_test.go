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
	"database/sql"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/microbus-io/sequel"
	"github.com/microbus-io/testarossa"
)

// nonUTCZone is the default the tests give their database. Its offset is never zero, so a value converted
// through it is hours off, far outside any tolerance below.
const nonUTCZone = "America/Los_Angeles"

// openNonUTCDatabase provisions a PostgreSQL testing database whose default session time zone is nonUTCZone,
// and returns its DSN. The setting belongs to the database, so dropping the database removes it. It skips on
// every other driver.
func openNonUTCDatabase(t *testing.T) string {
	t.Helper()
	if os.Getenv("PGTZ") != "" || strings.Contains(strings.ToLower(os.Getenv("PGOPTIONS")), "timezone") {
		t.Skip("PGTZ or PGOPTIONS chooses the session time zone, so sequel rightly does not pin it")
	}
	assert := testarossa.For(t)
	dsn, err := sequel.CreateTestingDatabase("", "", t.Name())
	assert.NoError(err)
	db, err := sequel.Open("", dsn)
	assert.NoError(err)
	if !assert.NotNil(db) {
		t.FailNow()
	}
	// This handle keeps the database alive until the test ends; the last Close drops it.
	t.Cleanup(func() { db.Close() })
	if db.DriverName() != "pgx" {
		t.Skip("the database default time zone is a PostgreSQL setting")
	}
	// A postgres:// DSN to CockroachDB infers "pgx" too, and there a database default does not reach the
	// root user the suite connects as.
	var version string
	assert.NoError(db.QueryRow("SELECT version()").Scan(&version))
	if strings.Contains(version, "CockroachDB") {
		t.Skip("CockroachDB does not apply a database default time zone to the root user")
	}
	var name string
	assert.NoError(db.QueryRow("SELECT current_database()").Scan(&name))
	_, err = db.Exec("ALTER DATABASE " + name + " SET timezone = '" + nonUTCZone + "'")
	assert.NoError(err)

	// A session opened without sequel takes the database default, so the server really is not in UTC for
	// this database and the assertions that follow cannot pass by default.
	raw, err := sql.Open("pgx", dsn)
	assert.NoError(err)
	defer raw.Close()
	var zone string
	assert.NoError(raw.QueryRow("SHOW timezone").Scan(&zone))
	if !assert.Equal(nonUTCZone, zone, "the database default must reach a session that sequel did not open") {
		t.FailNow()
	}
	return dsn
}

// withQueryParam appends a parameter to a URL-form DSN.
func withQueryParam(dsn string, param string) string {
	if strings.Contains(dsn, "?") {
		return dsn + "&" + param
	}
	return dsn + "?" + param
}

// TestTimeZone_NowUTCSurvivesANonUTCDatabase checks that NOW_UTC() stores the current instant in both a
// TIMESTAMP and a TIMESTAMPTZ column when the database's default time zone is not UTC. A TIMESTAMPTZ
// column converts the zone-less NOW_UTC() through the session time zone, so it is correct only because
// sequel opens every PostgreSQL session in UTC.
func TestTimeZone_NowUTCSurvivesANonUTCDatabase(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	dsn := openNonUTCDatabase(t)

	// A fresh handle, so every connection is opened after the database default changed.
	db, err := sequel.Open("", dsn)
	assert.NoError(err)
	defer db.Close()

	var zone string
	assert.NoError(db.QueryRow("SHOW timezone").Scan(&zone))
	assert.Equal("UTC", zone)

	_, err = db.Exec("CREATE TABLE tz_probe (id INT PRIMARY KEY, ts TIMESTAMP, tstz TIMESTAMPTZ)")
	assert.NoError(err)
	_, err = db.Exec("INSERT INTO tz_probe (id, ts, tstz) VALUES (1, NOW_UTC(), NOW_UTC())")
	assert.NoError(err)
	var ts, tstz time.Time
	assert.NoError(db.QueryRow("SELECT ts, tstz FROM tz_probe WHERE id=1").Scan(&ts, &tstz))
	assert.True(time.Since(ts).Abs() < 30*time.Second, "TIMESTAMP read back %v, now is %v", ts, time.Now().UTC())
	assert.True(time.Since(tstz).Abs() < 30*time.Second, "TIMESTAMPTZ read back %v, now is %v", tstz, time.Now().UTC())

	// The server agrees, measured against NOW(), which is an instant and so involves no session time zone.
	// (Measuring against NOW_UTC() would not do: the comparison converts it through the same session zone
	// as the stored value, and the two errors cancel.)
	var lag float64
	assert.NoError(db.QueryRow("SELECT EXTRACT(EPOCH FROM NOW() - tstz) FROM tz_probe WHERE id=1").Scan(&lag))
	assert.True(lag > -30 && lag < 30, "TIMESTAMPTZ is %vs from NOW()", lag)
}

// TestTimeZone_ExplicitZoneIsKept checks that a time zone the caller names in the DSN, directly or through
// the options parameter, wins over sequel's UTC default.
func TestTimeZone_ExplicitZoneIsKept(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	dsn := openNonUTCDatabase(t)

	for _, param := range []string{"timezone=Asia/Tokyo", "TimeZone=Asia/Tokyo", "options=-c%20timezone%3DAsia/Tokyo"} {
		db, err := sequel.Open("", withQueryParam(dsn, param))
		if !assert.NoError(err, "for %s", param) {
			continue
		}
		var zone string
		assert.NoError(db.QueryRow("SHOW timezone").Scan(&zone), "for %s", param)
		assert.Equal("Asia/Tokyo", zone, "for %s", param)
		db.Close()
	}
}
