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

	"github.com/microbus-io/sequel/testdata"
	"github.com/microbus-io/testarossa"
)

func TestDB_AutoCreate(t *testing.T) {
	t.Parallel()
	dsns := map[string]string{
		"mysql": "root:root@tcp(127.0.0.1:3306)/",
		"pgx":   "postgres://postgres:postgres@127.0.0.1:5432/",
		// "mssql": "sqlserver://sa:Password123@127.0.0.1:1433",
	}
	for drv, dsn := range dsns {
		t.Run(drv, func(t *testing.T) {
			assert := testarossa.For(t)

			db, err := OpenTesting(drv, dsn, t.Name())
			assert.NoError(err)
			if !assert.NotNil(db) {
				return
			}
			defer db.Close()

			err = db.Migrate(t.Name(), testdata.FS)
			assert.NoError(err)

			var count int
			stmt := "SELECT COUNT(id) FROM foo"
			err = db.QueryRow(stmt).Scan(&count)
			assert.NoError(err)
			assert.Equal(3, count)

			var id int
			stmt = db.ConformArgPlaceholders("SELECT id FROM foo WHERE id=?")
			err = db.QueryRow(stmt, 1).Scan(&id)
			assert.NoError(err)
			assert.Equal(1, id)
		})
	}
}

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

	// mssql
	name, err = databaseNameFromDataSourceName("mssql", "sqlserver://user:pw@127.0.0.1:1433?database=my_database")
	assert.Expect(name, "my_database", err, nil)
	name, err = databaseNameFromDataSourceName("mssql", "sqlserver://user:pw@127.0.0.1:1433")
	assert.Expect(name, "", err, nil)

	// empty dsn
	_, err = databaseNameFromDataSourceName("mysql", "")
	assert.Error(err)

	// unsupported driver
	_, err = databaseNameFromDataSourceName("sqlite", "file.db")
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

	// Port-based inference
	assert.Equal("mysql", inferDriverName("root:root@127.0.0.1:3306/"))
	assert.Equal("pgx", inferDriverName("user:pw@127.0.0.1:5432/"))
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

	// unsupported driver
	_, err = setDatabaseInDataSourceName("sqlite", "file.db", "mydb")
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
	assert.Equal("SYSUTCDATETIME()", db.NowUTC())

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

	// Unknown driver
	db = &DB{driverName: "unknown"}
	assert.Equal("", db.RegexpTextSearch("name"))
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
