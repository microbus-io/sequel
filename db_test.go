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

package sequel

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/microbus-io/testarossa"
)

func TestDB_ConformArgPlaceholders(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := &DB{
		driverName: "pgx",
	}
	stmt := `SELECT completed FROM sequel_migrations WHERE seq_name=? AND seq_num=?`
	pgxStmt := db.ConformArgPlaceholders(stmt)
	assert.Expect(pgxStmt, `SELECT completed FROM sequel_migrations WHERE seq_name=$1 AND seq_num=$2`)

	stmt = `INSERT INTO sequel_migrations (seq_name, seq_num) VALUES (?, ?)`
	pgxStmt = db.ConformArgPlaceholders(stmt)
	assert.Expect(pgxStmt, `INSERT INTO sequel_migrations (seq_name, seq_num) VALUES ($1, $2)`)

	// CockroachDB uses the Postgres wire protocol and therefore the same $N syntax.
	crdb := &DB{driverName: "cockroachdb"}
	stmt = `SELECT * FROM users WHERE id=? AND tenant_id=?`
	crdbStmt := crdb.ConformArgPlaceholders(stmt)
	assert.Expect(crdbStmt, `SELECT * FROM users WHERE id=$1 AND tenant_id=$2`)
}

func TestDB_ConformArgPlaceholders_Quotes(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := &DB{
		driverName: "pgx",
	}

	// Single-quoted string containing ? should not be replaced
	stmt := `SELECT * FROM users WHERE name='What?' AND id=?`
	pgxStmt := db.ConformArgPlaceholders(stmt)
	assert.Expect(pgxStmt, `SELECT * FROM users WHERE name='What?' AND id=$1`)

	// Double-quoted identifier containing ? should not be replaced
	stmt = `SELECT * FROM "is_this?" WHERE id=?`
	pgxStmt = db.ConformArgPlaceholders(stmt)
	assert.Expect(pgxStmt, `SELECT * FROM "is_this?" WHERE id=$1`)

	// Multiple quoted regions with ? outside
	stmt = `INSERT INTO t (a, b, c) VALUES ('hello?', ?, "col?")`
	pgxStmt = db.ConformArgPlaceholders(stmt)
	assert.Expect(pgxStmt, `INSERT INTO t (a, b, c) VALUES ('hello?', $1, "col?")`)

	// No quotes falls back to fast path
	stmt = `SELECT * FROM t WHERE a=? AND b=?`
	pgxStmt = db.ConformArgPlaceholders(stmt)
	assert.Expect(pgxStmt, `SELECT * FROM t WHERE a=$1 AND b=$2`)

	// Only quoted ?, no unquoted ?
	stmt = `SELECT * FROM t WHERE name='really?'`
	pgxStmt = db.ConformArgPlaceholders(stmt)
	assert.Expect(pgxStmt, `SELECT * FROM t WHERE name='really?'`)
}

func TestDB_DatabaseNameFromDataSourceName(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	// mysql
	name, err := databaseNameFromDataSourceName("mysql", "x:x@tcp(127.0.0.1:3306)/my_database")
	assert.Expect(name, "my_database", err, nil)
	name, err = databaseNameFromDataSourceName("mysql", "x:x@tcp(127.0.0.1:3306)/")
	assert.Expect(name, "", err, nil)
	name, err = databaseNameFromDataSourceName("mysql", "x:x@tcp(127.0.0.1:3306)")
	assert.Error(err) // Trailing slash is required

	// pgx
	name, err = databaseNameFromDataSourceName("pgx", "postgres://user:pw@127.0.0.1:5432/my_database")
	assert.Expect(name, "my_database", err, nil)
	name, err = databaseNameFromDataSourceName("pgx", "postgres://user:pw@127.0.0.1:5432/")
	assert.Expect(name, "", err, nil)
	name, err = databaseNameFromDataSourceName("pgx", "postgres://user:pw@127.0.0.1:5432")
	assert.Expect(name, "", err, nil)

	// cockroachdb (uses the same Postgres DSN parser as pgx)
	name, err = databaseNameFromDataSourceName("cockroachdb", "postgres://root@127.0.0.1:26257/my_database")
	assert.Expect(name, "my_database", err, nil)
	name, err = databaseNameFromDataSourceName("cockroachdb", "postgres://root@127.0.0.1:26257/")
	assert.Expect(name, "", err, nil)

	// mssql
	name, err = databaseNameFromDataSourceName("mssql", "sqlserver://user:pw@127.0.0.1:1433?database=my_database")
	assert.Expect(name, "my_database", err, nil)
	name, err = databaseNameFromDataSourceName("mssql", "sqlserver://user:pw@127.0.0.1:1433")
	assert.Expect(name, "", err, nil)

	// empty dsn
	_, err = databaseNameFromDataSourceName("mysql", "")
	assert.Error(err)

	// sqlite
	name, err = databaseNameFromDataSourceName("sqlite", "file:path/to/mydb.sqlite?_pragma=journal_mode(WAL)")
	assert.Expect(name, "path/to/mydb.sqlite", err, nil)
	name, err = databaseNameFromDataSourceName("sqlite", "/tmp/test.db")
	assert.Expect(name, "/tmp/test.db", err, nil)
	name, err = databaseNameFromDataSourceName("sqlite", ":memory:")
	assert.Expect(name, ":memory:", err, nil)

	// unsupported driver
	_, err = databaseNameFromDataSourceName("oracle", "oracle://user:pw@host/db")
	assert.Error(err)
}

