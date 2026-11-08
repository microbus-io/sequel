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
Rows shadows *sql.Rows. Inside a [DB.Transact] transaction, a Scan error and the streaming error that ends
iteration (what rows.Err() reports) are recorded like a statement error: the transaction will not commit, and
later statements short-circuit. A closure that stops checking errors therefore cannot commit work built on a
truncated read. So is an error from Close, which a driver can report while it discards unread results. Breaking out
of the loop early records nothing of itself.

It embeds *sql.Rows, so the usual `for rows.Next() { rows.Scan(...) }`, `rows.Err()`, and `rows.Close()`
call sites are unchanged; only code that explicitly stores the result as *sql.Rows needs adjustment. Outside a
Transact-managed Tx - a *DB query, or a Tx obtained from [DB.BeginTx] - Rows behaves exactly like *sql.Rows.
*/
type Rows struct {
	*sql.Rows
	// tx is the transaction errors are latched into (recording only in autoErr mode); nil for a *DB query,
	// where Rows is a pure passthrough.
	tx *Tx
}

// latch records a non-nil error into the owning transaction, when there is one.
func (r *Rows) latch(err error) {
	if err != nil && r.tx != nil {
		r.tx.recordErr(err)
	}
}

// Scan shadows sql.Rows.Scan and latches a scan error into the transaction (autoErr mode), so a closure
// that ignores the returned error still cannot commit state read from a failed scan.
func (r *Rows) Scan(dest ...any) error {
	err := r.Rows.Scan(dest...)
	r.latch(err)
	return traceErr(err)
}

// Next shadows sql.Rows.Next. When iteration ends (Next returns false) it latches any streaming error
// (rows.Err()), so a `for rows.Next()` loop that never checks rows.Err() still aborts the transaction on
// a mid-stream failure. An early break (Next still returning true) latches nothing - the caller stopped
// deliberately, and a streaming error would itself have made Next return false.
func (r *Rows) Next() bool {
	ok := r.Rows.Next()
	if !ok {
		r.latch(r.Rows.Err())
	}
	return ok
}

// NextResultSet shadows sql.Rows.NextResultSet. Like Next, a false return latches the streaming error, so
// a multi-result-set loop that never checks rows.Err() still aborts the transaction when advancing to the
// next result set failed rather than ran out.
func (r *Rows) NextResultSet() bool {
	ok := r.Rows.NextResultSet()
	if !ok {
		r.latch(r.Rows.Err())
	}
	return ok
}

// Close shadows sql.Rows.Close and latches its error. A driver discards the unread results at Close, and may find a
// statement's error there - a later statement of a multi-statement query, say - which no earlier call reported.
func (r *Rows) Close() error {
	err := r.Rows.Close()
	r.latch(err)
	return traceErr(err)
}

// Err shadows sql.Rows.Err and latches the streaming error, so an explicit rows.Err() check aborts the
// transaction as well as surfacing the error to the caller.
func (r *Rows) Err() error {
	err := r.Rows.Err()
	r.latch(err)
	return traceErr(err)
}
