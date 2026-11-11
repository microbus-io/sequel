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
	"fmt"
	"testing"
	"time"

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
	// SQLite reports this one as "no such table: f", the very text of a missing table, so it is taken for one there
	err = db.QueryRowContext(ctx, "SELECT f.* FROM present_table WHERE id=?", 1).Scan(&n)
	assert.Error(err)
	assert.Equal(db.DriverName() == "sqlite", sequel.IsUndefinedTableError(err), "a qualified star with no FROM-clause entry: %v", err)
}

// TestErrors_ConnectionErrorFromTheDatabase pins the classification against what the database and driver return:
// a statement's own failures are not connection errors, and a statement on a connection the server has killed is.
// SQLite has no connection to kill, and CockroachDB cancels a session rather than killing its connection, so on
// those only the first half runs.
func TestErrors_ConnectionErrorFromTheDatabase(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.ExecContext(ctx, "CREATE TABLE conn_table (id INT NOT NULL PRIMARY KEY)")
	assert.NoError(err)
	_, err = db.ExecContext(ctx, "INSERT INTO conn_table (id) VALUES (?)", 1)
	assert.NoError(err)
	_, err = db.ExecContext(ctx, "INSERT INTO conn_table (id) VALUES (?)", 1)
	assert.Error(err)
	assert.False(sequel.IsConnectionError(err), "a duplicate key: %v", err)
	var n int
	err = db.QueryRowContext(ctx, "SELECT id FROM absent_table WHERE id=?", 1).Scan(&n)
	assert.Error(err)
	assert.False(sequel.IsConnectionError(err), "a missing table: %v", err)

	var idQuery, kill string
	switch db.DriverName() {
	case "pgx":
		idQuery, kill = "SELECT pg_backend_pid()", "SELECT pg_terminate_backend(%d)"
	case "mysql", "mariadb":
		idQuery, kill = "SELECT CONNECTION_ID()", "KILL %d"
	case "mssql":
		idQuery, kill = "SELECT @@SPID", "KILL %d"
	default:
		return
	}
	// A pinned connection, because the pool would retry a dead one on a fresh connection and hide the error. The raw
	// one, because sequel.Conn reports the statements after the loss as sql.ErrConnDone, not the driver's shapes.
	conn, err := db.DB.Conn(ctx)
	assert.Must(assert.NoError(err))
	defer conn.Close()
	var id int64
	assert.Must(assert.NoError(conn.QueryRowContext(ctx, idQuery).Scan(&id)))
	_, err = db.ExecContext(ctx, fmt.Sprintf(kill, id))
	assert.Must(assert.NoError(err))
	// The server ends the session asynchronously, so the pinned connection may answer once more before it notices.
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err = conn.ExecContext(ctx, "SELECT 1")
		if err != nil || time.Now().After(deadline) {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	assert.Error(err, "the killed connection kept answering")
	assert.True(sequel.IsConnectionError(err), "a statement on a killed connection: %T %v", err, err)
}
