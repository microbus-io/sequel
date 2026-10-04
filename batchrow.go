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
	"database/sql"

	"github.com/microbus-io/errors"
)

// BatchRow is the row of a statement read from [BatchResults.QueryRow]. It reads like [sql.Row]: Scan copies the
// first row's columns into dest, or returns [sql.ErrNoRows] (unwrapped, so it compares with ==) when there is
// none. Scanning follows the rules [BatchRows] describes. [sql.RawBytes] is not a valid destination.
type BatchRow struct {
	rows *BatchRows
	err  error
}

// Scan copies the row's columns into dest and closes it.
func (r *BatchRow) Scan(dest ...any) error {
	if r.err != nil {
		return r.err
	}
	if r.rows.superseded {
		return r.rows.errSuperseded()
	}
	defer r.rows.Close()
	if !r.rows.Next() {
		if err := r.rows.Err(); err != nil {
			return err
		}
		return sql.ErrNoRows
	}
	for _, d := range dest {
		if _, raw := d.(*sql.RawBytes); raw {
			return r.rows.results.batch.tx.recordErr(errors.New("sql.RawBytes is not a valid QueryRow destination"))
		}
	}
	if err := r.rows.Scan(dest...); err != nil {
		return err
	}
	return r.rows.Close()
}

// Err returns the error of the statement, if it failed, without scanning.
func (r *BatchRow) Err() error {
	if r.err != nil {
		return r.err
	}
	return r.rows.Err()
}
