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
	"testing"

	"github.com/microbus-io/testarossa"
)

func TestDB_TransactCommitsOnSuccess(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	db, err := OpenSingleton("sqlite", dsn)
	assert.NoError(err)
	defer db.Close()

	err = db.Transact(ctx, func(tx *Tx) error {
		_, err := tx.ExecContext(ctx, "CREATE TABLE tx_ok (id INTEGER)")
		if err != nil {
			return err
		}
		_, err = tx.ExecContext(ctx, "INSERT INTO tx_ok (id) VALUES (1)")
		return err
	})
	assert.NoError(err)

	var n int
	err = db.QueryRow("SELECT COUNT(*) FROM tx_ok").Scan(&n)
	assert.NoError(err)
	assert.Equal(1, n)
}

func TestDB_TransactRollsBackAndSurfacesIgnoredError(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	dsn, err := CreateTestingDatabase("sqlite", "", t.Name())
	assert.NoError(err)
	db, err := OpenSingleton("sqlite", dsn)
	assert.NoError(err)
	defer db.Close()

	// fn deliberately ignores every statement error. The middle statement targets a missing table; the
	// recorded error must surface, the third statement must short-circuit, and nothing must commit.
	var thirdRan bool
	err = db.Transact(ctx, func(tx *Tx) error {
		tx.ExecContext(ctx, "CREATE TABLE tx_partial (id INTEGER)") //nolint:errcheck
		tx.ExecContext(ctx, "INSERT INTO tx_missing (id) VALUES (1)") //nolint:errcheck
		if _, e := tx.ExecContext(ctx, "INSERT INTO tx_partial (id) VALUES (2)"); e == nil {
			thirdRan = true
		}
		return nil // fn reports success; Transact must still fail via the recorded error
	})
	assert.Error(err)
	assert.False(thirdRan, "statement after the failure should have short-circuited")

	// The whole transaction rolled back: the table created before the failure must not exist.
	var name string
	scanErr := db.QueryRow("SELECT name FROM sqlite_master WHERE type='table' AND name='tx_partial'").Scan(&name)
	assert.Error(scanErr) // ErrNoRows: table was rolled back, so no partial commit
}
