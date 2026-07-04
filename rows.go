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

import "database/sql"

/*
Rows shadows *sql.Rows so sequel can latch a row-iteration or Scan error into a [DB.Transact]-managed
transaction, completing the "no partial commit" guarantee for streamed reads.

Transact already records the first Exec/Query *statement* error and short-circuits the rest, so a closure
that ignores a statement's error still cannot commit half its work. The gap that remained was the errors
that surface *while iterating a result set* — a mid-stream Scan failure or a streaming error reported by
rows.Err(). Those were invisible to Transact, so a closure that read rows in a loop and forgot to check
rows.Err() could build state from a truncated read and commit it. Rows closes that gap: Scan and the
end-of-iteration Err are latched exactly like a statement error, so such a closure can no longer commit
partial work.

It embeds *sql.Rows, so the usual `for rows.Next() { rows.Scan(...) }`, `rows.Err()`, and `rows.Close()`
call sites are unchanged; only code that explicitly stores the result as *sql.Rows needs adjustment (the
same source-compat caveat as [Row]). Outside a Transact-managed Tx — a *DB query, or a Tx obtained from
[DB.BeginTx] — recordErr is nil, so Rows is a pure passthrough and behaves exactly like *sql.Rows.
*/
type Rows struct {
	*sql.Rows
	// recordErr latches an error into the owning autoErr Tx (and returns it unchanged); nil for a *DB
	// query or a non-Transact Tx, where Rows is a pure passthrough.
	recordErr func(error) error
}

// latch records a non-nil error into the owning transaction, when there is one.
func (r *Rows) latch(err error) {
	if err != nil && r.recordErr != nil {
		r.recordErr(err)
	}
}

// Scan shadows sql.Rows.Scan and latches a scan error into the transaction (autoErr mode), so a closure
// that ignores the returned error still cannot commit state read from a failed scan.
func (r *Rows) Scan(dest ...any) error {
	err := r.Rows.Scan(dest...)
	r.latch(err)
	return err
}

// Next shadows sql.Rows.Next. When iteration ends (Next returns false) it latches any streaming error
// (rows.Err()), so a `for rows.Next()` loop that never checks rows.Err() still aborts the transaction on
// a mid-stream failure. An early break (Next still returning true) latches nothing — the caller stopped
// deliberately, and a streaming error would itself have made Next return false.
func (r *Rows) Next() bool {
	ok := r.Rows.Next()
	if !ok {
		r.latch(r.Rows.Err())
	}
	return ok
}

// Err shadows sql.Rows.Err and latches the streaming error, so an explicit rows.Err() check aborts the
// transaction as well as surfacing the error to the caller.
func (r *Rows) Err() error {
	err := r.Rows.Err()
	r.latch(err)
	return err
}
