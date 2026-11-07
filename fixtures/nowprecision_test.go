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
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/microbus-io/testarossa"
)

// TestVF_NowUTCNeverAheadOfItsColumn writes NOW_UTC(p) into a column of the same precision and checks, at once,
// that the stored value is not after a fresh NOW_UTC(p). A clock finer than its column is rounded on store, up to
// half a unit ahead of the instant it was taken, and the compare that follows sees a row from the future. Each
// write is followed by its compare within a fraction of a millisecond, so a rounding clock fails within a few
// dozen iterations. Each precision is also written through DATE_ADD_MILLIS(NOW_UTC(p), 0), the shape of a lease or
// a lock, which must compare with NOW_UTC(p) as its base does; on SQLite that requires the two to print alike.
func TestVF_NowUTCNeverAheadOfItsColumn(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)

	cases := []struct {
		name   string
		now    string
		column map[string]string
	}{
		{"default", "NOW_UTC()", map[string]string{
			"mysql": "DATETIME(3)", "pgx": "TIMESTAMP(3)", "cockroachdb": "TIMESTAMP(3)", "mssql": "DATETIME2(3)", "sqlite": "TEXT",
		}},
		{"millis", "NOW_UTC(3)", map[string]string{
			"mysql": "DATETIME(3)", "pgx": "TIMESTAMP(3)", "cockroachdb": "TIMESTAMP(3)", "mssql": "DATETIME2(3)", "sqlite": "TEXT",
		}},
		{"micros", "NOW_UTC(6)", map[string]string{
			"mysql": "DATETIME(6)", "pgx": "TIMESTAMP(6)", "cockroachdb": "TIMESTAMP(6)", "mssql": "DATETIME2(6)", "sqlite": "TEXT",
		}},
		{"tenths", "NOW_UTC(1)", map[string]string{
			"mysql": "DATETIME(1)", "pgx": "TIMESTAMP(1)", "cockroachdb": "TIMESTAMP(1)", "mssql": "DATETIME2(1)", "sqlite": "TEXT",
		}},
		{"seconds", "NOW_UTC(0)", map[string]string{
			"mysql": "DATETIME", "pgx": "TIMESTAMP(0)", "cockroachdb": "TIMESTAMP(0)", "mssql": "DATETIME2(0)", "sqlite": "TEXT",
		}},
	}
	for _, tc := range cases {
		writes := []struct {
			name  string
			value string
			args  []any
		}{
			{"direct", tc.now, nil},
			{"added", "DATE_ADD_MILLIS(" + tc.now + ", ?)", []any{0}},
		}
		for _, w := range writes {
			t.Run(tc.name+"_"+w.name, func(t *testing.T) {
				assert := testarossa.For(t)
				table := "now_" + tc.name + "_" + w.name
				_, err := db.Exec("CREATE TABLE " + table + " (id INT, ts " + tc.column[db.DriverName()] + ")")
				if !assert.NoError(err) {
					return
				}
				ahead := 0
				for i := range 200 {
					_, err := db.Exec("INSERT INTO "+table+" (id, ts) VALUES (?, "+w.value+")", append([]any{i}, w.args...)...)
					if !assert.NoError(err) {
						return
					}
					var due int
					err = db.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE id=? AND ts<="+tc.now, i).Scan(&due)
					if !assert.NoError(err) {
						return
					}
					if due != 1 {
						ahead++
					}
				}
				assert.Equal(0, ahead, "%s stored ahead of a later %s in %d of 200 writes", w.value, tc.now, ahead)
			})
		}
	}
}

// TestVF_NowUTCNeverAheadOfTheClock writes NOW_UTC(p) into a microsecond column, which stores it exactly, and checks
// at once that it is not after a fresh NOW_UTC(6). NOW_UTC(p) is truncated, so it is never after the instant it was
// taken; a rounded one is up to half a unit ahead, and the compare that follows within a fraction of a millisecond
// sees it there - at whole seconds, about half of all writes.
func TestVF_NowUTCNeverAheadOfTheClock(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)

	column := map[string]string{
		"mysql": "DATETIME(6)", "pgx": "TIMESTAMP(6)", "cockroachdb": "TIMESTAMP(6)", "mssql": "DATETIME2(6)", "sqlite": "TEXT",
	}[db.DriverName()]
	for i, now := range []string{"NOW_UTC(0)", "NOW_UTC(1)", "NOW_UTC(2)", "NOW_UTC()", "NOW_UTC(5)"} {
		t.Run(now, func(t *testing.T) {
			assert := testarossa.For(t)
			table := "clock_" + strconv.Itoa(i)
			_, err := db.Exec("CREATE TABLE " + table + " (id INT, ts " + column + ")")
			if !assert.NoError(err) {
				return
			}
			ahead := 0
			for i := range 200 {
				_, err := db.Exec("INSERT INTO "+table+" (id, ts) VALUES (?, "+now+")", i)
				if !assert.NoError(err) {
					return
				}
				var due int
				err = db.QueryRow("SELECT COUNT(*) FROM "+table+" WHERE id=? AND ts<=NOW_UTC(6)", i).Scan(&due)
				if !assert.NoError(err) {
					return
				}
				if due != 1 {
					ahead++
				}
			}
			assert.Equal(0, ahead, "%s was ahead of a later NOW_UTC(6) in %d of 200 writes", now, ahead)
		})
	}
}

