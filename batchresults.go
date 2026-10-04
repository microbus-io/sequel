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

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/microbus-io/errors"
)

/*
BatchResults reads the results of a sent [Batch], one statement at a time in the order they were queued: Exec
for a statement that returns no rows, Query for one that returns rows, QueryRow for one that returns at most one.
Reading a statement closes the rows of the statement read before it, which can then no longer be read. Close must
be called: it runs any statement not read yet and returns the first failure.

A failed statement's read returns its error, and so does every read after it. When the batch was one round trip,
a statement rejected before anything ran - a syntax error, a missing table, an argument the driver cannot encode,
in pgx's default statement-cache mode - fails the reads of the statements before it too.

From Send until Close the transaction belongs to the batch: any other statement on it fails, and it does not
commit. Ending the transaction closes the results.
*/
type BatchResults struct {
	batch     *Batch
	ctx       context.Context
	next      int   // index of the next statement to read
	err       error // the first failure seen
	closed    bool
	open      *BatchRows // the rows of the statement read last, superseded by the next read
	pipelined bool
	typeMap   *pgtype.Map
}

// fail records err as the batch's first failure, if it is the first, and into the transaction, and returns it.
// An error is traced once, when stored, and returned as stored by every later read; an error the transaction
// already recorded is kept as recorded, so a doomed transaction's error is reported verbatim.
func (r *BatchResults) fail(err error) error {
	if err == nil {
		return nil
	}
	if t := r.batch.tx; t.err != nil && errors.Is(err, t.err) {
		err = t.err
	} else {
		err = traceErr(err)
	}
	if r.err == nil {
		r.err = err
	}
	r.batch.tx.recordErr(err)
	return err
}

// doomed fails the batch with the transaction's recorded error, if a Transact transaction is already doomed - by a
// scan error, say - so that reads and Close report it whichever way the batch was sent.
func (r *BatchResults) doomed() {
	if t := r.batch.tx; t.autoErr && t.err != nil {
		r.fail(t.err)
	}
}

// advance supersedes the rows of the statement read last and moves to the next statement, returning its item, or
// the error its read must report instead.
func (r *BatchResults) advance() (*batchItem, error) {
	r.supersedeOpen()
	if r.closed {
		return nil, errors.New("batch results are closed")
	}
	r.doomed()
	if r.err != nil {
		r.next++
		return nil, r.err
	}
	if r.next >= len(r.batch.items) {
		return nil, errors.New("no more results in the batch")
	}
	it := r.batch.items[r.next]
	r.next++
	if r.pipelined && it.err != nil {
		return nil, r.fail(it.err)
	}
	return it, nil
}

// supersedeOpen closes the rows of the statement read last, so that reading them later is an error rather than an
// empty result.
func (r *BatchResults) supersedeOpen() {
	if r.open != nil {
		if !r.open.closed && !r.open.done {
			r.open.superseded = true
		}
		_ = r.open.Close()
		r.open = nil
	}
}

// run issues a statement of the batch on the transaction, which refuses every other statement while the batch's
// results are open. A statement that panics - an argument Valuer, say - fails the batch on the way out.
func (r *BatchResults) run(fn func() error) (err error) {
	tx := r.batch.tx
	tx.batchExecuting = true
	returned := false
	defer func() {
		tx.batchExecuting = false
		if !returned {
			r.fail(errors.New("panic while running a batch statement"))
		}
	}()
	err = fn()
	returned = true
	return err
}

// Exec reads the result of the next statement, one that returns no rows. When the batch was one round trip, the
// result's LastInsertId is not supported.
func (r *BatchResults) Exec() (sql.Result, error) {
	it, err := r.advance()
	if err != nil {
		return nil, err
	}
	if r.pipelined {
		return driver.RowsAffected(it.rowsAffected), nil
	}
	var res sql.Result
	err = r.run(func() (err error) {
		res, err = r.batch.tx.ExecContext(r.ctx, it.query, it.args...)
		return err
	})
	if err != nil {
		return nil, r.fail(err)
	}
	return res, nil
}

// Query reads the result of the next statement, one that returns rows. The rows are closed by the next read, or by
// Close, if the caller does not close them first.
func (r *BatchResults) Query() (*BatchRows, error) {
	it, err := r.advance()
	if err != nil {
		return nil, err
	}
	var src rowSource
	if r.pipelined {
		src = &bufferedRows{typeMap: r.typeMap, fields: it.fields, rows: it.rows}
	} else {
		var rows *Rows
		err = r.run(func() (err error) {
			rows, err = r.batch.tx.QueryContext(r.ctx, it.query, it.args...)
			return err
		})
		if err != nil {
			return nil, r.fail(err)
		}
		src = rows
	}
	r.open = &BatchRows{src: src, results: r}
	return r.open, nil
}

// QueryRow reads the result of the next statement, one that returns at most one row; any rows past the first are
// discarded.
func (r *BatchResults) QueryRow() *BatchRow {
	rows, err := r.Query()
	return &BatchRow{rows: rows, err: err}
}

/*
Close runs any statement of the batch not read yet, releases the transaction for other statements, and returns
the first failure: of a statement, of a scan, or the transaction's own when it is already doomed. It is safe to
call more than once.
*/
func (r *BatchResults) Close() error {
	if r.closed {
		return r.err
	}
	// The results are released however the unread statements end - a statement whose argument panics included -
	// so that the transaction is not left refusing every statement.
	defer r.abandon()
	r.supersedeOpen()
	r.doomed()
	if r.pipelined {
		// A statement that failed is the batch's failure whether or not it was read.
		for _, it := range r.batch.items {
			if it.err != nil {
				r.fail(it.err)
				break
			}
		}
	} else {
		// A statement not read is still part of the batch.
		for r.err == nil && r.next < len(r.batch.items) {
			it := r.batch.items[r.next]
			r.next++
			r.fail(r.run(func() error {
				_, err := r.batch.tx.ExecContext(r.ctx, it.query, it.args...)
				return err
			}))
		}
	}
	return r.err
}

// abandon closes the results without running anything more: it supersedes their open rows, drops the buffered rows,
// and releases the transaction. Close ends with it, and the transaction calls it when it ends with the results still
// open, so that nothing scans with a type map belonging to a connection back in the pool.
func (r *BatchResults) abandon() {
	r.supersedeOpen()
	r.closed = true
	r.typeMap = nil
	for _, it := range r.batch.items {
		it.fields, it.rows = nil, nil
	}
	if r.batch.tx.openBatch == r {
		r.batch.tx.openBatch = nil
	}
}