func TestDB_InferDriverName(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	// Postgres prefix
	assert.Equal("pgx", inferDriverName("postgres://user:pw@127.0.0.1:5432/mydb"))

	// SQL Server prefix
	assert.Equal("mssql", inferDriverName("sqlserver://user:pw@127.0.0.1:1433"))

	// MySQL tcp() style
	assert.Equal("mysql", inferDriverName("root:root@tcp(127.0.0.1:3306)/"))

	// SQLite
	assert.Equal("sqlite", inferDriverName("file:test.db"))
	assert.Equal("sqlite", inferDriverName("file::memory:?cache=shared"))
	assert.Equal("sqlite", inferDriverName(":memory:"))
	assert.Equal("sqlite", inferDriverName("/tmp/data.db"))
	assert.Equal("sqlite", inferDriverName("my_database.sqlite"))
	assert.Equal("sqlite", inferDriverName("my_database.sqlite3"))

	// Port-based inference
	assert.Equal("mysql", inferDriverName("root:root@127.0.0.1:3306/"))
	assert.Equal("pgx", inferDriverName("user:pw@127.0.0.1:5432/"))
	assert.Equal("cockroachdb", inferDriverName("user:pw@127.0.0.1:26257/"))
	assert.Equal("mssql", inferDriverName("user:pw@127.0.0.1:1433"))

	// Empty string
	assert.Equal("", inferDriverName(""))

	// Unrecognizable DSN
	assert.Equal("", inferDriverName("some-unknown-dsn"))
}

func TestDB_SetDatabaseInDataSourceName(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	// mysql - set database
	dsn, err := setDatabaseInDataSourceName("mysql", "root:root@tcp(127.0.0.1:3306)/", "mydb")
	assert.NoError(err)
	name, _ := databaseNameFromDataSourceName("mysql", dsn)
	assert.Equal("mydb", name)

	// mysql - clear database
	dsn, err = setDatabaseInDataSourceName("mysql", "root:root@tcp(127.0.0.1:3306)/mydb", "")
	assert.NoError(err)
	name, _ = databaseNameFromDataSourceName("mysql", dsn)
	assert.Equal("", name)

	// pgx - set database
	dsn, err = setDatabaseInDataSourceName("pgx", "postgres://user:pw@127.0.0.1:5432/", "mydb")
	assert.NoError(err)
	name, _ = databaseNameFromDataSourceName("pgx", dsn)
	assert.Equal("mydb", name)

	// pgx - clear database
	dsn, err = setDatabaseInDataSourceName("pgx", "postgres://user:pw@127.0.0.1:5432/mydb", "")
	assert.NoError(err)
	name, _ = databaseNameFromDataSourceName("pgx", dsn)
	assert.Equal("", name)

	// cockroachdb - set database (shares the pgx DSN parser)
	dsn, err = setDatabaseInDataSourceName("cockroachdb", "postgres://root@127.0.0.1:26257/", "mydb")
	assert.NoError(err)
	name, _ = databaseNameFromDataSourceName("cockroachdb", dsn)
	assert.Equal("mydb", name)

	// mssql - set database
	dsn, err = setDatabaseInDataSourceName("mssql", "sqlserver://user:pw@127.0.0.1:1433", "mydb")
	assert.NoError(err)
	name, _ = databaseNameFromDataSourceName("mssql", dsn)
	assert.Equal("mydb", name)

	// mssql - clear database
	dsn, err = setDatabaseInDataSourceName("mssql", "sqlserver://user:pw@127.0.0.1:1433?database=mydb", "")
	assert.NoError(err)
	name, _ = databaseNameFromDataSourceName("mssql", dsn)
	assert.Equal("", name)

	// empty dsn
	_, err = setDatabaseInDataSourceName("mysql", "", "mydb")
	assert.Error(err)

	// sqlite - set database with file: prefix
	dsn, err = setDatabaseInDataSourceName("sqlite", "file:old.db?_pragma=journal_mode(WAL)", "new.db")
	assert.NoError(err)
	assert.Equal("file:new.db?_pragma=journal_mode(WAL)", dsn)

	// sqlite - set database without prefix
	dsn, err = setDatabaseInDataSourceName("sqlite", "old.db", "new.db")
	assert.NoError(err)
	assert.Equal("new.db", dsn)

	// unsupported driver
	_, err = setDatabaseInDataSourceName("oracle", "oracle://host/db", "mydb")
	assert.Error(err)
}

