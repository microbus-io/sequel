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
	"testing"

	"github.com/microbus-io/sequel/testdata"
	"github.com/microbus-io/testarossa"
)

func TestDB_AutoCreate(t *testing.T) {
	for _, driver := range []string{"mysql", "pgx" /* , "mssql" */} {
		t.Run(driver, func(t *testing.T) {
			assert := testarossa.For(t)

			db, err := OpenTesting(driver, "", "AutoCreate")
			assert.NoError(err)
			assert.NotNil(db)
			defer db.Close()

			err = db.Migrate("auto_create", testdata.FS)
			assert.NoError(err)

			row := db.QueryRow("SELECT COUNT(id) FROM foo")
			var count int
			err = row.Scan(&count)
			assert.NoError(err)
			assert.Equal(3, count)
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
}
