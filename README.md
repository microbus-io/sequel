# Sequel

[![License Apache 2](https://img.shields.io/badge/License-Apache2-blue.svg)](https://www.apache.org/licenses/LICENSE-2.0)
[![Go](https://img.shields.io/badge/Go-1.25+-00ADD8?logo=go&logoColor=white)](https://go.dev/)
[![Reference](https://pkg.go.dev/badge/github.com/microbus-io/sequel)](https://pkg.go.dev/github.com/microbus-io/sequel)
[![Test](https://github.com/microbus-io/sequel/actions/workflows/test.yaml/badge.svg?branch=main&event=push)](https://github.com/microbus-io/sequel/actions/workflows/test.yaml)
[![Discord](https://img.shields.io/badge/Discord-Join-5865F2?logo=discord&logoColor=white)](https://discord.gg/FAJHnGkNqJ)

A Go library that enhances `database/sql` with cross-driver SQL, schema migration, ephemeral test databases, and adaptive connection pooling.

## Features at a Glance

- **Connection pool management** - Prevents database exhaustion when many consumers in one process share a DSN
- **Schema migration** - Concurrency-safe, incremental database migrations
- **Cross-driver support** - MySQL, PostgreSQL, CockroachDB, SQL Server, and SQLite with unified API
- **Retrying transactions** - `Transact` runs a closure in a transaction, retries on deadlock/lock contention, and never commits partial work
- **Ephemeral test databases** - Isolated databases per test with automatic cleanup, with optional simulated network latency

## Quick Start

```go
import "github.com/microbus-io/sequel"

// Open a database connection with its own pool
db, err := sequel.Open("", "root:root@tcp(127.0.0.1:3306)/mydb")

// Run migrations
err = db.Migrate("myservice@v1", migrationFilesFS)

// Use db.DB for standard sql.DB operations
rows, err := db.Query("SELECT * FROM users WHERE tenant_id=?", tenantID)
```

## Connection Pool Management

Sequel exposes two constructors so the connection-pool strategy is self-documenting at the call site:

- **`Open(driver, dsn)`** returns a fresh `*DB` with its own pool. Each call returns a distinct instance; sequel does not coalesce by DSN and does not size the pool automatically. The standard `database/sql` defaults apply (unlimited open, 2 idle) until the caller adjusts them with `SetMaxOpenConns` / `SetMaxIdleConns`. Use this for a single heavy consumer (e.g. a long-running worker pool) where you want to size the pool to the workload.

- **`OpenSingleton(driver, dsn)`** returns a coalesced `*DB`: multiple calls with the same `(driver, dsn)` share one `*sql.DB` and one connection pool. Sequel automatically sizes that pool based on the number of openers using a sqrt-based formula:
  - `maxIdle ≈ sqrt(N)` where N is the number of openers
  - `maxOpen ≈ (sqrt(N) * 2) + 2`

  This is the right choice when many parts of the same process each open the same DSN occasionally — the pool grows gently with the number of openers and no caller has to think about pool sizing.

```go
// Single heavy consumer — caller manages the pool.
db, err := sequel.Open("", dsn)
db.SetMaxOpenConns(32)
db.SetMaxIdleConns(8)

// Multiple consumers sharing a DSN — sequel manages one pool across them.
db, err := sequel.OpenSingleton("", dsn)
```

## Schema Migration

Sequel performs incremental schema migration using numbered SQL files (`1.sql`, `2.sql`, etc.). Migrations are:

- **Concurrency-safe** - Distributed locking ensures only one replica executes each migration
- **Tracked** - A `sequel_migrations` table records completed migrations
- **Driver-aware** - Use `-- DRIVER: drivername` comments for driver-specific SQL (list multiple, space-separated, to share a statement across drivers)

```go
// Embed migration files
//go:embed sql/*.sql
var migrationFS embed.FS

// Run migrations (safe to call from multiple replicas)
err := db.Migrate("unique-sequence-name", migrationFS)
```

Example migration file with driver-specific syntax:

```sql
-- DRIVER: mysql
ALTER TABLE users MODIFY COLUMN email VARCHAR(384) NOT NULL;

-- DRIVER: pgx cockroachdb
ALTER TABLE users ALTER COLUMN email TYPE VARCHAR(384);

-- DRIVER: mssql
ALTER TABLE users ALTER COLUMN email NVARCHAR(384) NOT NULL;

-- DRIVER: sqlite
-- SQLite does not support ALTER COLUMN; a table rebuild would be needed
```

## Cross-Driver Support

Sequel supports MySQL, PostgreSQL, CockroachDB, SQL Server, and SQLite through a unified API. Write your SQL once using MySQL-style `?` placeholders and virtual functions, and Sequel automatically adapts queries for the active driver.

CockroachDB speaks the PostgreSQL wire protocol and shares the `pgx` driver, but it is exposed as a distinct driver name (`cockroachdb`) because callers may need to branch on Cockroach-specific behavior — retry semantics and async schema changes in particular. Internally, every PostgreSQL expansion (placeholders, virtual functions, DSN parsing) applies identically to `cockroachdb`.

### Automatic Placeholder Conversion

All query methods (`Exec`, `Query`, `QueryRow`, `Prepare`, and their `Context` variants) automatically convert `?` placeholders to the driver's native syntax. For PostgreSQL, `?` becomes `$1`, `$2`, etc. For MySQL, SQL Server, and SQLite, `?` is left as-is. Placeholders inside quoted strings are left untouched.

```go
// Works on all drivers - placeholders are converted automatically
rows, err := db.Query("SELECT * FROM users WHERE tenant_id = ? AND active = ?", tenantID, true)
// PostgreSQL receives: SELECT * FROM users WHERE tenant_id = $1 AND active = $2
```

### Virtual Functions

Virtual functions are driver-agnostic function calls in your SQL that Sequel expands into driver-specific expressions before execution. They are matched case-insensitively and support nesting. Quoted strings inside arguments are handled correctly.

#### Built-in Virtual Functions

**`NOW_UTC()`** returns the current UTC timestamp with millisecond precision.

| Driver     | `NOW_UTC()` expands to                       |
|------------|----------------------------------------------|
| MySQL      | `(UTC_TIMESTAMP(3))`                         |
| PostgreSQL | `(NOW() AT TIME ZONE 'UTC')`                 |
| SQL Server | `(CONVERT(DATETIME2(3), SYSUTCDATETIME()))` |
| SQLite     | `STRFTIME('%Y-%m-%d %H:%M:%f', 'now')`       |

On SQL Server the value is rounded to millisecond precision so it matches the other drivers and the precision of a `DATETIME2(3)` column. `SYSUTCDATETIME()` alone is 100-nanosecond precision, which rounds *up* when stored into a millisecond column and can leave a just-written "now" timestamp slightly in the future relative to a later `NOW_UTC()` comparison.

**`REGEXP_TEXT_SEARCH(expr IN col1, col2, ...)`** performs a case-insensitive regular expression search across one or more columns.

| Driver     | `REGEXP_TEXT_SEARCH(? IN name, email)` expands to             |
|------------|---------------------------------------------------------------|
| MySQL      | `CONCAT_WS(' ',name,email) REGEXP ?`                         |
| PostgreSQL | `REGEXP_LIKE(CONCAT_WS(' ',name,email), ?, 'i')`             |
| SQL Server | `REGEXP_LIKE(CONCAT_WS(' ',name,email), ?, 'i')`             |
| SQLite     | `CONCAT_WS(' ',name,email) LIKE '%' \|\| ? \|\| '%'`        |

**`DATE_ADD_MILLIS(baseExpr, milliseconds)`** adds milliseconds to a timestamp expression.

| Driver     | `DATE_ADD_MILLIS(created_at, ?)` expands to                                       |
|------------|-----------------------------------------------------------------------------------|
| MySQL      | `DATE_ADD(created_at, INTERVAL (?) * 1000 MICROSECOND)`                          |
| PostgreSQL | `created_at + MAKE_INTERVAL(secs => (?) / 1000.0)`                              |
| SQL Server | `DATEADD(MILLISECOND, ?, created_at)`                                             |
| SQLite     | `STRFTIME('%Y-%m-%d %H:%M:%f', created_at, '+' \|\| ((?) / 1000.0) \|\| ' seconds')` |

**`DATE_DIFF_MILLIS(a, b)`** returns the difference `(a - b)` in milliseconds.

| Driver     | `DATE_DIFF_MILLIS(updated_at, created_at)` expands to                   |
|------------|--------------------------------------------------------------------------|
| MySQL      | `TIMESTAMPDIFF(MICROSECOND, created_at, updated_at) / 1000.0`          |
| PostgreSQL | `EXTRACT(EPOCH FROM (updated_at - created_at)) * 1000.0`               |
| SQL Server | `DATEDIFF_BIG(MILLISECOND, created_at, updated_at)`                     |
| SQLite     | `(JULIANDAY(updated_at) - JULIANDAY(created_at)) * 86400000.0`         |

**`LIMIT_OFFSET(limit, offset)`** provides cross-driver pagination. Note that SQL Server requires an `ORDER BY` clause.

| Driver     | `LIMIT_OFFSET(10, 0)` expands to                      |
|------------|--------------------------------------------------------|
| MySQL      | `LIMIT 10 OFFSET 0`                                   |
| PostgreSQL | `LIMIT 10 OFFSET 0`                                   |
| SQL Server | `OFFSET 0 ROWS FETCH NEXT 10 ROWS ONLY`               |
| SQLite     | `LIMIT 10 OFFSET 0`                                   |

```go
db.Query("SELECT * FROM users ORDER BY id LIMIT_OFFSET(?, ?)", limit, offset)
```

**`JSON_FIELD(column, '$.path')`** extracts one field from a JSON column and returns it as text.

| Driver     | `JSON_FIELD(doc, '$.name')` expands to                                                                    |
|------------|----------------------------------------------------------------------------------------------------------|
| MySQL      | `(CASE WHEN JSON_TYPE(JSON_EXTRACT(doc, '$.name')) = 'NULL' THEN NULL ELSE JSON_UNQUOTE(JSON_EXTRACT(doc, '$.name')) END)` |
| PostgreSQL | `((doc)::jsonb #>> '{"name"}')`                                                                           |
| SQL Server | `(COALESCE(JSON_QUERY(doc, '$.name'), JSON_VALUE(doc, '$.name')))`                                        |
| SQLite     | `(JSON_EXTRACT(doc, '$.name'))`                                                                           |

The return contract is the same on every driver:

- a JSON string comes back **unquoted**,
- an object or array comes back as its **JSON text**,
- a number or boolean comes back as its **text form**,
- a JSON `null`, or a path that does not exist, comes back as **SQL NULL**.

```go
db.Query("SELECT JSON_FIELD(doc, '$.name') FROM users WHERE JSON_FIELD(doc, '$.address.city') = ?", city)
```

The path supports member access and array indexes (`$.a.b`, `$.tags[0]`). The JSONPath `$` root is **optional** — `'$.name'` and `'name'` are the same path — so a path copied from the MySQL, SQLite or SQL Server documentation works as-is, and you can leave the `$` off when writing one yourself.

The path must be a **literal**, not a `?` placeholder — PostgreSQL needs it as an array of keys and SQL Server needs it split across two functions, so it has to be known before the query is bound. Member names are restricted to `[A-Za-z_][A-Za-z0-9_]*`. The column expression is referenced twice on MySQL and SQL Server, so it must not itself contain a `?`.

> **SQL Server caps scalars at 4000 characters.** `JSON_VALUE` returns `NVARCHAR(4000)` and yields `NULL` for anything longer, so on SQL Server a JSON *string* over 4000 characters reads back as `NULL`. Objects and arrays are unaffected (they go through `JSON_QUERY`, which is `NVARCHAR(MAX)`), as is every other driver. Lifting the cap requires an `OPENJSON ... WITH` rowset, which is a different statement shape than a virtual function can expand into; select the whole column and extract in Go if you need large scalars there.

#### Nesting

Virtual functions can be nested. Inner functions are expanded first across multiple passes:

```go
db.Exec("UPDATE t SET expires_at = DATE_ADD_MILLIS(NOW_UTC(), ?) WHERE id = ?", ttlMs, id)
// MySQL:      UPDATE t SET expires_at = DATE_ADD(UTC_TIMESTAMP(3), INTERVAL (?) * 1000 MICROSECOND) WHERE id = ?
// PostgreSQL: UPDATE t SET expires_at = (NOW() AT TIME ZONE 'UTC') + MAKE_INTERVAL(secs => ($1) / 1000.0) WHERE id = $2
```

#### Custom Virtual Functions

Register your own virtual functions with `RegisterVirtualFunc`:

```go
sequel.RegisterVirtualFunc("BOOL", func(driverName string, args string) (string, error) {
    switch driverName {
    case "mysql", "pgx", "sqlite":
        return args, nil
    case "mssql":
        // SQL Server uses BIT, not BOOL
        return "CAST(" + args + " AS BIT)", nil
    default:
        return "", errors.New("unsupported driver: %s", driverName)
    }
})
```

#### UnpackQuery

`UnpackQuery` is the public method that expands virtual functions and conforms placeholders. It is called automatically by the query shadow methods, but can be used directly if needed:

```go
expanded, err := db.UnpackQuery("SELECT * FROM t WHERE updated_at > DATE_ADD_MILLIS(NOW_UTC(), ?) AND active = ?")
```

### InsertReturnID

`InsertReturnID` executes an INSERT statement and returns the auto-generated ID for the named ID column. Each driver uses its native mechanism:

| Driver     | Mechanism                                         |
|------------|---------------------------------------------------|
| MySQL      | `LastInsertId()` from the result                  |
| PostgreSQL | Appends `RETURNING <idColumn>` to the query       |
| SQL Server | Injects `OUTPUT INSERTED.<idColumn>` before `VALUES` |
| SQLite     | `LastInsertId()` from the result                  |

```go
id, err := db.InsertReturnID(ctx, "id", "INSERT INTO users (name, email) VALUES (?, ?)", name, email)
```

The ID column must be a plain identifier matching `[A-Za-z_][A-Za-z0-9_]*`. Because it is spliced into the statement on PostgreSQL, CockroachDB, and SQL Server, quoted or exotic column names are rejected up front — on every driver, so the contract is uniform.

### DriverName()

`DriverName()` returns the active driver name (`"mysql"`, `"pgx"`, `"mssql"`, or `"sqlite"`) for cases where you need driver-specific logic in Go code.

## Transactions

`db.BeginTx` returns a `sequel.Tx` that shadows `sql.Tx` with virtual-function expansion and placeholder conforming — use it exactly like `sql.Tx`.

For transactions that must survive contention, `db.Transact` runs a closure in a transaction, commits on success, and **retries the whole closure on a deadlock or lock-contention error** with a short jittered backoff:

```go
err := db.Transact(ctx, func(tx *sequel.Tx) error {
    if _, err := tx.ExecContext(ctx, "UPDATE accounts SET balance = balance - ? WHERE id = ?", amt, from); err != nil {
        return err
    }
    _, err := tx.ExecContext(ctx, "UPDATE accounts SET balance = balance + ? WHERE id = ?", amt, to)
    return err
})
```

- **Retry-safe by re-running.** A retried attempt re-executes the closure from the start in a new transaction (the previous attempt is rolled back), so the closure must be safe to run more than once — any non-transactional side effects it performs may repeat. Because retries re-run the Go code rather than replay recorded statements, a transaction whose control flow depends on data committed by another transaction between attempts stays correct.
- **No partial commits.** The `Tx` passed to the closure records the first error and short-circuits every statement after it, so the transaction never commits half its work even if the closure forgets to check an error. This covers every way an error surfaces: a failed `Exec`/`Query`/`InsertReturnID` statement, an error while iterating a result set (a `rows.Scan` failure or a streaming error from `rows.Err()`), a failed `QueryRow(...).Scan` — with `sql.ErrNoRows` exempt, since a missing row is normal control flow rather than a failure — and executions of a prepared statement, whether prepared inside the transaction (`tx.Prepare`) or bound to it (`tx.Stmt`).
- **SQL Server `XACT_ABORT ON`.** Applied automatically inside `Transact` so any statement error aborts the whole transaction server-side.

A `Tx` from `BeginTx` does neither error-recording nor retry — it behaves exactly like `sql.Tx`.

## Ephemeral Test Databases

Provisioning a per-test database is a separate step from opening a connection. `CreateTestingDatabase(driver, baseDSN, uniqueTestID)` creates (or reuses) a uniquely-named database and returns its DSN; pass that DSN to `Open` or `OpenSingleton` to connect.

```go
// Test fixture
func TestUserService(t *testing.T) {
    dsn, err := sequel.CreateTestingDatabase("", "root:root@tcp(127.0.0.1:3306)/mydb", t.Name())
    if err != nil { t.Fatal(err) }
    db, err := sequel.OpenSingleton("", dsn)
    if err != nil { t.Fatal(err) }
    defer db.Close()  // also drops the testing database
}
```

The same helper can be invoked from production startup paths that want to swap in a per-test database without rewriting the rest of the wiring:

```go
func startup(cfg Config) (*sequel.DB, error) {
    dsn := cfg.DSN
    if cfg.Testing {
        var err error
        dsn, err = sequel.CreateTestingDatabase("", cfg.DSN, cfg.TestID)
        if err != nil { return nil, err }
    }
    return sequel.OpenSingleton("", dsn)
}
```

Repeated calls within the same process with the same `(driver, baseDSN, uniqueTestID)` reuse the same testing database — the `DROP+CREATE` runs only once, for as long as a handle on it remains open. The returned DSN points at a database whose name has the `testing_NN_` prefix; sequel inspects this on `Close` and drops the database automatically when the last referencing `*DB` is closed — last across every handle, since `Open` returns a distinct `*DB` per call. There is no separate cleanup call to remember. A call after that point provisions the database again rather than returning a DSN for one that no longer exists. If a process exits before `Close` runs, the leftover-cleanup sweep on the next `CreateTestingDatabase` call removes stale databases older than 1–2 hours.

### Choosing the server with `SEQUEL_TESTING_DSN`

When `CreateTestingDatabase` is called with **neither** a driver nor a base DSN, it falls back to the `SEQUEL_TESTING_DSN` environment variable. This lets you run the same test suite against any supported server without touching test code — leave the variable unset to use in-memory SQLite (the default, no server required), or set it to a base DSN to run against that server instead, with the driver inferred from the DSN:

```go
func TestUserService(t *testing.T) {
    // "" driver + "" DSN → SEQUEL_TESTING_DSN, or in-memory SQLite if it is unset.
    dsn, err := sequel.CreateTestingDatabase("", "", t.Name())
    if err != nil { t.Fatal(err) }
    db, err := sequel.OpenSingleton("", dsn)
    if err != nil { t.Fatal(err) }
    defer db.Close()
}
```

```sh
# Same tests, different engine — no code change.
go test ./...                                                          # SQLite (default)
SEQUEL_TESTING_DSN='postgres://user:pw@127.0.0.1:5432/' go test ./...  # PostgreSQL
SEQUEL_TESTING_DSN='root:pw@tcp(127.0.0.1:3306)/'       go test ./...  # MySQL
```

Passing an explicit driver — even with an empty DSN, which just selects that driver's localhost default — opts out of the fallback, so a test that deliberately targets a specific engine keeps using it regardless of the environment. Because the variable is read inside `CreateTestingDatabase`, any project that provisions its test databases through sequel inherits this behavior with no additional wiring.

### Simulating network latency with `SimulateRTT`

Tests run against in-memory SQLite or a server on localhost, where a round trip costs microseconds. Code that is needlessly chatty — a loop that issues one query per element, a transaction that could have batched — therefore performs indistinguishably from code that is not, and the timeout paths never fire. `SimulateRTT` makes every operation sequel sends over the wire pause first, so the cost of a real network shows up in the test:

```go
db, _ := sequel.Open("", dsn)
db.SimulateRTT(20 * time.Millisecond) // testing only — this is deliberate latency injection

start := time.Now()
err := db.Transact(ctx, func(tx *sequel.Tx) error {
    for _, u := range users {
        if _, err := tx.ExecContext(ctx, "INSERT INTO users (name) VALUES (?)", u.Name); err != nil {
            return err
        }
    }
    return nil
})
// 100 users → 102 round trips (BEGIN + 100 statements + COMMIT) → over 2 seconds.
// The same loop takes microseconds against localhost, which is what hides the problem.

db.SimulateRTT(0) // off again
```

The delay is charged **per round trip**, not per call: the statement methods on `DB` and `Tx`, each execution of a prepared `Stmt`, `Begin`/`BeginTx`, `Commit`, `Rollback` and `Ping` each pay it once. Operations sequel is not in the path for are not delayed — a `*sql.Conn` talks to the driver directly, and fetching successive rows from an open `Rows` is batched by the driver, so charging a full round trip per `Next()` would model the wire worse than charging nothing.

The `Context` variants honor their context: a deadline shorter than the simulated latency fails the operation with the context's error and never reaches the database, which is what a real round trip that outlives its deadline does. That makes cancellation and timeout handling testable without an unreliable server to provoke it.

A `Tx` captures the setting when the transaction begins, so a transaction runs at one consistent latency even if the setting changes underneath it. The default is zero (off), and a negative duration is treated as zero. For a `*DB` shared by `OpenSingleton` the setting is process-wide for that pool, so set it from the owning caller.

## Observability

Sequel emits OpenTelemetry traces and metrics, and `slog` logs. A freshly opened `*DB` is **not** uninstrumented: it starts on the process-wide `otel.GetTracerProvider()` / `otel.GetMeterProvider()`, so a program that configures OpenTelemetry globally gets sequel's spans and metrics with no further setup. Those globals are delegating no-ops until real providers are installed, and they start working the moment they are — no re-open needed. Logging defaults to a discard logger. To genuinely disable a signal, install an explicit no-op provider; "unset" does not mean "off".

Providers are attached **after** `Open`/`OpenSingleton` (which keep the standard `database/sql` signature) rather than at construction. Nothing is lost by this: `sql.Open` does no I/O — it only prepares a lazy pool — so there is no work inside `Open` worth instrumenting; every operation that does real work happens later on the returned `*DB`.

```go
db, _ := sequel.Open("", dsn)
db.SetTracerProvider(tracerProvider) // trace.TracerProvider — client spans per query/transaction/migration
db.SetMeterProvider(meterProvider)   // metric.MeterProvider — sequel_* metrics
db.SetLogger(logger)                 // *slog.Logger — migration events; per-query when enabled at Debug
```

Configure once, before the `*DB` is used concurrently. For an `OpenSingleton`-shared `*DB`, the providers are process-wide for that pool; set them from the owning caller (last writer wins). Pass `nil` to any setter to disable that signal.

### Spans

Each query, `Transact`, and `Migrate` gets a client span following OpenTelemetry database semantic conventions:

- `db.system.name` — the driver (`mysql`, `pgx`, `cockroachdb`, `mssql`, `sqlite`)
- `db.operation.name` — the SQL verb (`SELECT`, `INSERT`, …), whatever the dialect: common verbs are reported immediately and rarer ones are picked up on first use, so nothing needs to be on a list. The number of distinct verbs a process reports is capped (at 128) and only verb-shaped tokens count toward it, so the attribute stays low-cardinality even if an application interpolates uncontrolled input into its SQL; past the cap, further unrecognized verbs report as `OTHER` and sequel logs that once at Info
- `db.collection.name` — the table, **only when it can be determined unambiguously** (omitted for joins, multi-table `FROM` lists, and subqueries, so a present value is trustworthy)

The span name is `"{operation} {table}"` (e.g. `SELECT users`), or just the operation when no table is captured. The statement text is never attached to a span; operation, table, and the caller's own parent span identify it, and the full parameterized statement is available in the per-query Debug log instead.

`BEGIN`, `COMMIT` and `ROLLBACK` are round trips too, so each gets its own span, nested under the transaction it belongs to. This matters beyond bookkeeping: a serialization failure surfaces at commit time rather than at a statement on CockroachDB and on PostgreSQL under `SERIALIZABLE`, so a commit span is what puts that failure into `sequel_query_duration` and `sequel_lock_contention`.

A call on an already-finalized transaction reports `sql.ErrTxDone` without reaching the database, and correspondingly emits **nothing** — no span, no duration sample. That covers the ubiquitous `defer tx.Rollback()` next to a successful `Commit`, and equally the transaction that `database/sql` finalized itself when its context was cancelled, so a cancelled request does not show up as a failed rollback.

### Metrics

All metric names carry the `sequel_` prefix. Counter instrument names carry **no** `_total` suffix; a
Prometheus exporter appends it at the scrape boundary, so `sequel_lock_contention` is queried in PromQL as
`sequel_lock_contention_total` (and `sequel_migration_runs` as `sequel_migration_runs_total`):

| Metric | Type | Notes |
|--------|------|-------|
| `sequel_query_duration` | histogram (s) | attrs: `db.system.name`, `db.operation.name`, `status` (ok/error) |
| `sequel_transaction_duration` | histogram (s) | attrs: `db.system.name`, `outcome` (committed/rolledback) |
| `sequel_lock_contention` | counter | incremented once per surfaced lock-contention/deadlock error (PromQL: `sequel_lock_contention_total`) |
| `sequel_migration_runs` | counter | counts migrations that actually ran (skipped ones excluded); attrs include `status` (PromQL: `sequel_migration_runs_total`) |
| `sequel_pool_open_connections` | gauge | from `sql.DBStats`, attr `database` (never the raw DSN) |
| `sequel_pool_in_use_connections` | gauge | |
| `sequel_pool_idle_connections` | gauge | |
| `sequel_pool_wait_count` | gauge | cumulative total |
| `sequel_pool_wait_duration_seconds` | gauge | cumulative total |

The two `wait` gauges come straight from `sql.DBStats` and only ever increase, so query them with `rate()`
or `increase()` rather than reading the raw value.

### Logs

The library **does not log operation errors** — every error is returned to the caller, who is best placed to log it. Logging is reserved for:

- **Info** — one-off events: each schema migration as it is attempted (regardless of outcome).
- **Debug** — every query, including the full parameterized statement text. There is no separate sequel switch: the lines are gated on your own logger's level, so they cost nothing when Debug is disabled. (Statement text is never a privacy risk — sequel always parameterizes, so the text carries `?`/`$1` placeholders, never argument values.)

### `Query`, `QueryRow` and `Prepare` return `*sequel.Rows` / `*sequel.Row` / `*sequel.Stmt`

Query methods return sequel's own types rather than `database/sql`'s: `Query`/`QueryContext` return a `*sequel.Rows` (embedding `*sql.Rows`), `QueryRow`/`QueryRowContext` return a `*sequel.Row` (embedding `*sql.Row`), and `Prepare`/`PrepareContext` return a `*sequel.Stmt` (embedding `*sql.Stmt`). All embed, so ordinary call sites are unchanged:

```go
rows, err := db.Query("SELECT id, name FROM users")   // type inference — no change needed
for rows.Next() { rows.Scan(&id, &name) }
err = db.QueryRow("SELECT name FROM users WHERE id=?", id).Scan(&name)
stmt, err := db.Prepare("INSERT INTO users (name) VALUES (?)")
_, err = stmt.Exec("Rivka")
```

Only code that *explicitly* types a result as `*sql.Rows` / `*sql.Row` / `*sql.Stmt`, or that implements the `Executor` interface itself, needs adjustment.

These types exist for two reasons. **Instrumentation:** `database/sql` defers a `QueryRow` error to `Scan` and a streaming error to `rows.Err()`, so the shadows capture the error where it actually becomes available; executions of a prepared `Stmt` get spans, duration samples and the simulated round-trip delay like any other statement. **Transaction safety:** inside a `Transact` closure they latch errors into the transaction, so a closure that ignores a failed scan, a truncated read, or a failed prepared-statement execution cannot commit state built on it — see [Transactions](#transactions). Outside `Transact` — a `*DB` query, or a `Tx` from `BeginTx` — they are passthroughs with no latching.

## Errors

Errors returned by sequel wrap the driver's error with a stack trace (via [`github.com/microbus-io/errors`](https://github.com/microbus-io/errors)), recording where in your code the failure surfaced. The wrapping preserves `Unwrap`, so `errors.Is` and `errors.As` see through it — compare and unwrap exactly as you would any wrapped Go error:

```go
if errors.Is(err, context.DeadlineExceeded) { ... }
var pgErr *pgconn.PgError
if errors.As(err, &pgErr) { ... }
```

The two `database/sql` sentinels are deliberately **not** wrapped: `sql.ErrNoRows` and `sql.ErrTxDone` are routine control flow rather than failures, and they pass through exactly as `database/sql` returns them, so existing `err == sql.ErrNoRows` comparisons keep working. `errors.Is(err, sql.ErrNoRows)` works too, and is the more robust habit.

Every other error — a driver error, a context cancellation — is wrapped, so never compare those with `==`; use `errors.Is`/`errors.As`. (Code that compares a driver error with `==` is broken with plain `database/sql` as well — drivers return distinct error values per occurrence — so in practice this asks nothing new.)

## Legal

Sequel is the copyrighted work of various contributors. It is licensed to you free of charge by Microbus LLC - a Delaware limited liability company formed to hold rights to the combined intellectual property of all contributors - under the [Apache License 2.0](http://www.apache.org/licenses/LICENSE-2.0).