func TestDB_ConformArgPlaceholders_NoArgs(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := &DB{driverName: "pgx"}
	stmt := `SELECT * FROM foo WHERE id=1`
	assert.Equal(stmt, db.ConformArgPlaceholders(stmt))
}

func TestDB_ConformArgPlaceholders_NonPgx(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	// MySQL driver should return the statement unchanged
	db := &DB{driverName: "mysql"}
	stmt := `SELECT * FROM foo WHERE id=? AND name=?`
	assert.Equal(stmt, db.ConformArgPlaceholders(stmt))

	// MSSQL driver should also return unchanged
	db = &DB{driverName: "mssql"}
	assert.Equal(stmt, db.ConformArgPlaceholders(stmt))
}

func TestDB_NowUTC(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := &DB{driverName: "mysql"}
	assert.Equal("UTC_TIMESTAMP(3)", db.NowUTC())

	db = &DB{driverName: "pgx"}
	assert.Equal("(NOW() AT TIME ZONE 'UTC')", db.NowUTC())

	db = &DB{driverName: "mssql"}
	assert.Equal("(CONVERT(DATETIME2(3), SYSUTCDATETIME()))", db.NowUTC())

	db = &DB{driverName: "sqlite"}
	assert.Equal("STRFTIME('%Y-%m-%d %H:%M:%f', 'now')", db.NowUTC())

	db = &DB{driverName: "unknown"}
	assert.Equal("", db.NowUTC())
}

func TestDB_RegexpTextSearch(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	// MySQL
	db := &DB{driverName: "mysql"}
	assert.Equal("''"+` REGEXP ?`, db.RegexpTextSearch())
	assert.Equal("name REGEXP ?", db.RegexpTextSearch("name"))
	assert.Equal("CONCAT_WS(' ',name,email) REGEXP ?", db.RegexpTextSearch("name", "email"))

	// Postgres
	db = &DB{driverName: "pgx"}
	assert.Equal("REGEXP_LIKE('', ?, 'i')", db.RegexpTextSearch())
	assert.Equal("REGEXP_LIKE(name, ?, 'i')", db.RegexpTextSearch("name"))
	assert.Equal("REGEXP_LIKE(CONCAT_WS(' ',name,email), ?, 'i')", db.RegexpTextSearch("name", "email"))

	// MSSQL
	db = &DB{driverName: "mssql"}
	assert.Equal("REGEXP_LIKE('', ?, 'i')", db.RegexpTextSearch())
	assert.Equal("REGEXP_LIKE(name, ?, 'i')", db.RegexpTextSearch("name"))

	// SQLite
	db = &DB{driverName: "sqlite"}
	assert.Equal("'' LIKE ('%' || ? || '%')", db.RegexpTextSearch())
	assert.Equal("name LIKE ('%' || ? || '%')", db.RegexpTextSearch("name"))
	assert.Equal("CONCAT_WS(' ',name,email) LIKE ('%' || ? || '%')", db.RegexpTextSearch("name", "email"))

	// Unknown driver
	db = &DB{driverName: "unknown"}
	assert.Equal("", db.RegexpTextSearch("name"))
}

func newTestDB(driverName string) *DB {
	return &DB{
		driverName: driverName,
	}
}

