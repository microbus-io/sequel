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
	"encoding/json"
	"math"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/microbus-io/sequel"
	"github.com/microbus-io/sequel/testdata"
	"github.com/microbus-io/testarossa"
)

// The virtual-function unit tests in the root package only assert the SQL that each function expands to.
// These tests close the loop: they execute that generated SQL on the configured engine and check the
// result is correct, proving the per-dialect expansion is not just well-formed text but valid, working SQL
// on MySQL, PostgreSQL, SQL Server and SQLite alike.

// TestVF_NowUTC checks that two NOW_UTC() calls evaluated in one statement read the same instant - the
// generated per-driver "current UTC timestamp" expression runs and the DATE_DIFF_MILLIS around it returns
// approximately zero.
func TestVF_NowUTC(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	diff := queryFloat(t, db, "SELECT DATE_DIFF_MILLIS(NOW_UTC(), NOW_UTC())")
	assert.True(math.Abs(diff) < 1000, "two NOW_UTC() in one statement should be ~equal, got %v ms", diff)
}

// tsLiteral returns a timestamp literal of millisecond precision in the configured engine's natural type, so
// that date arithmetic can be checked exactly from a fixed base rather than approximately around NOW_UTC().
// SQLite has no timestamp type; its date functions read this text form.
func tsLiteral(db *sequel.DB, t time.Time) string {
	s := "'" + t.UTC().Format("2006-01-02 15:04:05.000") + "'"
	switch db.DriverName() {
	case "mysql":
		return "CAST(" + s + " AS DATETIME(3))"
	case "pgx", "cockroachdb":
		return "CAST(" + s + " AS TIMESTAMP(3))"
	case "mssql":
		return "CAST(" + s + " AS DATETIME2(3))"
	default:
		return s
	}
}

// TestVF_NowUTCIsNow checks that NOW_UTC() is the current instant in UTC, against Go's clock. A local-time
// expansion would be off by the server's UTC offset. The tolerance allows for clock skew between the test host
// and a database container.
func TestVF_NowUTCIsNow(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	epoch := tsLiteral(db, time.Unix(0, 0))
	before := time.Now().UnixMilli()
	got := queryFloat(t, db, "SELECT DATE_DIFF_MILLIS(NOW_UTC(), "+epoch+")")
	after := time.Now().UnixMilli()
	assert.True(got > float64(before-60000) && got < float64(after+60000),
		"NOW_UTC() is %v ms after the epoch, Go's clock says %d-%d", got, before, after)
}

// TestVF_DateMillisExact checks DATE_DIFF_MILLIS and DATE_ADD_MILLIS to the millisecond from fixed timestamps.
// DATE_DIFF_MILLIS is checked against known differences in both directions first, so that it can then judge
// DATE_ADD_MILLIS: adding n ms to the base must land exactly on the timestamp Go computes. Each n is passed as
// a literal, as an expression and as a bind followed by another bind, and the values cover sub-second remainders, both signs and
// both sides of 2^31.
func TestVF_DateMillisExact(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	baseLit := tsLiteral(db, base)
	later := base.Add(26*time.Hour + 4*time.Minute + 5*time.Second + 6*time.Millisecond)
	// SQLite's JULIANDAY arithmetic is a float, a fraction of a millisecond off at these magnitudes
	const tolerance = 0.5

	diff := queryFloat(t, db, "SELECT DATE_DIFF_MILLIS("+tsLiteral(db, later)+", "+baseLit+")")
	assert.True(math.Abs(diff-float64(later.Sub(base).Milliseconds())) < tolerance, "a after b: got %v ms", diff)
	diff = queryFloat(t, db, "SELECT DATE_DIFF_MILLIS("+baseLit+", "+tsLiteral(db, later)+")")
	assert.True(math.Abs(diff+float64(later.Sub(base).Milliseconds())) < tolerance, "a before b: got %v ms", diff)

	for _, ms := range []int64{
		0, 1, -1, 999, 1001, 3723004, -3723004,
		31*86400000 + 5, -(400*86400000 + 7),
		math.MaxInt32, math.MaxInt32 + 1, math.MinInt32, math.MinInt32 - 1,
	} {
		want := tsLiteral(db, base.Add(time.Duration(ms)*time.Millisecond))
		s := strconv.FormatInt(ms, 10)
		diff := queryFloat(t, db, "SELECT DATE_DIFF_MILLIS(DATE_ADD_MILLIS("+baseLit+", "+s+"), "+want+")")
		assert.True(math.Abs(diff) < tolerance, "literal %d: off by %v ms", ms, diff)
		diff = queryFloat(t, db, "SELECT DATE_DIFF_MILLIS(DATE_ADD_MILLIS("+baseLit+", ?), "+want+") WHERE 7 = ?", ms, 7)
		assert.True(math.Abs(diff) < tolerance, "bind %d: off by %v ms", ms, diff)
		diff = queryFloat(t, db, "SELECT DATE_DIFF_MILLIS(DATE_ADD_MILLIS("+baseLit+", "+s+" * 1), "+want+")")
		assert.True(math.Abs(diff) < tolerance, "expression %d: off by %v ms", ms, diff)
	}
}

