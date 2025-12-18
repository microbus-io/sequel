# Sequel

A Go library that enhances `sql.DB` for building SQL-backed CRUD microservices with the [Microbus](https://microbus.io) framework.

## Features at a Glance

- **Connection Pool Management** - Prevents database exhaustion in multi-microservice solutions
- **Schema Migration** - Concurrency-safe, incremental database migrations
- **Ephemeral Test Databases** - Isolated databases per test with automatic cleanup
- **Code Generation** - Generate complete CRUD microservices from minimal configuration
- **Multi-Tenant Architecture** - Built-in tenant isolation via discriminator columns
- **AI Agent Integration** - Rules and skills for Claude Code and other coding agents
- **Cross-Driver Support** - MySQL, PostgreSQL, and SQL Server with unified API

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

Configure database credentials in `config.local.yaml` at the project root:

```yaml
all:
  SQLDataSourceName: root:root@tcp(127.0.0.1:3306)/mydb
```

## Code Generation

Sequel's code generator produces complete CRUD microservices from minimal configuration.

### Setup

Add the generator directive to your project's `doc.go`:

```go
//go:generate go run github.com/microbus-io/fabric/codegen
//go:generate go run github.com/microbus-io/sequel/codegen
```

Run `go generate` at the root of the project.

### Creating a CRUD Microservice

1. Create a microservice directory with `doc.go`:
```go
//go:generate go run github.com/microbus-io/fabric/codegen
//go:generate go run github.com/microbus-io/sequel/codegen
```

2. Run `go generate` - creates `service.yaml` with SQL configuration:
```yaml
sql:
  table: book      # snake_case database table
  object: Book     # PascalCase Go type
```

3. Run `go generate` again - generates complete implementation:
   - `bookapi/object.go` - Domain object with validation
   - `bookapi/objectkey.go` - Encrypted key handling
   - `bookapi/query.go` - Query filtering options
   - `resources/sql/1.sql` - Initial schema migration
   - `service.go` - CRUD endpoint implementations
   - `service_test.go` - Integration tests

### Generated CRUD Endpoints

| Endpoint | Description |
|----------|-------------|
| `Create` / `BulkCreate` | Insert new objects |
| `Store` / `BulkStore` | Update existing objects |
| `Revise` / `BulkRevise` | Update with optimistic locking |
| `Delete` / `BulkDelete` | Delete objects |
| `Load` / `BulkLoad` | Fetch by key |
| `List` | Query with filtering and pagination |
| `Lookup` / `MustLookup` | Single object queries |
| `Purge` | Batch delete by query |
| `Count` | Count matching objects |

### Customization

After generation, customize your microservice:

1. **Add fields** to `object.go` and corresponding columns in a new migration script
2. **Implement column mappings** - Follow the `HINT` comments in `service.go`:
   - `mapColumnsOnInsert()` - Maps fields for INSERT
   - `mapColumnsOnUpdate()` - Maps fields for UPDATE
   - `mapColumnsOnSelect()` - Maps columns to object fields on SELECT
   - `prepareWhereClauses()` - Builds WHERE clauses from Query
3. **Add query filters** to `query.go` for custom search capabilities
4. **Enhance tests** in `service_test.go`

## Multi-Tenant Architecture

Generated microservices include built-in multi-tenant isolation:

- Every table includes a `tenant_id` discriminator column
- All SQL statements (INSERT, UPDATE, DELETE, SELECT) include tenant filtering
- Composite primary keys start with `tenant_id` for data locality
- All indexes are prefixed with `tenant_id`

The tenant ID is extracted from the actor's JWT claims (`tenant` or `tid`). Solutions without multi-tenancy can ignore this; the tenant defaults to `0`.

## Null Value Handling

Sequel provides utilities for working with nullable columns:

```go
// Writing: Convert zero values to NULL
columnMapping := map[string]any{
    "nickname": sequel.Nullify(user.Nickname),  // "" becomes NULL
}

// Reading: Convert NULL to zero values
columnMapping := map[string]any{
    "nickname": sequel.Nullable(&user.Nickname),  // NULL becomes ""
}

// Custom binding for complex transformations
columnMapping := map[string]any{
    "tags": sequel.Bind(func(jsonStr string) error {
        return json.Unmarshal([]byte(jsonStr), &obj.Tags)
    }),
}
```

## AI Agent Integration

Sequel includes rules and skills for AI coding agents like Claude Code.

### Skills for CRUD Microservices

| Skill | Description |
|-------|-------------|
| `sequel/add-microservice` | Create a new CRUD microservice |
| `sequel/add-fields` | Add columns to object and schema |
| `sequel/chg-fields` | Modify existing field definitions |
| `sequel/rm-fields` | Remove fields from object |
| `sequel/rename-object` | Rename object and update references |
| `sequel/rename-table` | Rename database table |

### Sample Prompts

Create a new microservice:
```
Create a new microservice to persist books in a SQL database
```

Add fields:
```
For the @book/ microservice, add the following fields: Title, Author, ISBN (unique)
```

Skip tests and documentation:
```
Create a new microservice to persist car in a SQL database. Be quick about it!
```

## Microbus Integration Example

```go
import "github.com/microbus-io/sequel"

type Service struct {
    *intermediate.Intermediate
    db *sequel.DB
}

func (svc *Service) OnStartup(ctx context.Context) (err error) {
    if svc.Deployment() == connector.TESTING {
        svc.db, err = sequel.OpenTesting("", svc.SQLDataSourceName(), svc.Plane())
    } else {
        svc.db, err = sequel.Open("", svc.SQLDataSourceName())
    }
    if err != nil {
        return errors.Trace(err)
    }

    sqlFS, _ := fs.Sub(svc.ResFS(), "sql")
    err = svc.db.Migrate("myservice@v1", sqlFS)
    return errors.Trace(err)
}

func (svc *Service) OnShutdown(ctx context.Context) (err error) {
    if svc.db != nil {
        svc.db.Close()
    }
    return nil
}
```

## Supported Drivers

| Database | Driver | Data Source Name |
|----------|--------|------------------|
| MySQL | `mysql` | `root:root@tcp(127.0.0.1:3306)/db` |
| PostgreSQL | `pgx` | `postgres://postgres:postgres@127.0.0.1:5432/db` |
| SQL Server | `mssql` | `sqlserver://sa:sa@127.0.0.1:1433?database=db` |

The driver is automatically inferred from the data source name format.

## Cross-Driver Utilities

```go
// Convert ? placeholders to $1, $2 for PostgreSQL
stmt := db.ConformArgPlaceholders("SELECT * FROM users WHERE id=? AND name=?")

// Get driver-specific current UTC time function
stmt := fmt.Sprintf("UPDATE users SET updated_at=%s WHERE id=?", db.NowUTC())

// Generate cross-driver REGEXP search
stmt := db.RegexpTextSearch("col1", "col2", "col3")
```

## Legal

Sequel is the copyrighted work of various contributors. It is licensed to you free of charge by Microbus LLC - a Delaware limited liability company formed to hold rights to the combined intellectual property of all contributors - under the [Apache License 2.0](http://www.apache.org/licenses/LICENSE-2.0).