func TestDB_UnpackQuery_NowUTC(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB("mysql")
	q, err := db.UnpackQuery("UPDATE t SET updated_at=NOW_UTC() WHERE id=?")
	assert.NoError(err)
	assert.Equal("UPDATE t SET updated_at=(UTC_TIMESTAMP(3)) WHERE id=?", q)

	db = newTestDB("pgx")
	q, err = db.UnpackQuery("UPDATE t SET updated_at=NOW_UTC() WHERE id=?")
	assert.NoError(err)
	assert.Equal("UPDATE t SET updated_at=(NOW() AT TIME ZONE 'UTC') WHERE id=$1", q)

	db = newTestDB("mssql")
	q, err = db.UnpackQuery("UPDATE t SET updated_at=NOW_UTC() WHERE id=?")
	assert.NoError(err)
	assert.Equal("UPDATE t SET updated_at=(CONVERT(DATETIME2(3), SYSUTCDATETIME())) WHERE id=?", q)

	db = newTestDB("sqlite")
	q, err = db.UnpackQuery("UPDATE t SET updated_at=NOW_UTC() WHERE id=?")
	assert.NoError(err)
	assert.Equal("UPDATE t SET updated_at=(STRFTIME('%Y-%m-%d %H:%M:%f', 'now')) WHERE id=?", q)

	// Case insensitive
	db = newTestDB("mysql")
	q, err = db.UnpackQuery("SELECT now_utc()")
	assert.NoError(err)
	assert.Equal("SELECT (UTC_TIMESTAMP(3))", q)

}

func TestDB_UnpackQuery_RegexpTextSearch(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB("mysql")
	q, err := db.UnpackQuery("SELECT * FROM t WHERE REGEXP_TEXT_SEARCH(? IN name, email)")
	assert.NoError(err)
	assert.Equal("SELECT * FROM t WHERE (CONCAT_WS(' ',name,email) REGEXP ?)", q)

	db = newTestDB("pgx")
	q, err = db.UnpackQuery("SELECT * FROM t WHERE REGEXP_TEXT_SEARCH(? IN name)")
	assert.NoError(err)
	assert.Equal("SELECT * FROM t WHERE REGEXP_LIKE(name, $1, 'i')", q)

	db = newTestDB("sqlite")
	q, err = db.UnpackQuery("SELECT * FROM t WHERE REGEXP_TEXT_SEARCH(? IN name, email)")
	assert.NoError(err)
	assert.Equal("SELECT * FROM t WHERE (CONCAT_WS(' ',name,email) LIKE ('%' || ? || '%'))", q)

	// Missing IN
	db = newTestDB("mysql")
	_, err = db.UnpackQuery("SELECT * FROM t WHERE REGEXP_TEXT_SEARCH(name, email)")
	assert.Error(err)
}

func TestDB_UnpackQuery_DateAddMillis(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB("mysql")
	q, err := db.UnpackQuery("SELECT DATE_ADD_MILLIS(created_at, 5000)")
	assert.NoError(err)
	assert.Equal("SELECT DATE_ADD(created_at, INTERVAL (5000) * 1000 MICROSECOND)", q)

	db = newTestDB("pgx")
	q, err = db.UnpackQuery("SELECT DATE_ADD_MILLIS(created_at, ?)")
	assert.NoError(err)
	assert.Equal("SELECT (created_at + MAKE_INTERVAL(secs => ($1) / 1000.0))", q)

	db = newTestDB("mssql")
	q, err = db.UnpackQuery("SELECT DATE_ADD_MILLIS(created_at, 5000)")
	assert.NoError(err)
	assert.Equal("SELECT DATEADD(MILLISECOND, 5000, created_at)", q)

	db = newTestDB("sqlite")
	q, err = db.UnpackQuery("SELECT DATE_ADD_MILLIS(created_at, 5000)")
	assert.NoError(err)
	assert.Equal("SELECT (STRFTIME('%Y-%m-%d %H:%M:%f', created_at, ((5000) / 1000.0) || ' seconds'))", q)

	// Missing comma
	db = newTestDB("mysql")
	_, err = db.UnpackQuery("SELECT DATE_ADD_MILLIS(created_at)")
	assert.Error(err)
}

func TestDB_UnpackQuery_DateDiffMillis(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB("mysql")
	q, err := db.UnpackQuery("SELECT DATE_DIFF_MILLIS(updated_at, created_at)")
	assert.NoError(err)
	assert.Equal("SELECT (TIMESTAMPDIFF(MICROSECOND, created_at, updated_at) / 1000.0)", q)

	db = newTestDB("pgx")
	q, err = db.UnpackQuery("SELECT DATE_DIFF_MILLIS(updated_at, created_at)")
	assert.NoError(err)
	assert.Equal("SELECT (EXTRACT(EPOCH FROM (updated_at - created_at)) * 1000.0)", q)

	db = newTestDB("mssql")
	q, err = db.UnpackQuery("SELECT DATE_DIFF_MILLIS(updated_at, created_at)")
	assert.NoError(err)
	assert.Equal("SELECT DATEDIFF_BIG(MILLISECOND, created_at, updated_at)", q)

	db = newTestDB("sqlite")
	q, err = db.UnpackQuery("SELECT DATE_DIFF_MILLIS(updated_at, created_at)")
	assert.NoError(err)
	assert.Equal("SELECT ((JULIANDAY(updated_at) - JULIANDAY(created_at)) * 86400000.0)", q)

	// Missing comma
	db = newTestDB("mysql")
	_, err = db.UnpackQuery("SELECT DATE_DIFF_MILLIS(created_at)")
	assert.Error(err)
}

