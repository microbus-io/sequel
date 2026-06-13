# Sequel

A Go library that enhances `database/sql` with cross-driver SQL, schema migration, ephemeral test databases, and adaptive connection pooling.

## Features at a Glance

- **Connection pool management** - Prevents database exhaustion when many consumers in one process share a DSN
- **Schema migration** - Concurrency-safe, incremental database migrations
- **Cross-driver support** - MySQL, PostgreSQL, CockroachDB, SQL Server, and SQLite with unified API
- **Retrying transactions** - `Transact` runs a closure in a transaction, retries on deadlock/lock contention, and never commits partial work
- **Ephemeral test databases** - Isolated databases per test with automatic cleanup

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
- **No partial commits.** The `Tx` passed to the closure records the first statement error and short-circuits the rest, so the transaction never commits half its work even if the closure forgets to check a statement's error.
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

Repeated calls within the same process with the same `(driver, baseDSN, uniqueTestID)` reuse the same testing database — the `DROP+CREATE` runs only once. The returned DSN points at a database whose name has the `testing_NN_` prefix; sequel inspects this on `Close` and drops the database automatically when the last referencing `*DB` is closed. There is no separate cleanup call to remember. If a process exits before `Close` runs, the leftover-cleanup sweep on the next `CreateTestingDatabase` call removes stale databases older than 1–2 hours.

## Legal

Sequel is the copyrighted work of various contributors. It is licensed to you free of charge by Microbus LLC - a Delaware limited liability company formed to hold rights to the combined intellectual property of all contributors - under the [Apache License 2.0](http://www.apache.org/licenses/LICENSE-2.0).
