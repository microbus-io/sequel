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
	"database/sql"
	"database/sql/driver"
	"errors"
	"io"
	"sync/atomic"
	"testing"

	"github.com/microbus-io/testarossa"
)

// closeFailDriver is a database/sql driver whose result sets fail at Close, as MySQL's do when a later statement of
// a multi-statement query fails while the unread results are discarded. It counts commits and rollbacks.
type closeFailDriver struct{ commits, rollbacks atomic.Int32 }

type closeFailConn struct{ d *closeFailDriver }
type closeFailStmt struct{}
type closeFailTx struct{ d *closeFailDriver }
type closeFailRows struct{ sent bool }

func (d *closeFailDriver) Open(string) (driver.Conn, error)      { return closeFailConn{d}, nil }
func (c closeFailConn) Prepare(string) (driver.Stmt, error)      { return closeFailStmt{}, nil }
func (c closeFailConn) Close() error                             { return nil }
func (c closeFailConn) Begin() (driver.Tx, error)                { return closeFailTx{c.d}, nil }
func (closeFailStmt) Close() error                               { return nil }
func (closeFailStmt) NumInput() int                              { return -1 }
func (closeFailStmt) Exec([]driver.Value) (driver.Result, error) { return driver.RowsAffected(1), nil }
func (closeFailStmt) Query([]driver.Value) (driver.Rows, error)  { return &closeFailRows{}, nil }
func (t closeFailTx) Commit() error                              { t.d.commits.Add(1); return nil }
func (t closeFailTx) Rollback() error                            { t.d.rollbacks.Add(1); return nil }
func (*closeFailRows) Columns() []string                         { return []string{"v"} }
func (*closeFailRows) Close() error                              { return errors.New("late failure") }
func (r *closeFailRows) Next(dest []driver.Value) error {
	if r.sent {
		return io.EOF
	}
	r.sent, dest[0] = true, int64(1)
	return nil
}

var closeFailDrv = &closeFailDriver{}

func init() { sql.Register("sequel_closefail", closeFailDrv) }

// An error that surfaces only at Rows.Close - a closure that broke out of its loop and ignored Close's error - still
// keeps Transact from committing.
func TestRows_CloseErrorIsRecorded(t *testing.T) {
	assert := testarossa.For(t)

	db, err := Open("sequel_closefail", "x")
	if !assert.NoError(err) {
		return
	}
	defer db.Close()
	commits, rollbacks := closeFailDrv.commits.Load(), closeFailDrv.rollbacks.Load()
	err = db.Transact(context.Background(), func(tx *Tx) error {
		if _, err := tx.Exec("INSERT INTO t VALUES (1)"); err != nil {
			return err
		}
		rows, err := tx.Query("SELECT v FROM t")
		if err != nil {
			return err
		}
		rows.Next() // Breaks out after the first row
		_ = rows.Close()
		return nil
	})
	assert.Error(err)
	assert.Equal(commits, closeFailDrv.commits.Load(), "committed despite the Close error")
	assert.Equal(rollbacks+1, closeFailDrv.rollbacks.Load())

	// Outside Transact, Close reports the error and records nothing
	rows, err := db.Query("SELECT v FROM t")
	if assert.NoError(err) {
		assert.Error(rows.Close())
	}
}