func TestDB_UnpackQuery_LimitOffset(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB("mysql")
	q, err := db.UnpackQuery("SELECT * FROM users ORDER BY id LIMIT_OFFSET(10, 0)")
	assert.NoError(err)
	assert.Equal("SELECT * FROM users ORDER BY id LIMIT 10 OFFSET 0", q)

	db = newTestDB("pgx")
	q, err = db.UnpackQuery("SELECT * FROM users ORDER BY id LIMIT_OFFSET(?, ?)")
	assert.NoError(err)
	assert.Equal("SELECT * FROM users ORDER BY id LIMIT $1 OFFSET $2", q)

	db = newTestDB("mssql")
	q, err = db.UnpackQuery("SELECT * FROM users ORDER BY id LIMIT_OFFSET(10, 20)")
	assert.NoError(err)
	assert.Equal("SELECT * FROM users ORDER BY id OFFSET 20 ROWS FETCH NEXT 10 ROWS ONLY", q)

	db = newTestDB("mssql")
	q, err = db.UnpackQuery("SELECT * FROM users ORDER BY id LIMIT_OFFSET(?, ?)")
	assert.NoError(err)
	assert.Equal("SELECT * FROM users ORDER BY id OFFSET ? ROWS FETCH NEXT ? ROWS ONLY", q)

	db = newTestDB("sqlite")
	q, err = db.UnpackQuery("SELECT * FROM users ORDER BY id LIMIT_OFFSET(10, 0)")
	assert.NoError(err)
	assert.Equal("SELECT * FROM users ORDER BY id LIMIT 10 OFFSET 0", q)

	// Missing comma
	db = newTestDB("mysql")
	_, err = db.UnpackQuery("SELECT * FROM users LIMIT_OFFSET(10)")
	assert.Error(err)
}

func TestDB_UnpackQuery_Composed(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	// NOW_UTC inside DATE_ADD_MILLIS
	db := newTestDB("mysql")
	q, err := db.UnpackQuery("SELECT DATE_ADD_MILLIS(NOW_UTC(), ?)")
	assert.NoError(err)
	assert.Equal("SELECT DATE_ADD((UTC_TIMESTAMP(3)), INTERVAL (?) * 1000 MICROSECOND)", q)

	// NOW_UTC inside DATE_DIFF_MILLIS
	db = newTestDB("pgx")
	q, err = db.UnpackQuery("SELECT DATE_DIFF_MILLIS(NOW_UTC(), created_at)")
	assert.NoError(err)
	assert.Equal("SELECT (EXTRACT(EPOCH FROM ((NOW() AT TIME ZONE 'UTC') - created_at)) * 1000.0)", q)

	// NOW_UTC inside DATE_DIFF_MILLIS on SQLite — the comma inside STRFTIME must not split the arguments
	db = newTestDB("sqlite")
	q, err = db.UnpackQuery("SELECT DATE_DIFF_MILLIS(not_before, NOW_UTC())")
	assert.NoError(err)
	assert.Equal("SELECT ((JULIANDAY(not_before) - JULIANDAY((STRFTIME('%Y-%m-%d %H:%M:%f', 'now')))) * 86400000.0)", q)

	// No virtual functions, just placeholders
	db = newTestDB("pgx")
	q, err = db.UnpackQuery("SELECT * FROM t WHERE a=? AND b=?")
	assert.NoError(err)
	assert.Equal("SELECT * FROM t WHERE a=$1 AND b=$2", q)

	// Quoted parens should not affect balancing
	db = newTestDB("mysql")
	q, err = db.UnpackQuery("SELECT DATE_ADD_MILLIS(created_at, ?) WHERE name='hello (world)'")
	assert.NoError(err)
	assert.Equal("SELECT DATE_ADD(created_at, INTERVAL (?) * 1000 MICROSECOND) WHERE name='hello (world)'", q)

	// Quoted ) without matching ( should not close the virtual function early
	db = newTestDB("mysql")
	q, err = db.UnpackQuery("SELECT DATE_ADD_MILLIS(created_at, ?) WHERE name='smile :)'")
	assert.NoError(err)
	assert.Equal("SELECT DATE_ADD(created_at, INTERVAL (?) * 1000 MICROSECOND) WHERE name='smile :)'", q)

	// No transformations needed
	db = newTestDB("mysql")
	q, err = db.UnpackQuery("SELECT 1")
	assert.NoError(err)
	assert.Equal("SELECT 1", q)
}

