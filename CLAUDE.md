# Sequel Design Notes

This document captures the *why* behind sequel's less-obvious design decisions — the rationale that godoc
does not record. Sequel enhances `database/sql` with cross-driver SQL (virtual functions), schema
migration, ephemeral test databases, adaptive connection pooling, and retrying transactions across
MySQL, PostgreSQL, CockroachDB, SQL Server, and SQLite.

## Cross-driver timestamp precision (`NOW_UTC()`)

`NOW_UTC()` is contracted to return the current UTC timestamp **at millisecond precision** on every
driver. The non-obvious case is SQL Server. The other drivers already generate at a precision their
natural timestamp column stores exactly — MySQL `UTC_TIMESTAMP(3)` and SQLite `%f` are milliseconds,
PostgreSQL `NOW()` is microseconds into a microsecond `timestamptz`. SQL Server's `SYSUTCDATETIME()`,
however, is `DATETIME2(7)` (100-nanosecond) precision, finer than the `DATETIME2(3)` columns it is
typically stored in.

That mismatch is a correctness trap, not just cosmetics. Storing a 100ns value into a millisecond column
**rounds to the nearest millisecond — upward roughly half the time** — so a value written as "now" can
land up to ~0.5ms in the *future* of the instant it was generated. Code that writes `not_before = NOW_UTC()`
and later checks `not_before <= NOW_UTC()` (or `DATE_DIFF_MILLIS(not_before, NOW_UTC())`) then sees the
row as "not yet due" for a sub-millisecond window. The store rounded; the comparison did not.

The fix is to make `NOW_UTC()` itself millisecond precision on SQL Server —
`CONVERT(DATETIME2(3), SYSUTCDATETIME())` — so the generated value, the stored value, and every later
comparison are all rounded the same way. Rounding is monotonic, so consistent rounding on both sides of
a comparison preserves ordering and the phantom disappears. The general principle: a `NOW_UTC()` value
must never exceed the instant it represents at the precision the schema actually stores, and the only way
to guarantee that across store-and-compare is to generate at the column's precision.

## Transactions: `DB.Transact`

`BeginTx` returns a `Tx` that is a thin shadow of `sql.Tx` (virtual-function expansion + placeholder
conforming) and otherwise behaves identically. `Transact` is the resilient path: it owns
begin → run closure → commit, and **retries the whole closure on lock contention / deadlock**.

### Why a closure, not a "retrying Commit" on `Tx`

A deadlocked transaction is already dead — retrying means re-running the work in a *fresh* transaction.
It is tempting to let a `Tx` record its statements and have a `CommitWithRetry()` replay them, but that is
incorrect for any transaction whose control flow depends on data it reads. Consider a transaction that
reads a counter and branches on it; between the failed attempt and the retry, another transaction may
commit a change to that counter, so the correct branch — and therefore the correct set of statements —
differs on the second attempt. Replaying the first attempt's recorded statements would apply a stale
decision. Re-running the **closure** re-reads and re-decides, so the retry stays correct. This is why
`Transact` takes `func(tx *Tx) error` rather than exposing a retrying commit on an already-begun `Tx`.

The contract that falls out of this: the closure must be safe to run more than once. Its database work is
transactional (a failed attempt rolls back), but any *non-transactional* side effect it performs —
in-memory mutation, channel send, counter bump — may run on each attempt.

### Retry bound and jitter

`transactMaxAttempts` is a constant (8). Lock-contention retry exists to ride out *transient* deadlocks,
which clear in one or two attempts; if eight jittered retries do not clear it, the contention is
structural and more retries only delay surfacing the real problem while piling up latency. So there is no
workload where a larger bound is meaningfully better, and a per-call or per-`DB` knob would be surface
area nobody tunes (it can be added later if a real deployment ever proves the need — easy to add, hard to
remove). The backoff is jittered so that several callers losing the same deadlock do not retry in
lockstep and re-collide.

### `XACT_ABORT ON` for SQL Server

`Transact` issues `SET XACT_ABORT ON` for the mssql driver at the start of each attempt. Without it, SQL
Server leaves some statement errors non-fatal to the transaction, so a doomed transaction can limp along
and the *eventual* failure surfaces as an opaque "COMMIT TRANSACTION request has no corresponding BEGIN
TRANSACTION" — masking the original error (e.g. a deadlock) and defeating retry classification. With
`XACT_ABORT ON`, any statement error aborts the whole transaction server-side, so the real error is the
one that surfaces.

## `Tx` error recording and short-circuit

In `Transact` mode (and only then — `BeginTx` callers are unaffected) a `Tx` records the **first**
statement error and short-circuits every subsequent statement, returning that recorded error without
touching the database. Three reasons, in priority order:

1. **Preserve the first, real error.** After a statement fails, the transaction is doomed: PostgreSQL
   reports `current transaction is aborted, commands ignored until end of block` on the next statement,
   SQL Server (under `XACT_ABORT ON`) has rolled back, and the eventual `Commit` throws its own error.
   These cascade errors *mask* the original one. Freezing at the first error lets `Transact` classify it
   (`IsLockContentionError`) and decide whether to retry. Without this, a deadlock looks like an
   un-retryable commit failure.

