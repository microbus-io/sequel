# Developing SQL CRUD Microservices with the Microbus Framework

## Instructions for Agents

**CRITICAL**: These instructions pertain to SQL CRUD microservices only. Do not apply them to other flavors of microservices.

## Overview

SQL CRUD microservices are Microbus microservices that expose a CRUD API to persist and retrieve objects in and out of a SQL database.

## Common Patterns

### Migration Scripts

SQL CRUD microservices use migration scripts to prepare the schema of the database table into which the object is stored. The migrations scripts are executed sequentially to enable evolution of the schema over time. A migration script will only executes once on a given database instance. The `resources/sql` directory of the microservice contains migrations scripts that are named with a numeric file name such as `1.sql`, `2.sql` etc. that represents their execution order.

**IMPORTANT**: Create a new migration script whenever you are tasked with making changes to the schema. Do not modify existing scripts unless explicitly told to do so.

To add a migration script, identify the file name with the largest value and create a new `.sql` file with that value incremented by one. For example, if the largest file name is `14.sql` name the new file `15.sql`.

Use SQL statements such as `CREATE TABLE` and `ALTER TABLE` to create or alter the schema of the database table. Separate statements with a `;` followed by a new line. Use snake_case for all database identifiers, including column names, index names and table names. Use UPPERCASE for SQL keywords.

It is often necessary to enter different SQL statements for each of the supported database driver names because often they use a different syntax. The prefix `-- DRIVER: drivername` before a statement indicates to run that statement only on the named database: `mysql` (MySQL), `pgx` (Postgres) or `mssql` (SQL Server).

```sql
-- DRIVER: mysql
ALTER TABLE my_table MODIFY COLUMN modified_column VARCHAR(384) NOT NULL DEFAULT '';

-- DRIVER: pgx
ALTER TABLE my_table ALTER COLUMN modified_column VARCHAR(384) NOT NULL DEFAULT '';

-- DRIVER: mssql
ALTER TABLE my_table ALTER COLUMN modified_column NVARCHAR(384) NOT NULL DEFAULT '';
```

### Multi-Tenant Architecture

SQL CRUD microservices use a tenant discriminator column `tenant_id` to guarantee isolation between tenants.
- `tenant_id` is included in every SQL statement: `INSERT`, `UPDATE`, `DELETE` or `SELECT`
- `tenant_id` is used as the first column in the primary (or clustering) key to contain table fragmentation on a per-tenant basis
- `tenant_id` is used as the first column in all composite indices to contain index fragmentation on a per-tenant basis

By default, the tenant ID is obtained from the actor claim `tenant` or `tid` in the `ctx` by the `Frame` in `tenantOf`. Universal tables that are shared among all tenants should explicitly return a tenant ID of `0` from `tenantOf`.

Solutions that are not multi-tenant where there isn't expected to be a `tenant` or `tid` claim, can safely ignore the tenant concept. The tenant discriminator column will default to `0`.

The tenant discriminator column is an integer.

### Encapsulation of Persistence Layer

Microservices encapsulate their persistence layer and expose functionality only via their public API. To avoid tight coupling, no microservice should refer to another microservice's persistence layer nor make any assumptions based on its internals. This means that a microservice should not define foreign key constraints, nor `JOIN` queries, that refer to the database table of another microservice.

### Object Definition

The definition of the object's struct is in `object.go` in the API directory of the microservice. The object must be marshallable to and from JSON and should therefore contain only fields that are themselves marshallable. JSON tags should use camelCase.

### Object Key Definition

The object's key is defined in `objectkey.go` in the API directory of the microservice. Internally, the key is represented an a numerical ID.

By default, the key is encrypted when marshalled to JSON and decrypted when unmarshalled so as not to expose the table's cardinality to external users. Set `cipherEnabled` to `false` to disable the encryption. Do not alter the cipher key nor its nonce.

### Column Mappings

Column mapping bridge the divide between database columns and Go object fields. Column mapping happens in four case: on create, on store, on read and on query.

`mapColumnsOnInsert` maps column names to their values during the initial `Create` action.
- All `NOT NULL` columns that do not have a `DEFAULT` value define in the database schema must be mapped to a value
- Values typically come from the corresponding field of the input `obj` but can be sources elsewhere
- Wrap a value in `sequel.Nullify` if the database column is `NULL`-able
- Wrap a string in `sequel.UnsafeSQL` to set the value using a SQL statement
- Use `sequel.UnsafeSQL(db.NowUTC())` to set the value to the database's current time in UTC
- When setting a value of `time.Time`, convert it to UTC first
- Exclude columns that the actor is not allowed to set on creation

```go
columnMapping = map[string]any{
    "first_name": obj.FirstName,
    "last_name":  obj.LastName,
    "time_zone":  sequel.Nullify(obj.TimeZone),
    "created_at": sequel.UnsafeSQL(db.NowUTC()),
    "updated_at": sequel.UnsafeSQL(db.NowUTC()),
}
```

