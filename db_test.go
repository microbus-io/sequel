/*
Copyright (c) 2025 Microbus LLC and various contributors

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
	for _, driver := range []string{"mysql", "pgx", "mssql"} {
		t.Run(driver, func(t *testing.T) {
			assert := testarossa.For(t)

			db, err := Open(driver, "")
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

func TestDB_ArgsPlaceholdersToPGX(t *testing.T) {
	assert := testarossa.For(t)

	stmt := `SELECT completed FROM sequel_migrations WHERE seq_name=? AND seq_num=?`
	pgxStmt := ArgPlaceholdersToPGX(stmt)
	assert.Expect(pgxStmt, `SELECT completed FROM sequel_migrations WHERE seq_name=$1 AND seq_num=$2`)

	stmt = `INSERT INTO sequel_migrations (seq_name, seq_num) VALUES (?, ?)`
	pgxStmt = ArgPlaceholdersToPGX(stmt)
	assert.Expect(pgxStmt, `INSERT INTO sequel_migrations (seq_name, seq_num) VALUES ($1, $2)`)
}
