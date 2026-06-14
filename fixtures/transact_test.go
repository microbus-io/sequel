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

func TestTransact_CommitsOnSuccess(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE tx_ok (id INT)")
	assert.NoError(err)

	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		if _, err := tx.ExecContext(ctx, "INSERT INTO tx_ok (id) VALUES (1)"); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, "INSERT INTO tx_ok (id) VALUES (2)")
		return err
	})
	assert.NoError(err)

	var n int
	assert.NoError(db.QueryRow("SELECT COUNT(*) FROM tx_ok").Scan(&n))
	assert.Equal(2, n)
}

// TestTransact_RollsBackAndSurfacesIgnoredError exercises the "first error wins, nothing after it runs,
// nothing commits" contract on a real engine. The closure deliberately ignores every statement error; the
// middle statement targets a missing table. The recorded error must surface even though the closure
// returns nil, the statement after the failure must short-circuit, and the row written before the failure
// must not commit. The rollback is proven with DML (not DDL) so it holds on MySQL too, where DDL inside a
// transaction implicitly commits and would mask a missing rollback.
func TestTransact_RollsBackAndSurfacesIgnoredError(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)
	ctx := context.Background()

	db := newTestDB(t)
	_, err := db.Exec("CREATE TABLE tx_partial (id INT)")
	assert.NoError(err)

	var thirdRan bool
	err = db.Transact(ctx, func(tx *sequel.Tx) error {
		tx.ExecContext(ctx, "INSERT INTO tx_partial (id) VALUES (1)") //nolint:errcheck
		tx.ExecContext(ctx, "INSERT INTO tx_missing (id) VALUES (1)") //nolint:errcheck // missing table: fails
		if _, e := tx.ExecContext(ctx, "INSERT INTO tx_partial (id) VALUES (2)"); e == nil {
			thirdRan = true
		}
		return nil // closure reports success; Transact must still fail via the recorded error
	})
	assert.Error(err)
	assert.False(thirdRan, "statement after the failure should have short-circuited")

	// The whole transaction rolled back: the row inserted before the failure must not be present.
	var n int
	assert.NoError(db.QueryRow("SELECT COUNT(*) FROM tx_partial").Scan(&n))
	assert.Equal(0, n)
}