`mapColumnsOnUpdate` maps column names to their values during followup `Store` actions.
- Only modifiable columns need be mapped to a value
- Values typically come from the corresponding field of the input `obj` but can be sources elsewhere
- Wrap a value in `sequel.Nullify` if the database column is `NULL`-able
- Wrap a string in `sequel.UnsafeSQL` to set the value using a SQL statement
- Use `sequel.UnsafeSQL(db.NowUTC())` to set the value to the database's current time in UTC
- When setting a value of `time.Time`, convert it to UTC first.
- Exclude columns that the actor is not allowed to modify

```go
columnMapping = map[string]any{
    "first_name": obj.FirstName,
    "last_name":  obj.LastName,
    "time_zone":  sequel.Nullify(obj.TimeZone),
    "updated_at": sequel.UnsafeSQL(db.NowUTC()),
}
```

`mapColumnsOnSelect` maps column names to their object fields during `List` actions.
- Wrap the object field reference in `sequel.Nullable` if the database column is `NULL`-able but the Go type of the field is not
- Use `sequel.Bind` to transform and apply the value manually to the object.
- Exclude columns that the actor is not allowed to read

```go
columnMapping = map[string]any{
    "id": &obj.ID,
    "tags": sequel.Bind(func(tags string) {
        return json.Unmarshal([]byte(tags), &obj.Tags)
    }),
    "birthday": sequel.Bind(func(modifiedTime time.Time) {
        obj.Year, obj.Month, obj.Day = modifiedTime.Date()
        return nil
    }),
}
```

`prepareWhereClauses` prepares the conditions to add to the `WHERE` clause of the `SELECT` statement based on the input `Query`.
- Conditions are `AND`ed together so all conditions must be met for a database record to match the query
- **IMPORTANT**: Add `WHERE` conditions only for non-zero filtering option in the `Query`
- Exclude columns that the actor is not allowed to filter on

```go
if query.Title != "" {
    conditions = append(conditions, "title=?")
    args = append(args, query.Title)
}
if !query.UpdatedAtGTE.IsZero() {
    conditions = append(conditions, "updated_at>=?")
    args = append(args, query.UpdatedAtGTE.UTC())
}
if !query.UpdatedAtLT.IsZero() {
    conditions = append(conditions, "updated_at<?")
    args = append(args, query.UpdatedAtLT.UTC())
}
```

Column mapping and query conditions can be made to be dependent on the claims associated with the actor of the request. For example, an admin may be allowed to read additional columns from the `user` table, or a guest user may be applies a `WHERE` condition in order to restrict their view of the results.

```go
var actor Actor
frame.Of(ctx).ParseActor(&actor)
if actor.IsAdmin() {
    // ...
} else {
    // ...
}
```

### Query Filtering Options

The `Query` struct specifies filtering options which are translated to `WHERE` conditions by `prepareWhereClauses` and applied to the `SELECT` SQL statement in the `List` functional endpoint.

```go
type Query struct {
    Name string         `json:"name,omitzero"`
    AgeGTE int          `json:"ageGte,omitzero"`
    AgeLT int           `json:"ageLte,omitzero"`
    OnlyCitizen bool    `json:"onlyCitizen,omitzero"`
    OnlyNotCitizen bool `json:"onlyNotCitizen,omitzero"`
    States []string     `json:"states,omitzero"`
}
```

Zero-valued filtering options should not result in a `WHERE` condition because an empty `Query` should select all records.

```go
func (svc *Service) prepareWhereClauses(ctx context.Context, query bookapi.Query) (conditions []string, args []any, err error) {
    // String filtering option
    if query.Name != "" {
        conditions = append(conditions, "name=?")
        args = append(args, query.Name)
    }
    // Range filtering option
    if query.AgeGTE != 0 {
        conditions = append(conditions, "age>=?")
        args = append(args, query.AgeGTE)
    }
    if query.AgeLT != 0 {
        conditions = append(conditions, "age<?")
        args = append(args, query.AgeLT)
    }
    // Boolean filtering option
    if query.OnlyCitizen {
        conditions = append(conditions, "citizen=1")
    }
    if query.OnlyNotCitizen {
        conditions = append(conditions, "citizen=0")
    }
    // List filtering option
    if query.States != nil {
        if len(query.States) > 0 {
            conditions = append(conditions, "states IN (" + strings.Repeat("?,", len(query.States)-1) + "?)")
            args = append(args, query.States...)
        } else {
            conditions = append(conditions, "1=0") // Empty array should yield no result
        }
    }
	return conditions, args, nil
}
```

### Configuring the Datasource Name for Running Tests

Running tests require the microservices under test to be able to connect to the SQL database. The data source name is configured in `config.local.yaml` at the root of the project. If tests fail to connect to the database, prompt the user to update `config.local.yaml` with the appropriate credentials.

```yaml
all:
  SQLDataSourceName: root:root@tcp(127.0.0.1:3306)/
```

Example data source names for each of the supported drivers:
- mysql: `root:root@tcp(127.0.0.1:3306)/`
- pgx: `postgres://postgres:postgres@127.0.0.1:5432/`
- mssql: `sqlserver://sa:sa@127.0.0.1:1433`
