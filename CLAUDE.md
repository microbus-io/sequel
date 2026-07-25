# Sequel Design Notes

This document captures the *why* behind sequel's less-obvious design decisions — the rationale that godoc
does not record. Sequel enhances `database/sql` with cross-driver SQL (virtual functions), schema
migration, ephemeral test databases, adaptive connection pooling, and retrying transactions across
MySQL, PostgreSQL, CockroachDB, SQL Server, and SQLite.

## Commit conventions

**Never run `git commit` without explicit approval.** Making an edit and recording it in history are
separate decisions, and the second belongs to the maintainer, who reviews working-tree changes before they
become commits. Finish the edit, report what changed, and stop — leave the result in the working tree and
wait to be asked. "Commit X" authorizes that commit only: it does not carry forward to later edits in the
same session, and it never implies `git push`. This overrides any default or tooling instruction to the
contrary.

**Do not add a `Co-Authored-By` trailer to commit messages**, for AI assistants or anyone else. No commit
in this repository's history carries one, and machine attribution is not wanted in the log. This likewise
overrides any tooling default.

When a commit *is* requested, match the existing style: a short imperative subject line, followed where the
change warrants it by a body explaining the *why* rather than restating the diff — see `Latch row-iteration
and Scan errors into Transact (sequel.Rows)` as the model. Commit to whatever branch is currently checked
out, whether that is `main` or a feature branch — do not create a new branch first, and do not switch away
from the current one. Choosing the branch is the maintainer's decision, already made by checking it out.

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

## Cross-driver JSON extraction (`JSON_FIELD()`)

`JSON_FIELD(col, '$.path')` pulls one field out of a JSON column. The interesting question was never the
syntax — every engine has an extractor — but **which contract all four can actually keep**, because each one
returns a different thing for the same document.

The contract chosen is `->>` semantics: a JSON string comes back **unquoted**, an object or array comes back
as its **JSON text**, a number or boolean comes back as its text form, and a JSON `null` *or* a missing path
comes back as **SQL NULL**. That is the strongest contract every dialect can reach; the alternative (return
raw JSON text for everything, so strings arrive quoted) is *not* reachable, because SQL Server's `JSON_VALUE`
hands back unquoted scalars and has no mode that re-quotes them.

Two dialects need work to keep it, and both are the kind of thing this library exists to absorb:

- **MySQL is the only engine that breaks the null rule.** `JSON_UNQUOTE(JSON_EXTRACT(doc, '$.x'))` on a JSON
  `null` returns the four-character *string* `'null'`, not SQL NULL — while PostgreSQL `#>>`, SQLite
  `json_extract` and SQL Server `JSON_VALUE` all return NULL. The expansion therefore guards with
  `CASE WHEN JSON_TYPE(...) = 'NULL' THEN NULL ELSE JSON_UNQUOTE(...) END`. This matters more than it looks:
  a JSON `null` is a common tombstone encoding (dwarf uses it for deleted state fields), so an engine that
  silently turns it into the string `"null"` corrupts a delete into a write.
- **SQL Server has no single function that spans the types.** `JSON_VALUE` sees scalars and returns NULL for
  an object/array; `JSON_QUERY` sees objects/arrays and returns NULL for a scalar. Neither alone is the
  contract, so the expansion is `COALESCE(JSON_QUERY(...), JSON_VALUE(...))`.

### The SQL Server 4000-character ceiling on scalars

`JSON_VALUE` returns `NVARCHAR(4000)` and yields **NULL** (in lax mode) for a scalar longer than that. So on
SQL Server — and only there — a JSON *string* over 4000 characters reads back as NULL. Objects and arrays are
unaffected: they take the `JSON_QUERY` branch, which is `NVARCHAR(MAX)`. This is why `JSON_QUERY` is written
*first* in the `COALESCE`: it confines the ceiling to scalars instead of letting a NULL from an oversized
scalar shadow an object that would have extracted fine.