func TestDB_Nullify(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	// Zero values should return nil
	assert.Nil(Nullify(""))
	assert.Nil(Nullify(0))
	assert.Nil(Nullify(false))
	assert.Nil(Nullify(time.Time{}))

	// Non-zero values should return the value itself
	assert.Equal("hello", Nullify("hello"))
	assert.Equal(42, Nullify(42))
	assert.Equal(true, Nullify(true))
	now := time.Now()
	assert.Equal(now, Nullify(now))
}

func TestDB_Nullable(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	var s string
	n := Nullable(&s)

	// Simulate scanning a value
	n.V = "hello"
	n.Valid = true
	err := ApplyBindings(n)
	assert.NoError(err)
	assert.Equal("hello", s)

	// Simulate scanning a NULL (Valid=false, V is zero)
	s = "previous"
	n2 := Nullable(&s)
	n2.Valid = false
	err = ApplyBindings(n2)
	assert.NoError(err)
	assert.Equal("", s)
}

func TestDB_Bind(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	var tags []string
	b := Bind(func(value string) error {
		return json.Unmarshal([]byte(value), &tags)
	})

	// Simulate scanning a JSON string
	b.V = `["a","b","c"]`
	b.Valid = true
	err := ApplyBindings(b)
	assert.NoError(err)
	assert.Len(tags, 3)
	assert.Equal("a", tags[0])
	assert.Equal("b", tags[1])
	assert.Equal("c", tags[2])
}

func TestDB_Bind_Error(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	b := Bind(func(value string) error {
		return json.Unmarshal([]byte(value), &[]int{})
	})

	// Simulate scanning invalid JSON
	b.V = `not-json`
	b.Valid = true
	err := ApplyBindings(b)
	assert.Error(err)
}

func TestDB_ApplyBindings_NoBindings(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	// ApplyBindings should be safe with non-binder args
	var x int
	var s string
	err := ApplyBindings(&x, &s)
	assert.NoError(err)

	// Empty args
	err = ApplyBindings()
	assert.NoError(err)
}

func TestDB_DriverName(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := &DB{driverName: "mysql"}
	assert.Equal("mysql", db.DriverName())

	db = &DB{driverName: "pgx"}
	assert.Equal("pgx", db.DriverName())
}

func TestDB_CloseNil(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	// Close on nil DB should not panic
	var db *DB
	err := db.Close()
	assert.NoError(err)
}

func TestDB_OpenEmptyDSN(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	_, err := Open("mysql", "")
	assert.Error(err)
}

func TestDB_OpenInferDriverFails(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	// Unrecognizable DSN without explicit driver
	_, err := Open("", "some-unknown-connection-string")
	assert.Error(err)
}

func TestDB_OpenIndependentPools(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	dsn := "file:openindependentpools?mode=memory&cache=shared"

	db1, err := Open("sqlite", dsn)
	assert.NoError(err)
	if !assert.NotNil(db1) {
		return
	}
	defer db1.Close()

	db2, err := Open("sqlite", dsn)
	assert.NoError(err)
	if !assert.NotNil(db2) {
		return
	}
	defer db2.Close()

	// Open does not coalesce: each call returns a distinct *DB.
	assert.False(db1 == db2)

	// Closing one pool must not affect the other.
	err = db1.Close()
	assert.NoError(err)
	var x int
	err = db2.QueryRow("SELECT 1").Scan(&x)
	assert.NoError(err)
	assert.Equal(1, x)
}

func TestDB_OpenNoAutoPoolSizing(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	dsn := "file:opennoautopool?mode=memory&cache=shared"

	db, err := Open("sqlite", dsn)
	assert.NoError(err)
	if !assert.NotNil(db) {
		return
	}
	defer db.Close()

	// Caller-managed pool size must stick. Open of the same DSN does not coalesce,
	// so nothing else can touch this *DB's pool.
	db.SetMaxOpenConns(7)

	other, err := Open("sqlite", dsn)
	assert.NoError(err)
	defer other.Close()

	assert.Equal(7, db.Stats().MaxOpenConnections)
}