// TestVF_NowUTCPrecision checks the sub-second part each precision yields: whole seconds at 0, whole tenths at 1,
// and whole milliseconds by default and at 3. It also checks that the probe can see a microsecond, or the other
// checks would pass whatever NOW_UTC returned. The probe is the exact offset from the epoch: DATE_DIFF_MILLIS where
// it keeps the fraction, and nanoseconds on SQL Server, whose DATE_DIFF_MILLIS is whole milliseconds by design.
// SQLite's clock is milliseconds and its DATE_DIFF_MILLIS is float arithmetic a fraction of a millisecond off at
// today's dates, so its text is checked instead: always three digits, the ones past p zero.
func TestVF_NowUTCPrecision(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	if db.DriverName() == "sqlite" {
		for expr, zeros := range map[string]string{"NOW_UTC(0)": ".000", "NOW_UTC(1)": "00", "NOW_UTC(2)": "0", "NOW_UTC()": "", "NOW_UTC(3)": "", "NOW_UTC(6)": ""} {
			var s string
			err := db.QueryRow("SELECT " + expr).Scan(&s)
			if assert.NoError(err) {
				assert.Len(s, 23, "%s is %q", expr, s)
				assert.True(strings.HasSuffix(s, zeros), "%s is %q", expr, s)
			}
		}
		return
	}
	epoch := tsLiteral(db, time.Unix(0, 0))
	// whole reports whether expr is a whole number of units past the epoch in each of 20 samples
	whole := func(expr string, unitMillis int64) bool {
		for range 20 {
			if db.DriverName() == "mssql" {
				rem := queryFloat(t, db, "SELECT DATEDIFF_BIG(NANOSECOND, "+epoch+", "+expr+") % "+strconv.FormatInt(unitMillis*1000000, 10))
				if rem != 0 {
					return false
				}
				continue
			}
			ms := queryFloat(t, db, "SELECT DATE_DIFF_MILLIS("+expr+", "+epoch+")")
			unit := float64(unitMillis)
			if math.Abs(ms-math.Round(ms/unit)*unit) >= 0.0005 {
				return false
			}
		}
		return true
	}
	assert.False(whole("NOW_UTC(6)", 1), "the probe cannot see past milliseconds")
	assert.True(whole("NOW_UTC(0)", 1000), "NOW_UTC(0) is finer than seconds")
	assert.True(whole("NOW_UTC(1)", 100), "NOW_UTC(1) is finer than tenths")
	assert.True(whole("NOW_UTC()", 1), "NOW_UTC() is finer than milliseconds")
	assert.True(whole("NOW_UTC(3)", 1), "NOW_UTC(3) is finer than milliseconds")
}

// TestVF_NowUTCTruncatesInOneStatement compares NOW_UTC(p) with NOW_UTC(6) in one statement, where every engine
// reads its clock once, so the two are the same instant at two precisions. NOW_UTC(p) must not be after it, and must
// be less than one of its units before it: truncated, neither rounded up nor cut too far. A rounding expansion fails
// about half the samples at every precision, however fast the round trip.
func TestVF_NowUTCTruncatesInOneStatement(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)

	// SQLite's DATE_DIFF_MILLIS is float arithmetic a fraction of a millisecond off at today's dates
	tolerance := 0.0
	if db.DriverName() == "sqlite" {
		tolerance = 0.1
	}
	for p := range 6 {
		now := "NOW_UTC(" + strconv.Itoa(p) + ")"
		t.Run(now, func(t *testing.T) {
			assert := testarossa.For(t)
			unit := math.Pow(10, float64(3-p)) // In milliseconds
			ahead, early := 0, 0
			for range 200 {
				var notAhead int
				var diff string
				err := db.QueryRow("SELECT CASE WHEN "+now+" <= NOW_UTC(6) THEN 1 ELSE 0 END, "+
					"DATE_DIFF_MILLIS(NOW_UTC(6), "+now+")").Scan(&notAhead, &diff)
				if !assert.NoError(err) {
					return
				}
				ms, err := strconv.ParseFloat(strings.TrimSpace(diff), 64)
				if !assert.NoError(err) {
					return
				}
				if notAhead != 1 || ms < -tolerance {
					ahead++
				}
				if ms >= unit+tolerance {
					early++
				}
			}
			assert.Equal(0, ahead, "%s was after NOW_UTC(6) in %d of 200 statements", now, ahead)
			assert.Equal(0, early, "%s was a unit or more before NOW_UTC(6) in %d of 200 statements", now, early)
		})
	}
}

// TestVF_DateAddMillisToNowUTCIsExact adds milliseconds to NOW_UTC(p) at every precision and measures the result
// against the same NOW_UTC(p) in one statement. On SQL Server, DATEADD rounds the interval to its base's type, so
// without widening, 400 ms added to NOW_UTC(0) added nothing and 1500 added 2000.
func TestVF_DateAddMillisToNowUTCIsExact(t *testing.T) {
	t.Parallel()
	db := newTestDB(t)

	// SQLite's DATE_DIFF_MILLIS is float arithmetic a fraction of a millisecond off at today's dates
	tolerance := 0.0005
	if db.DriverName() == "sqlite" {
		tolerance = 0.5
	}
	for _, now := range []string{"NOW_UTC(0)", "NOW_UTC(1)", "NOW_UTC(2)", "NOW_UTC()", "NOW_UTC(6)"} {
		t.Run(now, func(t *testing.T) {
			assert := testarossa.For(t)
			for _, ms := range []int64{400, 1500, -250, 1, 86_400_123} {
				diff := queryFloat(t, db, "SELECT DATE_DIFF_MILLIS(DATE_ADD_MILLIS("+now+", ?), "+now+")", ms)
				assert.True(math.Abs(diff-float64(ms)) < tolerance, "%s plus %d ms is %v ms later", now, ms, diff)
			}
		})
	}
}
