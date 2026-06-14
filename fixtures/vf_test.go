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
	"strings"
	"testing"

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