func TestDB_OpenSingletonCoalesces(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	dsn := "file:opensingletoncoalesces?mode=memory&cache=shared"

	db1, err := OpenSingleton("sqlite", dsn)
	assert.NoError(err)
	if !assert.NotNil(db1) {
		return
	}
	defer db1.Close()

	db2, err := OpenSingleton("sqlite", dsn)
	assert.NoError(err)
	if !assert.NotNil(db2) {
		return
	}
	defer db2.Close()

	// OpenSingleton coalesces by DSN: both callers see the same *DB.
	assert.True(db1 == db2)
}

func TestDB_CreateTestingDatabaseCached(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	// Repeated calls with the same (driver, baseDSN, uniqueTestID) return the
	// same resolved DSN — the DROP+CREATE happens once.
	dsn1, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	dsn2, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	assert.Equal(dsn1, dsn2)
}

func TestDB_CreateTestingDatabase_SharedSingleton(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	// Two consumers in the same test go through CreateTestingDatabase then
	// OpenSingleton — they end up sharing one *DB and one pool.
	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	db1, err := OpenSingleton("sqlite", dsn)
	assert.NoError(err)
	defer db1.Close()
	db2, err := OpenSingleton("sqlite", dsn)
	assert.NoError(err)
	defer db2.Close()

	assert.True(db1 == db2)

	// Sanity: writes from one are visible to the other.
	_, err = db1.Exec("CREATE TABLE IF NOT EXISTS shared_t (x INT)")
	assert.NoError(err)
	_, err = db1.Exec("INSERT INTO shared_t (x) VALUES (?)", 42)
	assert.NoError(err)
	var x int
	err = db2.QueryRow("SELECT x FROM shared_t").Scan(&x)
	assert.NoError(err)
	assert.Equal(42, x)
}

func TestDB_CreateTestingDatabase_DistinctPools(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	// Two consumers using plain Open get distinct *DB instances and
	// independent pools, while still talking to the same underlying database.
	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	db1, err := Open("sqlite", dsn)
	assert.NoError(err)
	defer db1.Close()
	db2, err := Open("sqlite", dsn)
	assert.NoError(err)
	defer db2.Close()

	assert.False(db1 == db2)

	_, err = db1.Exec("CREATE TABLE IF NOT EXISTS shared_t (x INT)")
	assert.NoError(err)
	_, err = db1.Exec("INSERT INTO shared_t (x) VALUES (?)", 7)
	assert.NoError(err)
	var x int
	err = db2.QueryRow("SELECT x FROM shared_t").Scan(&x)
	assert.NoError(err)
	assert.Equal(7, x)
}

// testingRefsOf reads the live-handle count sequel keeps for a testing database.
func testingRefsOf(databaseName string) int {
	testingGlobalMutex.Lock()
	defer testingGlobalMutex.Unlock()
	return testingDBRefs[databaseName]
}

func testingDSNIsCached(databaseName string) bool {
	testingGlobalMutex.Lock()
	defer testingGlobalMutex.Unlock()
	cacheKey, ok := testingDBKeys[databaseName]
	if !ok {
		return false
	}
	_, cached := testingDSNs[cacheKey]
	return cached
}

// Several handles on one testing database drop it once, on the last close.
func TestDB_TestingDatabaseDropsOnLastCloseOnly(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	name, ok := testingDatabaseNameOf("sqlite", dsn)
	assert.True(ok, "the minted DSN must name a testing database")

	db1, err := Open("sqlite", dsn)
	assert.NoError(err)
	db2, err := Open("sqlite", dsn)
	assert.NoError(err)
	assert.False(db1 == db2, "Open hands out a distinct *DB per call")
	assert.Equal(2, testingRefsOf(name))

	// First close must not drop: db2 is still using the database, so the DSN stays cached.
	assert.NoError(db1.Close())
	assert.Equal(1, testingRefsOf(name))
	assert.True(testingDSNIsCached(name), "the DSN is still usable while a handle remains")

	// Last close drops, and takes the cached DSN with it.
	assert.NoError(db2.Close())
	assert.Equal(0, testingRefsOf(name))
	assert.False(testingDSNIsCached(name), "a dropped database must not stay cached under its DSN")
}

// OpenSingleton contributes one handle however many callers coalesce onto it.
func TestDB_TestingDatabaseSingletonCountsOnce(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	name, ok := testingDatabaseNameOf("sqlite", dsn)
	assert.True(ok)

	db1, err := OpenSingleton("sqlite", dsn)
	assert.NoError(err)
	db2, err := OpenSingleton("sqlite", dsn)
	assert.NoError(err)
	assert.True(db1 == db2, "OpenSingleton coalesces by DSN")
	assert.Equal(1, testingRefsOf(name), "coalesced callers are one handle, not two")

	assert.NoError(db1.Close())
	assert.Equal(1, testingRefsOf(name), "the shared handle is still open")
	assert.NoError(db2.Close())
	assert.Equal(0, testingRefsOf(name))
}

