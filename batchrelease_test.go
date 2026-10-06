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

// A pipelined statement's rows are released as the caller reads past them, so a large result is held raw only
// until each row has been read, not until the batch closes.
func TestBufferedRows_ReleasesRowsAsRead(t *testing.T) {
	assert := testarossa.For(t)

	r := &bufferedRows{rows: [][][]byte{{[]byte("a")}, {[]byte("b")}, {[]byte("c")}}}
	assert.True(r.Next())
	assert.NotNil(r.rows[0], "the current row is kept")
	assert.True(r.Next())
	assert.Nil(r.rows[0], "the row read past is released")
	assert.NotNil(r.rows[1])
	assert.True(r.Next())
	assert.False(r.Next())
	for i, row := range r.rows {
		assert.Nil(row, "row %d is released once the rows are exhausted", i)
	}
	assert.False(r.Next(), "an exhausted reader stays exhausted")
	assert.NoError(r.Close())
	assert.Nil(r.rows)
}

// Reading a pipelined statement hands its rows to the reader, so the batch does not keep a second reference to
// them until it closes.
func TestBatch_QueryHandsRowsToReader(t *testing.T) {
	assert := testarossa.For(t)

	dsn, err := CreateTestingDatabase("", "", t.Name())
	assert.NoError(err)
	db, err := OpenSingleton("", dsn)
	assert.NoError(err)
	defer db.Close()
	if db.DriverName() != "pgx" {
		t.Skip("only a PostgreSQL batch is pipelined")
	}

	ctx := context.Background()
	err = db.Transact(ctx, func(tx *Tx) error {
		b := tx.Batch()
		b.Queue("SELECT 1 UNION ALL SELECT 2")
		res := b.Send(ctx)
		assert.True(res.pipelined)
		rows, err := res.Query()
		if !assert.NoError(err) {
			return err
		}
		assert.Nil(res.batch.items[0].rows, "the batch no longer holds the rows")
		var got []int
		for rows.Next() {
			var n int
			assert.NoError(rows.Scan(&n))
			got = append(got, n)
		}
		assert.Equal([]int{1, 2}, got)
		return res.Close()
	})
	assert.NoError(err)
}
