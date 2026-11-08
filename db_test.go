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
	"context"
	"database/sql/driver"
	"encoding/json"
	"net/url"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/fstest"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/microbus-io/errors"
	"github.com/microbus-io/testarossa"
	"github.com/microsoft/go-mssqldb/msdsn"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/sdk/trace/tracetest"
	sqlite "modernc.org/sqlite"
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

// TestDB_MySQLSchemes pins that mysql:// and mariadb:// are prefixes in front of the MySQL driver's own DSN format,
// stripped and not parsed as URLs: whatever follows reaches the driver as written.
func TestDB_MySQLSchemes(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	assert.Equal("root:pw@tcp(127.0.0.1:3306)/app", trimMySQLScheme("mysql://root:pw@tcp(127.0.0.1:3306)/app"))
	assert.Equal("root:pw@tcp(127.0.0.1:3306)/app", trimMySQLScheme("mariadb://root:pw@tcp(127.0.0.1:3306)/app"))
	assert.Equal("root:pw@tcp(127.0.0.1:3306)/app", trimMySQLScheme("root:pw@tcp(127.0.0.1:3306)/app"))

	// Characters a URL would need escaped pass through raw, as the driver's own format takes them
	driver, dsn, err := normalizeDriverAndDSN("", "mariadb://root:p@ss/w%rd@tcp(127.0.0.1:3306)/app?tls=false")
	assert.Expect(driver, "mariadb", err, nil)
	cfg, err := mysql.ParseDSN(dsn)
	if assert.NoError(err) {
		assert.Equal("root", cfg.User)
		assert.Equal("p@ss/w%rd", cfg.Passwd)
		assert.Equal("127.0.0.1:3306", cfg.Addr)
		assert.Equal("app", cfg.DBName)
		assert.True(cfg.ParseTime)
	}
	driver, _, err = normalizeDriverAndDSN("", "MySQL://root@unix(/tmp/mysql.sock)/app")
	assert.Expect(driver, "mysql", err, nil)

	// Only a known scheme is folded: this is user Admin with a password that starts with "//"
	_, dsn, err = normalizeDriverAndDSN("mysql", "Admin://secret@tcp(127.0.0.1:3306)/app")
	assert.NoError(err)
	cfg, err = mysql.ParseDSN(dsn)
	if assert.NoError(err) {
		assert.Equal("Admin", cfg.User)
		assert.Equal("//secret", cfg.Passwd)
	}
	assert.Equal("mysql", physicalDriverName("mariadb"))
}

func TestDB_InferDriverName(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	// Postgres prefix
	assert.Equal("pgx", inferDriverName("postgres://user:pw@127.0.0.1:5432/mydb"))
	assert.Equal("pgx", inferDriverName("postgresql://user:pw@127.0.0.1:5432/mydb"))
	assert.Equal("pgx", inferDriverName("postgresql://user:pw@db.example.com/mydb"))

	// CockroachDB scheme, on any port
	assert.Equal("cockroachdb", inferDriverName("cockroachdb://root@127.0.0.1:26257/"))
	assert.Equal("cockroachdb", inferDriverName("cockroachdb://root@db.example.com:9999/mydb"))

	// MySQL and MariaDB schemes
	assert.Equal("mysql", inferDriverName("mysql://root@tcp(127.0.0.1:3306)/"))
	assert.Equal("mariadb", inferDriverName("mariadb://root@tcp(127.0.0.1:3306)/"))
	assert.Equal("mariadb", inferDriverName("mariadb://root@tcp(db.example.com:5432)/"))

	// Schemes are case-insensitive
	assert.Equal("mariadb", inferDriverName("MariaDB://root@tcp(127.0.0.1:3306)/"))
	assert.Equal("pgx", inferDriverName("POSTGRES://root@127.0.0.1/"))

	// A MySQL user named "file" does not make a SQLite URI
	assert.Equal("mysql", inferDriverName("file:pw@tcp(127.0.0.1:3306)/"))
	assert.Equal("sqlite", inferDriverName("file:data@home.db"))

	// A scheme decides ahead of any port, wherever the port appears
	assert.Equal("pgx", inferDriverName("postgres://root@127.0.0.1:26257/?sslmode=disable"))
	assert.Equal("pgx", inferDriverName("postgresql://root@h1:5432,h2:26257/db"))
	assert.Equal("pgx", inferDriverName("postgres://app:26257pw@db:5432/app"))
	assert.Equal("pgx", inferDriverName("postgres://app@db:5432/app?options=x:26257"))
	assert.Equal("mysql", inferDriverName("root:26257secret@tcp(127.0.0.1:3306)/app"))
	assert.Equal("mssql", inferDriverName("sqlserver://sa:26257Pw@127.0.0.1:1433"))
	assert.Equal("sqlite", inferDriverName("file:/data/snap:26257.db"))

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

	// mariadb - the prefix is stripped; CreateTestingDatabase puts it back
	name, _ = databaseNameFromDataSourceName("mariadb", "mariadb://root:pw@tcp(127.0.0.1:3306)/mydb?tls=false")
	assert.Equal("mydb", name)
	dsn, err = setDatabaseInDataSourceName("mariadb", "mariadb://root:pw@tcp(127.0.0.1:3306)/?tls=false", "mydb")
	assert.NoError(err)
	name, _ = databaseNameFromDataSourceName("mariadb", dsn)
	assert.Equal("mydb", name)

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

	// SQLite driver should also return unchanged
	db = &DB{driverName: "sqlite"}
	assert.Equal(stmt, db.ConformArgPlaceholders(stmt))

	// SQL Server numbers its placeholders like Postgres
	db = &DB{driverName: "mssql"}
	assert.Equal(`SELECT * FROM foo WHERE id=$1 AND name=$2`, db.ConformArgPlaceholders(stmt))
}

func TestDB_ConformArgPlaceholders_CommentsAndBrackets(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	for _, driverName := range []string{"pgx", "mssql"} {
		db := &DB{driverName: driverName}
		assert.Equal("SELECT a -- why?\nFROM t WHERE id=$1", db.ConformArgPlaceholders("SELECT a -- why?\nFROM t WHERE id=?"))
		assert.Equal("SELECT a /* why? */ FROM t WHERE id=$1 AND b=$2", db.ConformArgPlaceholders("SELECT a /* why? */ FROM t WHERE id=? AND b=?"))
		assert.Equal("SELECT a FROM t WHERE id=$1 -- why?", db.ConformArgPlaceholders("SELECT a FROM t WHERE id=? -- why?"))
		assert.Equal("SELECT a FROM t WHERE id=$1 /* why?", db.ConformArgPlaceholders("SELECT a FROM t WHERE id=? /* why?"))
		assert.Equal("SELECT a /*/ why? */ FROM t WHERE id=$1", db.ConformArgPlaceholders("SELECT a /*/ why? */ FROM t WHERE id=?"))
		assert.Equal("SELECT a FROM t WHERE x=$1-$2", db.ConformArgPlaceholders("SELECT a FROM t WHERE x=?-?"))
		assert.Equal("SELECT a FROM t WHERE s='it''s ?' AND id=$1", db.ConformArgPlaceholders("SELECT a FROM t WHERE s='it''s ?' AND id=?"))
	}

	// Brackets quote an identifier on SQL Server only; elsewhere they subscript an array
	db := &DB{driverName: "mssql"}
	assert.Equal("SELECT [why?], [a]]?] FROM t WHERE id=$1", db.ConformArgPlaceholders("SELECT [why?], [a]]?] FROM t WHERE id=?"))
	db = &DB{driverName: "pgx"}
	assert.Equal("SELECT arr[$1] FROM t WHERE id=$2", db.ConformArgPlaceholders("SELECT arr[?] FROM t WHERE id=?"))
}

