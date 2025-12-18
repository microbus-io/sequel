---
name: Changing fields of the object persisted by a SQL CRUD microservice
description: Changes fields of an object that is persisted to a SQL database by a CRUD microservice. Use when explicitly asked by the user to change, modify or rename fields (or properties) of the object; or when explicitly asked by the user to change, modify or rename columns of the database table.
---

## Workflow

Copy this checklist and track your progress:

```
Changing fields of the object:
- [ ] Step 1: Update the type definition of the object
- [ ] Step 2: Update the type definition of the query
- [ ] Step 3: Update database schema
- [ ] Step 4: Update mappings of column names to object fields
- [ ] Step 5: Update query conditions
- [ ] Step 6: Update integration tests
- [ ] Step 7: Document the microservice
- [ ] Step 8: Versioning
```

#### Step 1: Update the type definition of the object

Find the type definition of the object in `object.go` in the API directory of the microservice.
Change the fields in the type definition of the struct appropriately.
Change the code in the object's `Validate` method appropriately.

#### Step 2: Update the type definition of the query

Find the type definition of the query in `query.go` in the API directory of the microservice.
Change the fields in the type definition of the struct appropriately.
Change the code in the object's `Validate` method appropriately.

#### Step 3: Update database schema

Skip this step if the changes do not necessitate a database schema change.

Create a new migration script file in `resources/sql` with an incremental file name. **IMPORTANT**: Do not edit an existing migration file.

Append `ALTER TABLE` statements to change the type, size or constraints of columns, if applicable.

```sql
-- DRIVER: mysql
ALTER TABLE my_table
	MODIFY COLUMN modified_column VARCHAR(384) NOT NULL DEFAULT '',
	MODIFY COLUMN changed_column BIGINT NOT NULL DEFAULT 0;

-- DRIVER: pgx
ALTER TABLE my_table
	ALTER COLUMN modified_column VARCHAR(384) NOT NULL DEFAULT '',
	ALTER COLUMN changed_column BIGINT NOT NULL DEFAULT 0;

-- DRIVER: mssql
ALTER TABLE my_table ALTER COLUMN
	modified_column NVARCHAR(384) NOT NULL DEFAULT '',
	changed_column BIGINT NOT NULL DEFAULT 0;
```

Append `ALTER TABLE` or `EXEC` statements to rename columns, if applicable. For `mysql`, prefer using the new `RENAME COLUMN` syntax over the old `CHANGE COLUMN` syntax.

```sql
-- DRIVER: mysql
ALTER TABLE my_table
    RENAME COLUMN old_column TO new_column;

-- DRIVER: pgx
ALTER TABLE my_table
    RENAME COLUMN old_column TO new_column;

-- DRIVER: mssql
EXEC sp_rename 'my_table.old_column', 'new_column', 'COLUMN';
```

#### Step 4: Update mappings of column names to object fields

Update the mappings of renamed database column names to their corresponding object fields in `mapColumnsOnInsert`, `mapColumnsOnUpdate` and `mapColumnsOnSelect` in `service.go`.

#### Step 5: Update query conditions

Adjust the query conditions and searchable columns in `prepareWhereClauses` in `service.go` that correspond to the changed fields or columns.

#### Step 6: Update integration tests

Skip this step if integration tests were skipped for this microservice and there is no `service_test.go`, or if instructed to be "quick".

Adjust references to the changed fields in `service_test.go`, including in the `NewObject` constructor.

Adjust references to the changed fields in `query_test.go`, located in the API directory of the microservice.

#### Step 7: Document the microservice

Skip this step if instructed to be "quick".

Update the microservice's local `AGENTS.md` file to reflect the changes. Capture purpose, context, and design rationale. Focus on the reasons behind decisions rather than describing what the code does. Explain design choices, tradeoffs, and the context needed for someone to safely evolve this microservice in the future.

#### Step 8: Versioning

Run `go generate` to version the code.