The ceiling is not fixable inside a virtual function. Lifting it requires
`OPENJSON(col, '$.parent') WITH (field NVARCHAR(MAX) '$.field')`, which is a **rowset**, not a scalar
expression — a different *statement shape* than a macro that must expand in place inside a `SELECT` list or a
`WHERE` clause. A correlated scalar subquery over `OPENJSON` would technically fit, but it parses the document
per row and needs a `COLLATE Latin1_General_BIN2` on the key comparison (JSON keys are case-sensitive; SQL
Server's default collation is not), which is a large tax to levy on every driver's use of the function for one
engine's edge case. So the limit is **documented and pinned by a test** (`fixtures/vf_test.go`,
`TestVF_JSONFieldLongScalar`, which asserts NULL on mssql and the full value everywhere else) rather than
papered over. Callers who need large scalars on SQL Server should select the whole column and extract in Go.

### Why the path must be a literal, and the column must not hold a placeholder

The path cannot be a `?` placeholder: PostgreSQL needs it as a `text[]` of keys (`'{"a","b"}'`) and SQL Server
needs it split across two functions, so it has to be **known at expansion time**, not at bind time. It is
parsed into elements and then **re-rendered canonically** rather than passed through, so only the validated
subset ever reaches the database — and the accepted charset for a member name is deliberately narrow
(`[A-Za-z_][A-Za-z0-9_]*`, plus `[0]` array indexes). That narrowness is what makes it safe to splice the path
into a SQL string literal on four dialects without per-dialect escaping rules: a quote or a bracket in a path
is rejected at expansion, not escaped.

### The `$` root is optional

Because the path is parsed and re-rendered per dialect (PostgreSQL's `#>>` takes an array of keys, not a path
string), the conventional JSONPath `$` **carries no information the function does not already supply itself**:
it is validated, discarded, and then re-emitted by us. So `'$.name'` and `'name'` are the same path. The `$` is
*accepted* because three of the four engines spell paths that way natively (MySQL `JSON_EXTRACT`, SQLite
`json_extract`, SQL Server `JSON_VALUE`), and a path copied out of their documentation must not be rejected. It
is *optional* because demanding a token we then throw away is ceremony. `TestVF_JSONFieldRootOptional` pins the
equivalence on every driver.

Separately, the MySQL and SQL Server expansions reference the **column expression twice**, so it must not
itself contain a `?` — a bound argument there would be consumed twice and misalign every later placeholder.
Columns don't normally carry binds, so this is a documented constraint rather than an enforced one.

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

### The "no partial commit" guarantee extends to row reads (`Rows`)

The statement-level recording above covers `Exec`/`Query`/`InsertReturnID` — the errors returned when a
statement is *issued*. It did **not** originally cover the errors that surface *while iterating a result
set*: a mid-stream `rows.Scan` failure or a streaming error reported by `rows.Err()` (a connection drop
mid-fetch, a type-conversion failure on a row). Those are invisible to the issuing call, so a closure that
looped over rows and forgot to check `rows.Err()` could build state from a **truncated read** and commit
it — the one hole in the "a closure that ignores an error can never commit half its work" promise. This is
the failure class an upstream consumer (dwarf) hit: a fan-in step committed with partially-merged state
because a cohort-scan loop dropped a row error.

`Query`/`QueryContext` therefore return a **`sequel.Rows`** (embeds `*sql.Rows`, so `for rows.Next() {
rows.Scan(...) }` / `rows.Err()` / `rows.Close()` are unchanged — same source-compat shape as `Row`). In
`Transact` (`autoErr`) mode it **latches** a `Scan` error, and the `rows.Err()` observed when `Next()`
returns false, into the same recorded-error slot as a statement error — so the transaction refuses to
commit and the subsequent statements short-circuit, exactly as for a failed `Exec`. The latch fires only
on a real error: draining a healthy result set latches a nil `rows.Err()` (no-op), and an early `break`
(where `Next()` is still returning `true`) latches nothing — a streaming error would itself have made
`Next()` return `false`. Outside `Transact` — a `*DB` query or a `BeginTx` `Tx` — `recordErr` is nil and
`Rows` is a pure passthrough, so direct callers are unaffected (same opt-in boundary as statement
recording). `fixtures/rows_test.go` pins both halves: an ignored `Scan` error rolls the closure's write back; a healthy
full drain commits.