// TestVF_DateAddAndDiffMillis exercises DATE_ADD_MILLIS and DATE_DIFF_MILLIS nested over NOW_UTC(): adding
// 5000ms to "now" and diffing against "now" must come back as ~5000ms on every engine.
func TestVF_DateAddAndDiffMillis(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	diff := queryFloat(t, db, "SELECT DATE_DIFF_MILLIS(DATE_ADD_MILLIS(NOW_UTC(), 5000), NOW_UTC())")
	assert.True(math.Abs(diff-5000) < 1000, "expected ~5000ms, got %v ms", diff)

	diff = queryFloat(t, db, "SELECT DATE_DIFF_MILLIS(DATE_ADD_MILLIS(NOW_UTC(), -5000), NOW_UTC())")
	assert.True(math.Abs(diff-(-5000)) < 1000, "expected ~-5000ms, got %v ms", diff)
}

// TestVF_DateAddMillisPastInt32InStatements binds a 30-day DATE_ADD_MILLIS in the places a scheduler puts one -
// an INSERT's VALUES, an UPDATE's SET and a WHERE - with binds on both sides of it, and inside an aggregate and a
// GROUP BY.
func TestVF_DateAddMillisPastInt32InStatements(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	tsType := map[string]string{
		"mysql":       "DATETIME(3)",
		"pgx":         "TIMESTAMP(3)",
		"cockroachdb": "TIMESTAMP(3)",
		"mssql":       "DATETIME2(3)",
		"sqlite":      "TEXT",
	}[db.DriverName()]
	_, err := db.Exec("CREATE TABLE vf_sched (id INT, not_before " + tsType + ", note VARCHAR(16))")
	assert.NoError(err)

	const month = int64(30 * 24 * 3600 * 1000)
	_, err = db.Exec("INSERT INTO vf_sched (id, not_before, note) VALUES (?, DATE_ADD_MILLIS(NOW_UTC(), ?), ?)", 1, month, "a")
	assert.NoError(err)
	_, err = db.Exec("INSERT INTO vf_sched (id, not_before, note) VALUES (?, NOW_UTC(), ?)", 2, "b")
	assert.NoError(err)
	res, err := db.Exec("UPDATE vf_sched SET not_before=DATE_ADD_MILLIS(NOW_UTC(), ?), note=? WHERE id=?", -month, "c", 2)
	assert.NoError(err)
	n, _ := res.RowsAffected()
	assert.Equal(int64(1), n)

	// Rows not due within 29 days, and rows older than 29 days
	var id int
	var note string
	err = db.QueryRow("SELECT id, note FROM vf_sched WHERE id>? AND not_before>DATE_ADD_MILLIS(NOW_UTC(), ?) AND note<>?", 0, month-24*3600*1000, "x").Scan(&id, &note)
	assert.NoError(err)
	assert.Equal(1, id)
	assert.Equal("a", note)
	err = db.QueryRow("SELECT id, note FROM vf_sched WHERE id>? AND not_before<DATE_ADD_MILLIS(NOW_UTC(), ?) AND note<>?", 0, -month+24*3600*1000, "x").Scan(&id, &note)
	assert.NoError(err)
	assert.Equal(2, id)
	assert.Equal("c", note)

	// 29 days on, the row 30 days back is still in the past
	future := queryFloat(t, db, "SELECT COUNT(*) FROM vf_sched WHERE id>? AND DATE_ADD_MILLIS(not_before, ?) > NOW_UTC() AND id<?", 0, month-24*3600*1000, 9)
	assert.Equal(1.0, future)
	latest := queryFloat(t, db, "SELECT DATE_DIFF_MILLIS(MAX(DATE_ADD_MILLIS(not_before, ?)), NOW_UTC()) FROM vf_sched WHERE id<?", -month, 9)
	assert.True(math.Abs(latest) < 60000, "expected ~0 ms, got %v ms", latest)
	groups := queryFloat(t, db, "SELECT COUNT(*) FROM (SELECT COUNT(*) AS n FROM vf_sched WHERE id<? GROUP BY DATE_ADD_MILLIS(not_before, ?)) AS g", 9, month)
	assert.Equal(2.0, groups)
}

