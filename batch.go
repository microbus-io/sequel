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
	"log/slog"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/stdlib"
	"github.com/microbus-io/errors"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"
)

/*
Batch queues statements of a transaction to send to the database together, so that statements whose arguments
do not depend on each other's results cost one round trip instead of one each. Obtain one with [Tx.Batch], queue
statements, call Send, then read the results from the returned [BatchResults] in the order the statements were
queued, and close it:

	err := db.Transact(ctx, func(tx *sequel.Tx) error {
		b := tx.Batch()
		b.Queue("UPDATE accounts SET balance = balance - ? WHERE id = ? AND balance >= ?", amt, from, amt)
		b.Queue("INSERT INTO ledger (account_id, amount) VALUES (?, ?)", from, -amt)
		res := b.Send(ctx)
		defer res.Close()
		debit, err := res.Exec()
		if err != nil {
			return err
		}
		if n, _ := debit.RowsAffected(); n == 0 {
			return errors.New("insufficient funds") // rolls back the ledger row too
		}
		return res.Close()
	})

In a transaction run by [DB.Transact] on PostgreSQL and CockroachDB, the batch is one round trip. On the other
drivers, and in a transaction from [DB.BeginTx], each statement runs when its result is read, so the same code
runs unchanged on every driver and reads its results the same way.

Statements run in the order they were queued, so a later statement sees the effects of an earlier one. What a
batch cannot do is feed one statement's result into another statement's arguments, or decide in Go whether to
send a statement: both need the earlier result back first, which is what a second batch, or a plain statement,
is for.

Arguments are positional; [sql.NamedArg] is not supported. A statement that controls the transaction (BEGIN,
COMMIT, SAVEPOINT) does not belong in a batch.

A Batch is not safe for concurrent use, and is sent once.
*/
type Batch struct {
	tx    *Tx
	items []*batchItem
	sent  bool
}

// batchItem is one queued statement and, when the batch was sent in one round trip, its outcome.
type batchItem struct {
	query    string
	unpacked string
	args     []any

	rowsAffected int64
	fields       []pgconn.FieldDescription
	rows         [][][]byte // raw values, scanned when read, after the connection is no longer borrowed
	err          error
}

// Queue adds a statement to the batch. Its result is read from the [BatchResults] Send returns, with Exec,
// Query or QueryRow according to what the statement returns.
func (b *Batch) Queue(query string, args ...any) {
	if b.sent {
		b.tx.recordErr(errors.New("statement queued after the batch was sent"))
		return
	}
	if len(b.items) == 0 {
		b.tx.unsentBatches++
	}
	b.items = append(b.items, &batchItem{query: query, args: args})
}

// Len is the number of statements queued.
func (b *Batch) Len() int {
	return len(b.items)
}

// failFrom records err as the outcome of every item from index i on, and returns it.
func (b *Batch) failFrom(i int, err error) error {
	for _, it := range b.items[i:] {
		it.err = err
	}
	return err
}

/*
Send sends the queued statements and returns their results, to be read in queue order and then closed. See
[Batch] for when the statements share one round trip.

Send reports no error of its own: a failure is reported by the read of the statement that failed, by every read
after it, and by [BatchResults.Close].
*/
func (b *Batch) Send(ctx context.Context) *BatchResults {
	r := &BatchResults{batch: b, ctx: ctx}
	if b.sent {
		r.fail(errors.New("batch already sent"))
		return r
	}
	b.sent = true
	if len(b.items) > 0 {
		b.tx.unsentBatches--
	}
	if err := b.tx.shortCircuit(); err != nil {
		// Returned as recorded, as a short-circuited statement returns it.
		r.err = err
		return r
	}
	for _, it := range b.items {
		for _, arg := range it.args {
			if _, named := arg.(sql.NamedArg); named {
				r.fail(errors.New("named arguments are not supported in a batch"))
				return r
			}
		}
	}
	if len(b.items) == 0 {
		return r
	}
	b.tx.openBatch = r
	if b.tx.conn != nil {
		r.pipelined = true
		// A failure is recorded into the transaction when a read or Close reaches it, in queue order, as when the
		// statements run one by one; results left open keep the transaction from committing regardless.
		r.typeMap, _ = b.pipeline(ctx)
	}
	return r
}