// Once every handle is gone the database is dropped, so asking for it again mints it afresh
// rather than returning a DSN for a database that no longer exists.
func TestDB_TestingDatabaseReprovisionsAfterFullClose(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	name, ok := testingDatabaseNameOf("sqlite", dsn)
	assert.True(ok)

	db, err := Open("sqlite", dsn)
	assert.NoError(err)
	_, err = db.Exec("CREATE TABLE reprovision_t (x INT)")
	assert.NoError(err)
	assert.NoError(db.Close())
	assert.False(testingDSNIsCached(name))

	// The name is deterministic, so the DSN is the same string; what must differ is that the
	// database behind it was provisioned afresh.
	again, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	assert.Equal(dsn, again)
	assert.True(testingDSNIsCached(name), "re-provisioning re-caches the DSN")

	db2, err := Open("sqlite", again)
	assert.NoError(err)
	defer db2.Close()
	_, err = db2.Exec("CREATE TABLE reprovision_t (x INT)")
	assert.NoError(err, "the table must not already exist - a stale database would still hold it")
}

// An ordinary DSN is neither counted nor dropped.
func TestDB_NonTestingDSNIsNotTracked(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	_, ok := testingDatabaseNameOf("sqlite", "file:production_db?mode=memory&cache=shared")
	assert.False(ok, "a database without the testing prefix names no testing database")

	db, err := Open("sqlite", "file:not_a_testing_db?mode=memory&cache=shared")
	assert.NoError(err)
	assert.Equal(0, testingRefsOf("not_a_testing_db"))
	assert.NoError(db.Close())
}

func TestDB_InjectOutputInserted(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	// Standard case
	result, err := injectOutputInserted("INSERT INTO foo (a, b) VALUES (?, ?)", "id")
	assert.NoError(err)
	assert.Equal("INSERT INTO foo (a, b) OUTPUT INSERTED.id VALUES (?, ?)", result)

	// Lowercase values
	result, err = injectOutputInserted("INSERT INTO foo (a, b) values (?, ?)", "id")
	assert.NoError(err)
	assert.Equal("INSERT INTO foo (a, b) OUTPUT INSERTED.id values (?, ?)", result)

	// Mixed case
	result, err = injectOutputInserted("INSERT INTO foo (a, b) Values (?, ?)", "id")
	assert.NoError(err)
	assert.Equal("INSERT INTO foo (a, b) OUTPUT INSERTED.id Values (?, ?)", result)

	// Extra whitespace before VALUES
	result, err = injectOutputInserted("INSERT INTO foo (a, b)   VALUES (?, ?)", "id")
	assert.NoError(err)
	assert.Equal("INSERT INTO foo (a, b) OUTPUT INSERTED.id   VALUES (?, ?)", result)

	// Tab before VALUES
	result, err = injectOutputInserted("INSERT INTO foo (a, b)\tVALUES (?, ?)", "id")
	assert.NoError(err)
	assert.Equal("INSERT INTO foo (a, b) OUTPUT INSERTED.id\tVALUES (?, ?)", result)

	// INSERT ... SELECT: OUTPUT is injected before the source SELECT
	result, err = injectOutputInserted("INSERT INTO foo (a, b) SELECT a, b FROM bar", "id")
	assert.NoError(err)
	assert.Equal("INSERT INTO foo (a, b) OUTPUT INSERTED.id SELECT a, b FROM bar", result)

	// INSERT ... SELECT with a nested subquery: the outer source SELECT is targeted, not the nested one
	result, err = injectOutputInserted("INSERT INTO foo (a) SELECT (SELECT MAX(x) FROM z) FROM bar", "id")
	assert.NoError(err)
	assert.Equal("INSERT INTO foo (a) OUTPUT INSERTED.id SELECT (SELECT MAX(x) FROM z) FROM bar", result)

	// Lowercase select
	result, err = injectOutputInserted("INSERT INTO foo (a, b) select a, b FROM bar", "id")
	assert.NoError(err)
	assert.Equal("INSERT INTO foo (a, b) OUTPUT INSERTED.id select a, b FROM bar", result)

	// Neither VALUES nor SELECT: error
	_, err = injectOutputInserted("INSERT INTO foo DEFAULT VALUES_BOGUS", "id")
	assert.Error(err)
}
