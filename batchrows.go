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

import "github.com/microbus-io/errors"

/*
BatchRows is the rows of a statement read from [BatchResults.Query]. It reads like [sql.Rows]: call Next before
each row, Scan to read it, and Err once Next returns false. Reading the batch's next result, or closing it,
closes the rows.

When the batch was one round trip, the rows were read in full when it was sent and are held in memory, and they
are scanned with the PostgreSQL driver's own conversions rather than database/sql's: the usual destinations work
(integers, floats, strings, []byte, bool, time.Time, the sql.Null types and any [sql.Scanner]), but cross-type
conversions database/sql performs, such as a SMALLINT into a bool or a number into a string, do not, and a *any
destination receives the driver's own Go types. Scan into the Go type the column holds.
*/
type BatchRows struct {
	src        rowSource
	results    *BatchResults
	closed     bool
	done       bool // Next has returned false: the caller read everything
	superseded bool // closed by a later read of the batch, or by its Close, before the caller finished with it
}

// errSuperseded reports rows read after the batch moved on, so that it is not mistaken for an empty result, and
// records it into the transaction: a caller that ignores it has read nothing it meant to.
func (r *BatchRows) errSuperseded() error {
	return r.results.batch.tx.recordErr(errors.New("batch rows read after a later result was read, or after the transaction ended"))
}

// Next prepares the next row for Scan, returning false when there is none or reading failed; Err tells which.
// Rows superseded by a later read of the batch return false, and Err reports it.
func (r *BatchRows) Next() bool {
	if r.superseded {
		_ = r.errSuperseded()
		return false
	}
	if r.closed {
		return false
	}
	ok := r.src.Next()
	if !ok {
		r.done = true
		r.results.fail(r.src.Err())
	}
	return ok
}

// Scan copies the current row's columns into dest. In a [DB.Transact] transaction a scan error is recorded like a
// failed statement, so the transaction rolls back even if the error is ignored.
func (r *BatchRows) Scan(dest ...any) error {
	err := r.src.Scan(dest...)
	if err != nil {
		r.results.batch.tx.recordErr(err)
	}
	return traceErr(err)
}

// Err returns the error that ended iteration early, if any, including the rows having been superseded by a later
// read of the batch.
func (r *BatchRows) Err() error {
	if r.superseded {
		return r.errSuperseded()
	}
	return traceErr(r.src.Err())
}

// Columns returns the column names.
func (r *BatchRows) Columns() ([]string, error) {
	cols, err := r.src.Columns()
	return cols, traceErr(err)
}

// Close closes the rows. It is safe to call more than once.
func (r *BatchRows) Close() error {
	if r.closed {
		return nil
	}
	r.closed = true
	err := r.src.Close()
	r.results.fail(r.src.Err())
	return traceErr(err)
}