### `Row` latches too — but must exempt `sql.ErrNoRows`

`Row` (from `QueryRow`) carries the same latch, for the same reason: a closure that drops a
`QueryRow(...).Scan` error can otherwise commit work built on a row it never read. The argument that it
need not — that a `QueryRow` error is checked at the call site rather than deferred behind an iteration
loop — is weaker than it looks, since `if err != nil { /* skip */ }` reproduces the dwarf failure exactly.

The asymmetry with `Rows` is not *whether* to latch but *what*. `Rows` can latch unconditionally because an
empty result set is `Next()` returning false, never an error. For `Row`, "no row" arrives **as an error** —
`sql.ErrNoRows` — and handling it is routine control flow (`if err == sql.ErrNoRows { …default… }`). A
`Row` that latched it would doom every transaction that legitimately defaults a missing row, so the
exemption is load-bearing, not a nicety, and `TestTransact_CommitsOnErrNoRows` pins it. Every other error —
deadlock, type-conversion failure, connection drop — latches.

Note that the latch is the *second* net for deadlock retry, not the first. `transactOnce` returns whichever
of `fn`'s error or `tx.err` is non-nil, so a deadlock on a `QueryRow` the closure **returns** was always
retried correctly; the latch only extends that to closures that drop the error.

### `QueryRow` short-circuits like every other statement method

`QueryRow`/`QueryRowContext` originally skipped the `shortCircuit()` guard that `Exec`/`Query`/`Prepare`/
`InsertReturnID` all open with — an oversight, not a decision. Without it, a `QueryRow` issued after the
transaction is already doomed still goes to the database and comes back with a driver cascade error
(`current transaction is aborted…` on PostgreSQL), which is exactly the masking the recorded-error design
exists to prevent. Because the guard must return a `*Row` rather than an `error`, a short-circuited `Row`
carries the recorded error in a `shortErr` field with a **nil embedded `*sql.Row`**, and `Scan`/`Err`
report it without touching the database or starting a span (consistent with short-circuited statements
emitting no telemetry). `TestTransact_QueryRowShortCircuits` asserts the *first* error surfaces verbatim.

## Observability (`telemetry.go`)

Sequel emits OpenTelemetry traces/metrics and `slog` logs when the caller supplies providers. The design
choices below are the non-obvious ones.

**Counter instrument names carry no `_total` suffix** (`sequel_lock_contention`, `sequel_migration_runs`).
`_total` is a Prometheus naming convention, not an OpenTelemetry one: a Prometheus exporter appends it to
every counter at the scrape boundary (and de-duplicates, so a name already ending in `_total` is not
doubled), while the OTLP push path uses the instrument name verbatim. So these query in PromQL as
`sequel_lock_contention_total` / `sequel_migration_runs_total`. Do not bake `_total` into a counter's
instrument name.

### Providers via setters, not constructor options — because `Open` has nothing to instrument

`Open`/`OpenSingleton` deliberately keep the standard `database/sql` signature; telemetry is attached with
`SetTracerProvider` / `SetMeterProvider` / `SetLogger` afterward. The reflexive worry is
that operations *inside* `Open` then go uninstrumented — but `sql.Open` is **lazy**: it validates arguments
and builds the pool struct but performs **no network I/O**; the first real connection is established later,
on the first query (or `Ping`). So there is no latency, no round trip, nothing inside `Open` worth a span —
only a possible argument error, which the caller already receives as a return value. Deferring configuration
to a setter therefore loses nothing real, and keeps the two constructors drop-in compatible with
`database/sql`.

### Defaults are the global OTEL providers and a discard logger, not "off"