func TestDB_NowUTC(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := &DB{driverName: "mysql"}
	assert.Equal("(UTC_TIMESTAMP(3))", db.NowUTC())

	db = &DB{driverName: "pgx"}
	assert.Equal("(CAST((NOW() AT TIME ZONE 'UTC') - INTERVAL '500 microseconds' AS TIMESTAMP(3)))", db.NowUTC())

	db = &DB{driverName: "mssql"}
	assert.Equal("(CONVERT(DATETIME2(3), DATEADD(NANOSECOND, -500000, SYSUTCDATETIME())))", db.NowUTC())

	db = &DB{driverName: "sqlite"}
	assert.Equal("(STRFTIME('%Y-%m-%d %H:%M:%f', 'now'))", db.NowUTC())

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
	assert.Equal("UPDATE t SET updated_at=(CAST((NOW() AT TIME ZONE 'UTC') - INTERVAL '500 microseconds' AS TIMESTAMP(3))) WHERE id=$1", q)

	db = newTestDB("mssql")
	q, err = db.UnpackQuery("UPDATE t SET updated_at=NOW_UTC() WHERE id=?")
	assert.NoError(err)
	assert.Equal("UPDATE t SET updated_at=(CONVERT(DATETIME2(3), DATEADD(NANOSECOND, -500000, SYSUTCDATETIME()))) WHERE id=$1", q)

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

	db = newTestDB("cockroachdb")
	q, err = db.UnpackQuery("SELECT DATE_ADD_MILLIS(created_at, ?)")
	assert.NoError(err)
	assert.Equal("SELECT (created_at + CAST(($1) AS FLOAT8) * INTERVAL '1 millisecond')", q)

	db = newTestDB("mssql")
	q, err = db.UnpackQuery("SELECT DATE_ADD_MILLIS(created_at, 5000)")
	assert.NoError(err)
	assert.Equal("SELECT DATEADD(MILLISECOND, 5000, created_at)", q)
	q, err = db.UnpackQuery("SELECT DATE_ADD_MILLIS(created_at, -2592000000)")
	assert.NoError(err)
	assert.Equal("SELECT DATEADD(MILLISECOND, 0, DATEADD(HOUR, -720, created_at))", q)
	q, err = db.UnpackQuery("SELECT DATE_ADD_MILLIS(created_at, 3599999)")
	assert.NoError(err)
	assert.Equal("SELECT DATEADD(MILLISECOND, 3599999, created_at)", q)
	q, err = db.UnpackQuery("SELECT DATE_ADD_MILLIS(created_at, 3723004)")
	assert.NoError(err)
	assert.Equal("SELECT DATEADD(MILLISECOND, 123004, DATEADD(HOUR, 1, created_at))", q)
	q, err = db.UnpackQuery("SELECT DATE_ADD_MILLIS(created_at, -2147483648)")
	assert.NoError(err)
	assert.Equal("SELECT DATEADD(MILLISECOND, -1883648, DATEADD(HOUR, -596, created_at))", q)
	q, err = db.UnpackQuery("SELECT DATE_ADD_MILLIS(created_at, ttl_ms)")
	assert.NoError(err)
	assert.Equal("SELECT DATEADD(MILLISECOND, CAST(ttl_ms AS BIGINT) % 3600000, DATEADD(HOUR, CAST(ttl_ms AS BIGINT) / 3600000, created_at))", q)
	q, err = db.UnpackQuery("SELECT DATE_ADD_MILLIS(created_at, ?) WHERE id=?")
	assert.NoError(err)
	assert.Equal("SELECT DATEADD(MILLISECOND, CAST($1 AS BIGINT) % 3600000, DATEADD(HOUR, CAST($1 AS BIGINT) / 3600000, created_at)) WHERE id=$2", q)
	// DATEADD rounds the interval to its base's type, so a NOW_UTC coarser than milliseconds is widened
	q, err = db.UnpackQuery("SELECT DATE_ADD_MILLIS(NOW_UTC(0), 400)")
	assert.NoError(err)
	assert.Equal("SELECT DATEADD(MILLISECOND, 400, CAST((CONVERT(DATETIME2(0), DATEADD(NANOSECOND, -500000000, SYSUTCDATETIME()))) AS DATETIME2(7)))", q)
	for _, coarse := range []string{"now_utc( 2 )", "NOW_UTC(00)", "NOW_UTC(+1)"} {
		q, err = db.UnpackQuery("SELECT DATE_ADD_MILLIS(" + coarse + ", 400)")
		assert.NoError(err)
		assert.Contains(q, "AS DATETIME2(7))", "for %s", coarse)
	}
	assert.False(isCoarseNowUTC("NOW_UTC(3)"))
	assert.False(isCoarseNowUTC("NOW_UTC(0) + 1"))
	assert.False(isCoarseNowUTC("NOW_UTC(0)) - (NOW_UTC(1)"))
	q, err = db.UnpackQuery("SELECT DATE_ADD_MILLIS(NOW_UTC(), 400)")
	assert.NoError(err)
	assert.Equal("SELECT DATEADD(MILLISECOND, 400, (CONVERT(DATETIME2(3), DATEADD(NANOSECOND, -500000, SYSUTCDATETIME()))))", q)

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
	assert.Equal("SELECT (-TIMESTAMPDIFF(MICROSECOND, updated_at, created_at) / 1000.0)", q)

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
	assert.Equal("SELECT * FROM users ORDER BY id OFFSET $2 ROWS FETCH NEXT $1 ROWS ONLY", q)
	q, err = db.UnpackQuery("SELECT * FROM users WHERE tenant=? ORDER BY id LIMIT_OFFSET(?, ?)")
	assert.NoError(err)
	assert.Equal("SELECT * FROM users WHERE tenant=$1 ORDER BY id OFFSET $3 ROWS FETCH NEXT $2 ROWS ONLY", q)

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
	assert.Equal("SELECT (EXTRACT(EPOCH FROM ((CAST((NOW() AT TIME ZONE 'UTC') - INTERVAL '500 microseconds' AS TIMESTAMP(3))) - created_at)) * 1000.0)", q)

	// NOW_UTC inside DATE_DIFF_MILLIS on SQLite - the comma inside STRFTIME must not split the arguments
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

	db = &DB{driverName: "mariadb"}
	assert.Equal("mariadb", db.DriverName())

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
	// same resolved DSN - the DROP+CREATE happens once.
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
	// OpenSingleton - they end up sharing one *DB and one pool.
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

// testingRefsOf reads the live-handle count sequel keeps for a SQLite testing database.
func testingRefsOf(databaseName string) int {
	testingGlobalMutex.Lock()
	defer testingGlobalMutex.Unlock()
	key := testingDatabaseKey("sqlite", "", databaseName)
	return testingDBRefs[testingGenerationKey(key, testingGenerations[key])]
}

func testingDSNIsCached(databaseName string) bool {
	testingGlobalMutex.Lock()
	defer testingGlobalMutex.Unlock()
	cacheKey, ok := testingDBKeys[testingDatabaseKey("sqlite", "", databaseName)]
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

// An in-memory SQLite testing database lives as long as a handle is open, not as long as its pool happens to hold
// a connection. A pool may close every connection it has while the handle stays open - shrinking its limits under
// concurrent returns does, and so does dropping the idle cap - and SQLite drops an in-memory database with its last
// connection.
func TestDB_TestingDatabaseOutlivesItsConnections(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	db, err := Open("sqlite", dsn)
	assert.NoError(err)
	defer db.Close()
	_, err = db.Exec("CREATE TABLE outlives_t (x INT)")
	assert.NoError(err)
	_, err = db.Exec("INSERT INTO outlives_t (x) VALUES (1)")
	assert.NoError(err)

	// Close every connection the pool holds; the next statement opens a fresh one.
	db.SetMaxIdleConns(0)
	assert.Equal(0, db.Stats().OpenConnections)

	var n int
	assert.NoError(db.QueryRow("SELECT COUNT(*) FROM outlives_t").Scan(&n), "the database was dropped with the pool's last connection")
	assert.Equal(1, n)
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

// The leftover-cleanup sweep DROPs database names read from the server's own catalog - input any
// principal with CREATE DATABASE rights can plant - spliced unquoted into the statement. The full-match
// pattern is the injection guard: only names CreateTestingDatabase could have minted qualify.
func TestDB_LeftoverSweepRejectsHostileNames(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	assert.True(leftoverTestingDatabasePattern.MatchString("testing_14_foo_bar_1"))
	assert.True(leftoverTestingDatabasePattern.MatchString("testing_09_"))
	// Anything CreateTestingDatabase could not have minted is left alone, not escaped.
	assert.False(leftoverTestingDatabasePattern.MatchString("testing_14_x; DROP DATABASE prod"))
	assert.False(leftoverTestingDatabasePattern.MatchString("testing_14_x--"))
	assert.False(leftoverTestingDatabasePattern.MatchString(`testing_14_"quoted"`))
	assert.False(leftoverTestingDatabasePattern.MatchString("testing_14_UPPER"))
	assert.False(leftoverTestingDatabasePattern.MatchString("prod_testing_14_x"))
	assert.False(leftoverTestingDatabasePattern.MatchString("testing_99_x"))
}

// The ID column is spliced into SQL on some drivers (RETURNING / OUTPUT INSERTED), so it is validated
// against a narrow identifier charset on every driver - rejected up front, never escaped.
func TestDB_InsertReturnIDRejectsInvalidColumn(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	db, err := Open("sqlite", dsn)
	assert.NoError(err)
	defer db.Close()
	_, err = db.Exec("CREATE TABLE ins_ident (id INTEGER PRIMARY KEY AUTOINCREMENT, v INT)")
	assert.NoError(err)

	ctx := context.Background()
	for _, col := range []string{"id; DROP TABLE ins_ident", "id--", `"id"`, "[id]", "id id", ""} {
		_, err = db.InsertReturnID(ctx, col, "INSERT INTO ins_ident (v) VALUES (?)", 1)
		assert.Error(err, "column %q must be rejected", col)
	}
	id, err := db.InsertReturnID(ctx, "id", "INSERT INTO ins_ident (v) VALUES (?)", 1)
	assert.NoError(err)
	assert.True(id > 0)

	var n int
	assert.NoError(db.QueryRow("SELECT COUNT(*) FROM ins_ident").Scan(&n))
	assert.Equal(1, n, "rejected calls must not have executed the statement")
}

// Every PostgreSQL DSN gains timezone=UTC unless the caller chose a session time zone, in the DSN or the
// environment, and the rewrite changes nothing else the driver parses: not the hosts, the credentials, nor
// the database, in URL or keyword form.
func TestDB_PinSessionTimeZoneUTC(t *testing.T) {
	// Not parallel: t.Setenv. Clear what the driver reads from the environment that could set a time zone or
	// fail the parse, so a developer's own shell cannot decide the outcome.
	for _, env := range []string{"PGTZ", "PGOPTIONS", "PGSERVICE", "PGSERVICEFILE", "PGSSLMODE", "PGSSLROOTCERT", "PGSSLCERT", "PGSSLKEY"} {
		t.Setenv(env, "")
	}
	assert := testarossa.For(t)

	pinned := []struct {
		dsn    string
		expect string
	}{
		{"postgres://u:p@h:5432/db", "postgres://u:p@h:5432/db?timezone=UTC"},
		{"postgres://u:p@h:5432/db?sslmode=disable", "postgres://u:p@h:5432/db?sslmode=disable&timezone=UTC"},
		{"postgres://u:p@h:5432/", "postgres://u:p@h:5432/?timezone=UTC"},
		{"postgres://u:p@h:5432", "postgres://u:p@h:5432?timezone=UTC"},
		{"postgresql://u:p@h/db", "postgresql://u:p@h/db?timezone=UTC"},
		{"postgres://u:p@h/db?", "postgres://u:p@h/db?timezone=UTC"},
		{"postgres://u:p@h/db?sslmode=disable&", "postgres://u:p@h/db?sslmode=disable&timezone=UTC"},
		{"postgres://u:pa?ss@h/db", "postgres://u:pa?ss@h/db?timezone=UTC"},
		{"postgres://u:p@h1:5432,h2:5433/db?target_session_attrs=any", "postgres://u:p@h1:5432,h2:5433/db?target_session_attrs=any&timezone=UTC"},
		{"postgres://u:p@[::1]:5432/db", "postgres://u:p@[::1]:5432/db?timezone=UTC"},
		{"postgres://u:p@h/db?options=-c%20search_path%3Dx", "postgres://u:p@h/db?options=-c%20search_path%3Dx&timezone=UTC"},
		{"host=h user=u password=p dbname=db", "timezone=UTC host=h user=u password=p dbname=db"},
		{`host=h user=u password=abc\`, `timezone=UTC host=h user=u password=abc\`},
		{"host=h user=u password='a b' dbname=db", "timezone=UTC host=h user=u password='a b' dbname=db"},
		{"postgres://u:p@h/db?options=-c%20search_path%3Dtimezone_schema", "postgres://u:p@h/db?options=-c%20search_path%3Dtimezone_schema&timezone=UTC"},
	}
	for _, c := range pinned {
		out := pinSessionTimeZoneUTC(c.dsn)
		assert.Equal(c.expect, out, "for %q", c.dsn)
		assert.Equal(out, pinSessionTimeZoneUTC(out), "pinning is idempotent for %q", c.dsn)

		before, err := pgx.ParseConfig(c.dsn)
		assert.NoError(err, "for %q", c.dsn)
		after, err := pgx.ParseConfig(out)
		if !assert.NoError(err, "for %q", out) {
			continue
		}
		assert.Equal("UTC", after.RuntimeParams["timezone"], "for %q", c.dsn)
		assert.Equal(before.Host, after.Host, "for %q", c.dsn)
		assert.Equal(before.Port, after.Port, "for %q", c.dsn)
		assert.Equal(before.User, after.User, "for %q", c.dsn)
		assert.Equal(before.Password, after.Password, "for %q", c.dsn)
		assert.Equal(before.Database, after.Database, "for %q", c.dsn)
		assert.Equal(len(before.Fallbacks), len(after.Fallbacks), "for %q", c.dsn)
		assert.Equal(before.ConnectTimeout, after.ConnectTimeout, "for %q", c.dsn)
		assert.Equal(before.TLSConfig == nil, after.TLSConfig == nil, "for %q", c.dsn)
		delete(after.RuntimeParams, "timezone")
		assert.Equal(before.RuntimeParams, after.RuntimeParams, "for %q", c.dsn)
	}

	// A time zone the caller named, in any spelling the server accepts, is kept. So is a name registered with
	// the pgx stdlib's RegisterConnConfig, which is a lookup key rather than a DSN.
	untouched := []string{
		"postgres://u:p@h/db?timezone=Asia/Tokyo",
		"postgres://u:p@h/db?TimeZone=Asia/Tokyo",
		"postgres://u:p@h/db?options=-c%20TimeZone%3DAsia/Tokyo",
		"postgres://u:p@h/db?options=-ctimezone%3DAsia/Tokyo",
		"postgres://u:p@h/db?options=--timezone%3DAsia/Tokyo",
		"host=h user=u timezone=Asia/Tokyo",
		"host=h user=u options='-c timezone=Asia/Tokyo'",
		"registeredConnConfig0",
	}
	for _, dsn := range untouched {
		assert.Equal(dsn, pinSessionTimeZoneUTC(dsn))
	}

	// A DSN the driver cannot parse now may parse on a later connection (a certificate or service file that
	// appears after Open), so it is judged by its text: pinned unless it mentions a time zone.
	missingCert := "postgres://u:p@h/db?sslmode=verify-full&sslrootcert=/nonexistent/sequel/ca.pem"
	_, err := pgx.ParseConfig(missingCert)
	assert.Error(err)
	assert.Equal(missingCert+"&timezone=UTC", pinSessionTimeZoneUTC(missingCert))
	assert.Equal(missingCert+"&TimeZone=Asia/Tokyo", pinSessionTimeZoneUTC(missingCert+"&TimeZone=Asia/Tokyo"))
	assert.Equal("timezone=UTC host=h user", pinSessionTimeZoneUTC("host=h user"))

	// A time zone set through the environment is the caller's choice too.
	t.Setenv("PGTZ", "Asia/Tokyo")
	assert.Equal("postgres://u:p@h/db", pinSessionTimeZoneUTC("postgres://u:p@h/db"))
	t.Setenv("PGTZ", "")
	t.Setenv("PGOPTIONS", "-c timezone=Asia/Tokyo")
	assert.Equal("postgres://u:p@h/db", pinSessionTimeZoneUTC("postgres://u:p@h/db"))
	t.Setenv("PGOPTIONS", "-c search_path=x")
	assert.Equal("postgres://u:p@h/db?timezone=UTC", pinSessionTimeZoneUTC("postgres://u:p@h/db"))
	t.Setenv("PGOPTIONS", "")

	// Open and OpenSingleton reach it through normalizeDriverAndDSN, for both PostgreSQL driver names and
	// an inferred driver, and for no other driver.
	for _, driver := range []string{"pgx", "cockroachdb", ""} {
		_, dsn, err := normalizeDriverAndDSN(driver, "postgres://u:p@h:5432/db")
		assert.Expect(dsn, "postgres://u:p@h:5432/db?timezone=UTC", err, nil)
	}
	_, dsn, err := normalizeDriverAndDSN("mssql", "sqlserver://u:p@h:1433")
	assert.Expect(dsn, "sqlserver://u:p@h:1433", err, nil)

	// A cockroachdb:// URL names the driver and reaches the driver as postgres://; a postgresql:// URL is
	// left in its own spelling, which the driver accepts.
	for _, driver := range []string{"cockroachdb", ""} {
		driver, dsn, err := normalizeDriverAndDSN(driver, "cockroachdb://u:p@h:9999/db?sslmode=disable")
		assert.Expect(driver, "cockroachdb", dsn, "postgres://u:p@h:9999/db?sslmode=disable&timezone=UTC", err, nil)
	}
	driver, dsn, err := normalizeDriverAndDSN("", "MariaDB://root@tcp(h:3306)/app")
	assert.Expect(driver, "mariadb", err, nil)
	assert.True(strings.HasPrefix(dsn, "root@tcp(h:3306)/app?"), "got %q", dsn)
	driver, dsn, err = normalizeDriverAndDSN("", "postgresql://u:p@h/db")
	assert.Expect(driver, "pgx", dsn, "postgresql://u:p@h/db?timezone=UTC", err, nil)

	// A pinned keyword DSN still names its pool by database, never by the DSN text, which carries the
	// password.
	_, dsn, err = normalizeDriverAndDSN("pgx", "host=h user=u password=secret dbname=mydb")
	assert.NoError(err)
	assert.Equal("mydb", databaseAttr("pgx", dsn))
	assert.Equal("pgx", databaseAttr("pgx", "host=h user=u password=secret"))
}

// TestMigrate_CreatesItsTableOnce pins that a DB makes sure of sequel_migrations once, not on every Migrate.
func TestMigrate_CreatesItsTableOnce(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	db := newSQLiteDB(t)
	db.SetTracerProvider(tp)
	// A migration that creates nothing, so every CREATE span is the migrations table's.
	fsys := fstest.MapFS{"1.sql": &fstest.MapFile{Data: []byte("SELECT 1")}}
	assert.NoError(db.Migrate(t.Name()+"_a", fsys))
	assert.NoError(db.Migrate(t.Name()+"_b", fsys))
	assert.NoError(db.Migrate(t.Name()+"_a", fsys))

	creates := 0
	var names []string
	for _, s := range tracetest.SpanStubsFromReadOnlySpans(sr.Ended()) {
		names = append(names, s.Name)
		if s.Name == "CREATE" {
			creates++
		}
	}
	assert.Equal(1, creates, "spans: %v", names)
}

// TestMigrate_AFreshMigrationTakesItsLockAtOnce pins that a migration's lock is taken on the first attempt when no
// one holds it: each migration makes two UPDATEs, taking the lock and marking it complete. A new row's lock is held
// until the moment it was inserted, so a strict comparison fails the first attempt whenever it lands in the same
// millisecond, and the attempt is repeated after a 20ms sleep.
func TestMigrate_AFreshMigrationTakesItsLockAtOnce(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	sr := tracetest.NewSpanRecorder()
	tp := sdktrace.NewTracerProvider(sdktrace.WithSpanProcessor(sr))

	db := newSQLiteDB(t)
	db.SetTracerProvider(tp)
	const files = 10
	fsys := fstest.MapFS{}
	for i := 1; i <= files; i++ {
		fsys[strconv.Itoa(i)+".sql"] = &fstest.MapFile{Data: []byte("SELECT 1")}
	}
	assert.NoError(db.Migrate(t.Name(), fsys))

	updates := 0
	var names []string
	for _, s := range tracetest.SpanStubsFromReadOnlySpans(sr.Ended()) {
		names = append(names, s.Name)
		if strings.HasPrefix(s.Name, "UPDATE") {
			updates++
		}
	}
	assert.Equal(2*files, updates, "spans: %v", names)
}

// TestMigrate_AReopenedSingletonMakesItsTableAgain pins that a singleton closed by its last user and opened again
// still migrates: the database under it may be new, as an in-memory SQLite is once its last connection closes, and
// the DB took its migrations table as made.
func TestMigrate_AReopenedSingletonMakesItsTableAgain(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	fsys := fstest.MapFS{"1.sql": &fstest.MapFile{Data: []byte("SELECT 1")}}
	db, err := OpenSingleton("sqlite", dsn)
	assert.NoError(err)
	assert.NoError(db.Migrate(t.Name(), fsys))
	assert.NoError(db.Close())

	db, err = OpenSingleton("sqlite", dsn)
	assert.NoError(err)
	defer db.Close()
	assert.NoError(db.Migrate(t.Name(), fsys))
}

// TestMigrate_ADroppedTableIsMadeAgain pins that Migrate makes sequel_migrations again when it is dropped under an
// open DB that already took it as made, rather than failing until the DB is reopened.
func TestMigrate_ADroppedTableIsMadeAgain(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newSQLiteDB(t)
	fsys := fstest.MapFS{"1.sql": &fstest.MapFile{Data: []byte("SELECT 1")}}
	assert.NoError(db.Migrate(t.Name()+"_a", fsys))
	_, err := db.Exec("DROP TABLE sequel_migrations")
	assert.NoError(err)
	assert.NoError(db.Migrate(t.Name()+"_b", fsys))
	var n int
	assert.NoError(db.QueryRow("SELECT COUNT(*) FROM sequel_migrations").Scan(&n))
	assert.Equal(1, n)
}

// A migration whose lock cannot be extended is stopped and waited for before the lock is given up, so that a runner
// taking the lapsed lock cannot run the same migration alongside it. Here the lock renewal fails while a long
// statement runs: Migrate must cancel it, not run the statement after it, and return the failure. Not parallel: it
// replaces package-level hooks.
func TestMigrate_StopsWhenItsLockCannotBeExtended(t *testing.T) {
	assert := testarossa.For(t)

	renewal, extend := migrationLockRenewal, extendMigrationLock
	defer func() { migrationLockRenewal, extendMigrationLock = renewal, extend }()
	migrationLockRenewal = 50 * time.Millisecond
	extendMigrationLock = func(context.Context, *DB, string, int, string) error { return errors.New("connection lost") }

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	db, err := Open("sqlite", dsn)
	if !assert.NoError(err) {
		return
	}
	defer db.Close()
	fs := fstest.MapFS{"1.slow.sql": {Data: []byte(
		"WITH RECURSIVE c(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM c WHERE x < 2000000000) SELECT COUNT(*) FROM c;\n" +
			"CREATE TABLE after_lost_lock (id INT);\n")}}
	before := runtime.NumGoroutine()
	start := time.Now()
	err = db.Migrate(t.Name(), fs)
	assert.Error(err)
	assert.True(time.Since(start) < 10*time.Second, "the slow statement was not cancelled")
	// The run was waited for: no goroutine is left executing the migration
	time.Sleep(200 * time.Millisecond)
	assert.True(runtime.NumGoroutine() <= before, "a goroutine is still running the migration: %d > %d", runtime.NumGoroutine(), before)
	var n int
	assert.NoError(db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name='after_lost_lock'").Scan(&n))
	assert.Equal(0, n, "the statement after the lost lock ran")
}

// Migrate strips -- comments, but not a "--" inside a quoted string.
func TestMigrate_StripsCommentsOutsideQuotes(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	assert.Equal("SELECT 'a--b', \"c--d\" \nFROM t", stripLineComments("sqlite", "SELECT 'a--b', \"c--d\" -- trailing\nFROM t"))
	assert.Equal("SELECT 'it''s--fine' ", stripLineComments("sqlite", "SELECT 'it''s--fine' -- note"))
	assert.Equal("\nSELECT 1", stripLineComments("sqlite", "-- DRIVER: sqlite\nSELECT 1"))
}

// A testing database name carries this process's token, so that two test processes never mint the same one; and only
// a name CreateTestingDatabase could have minted counts as a testing database, which Close drops.
func TestDB_TestingDatabaseNames(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	name, ok := testingDatabaseNameOf("sqlite", dsn)
	assert.True(ok)
	assert.Contains(name, "_"+testingProcessToken+"_")
	assert.Len(testingProcessToken, 6)

	long, err := CreateTestingDatabase("sqlite", "", strings.Repeat("LongTestName/", 10))
	assert.NoError(err)
	longName, ok := testingDatabaseNameOf("sqlite", long)
	assert.True(ok)
	assert.Len(longName, maxTestingDatabaseName)

	_, ok = testingDatabaseNameOf("mysql", "root@tcp(127.0.0.1:3306)/testing_14_Upper")
	assert.False(ok, "a name with an upper-case letter was taken for a testing database")
	_, ok = testingDatabaseNameOf("mysql", "root@tcp(127.0.0.1:3306)/testing_14_x-y")
	assert.False(ok)
	_, ok = testingDatabaseNameOf("mysql", "root@tcp(127.0.0.1:3306)/testing_14_abc123_x")
	assert.True(ok)

	// Two servers with a database of one name are two testing databases
	assert.NotEqual(
		testingDatabaseKey("mysql", "root@tcp(h1:3306)/testing_14_x", "testing_14_x"),
		testingDatabaseKey("mysql", "root@tcp(h2:3306)/testing_14_x", "testing_14_x"))
	assert.NotEqual(
		testingDatabaseKey("pgx", "postgres://h:5432/testing_14_x", "testing_14_x"),
		testingDatabaseKey("pgx", "postgres://h:5433/testing_14_x", "testing_14_x"))
	// And one server reached by two spellings is one
	assert.Equal(
		testingDatabaseKey("mariadb", "mariadb://root@tcp(h:3306)/testing_14_x", "testing_14_x"),
		testingDatabaseKey("mysql", "root:pw@tcp(h:3306)/testing_14_x?parseTime=true", "testing_14_x"))
}

// TestDB_ConformPlaceholdersSkipsQuotedCode pins that a ? which the server does not take for a parameter is left
// alone: inside a PostgreSQL dollar-quoted body, where it is jsonb's operator, and inside an E'...' string whose
// backslash escapes a quote.
func TestDB_ConformPlaceholdersSkipsQuotedCode(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	for _, driver := range []string{"pgx", "cockroachdb"} {
		assert.Equal("CREATE FUNCTION f(j JSONB) RETURNS BOOL AS $$ SELECT j ? 'k' $$ LANGUAGE SQL",
			conformPlaceholders(driver, "CREATE FUNCTION f(j JSONB) RETURNS BOOL AS $$ SELECT j ? 'k' $$ LANGUAGE SQL"))
		assert.Equal("SELECT $body$ ? $body$, $1", conformPlaceholders(driver, "SELECT $body$ ? $body$, ?"))
		assert.Equal(`SELECT E'\'?', $1`, conformPlaceholders(driver, `SELECT E'\'?', ?`))
		assert.Equal("SELECT arr[$1], $2", conformPlaceholders(driver, "SELECT arr[?], ?"))
		assert.Equal("SELECT a$b, $1", conformPlaceholders(driver, "SELECT a$b, ?"))
	}
	// SQL Server has no dollar quotes
	assert.Equal("SELECT $$ $1 $$", conformPlaceholders("mssql", "SELECT $$ ? $$"))
}

// DSN parse failures expose neither the input nor parser details, which can include credentials.
func TestDB_ParseErrorsDoNotLeakPasswords(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	for _, c := range []struct{ driver, dsn string }{
		{"mssql", "server=localhost;user id=sa;password=top secret;port=bad"},
		{"mssql", "server=h;password=top token=secret;port=bad"},
		{"mssql", "server=h;password='top' secret;port=bad"},
		{"mssql", `server=h;password="top" secret;port=bad`},
		{"mssql", "server=h;password={top} secret;port=bad"},
		{"mssql", "sqlserver://sa:secret@host:bad"},
		{"pgx", "host=x password='top secret' port=bad"},
		{"pgx", "postgres://u:secret@host:bad/db"},
		{"mysql", "user:secret@tcp(localhost)/db?timeout=bad"},
		{"mariadb", "user:secret@tcp(localhost)/db?timeout=bad"},
	} {
		for _, parse := range []func() error{
			func() error { _, err := databaseNameFromDataSourceName(c.driver, c.dsn); return err },
			func() error { _, err := setDatabaseInDataSourceName(c.driver, c.dsn, "test"); return err },
			func() error { _, err := CreateTestingDatabase(c.driver, c.dsn, t.Name()); return err },
		} {
			err := parse()
			if assert.Error(err) {
				assert.Equal("error parsing "+c.driver+" data source name", err.Error())
			}
		}
	}
}

// A closed DB fails its operations as a closed sql.DB does, rather than dereferencing nil, and a singleton opened
// again after its last close is a new DB, so the closed one is never written to while a straggler reads it.
func TestDB_ClosedDBFailsWithoutPanicking(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	db, err := Open("sqlite", dsn)
	assert.NoError(err)
	assert.NoError(db.Close())
	assert.NoError(db.Close()) // A second close is a no-op
	_, err = db.Exec("SELECT 1")
	assert.Error(err)
	assert.Error(db.Ping())
	_, err = db.Begin()
	assert.Error(err)
	var n int
	assert.Error(db.QueryRow("SELECT 1").Scan(&n))
	assert.Error(db.Transact(context.Background(), func(tx *Tx) error { return nil }))

	single, err := OpenSingleton("sqlite", dsn)
	assert.NoError(err)
	assert.NoError(single.Close())
	again, err := OpenSingleton("sqlite", dsn)
	assert.NoError(err)
	defer again.Close()
	assert.True(single != again, "a closed singleton was reused")
	_, err = single.Exec("SELECT 1")
	assert.Error(err)
	_, err = again.Exec("SELECT 1")
	assert.NoError(err)
}

// Queries racing the last Close of their DB fail or succeed, but never panic or race (run under -race).
func TestDB_CloseRacesQueries(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	for range 20 {
		db, err := OpenSingleton("sqlite", dsn)
		assert.NoError(err)
		var wg sync.WaitGroup
		for range 4 {
			wg.Go(func() {
				for range 20 {
					_, _ = db.Exec("SELECT 1")
					_ = db.Ping()
				}
			})
		}
		wg.Go(func() {
			_ = db.Close()
			reopened, err := OpenSingleton("sqlite", dsn)
			if err == nil {
				_ = reopened.Close()
			}
		})
		wg.Wait()
	}
}

// Test IDs that read the same once sanitized - by case, or by punctuation - get databases of their own, since a
// shared one would let one test read, and drop, another's data. Names stay within the leftover pattern and length.
func TestDB_TestingDatabaseNamesDoNotCollide(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	long := strings.Repeat("x", 80)
	seen := map[string]string{}
	for _, id := range []string{t.Name() + "/a", t.Name() + "_a", strings.ToLower(t.Name()) + "_a", long + "1", long + "2"} {
		dsn, err := CreateTestingDatabase("sqlite", "", id)
		if !assert.NoError(err) {
			continue
		}
		name, err := databaseNameFromDataSourceName("sqlite", dsn)
		assert.NoError(err)
		assert.True(leftoverTestingDatabasePattern.MatchString(name), "%s", name)
		assert.True(len(name) <= maxTestingDatabaseName, "%s", name)
		if other, ok := seen[name]; ok {
			t.Errorf("%q and %q share the database %s", other, id, name)
		}
		seen[name] = id
	}

	// One test ID still reaches one database
	a, err := CreateTestingDatabase("sqlite", "", t.Name()+"/a")
	assert.NoError(err)
	b, err := CreateTestingDatabase("sqlite", "", t.Name()+"/a")
	assert.NoError(err)
	assert.Equal(a, b)
}

// Whatever the SQLite base, a testing database is one in-memory database every handle reaches: a bare path keeps
// its parameters and names no file, and a base with only one of mode=memory and cache=shared gains the other.
func TestDB_SQLiteTestingDatabaseIsSharedMemory(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	for i, base := range []string{":memory:", "example.db", "file:partial?mode=memory", "file:partial?cache=shared",
		"file:other.db?mode=rwc&_pragma=foreign_keys(1)"} {
		dsn, err := CreateTestingDatabase("sqlite", base, t.Name()+strconv.Itoa(i))
		if !assert.NoError(err, "for %q", base) {
			continue
		}
		assert.True(strings.HasPrefix(dsn, "file:testing_"), "for %q: %s", base, dsn)
		assert.Contains(dsn, "mode=memory", "for %q", base)
		assert.Contains(dsn, "cache=shared", "for %q", base)
		assert.NotContains(dsn, "mode=rwc", "for %q", base)
		db1, err := Open("", dsn)
		if !assert.NoError(err, "for %q", base) {
			continue
		}
		db2, err := Open("", dsn)
		assert.NoError(err)
		_, err = db1.Exec("CREATE TABLE shared_mem (id INT)")
		assert.NoError(err)
		var n int
		assert.NoError(db2.QueryRow("SELECT COUNT(*) FROM shared_mem").Scan(&n), "for %q", base)
		db2.Close()
		db1.Close()
	}
	assert.Contains(sqliteTestingBase("file:other.db?mode=rwc&_pragma=foreign_keys(1)"), "_pragma=foreign_keys(1)")
}

// Statements end at a ; that ends a line, but not one inside a string literal, a quoted identifier, a block comment
// or a dollar-quoted body; one ending a -- comment line does, as it always has.
func TestMigrate_SplitMigration(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	assert.Equal([]string{"CREATE TABLE t (v TEXT)", "\nINSERT INTO t VALUES ('first;\nsecond')", "\n"},
		splitMigration("sqlite", "CREATE TABLE t (v TEXT);\nINSERT INTO t VALUES ('first;\nsecond');\n"))
	assert.Equal([]string{"SELECT 1", "\nSELECT 2"}, splitMigration("sqlite", "SELECT 1; \t\r\nSELECT 2"))
	assert.Equal([]string{"SELECT 1; -- not the end\nSELECT 2"}, splitMigration("sqlite", "SELECT 1; -- not the end\nSELECT 2"))
	assert.Equal([]string{"-- nothing here;\n", "SELECT 2"}, splitMigration("sqlite", "-- nothing here;\nSELECT 2"))
	assert.Equal([]string{"SELECT \"a;\nb\", /* c;\n */ 1"}, splitMigration("sqlite", "SELECT \"a;\nb\", /* c;\n */ 1"))
	body := "CREATE FUNCTION f() RETURNS INT AS $$\nBEGIN\n  PERFORM 1;\n  RETURN 1;\nEND;\n$$ LANGUAGE plpgsql"
	assert.Equal([]string{body, "\nSELECT f()"}, splitMigration("pgx", body+";\nSELECT f()"))
	assert.Equal([]string{"SELECT 'it\\'s;\n'"}, splitMigration("mysql", "SELECT 'it\\'s;\n'"))
}

// A -- DRIVER: directive counts only as a comment; the same text in a string literal is data.
func TestMigrate_DriverDirective(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	drivers, ok := migrationDriverDirective("sqlite", "-- DRIVER: mysql mariadb\nSELECT 1")
	assert.True(ok)
	assert.Equal([]string{"mysql", "mariadb"}, drivers)
	_, ok = migrationDriverDirective("sqlite", "INSERT INTO d VALUES ('-- DRIVER: mysql')")
	assert.False(ok)
	_, ok = migrationDriverDirective("sqlite", "INSERT INTO \"-- DRIVER: mysql\" VALUES (1)")
	assert.False(ok)
	_, ok = migrationDriverDirective("sqlite", "/* -- DRIVER: mysql */ SELECT 1")
	assert.False(ok)
	_, ok = migrationDriverDirective("sqlite", "-- just a comment\nSELECT 1")
	assert.False(ok)
	drivers, ok = migrationDriverDirective("sqlite", "-- a note\n-- DRIVER: sqlite\nSELECT 1")
	assert.True(ok)
	assert.Equal([]string{"sqlite"}, drivers)

	db := newSQLiteDB(t)
	fs := fstest.MapFS{"1.directives.sql": {Data: []byte(
		"CREATE TABLE directives (v TEXT);\nINSERT INTO directives VALUES ('-- DRIVER: mysql');\n" +
			"INSERT INTO directives VALUES ('first;\nsecond');\n")}}
	assert.NoError(db.Migrate(t.Name(), fs))
	var n int
	assert.NoError(db.QueryRow("SELECT COUNT(*) FROM directives").Scan(&n))
	assert.Equal(2, n)
	var v string
	assert.NoError(db.QueryRow("SELECT v FROM directives WHERE v LIKE 'first%'").Scan(&v))
	assert.Equal("first;\nsecond", v)
}

// A migration extends its lock on a pool of its own, so that a long statement holding the caller's only connection
// cannot keep the extension waiting until the lease lapses. Not parallel: it replaces package-level hooks.
func TestMigrate_ExtendsItsLockWhileHoldingEveryConnection(t *testing.T) {
	assert := testarossa.For(t)

	renewal, extend := migrationLockRenewal, extendMigrationLock
	defer func() { migrationLockRenewal, extendMigrationLock = renewal, extend }()
	migrationLockRenewal = 20 * time.Millisecond
	// Extensions that succeed while the 300ms statement still holds the only connection
	var extended atomic.Int32
	var start time.Time
	extendMigrationLock = func(ctx context.Context, db *DB, sequenceName string, seqNum int, owner string) error {
		err := extend(ctx, db, sequenceName, seqNum, owner)
		if err == nil && time.Since(start) < 250*time.Millisecond {
			extended.Add(1)
		}
		return err
	}

	db := newSQLiteDB(t)
	db.SetMaxOpenConns(1)
	fs := fstest.MapFS{"1.slow.sql": {Data: []byte("SELECT sequel_test_sleep(300);\n")}}
	start = time.Now()
	assert.NoError(db.Migrate(t.Name(), fs))
	assert.True(extended.Load() > 0, "the lock was not extended while the statement ran")
}

// sequel_test_sleep(ms) is a SQLite function that takes ms milliseconds, for statements that must run a known time.
func init() {
	sqlite.MustRegisterScalarFunction("sequel_test_sleep", 1, func(_ *sqlite.FunctionContext, args []driver.Value) (driver.Value, error) {
		ms, _ := args[0].(int64)
		time.Sleep(time.Duration(ms) * time.Millisecond)
		return ms, nil
	})
}

// A runner whose lock was taken by another while its migration ran does not record the migration as its own:
// completing it requires the lock to still name the runner. Not parallel: it replaces package-level hooks.
func TestMigrate_DoesNotCompleteALostLock(t *testing.T) {
	assert := testarossa.For(t)

	renewal, extend := migrationLockRenewal, extendMigrationLock
	defer func() { migrationLockRenewal, extendMigrationLock = renewal, extend }()
	migrationLockRenewal = 10 * time.Millisecond
	extendMigrationLock = func(ctx context.Context, db *DB, sequenceName string, seqNum int, owner string) error {
		// Another runner takes the lapsed lock, and this one's extension does not notice in time
		_, err := db.ExecContext(ctx, "UPDATE sequel_migrations SET locked_by='another' WHERE seq_name=?", sequenceName)
		return err
	}

	db := newSQLiteDB(t)
	fs := fstest.MapFS{"1.slow.sql": {Data: []byte("SELECT sequel_test_sleep(300);\n")}}
	err := db.Migrate(t.Name(), fs)
	if assert.Error(err) {
		assert.True(errors.Is(err, errMigrationLockLost), "%v", err)
	}
	var completed int
	assert.NoError(db.QueryRow("SELECT completed FROM sequel_migrations WHERE seq_name=?", t.Name()).Scan(&completed))
	assert.Equal(0, completed)

	// An extension that finds the lock taken fails, and stops the run
	extendMigrationLock = extend
	_, err = db.Exec("UPDATE sequel_migrations SET locked_by='another' WHERE seq_name=?", t.Name())
	assert.NoError(err)
	assert.True(errors.Is(extend(context.Background(), db, t.Name(), 1, "mine"), errMigrationLockLost))
}

// A sequel_migrations table made before the lock had an owner gains the column, and migrations keep running.
func TestMigrate_AddsTheOwnerColumnToAnOldTable(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newSQLiteDB(t)
	_, err := db.Exec(`CREATE TABLE sequel_migrations (
		seq_name TEXT NOT NULL,
		seq_num INTEGER NOT NULL,
		completed INTEGER NOT NULL DEFAULT 0,
		completed_on TEXT,
		locked_before TEXT NOT NULL DEFAULT NOW_UTC(),
		PRIMARY KEY (seq_name, seq_num)
	)`)
	assert.NoError(err)
	_, err = db.Exec("INSERT INTO sequel_migrations (seq_name, seq_num, completed) VALUES (?, 1, 1)", t.Name())
	assert.NoError(err)
	fs := fstest.MapFS{
		"1.done.sql": {Data: []byte("CREATE TABLE never_runs (id INT)")},
		"2.new.sql":  {Data: []byte("CREATE TABLE owner_col (id INT)")},
	}
	assert.NoError(db.Migrate(t.Name(), fs))
	var n int
	assert.NoError(db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name IN ('never_runs', 'owner_col')").Scan(&n))
	assert.Equal(1, n)
}

// The database of an ADO, ODBC or keyword DSN is read and set in the driver's own grammar, never as a URL: an ADO
// testing DSN once kept its original database, and a PostgreSQL keyword DSN was replaced whole. Every rewrite is
// checked with the driver's parser.
func TestDB_DatabaseInEveryDSNGrammar(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	for _, dsn := range []string{
		"server=localhost;uid=review;pwd=secret;database=production;",
		"server=localhost;user id=review;password=a:b@c;Initial Catalog=production",
		"odbc:server=localhost;uid=review;pwd={se;cret};database=production",
		"sqlserver://review:secret@localhost:1433?database=production",
	} {
		name, err := databaseNameFromDataSourceName("mssql", dsn)
		assert.NoError(err, "%q", dsn)
		assert.Equal("production", name, "%q", dsn)
		for _, db := range []string{"testing_02_review", ""} {
			altered, err := setDatabaseInDataSourceName("mssql", dsn, db)
			if !assert.NoError(err, "%q", dsn) {
				continue
			}
			before, _ := msdsn.Parse(dsn)
			cfg, err := msdsn.Parse(altered)
			assert.NoError(err)
			assert.Equal(db, cfg.Database, "%q", altered)
			assert.Equal("localhost", cfg.Host)
			assert.Equal(before.User, cfg.User)
			assert.Equal(before.Password, cfg.Password)
		}
		assert.True(strings.HasPrefix(serverAddress("mssql", dsn), "localhost:"), "%q", dsn)
	}

	for _, dsn := range []string{
		"host=localhost user=review password=secret dbname=production",
		"host=localhost user=review password='se cret' dbname = production sslmode=disable",
		"postgres://review:secret@localhost:5432/production?sslmode=disable",
	} {
		name, err := databaseNameFromDataSourceName("pgx", dsn)
		assert.NoError(err, "%q", dsn)
		assert.Equal("production", name, "%q", dsn)
		for _, db := range []string{"testing_02_review", ""} {
			altered, err := setDatabaseInDataSourceName("pgx", dsn, db)
			if !assert.NoError(err, "%q", dsn) {
				continue
			}
			cfg, err := pgx.ParseConfig(altered)
			assert.NoError(err)
			assert.Equal(db, cfg.Database, "%q", altered)
			assert.Equal("localhost", cfg.Host)
			assert.Equal("review", cfg.User)
		}
	}
	// A keyword DSN whose last value ends in a backslash cannot take another keyword; it is refused, not misread
	_, err := setDatabaseInDataSourceName("pgx", `host=localhost user=review password=abc\`, "testing_02_review")
	assert.Error(err)
}

// Two migration files with one sequence number - 1.sql and 01.sql, say - fail Migrate before anything runs, rather
// than one of them running and the number being recorded as done. A duplicate already below the watermark has been
// decided, and is left alone.
func TestMigrate_DuplicateSequenceNumbersFail(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newSQLiteDB(t)
	for _, names := range [][2]string{{"1.first.sql", "1.second.sql"}, {"2.sql", "02.sql"}} {
		err := db.Migrate(t.Name()+names[0], fstest.MapFS{
			names[0]: {Data: []byte("CREATE TABLE dup_a (id INT)")},
			names[1]: {Data: []byte("CREATE TABLE dup_b (id INT)")},
		})
		if assert.Error(err) {
			assert.Contains(err.Error(), names[0])
			assert.Contains(err.Error(), names[1])
		}
		var n int
		assert.NoError(db.QueryRow("SELECT COUNT(*) FROM sqlite_master WHERE name IN ('dup_a', 'dup_b')").Scan(&n))
		assert.Equal(0, n)
	}

	assert.NoError(db.Migrate(t.Name(), fstest.MapFS{"1.a.sql": {Data: []byte("CREATE TABLE dup_c (id INT)")}}))
	assert.NoError(db.Migrate(t.Name(), fstest.MapFS{
		"1.a.sql": {Data: []byte("CREATE TABLE dup_c (id INT)")},
		"1.b.sql": {Data: []byte("CREATE TABLE dup_d (id INT)")},
	}))
}

// SQL Server's OUTPUT INSERTED goes before the source clause of the top-level INSERT, which a VALUES or SELECT in a
// comment, a string, a quoted identifier or a CTE before it is not.
func TestDB_InjectOutputInsertedFindsTheSourceClause(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	for _, c := range []struct{ in, out string }{
		{"INSERT /* SELECT an example */ INTO t(v) VALUES (?)", "INSERT /* SELECT an example */ INTO t(v) OUTPUT INSERTED.id VALUES (?)"},
		{"INSERT INTO t(v) -- VALUES\nSELECT v FROM s", "INSERT INTO t(v) -- VALUES\n OUTPUT INSERTED.id SELECT v FROM s"},
		{"INSERT INTO [select values] (v) VALUES (?)", "INSERT INTO [select values] (v) OUTPUT INSERTED.id VALUES (?)"},
		{"INSERT INTO t (v) VALUES ('SELECT')", "INSERT INTO t (v) OUTPUT INSERTED.id VALUES ('SELECT')"},
		{"WITH x AS (SELECT 1 AS a) INSERT INTO t (v) SELECT a FROM x", "WITH x AS (SELECT 1 AS a) INSERT INTO t (v) OUTPUT INSERTED.id SELECT a FROM x"},
		{"INSERT INTO t DEFAULT VALUES", "INSERT INTO t OUTPUT INSERTED.id DEFAULT VALUES"},
		{"INSERT INTO t (selected, values_x) VALUES (?, ?)", "INSERT INTO t (selected, values_x) OUTPUT INSERTED.id VALUES (?, ?)"},
	} {
		out, err := injectOutputInserted(c.in, "id")
		assert.NoError(err, "%q", c.in)
		assert.Equal(c.out, out, "%q", c.in)
	}
	_, err := injectOutputInserted("INSERT /* VALUES */ INTO t EXEC p", "id")
	assert.Error(err)
}

func TestDB_SQLServerDatabaseKeyCaseVariants(t *testing.T) {
	t.Parallel()
	for _, query := range []string{
		"Database=production", "DATABASE=production", "dAtAbAsE=production",
	} {
		for _, target := range []string{"testing_02_review", ""} {
			dsn, err := setDatabaseInDataSourceName("mssql", "sqlserver://review:secret@localhost:1433?"+query, target)
			if err != nil {
				t.Fatal(err)
			}
			u, err := url.Parse(dsn)
			if err != nil {
				t.Fatal(err)
			}
			keys := 0
			for key, values := range u.Query() {
				if strings.EqualFold(key, "database") {
					keys++
					if key != "database" || len(values) != 1 || values[0] != target {
						t.Fatalf("noncanonical database key in %q", dsn)
					}
				}
			}
			if (target == "" && keys != 0) || (target != "" && keys != 1) {
				t.Fatalf("unexpected database keys in %q", dsn)
			}
			for range 100 {
				cfg, err := msdsn.Parse(dsn)
				if err != nil || cfg.Database != target {
					t.Fatalf("unstable database selection: %q, %v", cfg.Database, err)
				}
			}
		}
	}
	// The driver refuses a URL that repeats a key in any letter case, so there is no database to replace
	_, err := setDatabaseInDataSourceName("mssql", "sqlserver://review:secret@localhost:1433?Database=production&database=other", "testing_02_review")
	if err == nil {
		t.Fatal("expected an error for a repeated database key")
	}
}

func TestDB_KeywordDSNDriverPrefixes(t *testing.T) {
	t.Parallel()
	const native = "host=localhost port=54329 user=review password='se cret' dbname=example sslmode=disable options='-c timezone=UTC'"
	for _, dsn := range []string{native, "host=localhost dbname=app.db", "host=localhost password='tcp('"} {
		if got := inferDriverName(dsn); got != "pgx" {
			t.Fatalf("keyword DSN inferred as %q", got)
		}
	}
	if got := inferDriverName("root:pw=secret@tcp(localhost)/app"); got != "mysql" {
		t.Fatalf("MySQL password mistaken for a keyword DSN: %q", got)
	}
	for _, c := range []struct{ prefix, driver string }{
		{pgxKeywordScheme, "pgx"}, {cockroachKeywordScheme, "cockroachdb"},
	} {
		for _, prefix := range []string{c.prefix, strings.ToUpper(c.prefix)} {
			db, err := Open("", prefix+native)
			if err != nil {
				t.Fatal(err)
			}
			if db.DriverName() != c.driver || db.dataSourceName != native {
				t.Errorf("prefix changed driver or keyword parameters: %q", db.DriverName())
			}
			db.Close()
		}
	}
}

// A PostgreSQL keyword DSN is inferred as pgx, but only one: pgx parses any key=value text, an ADO SQL Server DSN
// included, which must not be opened as PostgreSQL with its SQL Server password.
func TestDB_InferKeywordDSN(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	for dsn, want := range map[string]string{
		"host=localhost user=review password=secret dbname=app": "pgx",
		"dbname=app":                    "pgx",
		"host=localhost password='a;b'": "pgx",
		"server=localhost;user id=sa;password=secret;database=app": "",
		"server=localhost,1433;uid=sa;pwd=secret":                  "",
		"password=secret;server=localhost;database=app":            "",
		"Server=tcp:host,1433;Database=app":                        "",
		"foo=bar":                                                  "",
		"host=localhost;port=1433":                                 "",
		"pgx+dsn://host=localhost dbname=app":                      "pgx",
		"cockroachdb+dsn://host=localhost dbname=app":              "cockroachdb",
	} {
		assert.Equal(want, inferDriverName(dsn), "%q", dsn)
	}
}

// A testing database provisioned again while the last close of its previous generation is still dropping it waits for
// that drop, rather than being created first and deleted by it. Not parallel: it replaces a package-level hook.
func TestDB_ReprovisionWaitsForThePendingDrop(t *testing.T) {
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	old, err := Open("sqlite", dsn)
	if !assert.NoError(err) {
		return
	}

	reached, release := make(chan struct{}), make(chan struct{})
	hook := testingRetireHook
	defer func() { testingRetireHook = hook }()
	testingRetireHook = func() {
		close(reached)
		<-release
	}
	closed := make(chan error, 1)
	go func() { closed <- old.Close() }()
	<-reached
	testingRetireHook = hook

	provisioned := make(chan string, 1)
	go func() {
		again, err := CreateTestingDatabase("sqlite", "", t.Name())
		assert.NoError(err)
		provisioned <- again
	}()
	again := ""
	select {
	case again = <-provisioned:
		t.Error("the database was provisioned again while its drop was pending")
	case <-time.After(200 * time.Millisecond):
	}
	close(release)
	assert.NoError(<-closed)
	if again == "" {
		again = <-provisioned
	}
	assert.Equal(dsn, again)

	fresh, err := Open("sqlite", again)
	if !assert.NoError(err) {
		return
	}
	defer fresh.Close()
	_, err = fresh.Exec("CREATE TABLE after_reprovision (id INT)")
	assert.NoError(err)
	_, err = fresh.Exec("INSERT INTO after_reprovision VALUES (1)")
	assert.NoError(err)
	assert.True(testingDSNIsCached(testingDatabaseNameFor(t, again)))
}

// A handle opened on a testing database after it was dropped belongs to that dropped generation. Its last close,
// after the database was provisioned again, must not drop the new generation or evict its DSN.
func TestDB_StaleGenerationDropsNothing(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	name := testingDatabaseNameFor(t, dsn)
	first, err := Open("sqlite", dsn)
	assert.NoError(err)
	assert.NoError(first.Close()) // Drops the first generation

	stale, err := Open("sqlite", dsn) // Opened without provisioning again
	assert.NoError(err)
	again, err := CreateTestingDatabase("sqlite", "", t.Name()) // The second generation
	assert.NoError(err)
	assert.Equal(dsn, again)
	assert.NoError(stale.Close())
	assert.True(testingDSNIsCached(name), "a stale handle evicted the new generation's DSN")

	fresh, err := Open("sqlite", again)
	assert.NoError(err)
	assert.Equal(1, testingRefsOf(name))
	assert.NoError(fresh.Close())
	assert.False(testingDSNIsCached(name))
}

// testingDatabaseNameFor returns the database name a testing DSN names.
func testingDatabaseNameFor(t *testing.T, dsn string) string {
	t.Helper()
	name, ok := testingDatabaseNameOf("sqlite", dsn)
	if !ok {
		t.Fatalf("not a testing DSN: %s", dsn)
	}
	return name
}