2. **Enforce no partial commits, portably.** Recording the error is what makes `Transact` refuse to
   commit. Short-circuiting is what makes that safe on drivers that do **not** auto-abort on a statement
   error — notably MySQL, where statements after a failure would otherwise keep executing and succeeding,
   building up state that only the final rollback undoes. Short-circuit makes "first error wins, nothing
   after it runs" a hard, dialect-independent rule, so a closure that ignores a statement's error can
   never commit half its work.

3. **Avoid pointless round-trips** on a transaction that is going to roll back regardless.

Recording is opt-in via an internal `autoErr` flag set only by `Transact`; a `Tx` from `BeginTx` records
nothing and short-circuits nothing, so existing direct-transaction callers see identical behavior.
`Tx.Err()` exposes the recorded error for callers that want to inspect it.

## Observability (`telemetry.go`)

Sequel emits OpenTelemetry traces/metrics and `slog` logs when the caller supplies providers. The design
choices below are the non-obvious ones.

### Providers via setters, not constructor options — because `Open` has nothing to instrument

`Open`/`OpenSingleton` deliberately keep the standard `database/sql` signature; telemetry is attached with
`SetTracerProvider` / `SetMeterProvider` / `SetLogger` / `SetVerbose` afterward. The reflexive worry is
that operations *inside* `Open` then go uninstrumented — but `sql.Open` is **lazy**: it validates arguments
and builds the pool struct but performs **no network I/O**; the first real connection is established later,
on the first query (or `Ping`). So there is no latency, no round trip, nothing inside `Open` worth a span —
only a possible argument error, which the caller already receives as a return value. Deferring configuration
to a setter therefore loses nothing real, and keeps the two constructors drop-in compatible with
`database/sql`.

The telemetry is held in a single `atomic.Pointer[telemetry]` on `*DB`. The hot path is one atomic load
plus a nil check (`enabled()`), so a `*DB` with no providers pays essentially nothing. Setters build a new
`telemetry` value under `db.mutex` (copy-on-write via `clone`) and swap the pointer, so reads never lock and
never see a torn value. A `*Tx` captures the pointer at begin time. For an `OpenSingleton`-shared `*DB` the
pointer is process-wide for that pool — last writer wins — so callers are told to configure once from the
owning caller rather than each opener racing to set its own.

### Spans carry operation + table; full statement only in verbose mode

`db.operation.name` (the leading SQL keyword) is always accurate. `db.collection.name` (the table) is
emitted **only when `parseOperation` can determine it unambiguously** — a single target after
`FROM`/`INTO`/`UPDATE`, with no join, multi-table list, or subquery. The alternative (best-effort table
extraction from arbitrary SQL) would produce confident-looking but wrong attributes and would inflate
span-name cardinality; omitting the table when unsure means a *present* table is trustworthy. The full
statement (`db.query.text`) is attached only under `SetVerbose`, because most deployments do not want
per-query text volume on every span. It is never a privacy risk: sequel always parameterizes arguments, so
the captured text holds only `?`/`$1` placeholders, never argument values.

### Lock contention is classified once, centrally

Every query funnels through one `instrument(...)` wrapper, which is the *only* place an operation's error is
classified for the `sequel_lock_contention_total` counter. Classifying opportunistically wherever
`IsLockContentionError` happens to be called would be unreliable — that function may be called zero times or
several times per error. `Transact` does **not** separately increment the counter; it only reads the error
to decide whether to retry. Each failed attempt still counts exactly once (at the statement level), which is
the correct semantics: N contended attempts → N increments. Statements short-circuited in `Transact` mode
return before the wrapper, so they emit no span, metric, or log.

### `QueryRow` returns `*sequel.Row` to capture the deferred error

`database/sql` defers a `QueryRow` error to `Scan`, so timing the call alone cannot observe success/failure.
`QueryRow`/`QueryRowContext` therefore return a `*sequel.Row` that embeds `*sql.Row` and overrides `Scan`
(and `Err`) to end the span, record duration, and classify contention at the moment the error becomes
available. This is the same shadowing technique already used for `sql.Tx` and the `DB` query methods, so it
introduces no new pattern. Embedding keeps `QueryRow(...).Scan(...)` source-compatible; only code that
explicitly stores the result as `*sql.Row` must change. The trade-off, documented on the type: duration is
measured until `Scan`, and a `Row` whose `Scan`/`Err` is never called leaves its span unended — the same
resource-leak shape as an unscanned `sql.Row`.

### Logs never carry operation errors

Traces and metrics record per-operation errors (span error status; `status=error` on the duration
histogram); `slog` does **not**. Every error is returned to the caller, who will log it — having the library
log it too would double every failure in operators' logs. Logging is reserved for what the caller cannot
already see at the call site: one-off lifecycle events (each migration as it is attempted, at Info) and, only
under `SetVerbose`, per-query Debug lines. `Migrate` logs at *attempt* time regardless of outcome, so the log
shows what was tried even when the migration then fails.
