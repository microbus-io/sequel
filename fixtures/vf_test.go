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
	"strings"
	"testing"

	"github.com/microbus-io/sequel"
	"github.com/microbus-io/sequel/testdata"
	"github.com/microbus-io/testarossa"
)

// The virtual-function unit tests in the root package only assert the SQL that each function expands to.
// These tests close the loop: they execute that generated SQL on the configured engine and check the
// result is correct, proving the per-dialect expansion is not just well-formed text but valid, working SQL
// on MySQL, PostgreSQL, SQL Server and SQLite alike.

// TestVF_NowUTC checks that two NOW_UTC() calls evaluated in one statement read the same instant — the
// generated per-driver "current UTC timestamp" expression runs and the DATE_DIFF_MILLIS around it returns
// approximately zero.
func TestVF_NowUTC(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	diff := queryFloat(t, db, "SELECT DATE_DIFF_MILLIS(NOW_UTC(), NOW_UTC())")
	assert.True(math.Abs(diff) < 1000, "two NOW_UTC() in one statement should be ~equal, got %v ms", diff)
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

// TestVF_LimitOffset verifies the paging expansion returns the right window of rows. SQL Server expands to
// OFFSET/FETCH (which requires the ORDER BY this query provides); the others expand to LIMIT/OFFSET.
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

	// Skip the first row, take the next two: ids 2 and 3.
	rows, err := db.Query("SELECT id FROM nums ORDER BY id LIMIT_OFFSET(2, 1)")
	if !assert.NoError(err) {
		return
	}
	defer rows.Close()
	var got []int
	for rows.Next() {
		var id int
		assert.NoError(rows.Scan(&id))
		got = append(got, id)
	}
	assert.NoError(rows.Err())
	assert.Equal([]int{2, 3}, got)
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

// jsonFieldDoc is the document the JSON_FIELD fixtures extract from. It carries one value of every JSON
// type, because the contract JSON_FIELD promises (scalars unquoted, objects and arrays as JSON text, JSON
// null as SQL NULL) is exactly the thing the four dialects disagree about until the expansion normalizes
// them. The "long" field is a scalar past SQL Server's JSON_VALUE ceiling — see TestVF_JSONFieldLongScalar.
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

	var id int
	err = db.QueryRow(
		"SELECT id FROM jsonbag WHERE JSON_FIELD(doc, '$.name') = ? AND JSON_FIELD(doc, '$.meta.k') = ?",
		"alice", "v",
	).Scan(&id)
	assert.NoError(err)
	assert.Equal(1, id)
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
