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
	"context"
	"testing"

	"github.com/microbus-io/sequel"
	"github.com/microbus-io/testarossa"
)

// TestErrors_UndefinedTableFromTheDatabase pins the classification against the error the database itself returns:
// a missing table, alone and inside a UNION ALL, is recognized, and a missing column or a bad qualifier is not.
func TestErrors_UndefinedTableFromTheDatabase(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.ExecContext(ctx, "CREATE TABLE present_table (id INT NOT NULL PRIMARY KEY)")
	assert.NoError(err)

	var n int
	err = db.QueryRowContext(ctx, "SELECT id FROM absent_table WHERE id=?", 1).Scan(&n)
	assert.True(sequel.IsUndefinedTableError(err), "a missing table: %v", err)

	rows, err := db.QueryContext(ctx,
		"SELECT id FROM present_table WHERE id=? UNION ALL SELECT id FROM absent_table WHERE id=?", 1, 1)
	if err == nil {
		for rows.Next() {
		}
		err = rows.Err()
		rows.Close()
	}
	assert.True(sequel.IsUndefinedTableError(err), "a missing table in a UNION ALL: %v", err)

	err = db.QueryRowContext(ctx, "SELECT absent_column FROM present_table WHERE id=?", 1).Scan(&n)
	assert.Error(err)
	assert.False(sequel.IsUndefinedTableError(err), "a missing column: %v", err)

	// PostgreSQL and CockroachDB report these with the undefined_table SQLSTATE, but every table they name exists.
	err = db.QueryRowContext(ctx, "SELECT f.id FROM present_table WHERE id=?", 1).Scan(&n)
	assert.Error(err)
	assert.False(sequel.IsUndefinedTableError(err), "a qualifier with no FROM-clause entry: %v", err)
	err = db.QueryRowContext(ctx, "SELECT present_table.id FROM present_table p WHERE p.id=?", 1).Scan(&n)
	assert.Error(err)
	assert.False(sequel.IsUndefinedTableError(err), "a table name hidden by its alias: %v", err)
}