/*
pipeline sends every statement in one round trip on the transaction's pgx connection and reads all of the results
back into the items: affected counts, and rows as raw values copied out of the driver's buffers. It returns the
connection's type map, for scanning them later, and the first failure; the failed item and every one after it
carry that failure.

Everything is read before it returns, and scanned only after, so the only caller code that runs while the
connection is borrowed is argument Valuers.
*/
func (b *Batch) pipeline(ctx context.Context) (*pgtype.Map, error) {
	tx := b.tx
	t, driver := tx.t, tx.driverName
	for _, it := range b.items {
		unpacked, err := unpackQuery(driver, it.query)
		if err != nil {
			_, finish := t.begin(ctx, driver, "BATCH")
			finish(err)
			return nil, b.failFrom(0, err)
		}
		it.unpacked = unpacked
	}
	ctx, finish := t.begin(ctx, driver, "BATCH")
	if t.tracing() {
		trace.SpanFromContext(ctx).SetAttributes(attribute.Int("db.operation.batch.size", len(b.items)))
	}
	if t.enabled() && t.logger != nil && t.logger.Enabled(ctx, slog.LevelDebug) {
		for _, it := range b.items {
			t.logger.DebugContext(ctx, "sequel batched query", "driver", driver, "query", it.unpacked)
		}
	}
	if err := simulateRTT(ctx, tx.rtt); err != nil {
		finish(err)
		return nil, b.failFrom(0, err)
	}
	var typeMap *pgtype.Map
	var first error
	var panicked any
	err := tx.conn.Raw(func(driverConn any) (err error) {
		pc, ok := driverConn.(*stdlib.Conn)
		if !ok {
			return errors.New("a pipelined batch needs a pgx connection, got %T", driverConn)
		}
		defer func() {
			if r := recover(); r != nil {
				// Panicking out of Raw would close the Conn, which waits for the open transaction, which only
				// this goroutine can end: report an error instead, and re-panic once the borrow is over. The
				// connection may be mid-pipeline, so it is closed, and database/sql discards it.
				panicked = r
				_ = pc.Conn().Close(context.Background())
				err = errors.New("panic in a pipelined batch")
			}
		}()
		if pc.Conn().PgConn().TxStatus() == 'I' {
			// The transaction already committed or rolled back; sent now, the statements would autocommit.
			return sql.ErrTxDone
		}
		typeMap = pc.Conn().TypeMap()
		pb := &pgx.Batch{}
		for _, it := range b.items {
			pb.Queue(it.unpacked, it.args...)
		}
		results := pc.Conn().SendBatch(ctx, pb)
		for _, it := range b.items {
			if first != nil {
				it.err = first
				continue
			}
			it.err = bufferResult(results, it)
			first = it.err
		}
		if err := results.Close(); err != nil && first == nil {
			first = b.failFrom(0, err)
		}
		return first
	})
	if panicked != nil {
		finish(err)
		panic(panicked)
	}
	if errors.Is(err, sql.ErrConnDone) {
		// database/sql discarded the connection when the transaction's context ended, which is what the
		// statement-by-statement path reports as a finished transaction.
		err = sql.ErrTxDone
	}
	if err != nil && first == nil {
		// The borrow itself failed, or the transaction was over: nothing was sent.
		first = b.failFrom(0, err)
	}
	finish(first)
	return typeMap, first
}

// bufferResult reads one statement's outcome from a pipelined batch: its rows as raw values, copied because the
// driver reuses its buffers, and its affected count.
func bufferResult(results pgx.BatchResults, it *batchItem) error {
	rows, err := results.Query()
	if err != nil {
		return err
	}
	defer rows.Close()
	it.fields = append([]pgconn.FieldDescription(nil), rows.FieldDescriptions()...)
	for rows.Next() {
		raw := rows.RawValues()
		row := make([][]byte, len(raw))
		for i, v := range raw {
			if v != nil {
				row[i] = append([]byte{}, v...)
			}
		}
		it.rows = append(it.rows, row)
	}
	rows.Close()
	it.rowsAffected = rows.CommandTag().RowsAffected()
	return rows.Err()
}

// rowSource is a result set as BatchRows reads it: *Rows when the statement runs as it is read, buffered raw rows
// when the batch was one round trip.
type rowSource interface {
	Next() bool
	Scan(dest ...any) error
	Err() error
	Close() error
	Columns() ([]string, error)
}

// bufferedRows is a pipelined statement's rows, scanned with the connection's type map.
type bufferedRows struct {
	typeMap *pgtype.Map
	fields  []pgconn.FieldDescription
	rows    [][][]byte
	next    int // 1-based index of the current row; past len(rows) once exhausted or closed
}

func (r *bufferedRows) Next() bool {
	if r.next >= len(r.rows) {
		// Exhausted: a later Scan is an error, as it is on sql.Rows, not a re-read of the last row.
		r.next = len(r.rows) + 1
		return false
	}
	r.next++
	return true
}

func (r *bufferedRows) Scan(dest ...any) error {
	if r.next == 0 || r.next > len(r.rows) {
		return errors.New("Scan called without a row")
	}
	return pgx.ScanRow(r.typeMap, r.fields, r.rows[r.next-1], dest...)
}

func (r *bufferedRows) Columns() ([]string, error) {
	names := make([]string, len(r.fields))
	for i, f := range r.fields {
		names[i] = f.Name
	}
	return names, nil
}

func (r *bufferedRows) Err() error { return nil }

func (r *bufferedRows) Close() error {
	// Dropping the rows and the type map frees the memory and keeps a closed result from ever scanning again.
	r.rows, r.typeMap = nil, nil
	r.next = 1
	return nil
}
