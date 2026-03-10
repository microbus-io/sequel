# Sequel

A Go library that enhances `sql.DB` for building SQL-backed CRUD microservices with the [Microbus](https://microbus.io) framework.

## Features at a Glance

- **Connection pool management** - Prevents database exhaustion in multi-microservice solutions
- **Schema migration** - Concurrency-safe, incremental database migrations
- **Cross-driver support** - MySQL, PostgreSQL, and SQL Server with unified API
- **Ephemeral test databases** - Isolated databases per test with automatic cleanup

## Quick Start

```go
import "github.com/microbus-io/sequel"

// Open a database connection
db, err := sequel.Open("", "root:root@tcp(127.0.0.1:3306)/mydb")

// Run migrations
err = db.Migrate("myservice@v1", migrationFilesFS)

// Use db.DB for standard sql.DB operations
rows, err := db.Query("SELECT * FROM users WHERE tenant_id=?", tenantID)
```

## Connection Pool Management

When many microservices connect to the same database, connection exhaustion becomes a concern. Sequel limits the connection pool of a single executable based on client count using a sqrt-based formula:

- `maxIdle ≈ sqrt(N)` where N is the number of clients
- `maxOpen ≈ (sqrt(N) * 2) + 2`

This prevents overwhelming the database while maintaining reasonable throughput.

## Schema Migration

Sequel performs incremental schema migration using numbered SQL files (`1.sql`, `2.sql`, etc.). Migrations are:

- **Concurrency-safe** - Distributed locking ensures only one replica executes each migration
- **Tracked** - A `sequel_migrations` table records completed migrations
- **Driver-aware** - Use `-- DRIVER: drivername` comments for driver-specific SQL

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

-- DRIVER: pgx
ALTER TABLE users ALTER COLUMN email TYPE VARCHAR(384);

-- DRIVER: mssql
ALTER TABLE users ALTER COLUMN email NVARCHAR(384) NOT NULL;
```

## Cross-Driver Support

Sequel supports MySQL, PostgreSQL, and SQL Server through a unified API. Write your SQL once using MySQL-style `?` placeholders and virtual functions, and Sequel automatically adapts queries for the active driver.

### Automatic Placeholder Conversion

All query methods (`Exec`, `Query`, `QueryRow`, `Prepare`, and their `Context` variants) automatically convert `?` placeholders to the driver's native syntax. For PostgreSQL, `?` becomes `$1`, `$2`, etc. For MySQL and SQL Server, `?` is left as-is. Placeholders inside quoted strings are left untouched.

```go
// Works on all drivers - placeholders are converted automatically
rows, err := db.Query("SELECT * FROM users WHERE tenant_id = ? AND active = ?", tenantID, true)
// PostgreSQL receives: SELECT * FROM users WHERE tenant_id = $1 AND active = $2
```

### Virtual Functions

Virtual functions are driver-agnostic function calls in your SQL that Sequel expands into driver-specific expressions before execution. They are matched case-insensitively and support nesting. Quoted strings inside arguments are handled correctly.

#### Built-in Virtual Functions

**`NOW_UTC()`** returns the current UTC timestamp with millisecond precision.

| Driver     | `NOW_UTC()` expands to        |
|------------|-------------------------------|
| MySQL      | `UTC_TIMESTAMP(3)`            |
| PostgreSQL | `(NOW() AT TIME ZONE 'UTC')`  |
| SQL Server | `SYSUTCDATETIME()`            |

**`REGEXP_TEXT_SEARCH(expr IN col1, col2, ...)`** performs a case-insensitive regular expression search across one or more columns.

| Driver     | `REGEXP_TEXT_SEARCH(? IN name, email)` expands to           |
|------------|-------------------------------------------------------------|
| MySQL      | `CONCAT_WS(' ',name,email) REGEXP ?`                       |
| PostgreSQL | `REGEXP_LIKE(CONCAT_WS(' ',name,email), ?, 'i')`           |
| SQL Server | `REGEXP_LIKE(CONCAT_WS(' ',name,email), ?, 'i')`           |

**`DATE_ADD_MILLIS(baseExpr, milliseconds)`** adds milliseconds to a timestamp expression.

| Driver     | `DATE_ADD_MILLIS(created_at, ?)` expands to                        |
|------------|--------------------------------------------------------------------|
| MySQL      | `DATE_ADD(created_at, INTERVAL (?) * 1000 MICROSECOND)`           |
| PostgreSQL | `created_at + MAKE_INTERVAL(secs => (?) / 1000.0)`               |
| SQL Server | `DATEADD(MILLISECOND, ?, created_at)`                              |

**`DATE_DIFF_MILLIS(a, b)`** returns the difference `(a - b)` in milliseconds.

| Driver     | `DATE_DIFF_MILLIS(updated_at, created_at)` expands to                   |
|------------|--------------------------------------------------------------------------|
| MySQL      | `TIMESTAMPDIFF(MICROSECOND, created_at, updated_at) / 1000.0`          |
| PostgreSQL | `EXTRACT(EPOCH FROM (updated_at - created_at)) * 1000.0`               |
| SQL Server | `DATEDIFF_BIG(MILLISECOND, created_at, updated_at)`                     |

**`LIMIT_OFFSET(limit, offset)`** provides cross-driver pagination. Note that SQL Server requires an `ORDER BY` clause.

| Driver     | `LIMIT_OFFSET(10, 0)` expands to                      |
|------------|--------------------------------------------------------|
| MySQL      | `LIMIT 10 OFFSET 0`                                   |
| PostgreSQL | `LIMIT 10 OFFSET 0`                                   |
| SQL Server | `OFFSET 0 ROWS FETCH NEXT 10 ROWS ONLY`               |

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
    case "mysql", "pgx":
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

### DriverName()

`DriverName()` returns the active driver name (`"mysql"`, `"pgx"`, or `"mssql"`) for cases where you need driver-specific logic in Go code.

## Ephemeral Test Databases

`OpenTesting` creates unique databases per test, providing isolation from other tests:

```go
func TestUserService(t *testing.T) {
    // Creates database: testing_{hour}_mydb_{testID}
    db, err := sequel.OpenTesting("", "root:root@tcp(127.0.0.1:3306)/mydb", t.Name())
    // Database is deleted when closed
    db.Close()
}
```

## Legal

Sequel is the copyrighted work of various contributors. It is licensed to you free of charge by Microbus LLC - a Delaware limited liability company formed to hold rights to the combined intellectual property of all contributors - under the [Apache License 2.0](http://www.apache.org/licenses/LICENSE-2.0).
