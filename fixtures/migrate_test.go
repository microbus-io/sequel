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
	"testing"

	"github.com/microbus-io/sequel/testdata"
	"github.com/microbus-io/testarossa"
)

// TestMigrate_AutoCreate runs the full migration set against the configured provider and verifies the
// resulting data, exercising CreateTestingDatabase, Migrate, virtual-function expansion and placeholder
// conforming end to end on a real engine.
func TestMigrate_AutoCreate(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	assert.NoError(db.Migrate(t.Name(), testdata.FS))

	var count int
	assert.NoError(db.QueryRow("SELECT COUNT(id) FROM foo").Scan(&count))
	assert.Equal(4, count)

	// ? placeholders are conformed to the driver's dialect automatically by the query shadow methods.
	var id int
	assert.NoError(db.QueryRow("SELECT id FROM foo WHERE id=?", 1).Scan(&id))
	assert.Equal(1, id)

	// 10.insert.sql sorts before 2.alter-table.sql by filename but must run after it (it uses the column
	// 2 adds). Its row proves migrations ran in numeric, not lexicographic, order.
	var updated int
	assert.NoError(db.QueryRow("SELECT updated FROM foo WHERE id=?", 10).Scan(&updated))
	assert.Equal(1, updated)
}

// TestMigrate_Idempotent verifies that applying the same migration set twice is a no-op the second time:
// already-completed statements are skipped, no error surfaces, and no rows are duplicated.
func TestMigrate_Idempotent(t *testing.T) {
	t.Parallel()
	assert := testarossa.For(t)

	db := newTestDB(t)
	assert.NoError(db.Migrate(t.Name(), testdata.FS))

	var first int
	assert.NoError(db.QueryRow("SELECT COUNT(id) FROM foo").Scan(&first))
	assert.Equal(4, first)

	// Re-running must not re-apply CREATE/INSERT statements (which would error or duplicate rows).
	assert.NoError(db.Migrate(t.Name(), testdata.FS))

	var second int
	assert.NoError(db.QueryRow("SELECT COUNT(id) FROM foo").Scan(&second))
	assert.Equal(4, second)
}