A freshly opened `*DB` does not start uninstrumented. `newDefaultTelemetry` seeds it with
`otel.GetTracerProvider()`, `otel.GetMeterProvider()`, and a `slog.DiscardHandler` logger. The reason is
that the overwhelmingly common deployment installs OpenTelemetry *globally* (`otel.SetTracerProvider(...)`)
and expects libraries to pick it up — requiring an explicit `db.SetTracerProvider` per pool would silently
drop sequel out of otherwise-configured traces. Defaulting to the global providers means "configured OTEL
globally" just works, and the setters remain available to override per-`DB` (or to pass `nil`, which reverts
that one signal to its default — the global provider, or the discard logger).

This is a deliberate trade against the old zero-overhead path. The global providers are **delegating
no-ops** until the application installs real ones, so an unconfigured process pays only OTEL's no-op cost
(a non-recording span from a noop tracer, `Record` calls into noop instruments) — not zero, but bounded and
constant. Because the delegating provider forwards to whatever is installed *later*, instruments and the
pool-stats callback created at `Open` time start working the moment the app calls `otel.SetMeterProvider`,
with no re-open. The practical consequence: `enabled()` is now effectively always true (every field is
non-nil), so the early-return guards exist for nil-pointer safety rather than as a live fast path. To truly
disable a signal, install an explicit no-op provider — sequel no longer treats "unset" as "off".

The telemetry is held in a single `atomic.Pointer[telemetry]` on `*DB`, seeded at `Open`/`OpenSingleton`.
Setters build a new `telemetry` value under `db.mutex` (copy-on-write via `clone`) and swap the pointer, so
reads never lock and never see a torn value. A `*Tx` captures the pointer at begin time. For an
`OpenSingleton`-shared `*DB` the pointer is process-wide for that pool — last writer wins — so callers are
told to configure once from the owning caller rather than each opener racing to set its own.

### Spans carry operation + table, never the statement text

`db.operation.name` (the leading SQL keyword) is always accurate. `db.collection.name` (the table) is
emitted **only when `parseOperation` can determine it unambiguously** — a single target after
`FROM`/`INTO`/`UPDATE`, with no join, multi-table list, or subquery. The alternative (best-effort table
extraction from arbitrary SQL) would produce confident-looking but wrong attributes and would inflate
span-name cardinality; omitting the table when unsure means a *present* table is trustworthy.

Spans **never** attach the statement text (`db.query.text`). Sequel is a child span under the caller's own
span, which is function-named, so operation + table + that parent identify the statement without paying
per-span text volume on every operation. The full parameterized statement is available instead in the
per-query Debug log (below), where the caller's logger level decides whether to pay for it. There was once a
`SetVerbose` switch that attached `db.query.text` to spans and force-enabled the Debug log; it was removed
because it duplicated `slog`'s own level filtering and put text on spans that the parent span already
distinguishes. (Statement text is never a privacy risk either way: sequel always parameterizes arguments, so
the text holds only `?`/`$1` placeholders, never argument values.)

### Lock contention is classified once, centrally

Every query funnels through one `instrument(...)` wrapper, which is the *only* place an operation's error is
classified for the `sequel_lock_contention` counter. Classifying opportunistically wherever
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
already see at the call site: one-off lifecycle events (each migration as it is attempted, at Info) and
per-query Debug lines. The Debug lines are gated on `logger.Enabled(ctx, slog.LevelDebug)`, so the caller's
own logger level — not a separate sequel switch — decides whether to pay for per-query logging, and the gate
keeps the string-building cost off the hot path when Debug is disabled. `Migrate` logs at *attempt* time
regardless of outcome, so the log shows what was tried even when the migration then fails.

## `CreateTestingDatabase` and the `SEQUEL_TESTING_DSN` fallback