// TestVF_LimitOffset checks the exact window of rows for several limit/offset pairs, each passed as literals,
// as binds, and as binds behind another bind. Limit and offset differ in every pair, so arguments bound in the
// wrong order (SQL Server's OFFSET comes before its FETCH) return different rows.
func TestVF_LimitOffset(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE nums (id INT)")
	assert.NoError(err)
	for i := 1; i <= 5; i++ {
		_, err := db.Exec("INSERT INTO nums (id) VALUES (?)", i)
		assert.NoError(err)
	}
	ids := func(query string, args ...any) []int {
		rows, err := db.Query(query, args...)
		if !assert.NoError(err) {
			return nil
		}
		defer rows.Close()
		got := []int{}
		for rows.Next() {
			var id int
			assert.NoError(rows.Scan(&id))
			got = append(got, id)
		}
		assert.NoError(rows.Err())
		return got
	}

	for _, c := range []struct {
		limit, offset int
		want          []int
		wantAfter1    []int // the same window over ids greater than 1
	}{
		{2, 1, []int{2, 3}, []int{3, 4}},
		{1, 3, []int{4}, []int{5}},
		{3, 0, []int{1, 2, 3}, []int{2, 3, 4}},
		{2, 4, []int{5}, []int{}},
		{4, 2, []int{3, 4, 5}, []int{4, 5}},
	} {
		lit := "LIMIT_OFFSET(" + strconv.Itoa(c.limit) + ", " + strconv.Itoa(c.offset) + ")"
		assert.Equal(c.want, ids("SELECT id FROM nums ORDER BY id "+lit), "literal %d, %d", c.limit, c.offset)
		assert.Equal(c.want, ids("SELECT id FROM nums ORDER BY id LIMIT_OFFSET(?, ?)", c.limit, c.offset), "bind %d, %d", c.limit, c.offset)
		assert.Equal(c.wantAfter1, ids("SELECT id FROM nums WHERE id>? ORDER BY id LIMIT_OFFSET(?, ?)", 1, c.limit, c.offset), "bind after bind %d, %d", c.limit, c.offset)
	}
}

// TestVF_RegexpTextSearch runs REGEXP_TEXT_SEARCH against the foo table. The search term "a" works both as
// a substring (SQLite's LIKE expansion) and as a regular expression (the REGEXP / REGEXP_LIKE expansions on
// the other engines), so the expected match is dialect-independent: exactly the one row whose str is "a".
func TestVF_RegexpTextSearch(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	assert.NoError(db.Migrate(t.Name(), testdata.FS))

	var count int
	err := db.QueryRow("SELECT COUNT(*) FROM foo WHERE REGEXP_TEXT_SEARCH(? IN str)", "a").Scan(&count)
	// On PostgreSQL and SQL Server, REGEXP_TEXT_SEARCH expands to REGEXP_LIKE, which only exists on
	// PostgreSQL 18+ and SQL Server 2025+. Older engines reject the function; skip rather than fail so the
	// suite stays green on them while CI (which runs the modern images) still exercises it for real.
	if err != nil && strings.Contains(strings.ToUpper(err.Error()), "REGEXP_LIKE") {
		t.Skipf("REGEXP_TEXT_SEARCH needs REGEXP_LIKE (PostgreSQL 18+/SQL Server 2025+); engine lacks it: %v", err)
	}
	assert.NoError(err)
	assert.Equal(1, count)
}

