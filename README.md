# Sequel

A Go library that enhances `sql.DB` for building SQL-backed CRUD microservices with the [Microbus](https://microbus.io) framework.

## Features at a Glance

- **Connection pool management** - Prevents database exhaustion in multi-microservice solutions
- **Schema migration** - Concurrency-safe, incremental database migrations
- **Ephemeral test databases** - Isolated databases per test with automatic cleanup
- **Cross-driver support** - MySQL, PostgreSQL, and SQL Server with unified API

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