`CreateTestingDatabase` provisions an isolated, auto-dropped database per test. When the caller names
**neither** a driver nor a base DSN, it falls back to the `SEQUEL_TESTING_DSN` environment variable (unset →
SQLite in-memory) and infers the driver from it. This is what lets the same test suite run against every
supported server without touching test code — CI sets the variable per provider, one job each — and it is
inherited by any *upstream* consumer that builds its ephemeral test databases through sequel (e.g. dwarf):
they get the env-var redirect for free, no plumbing of their own.

The non-obvious part is *why the fallback is gated on an empty driver too*, not just an empty DSN. A caller
that passes a driver name — even with an empty DSN, which only asks for that driver's localhost default — has
expressed intent, and the env var must not override it. Sequel's own unit tests rely on this: they call
`CreateTestingDatabase("sqlite", "", …)` and assert SQLite-specific behavior (the `db.system.name` attribute,
`sqlite_master`, file-DSN pool coalescing). Gating on the DSN alone would redirect those onto whatever server
`SEQUEL_TESTING_DSN` points at and break them. So the rule is: the fallback fills in only when the caller
expressed *no* preference at all. The integration tests under `fixtures/` pass empty/empty precisely to opt
in; the root package's white-box tests name `"sqlite"` precisely to opt out.

### The auto-drop is refcounted, and drops on the last close

`Close` drops a `testing_NN_…` database as a best-effort cleanup. The subtlety is *which* close does it.
`Open` hands out a distinct `*DB` per call — that is how a consumer gets independent pools on one shared
testing database — and every one of those handles reaches `maybeDropTestingDatabase` on its own `Close`.
Dropping per handle meant each close but the last issued a `DROP` the server could not grant while its
siblings still held connections, so it **blocked until the server gave up** — measured at 20s per attempt on
SQL Server, 5s on PostgreSQL — and then swallowed the error, leaving nothing to explain why the suite was
slow. So `testingDBRefs` counts live handles per database name and only the last one out drops.

Only handles that will actually attempt the drop are counted. `OpenSingleton` coalesces by DSN, so its extra
callers share one `*DB` and ride `DB.refCount`; their `Close` never reaches the drop path. Counting them
would leave the count permanently above zero and the database never dropped — hence the `retainTestingDatabase`
call sits on `OpenSingleton`'s pool-opening branch only, and on *every* `Open`.

The drop also evicts the cached DSN (`testingDBKeys` maps a database name back to its `testingDSNs` cache
key). Without that, the cache keeps handing out a DSN for a database that no longer exists — which is how a
second engine in a shared-database test failed at startup after the first had shut down. Evicting makes the
next `CreateTestingDatabase` with the same triple mint the database again, which is what asking for a testing
database after releasing every handle on the previous one should mean.

The `DROP` runs under a 5s context timeout. The refcount means it normally runs uncontended, but a handle
that never closes — a test binary that panicked — leaves the database in use and puts the server back into
that block-until-it-gives-up state. Since the drop is best-effort either way (errors are swallowed, and
`CreateTestingDatabase` sweeps leftovers on the next run), waiting longer buys nothing.

### Unit tests in the root package, integration tests under `fixtures/`

Tests that only exercise pure logic — DSN parsing, placeholder conforming, virtual-function string expansion,
lock-contention classification, telemetry plumbing — live in the root `sequel` package and never touch a
server (the telemetry tests are white-box: they read unexported `telemetry` internals, so they cannot move
and stay on SQLite). Tests that provision a real database and run SQL against it live in the test-only
`fixtures/` package, are black-box (exported API only), and are driven entirely by `SEQUEL_TESTING_DSN`. The
virtual-function tests are split deliberately across the two: the root package asserts the SQL each function
*expands to*; `fixtures/` *executes* that SQL on each engine, so a dialect expansion that is well-formed text
but invalid SQL on some server is caught. `REGEXP_TEXT_SEARCH` expands to `REGEXP_LIKE`, which exists only on
PostgreSQL 18+ and SQL Server 2025+; its fixture skips (not fails) when the engine lacks the function, so the
suite stays green on older servers while CI's modern images still exercise it.