// TestVF_RegexpTextSearchColumns searches several columns with binds on both sides of the search term. Terms
// are plain letters, which every dialect matches as a case-insensitive substring: SQLite with LIKE, the others
// with a regular expression.
func TestVF_RegexpTextSearchColumns(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE vf_people (id INT, name VARCHAR(32), email VARCHAR(64))")
	assert.NoError(err)
	_, err = db.Exec("INSERT INTO vf_people (id, name, email) VALUES (1, 'Alice', 'alice@x.io'), (2, 'Bob', 'bob@y.io'), (3, 'Carol', 'carol@x.io')")
	assert.NoError(err)

	for _, c := range []struct {
		after int
		term  string
		upTo  int
		want  []int
	}{
		{0, "x", 9, []int{1, 3}}, // email only
		{0, "BOB", 9, []int{2}},  // name, case-insensitive
		{1, "o", 9, []int{2, 3}}, // every email, cut by the bind before
		{0, "a", 3, []int{1}},    // cut by the bind after
		{0, "zzz", 9, []int{}},   // no match
	} {
		rows, err := db.Query("SELECT id FROM vf_people WHERE id>? AND REGEXP_TEXT_SEARCH(? IN name, email) AND id<? ORDER BY id", c.after, c.term, c.upTo)
		if err != nil && strings.Contains(strings.ToUpper(err.Error()), "REGEXP_LIKE") {
			t.Skipf("REGEXP_TEXT_SEARCH needs REGEXP_LIKE (PostgreSQL 18+/SQL Server 2025+); engine lacks it: %v", err)
		}
		if !assert.NoError(err) {
			return
		}
		got := []int{}
		for rows.Next() {
			var id int
			assert.NoError(rows.Scan(&id))
			got = append(got, id)
		}
		assert.NoError(rows.Err())
		rows.Close()
		assert.Equal(c.want, got, "term %q", c.term)
	}
}

// jsonFieldDoc is the document the JSON_FIELD fixtures extract from. It carries one value of every JSON
// type, because the contract JSON_FIELD promises (scalars unquoted, objects and arrays as JSON text, JSON
// null as SQL NULL) is exactly the thing the four dialects disagree about until the expansion normalizes
// them. The "long" field is a scalar past SQL Server's JSON_VALUE ceiling - see TestVF_JSONFieldLongScalar.
func jsonFieldDoc(t *testing.T) string {
	t.Helper()
	doc, err := json.Marshal(map[string]any{
		"name":    "alice",
		"age":     30,
		"active":  true,
		"tags":    []string{"x", "y"},
		"meta":    map[string]any{"k": "v"},
		"nothing": nil,
		"long":    strings.Repeat("z", 5000),
	})
	testarossa.For(t).NoError(err)
	return string(doc)
}

// jsonField runs a JSON_FIELD extraction against the single jsonbag row and returns the result, which is
// NULL-able by contract.
func jsonField(t *testing.T, db *sequel.DB, path string) sql.NullString {
	t.Helper()
	var got sql.NullString
	err := db.QueryRow("SELECT JSON_FIELD(doc, '" + path + "') FROM jsonbag WHERE id=1").Scan(&got)
	testarossa.For(t).NoError(err)
	return got
}

// noSpace strips whitespace so that object and array results compare across engines: each one re-renders
// JSON text with its own spacing (PostgreSQL's jsonb prints `["x", "y"]`, SQLite prints `["x","y"]`), and
// JSON_FIELD deliberately promises the *value*, not a byte-identical serialization.
func noSpace(s string) string {
	return strings.Join(strings.Fields(s), "")
}

// TestVF_JSONField executes JSON_FIELD on the configured engine and pins the cross-driver contract for
// every JSON type. The JSON-null and missing-path cases are the load-bearing ones: they are where MySQL
// (whose JSON_UNQUOTE yields the *string* 'null') and SQL Server (whose JSON_VALUE cannot see objects)
// would otherwise each diverge in their own direction.
func TestVF_JSONField(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	assert.NoError(db.Migrate(t.Name(), testdata.FS))
	_, err := db.Exec("INSERT INTO jsonbag (id, doc) VALUES (1, ?)", jsonFieldDoc(t))
	assert.NoError(err)

	// A JSON string comes back unquoted.
	assert.Equal("alice", jsonField(t, db, "$.name").String)
	// Numbers and booleans come back as text. Booleans render per-engine (true/1), so only the number is
	// pinned exactly.
	assert.Equal("30", jsonField(t, db, "$.age").String)
	assert.True(jsonField(t, db, "$.active").Valid)
	// Objects and arrays come back as JSON text.
	assert.Equal(`["x","y"]`, noSpace(jsonField(t, db, "$.tags").String))
	assert.Equal(`{"k":"v"}`, noSpace(jsonField(t, db, "$.meta").String))
	// Nested member access and array indexing.
	assert.Equal("v", jsonField(t, db, "$.meta.k").String)
	assert.Equal("x", jsonField(t, db, "$.tags[0]").String)
	assert.Equal("y", jsonField(t, db, "$.tags[1]").String)
	// A JSON null and a path that does not exist are both SQL NULL.
	assert.False(jsonField(t, db, "$.nothing").Valid)
	assert.False(jsonField(t, db, "$.notAField").Valid)
	assert.False(jsonField(t, db, "$.tags[9]").Valid)
	assert.False(jsonField(t, db, "$.meta.missing").Valid)
}

// TestVF_JSONFieldPredicate uses JSON_FIELD in a WHERE clause with a bound argument, which is the other
// half of its job: the expansion has to leave the caller's placeholders intact and correctly numbered on
// the drivers that renumber them ($1 on pgx, @p1 on mssql).
func TestVF_JSONFieldPredicate(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	assert.NoError(db.Migrate(t.Name(), testdata.FS))
	_, err := db.Exec("INSERT INTO jsonbag (id, doc) VALUES (1, ?)", jsonFieldDoc(t))
	assert.NoError(err)

	_, err = db.Exec("INSERT INTO jsonbag (id, doc) VALUES (2, ?)", `{"name":"bob","meta":{"k":"v"}}`)
	assert.NoError(err)

	var id int
	err = db.QueryRow(
		"SELECT id FROM jsonbag WHERE JSON_FIELD(doc, '$.name') = ? AND JSON_FIELD(doc, '$.meta.k') = ?",
		"alice", "v",
	).Scan(&id)
	assert.NoError(err)
	assert.Equal(1, id)

	// Binds on both sides of the extractions
	err = db.QueryRow(
		"SELECT id FROM jsonbag WHERE id>? AND JSON_FIELD(doc, '$.name') = ? AND JSON_FIELD(doc, '$.meta.k') = ? AND id<?",
		0, "bob", "v", 9,
	).Scan(&id)
	assert.NoError(err)
	assert.Equal(2, id)
}

// TestVF_JSONFieldLongScalar pins SQL Server's documented ceiling as *behavior* rather than leaving it as a
// comment: JSON_VALUE returns NVARCHAR(4000) and yields NULL in lax mode for a longer scalar, so a JSON
// string over 4000 characters reads back NULL there and reads back whole everywhere else. If a future SQL
// Server expansion (an OPENJSON rowset) lifts the limit, this test is what tells you to update the godoc.
func TestVF_JSONFieldLongScalar(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	assert.NoError(db.Migrate(t.Name(), testdata.FS))
	_, err := db.Exec("INSERT INTO jsonbag (id, doc) VALUES (1, ?)", jsonFieldDoc(t))
	assert.NoError(err)

	got := jsonField(t, db, "$.long")
	if db.DriverName() == "mssql" {
		assert.False(got.Valid, "SQL Server JSON_VALUE is capped at 4000 chars and should yield NULL")
		return
	}
	assert.Equal(strings.Repeat("z", 5000), got.String)
}
