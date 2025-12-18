---
name: Removing fields of the object persisted by a SQL CRUD microservice
description: Removes fields of an object that is persisted to a SQL database by a CRUD microservice. Use when explicitly asked by the user to remove fields (or properties) of the object; or when explicitly asked by the user to remove columns of the database table.
---

## Workflow

Copy this checklist and track your progress:

```
Removing fields of the object:
- [ ] Step 1: Update the type definition of the object
- [ ] Step 2: Update the type definition of the query
- [ ] Step 3: Update database schema
- [ ] Step 4: Remove mappings of column names to object fields
- [ ] Step 5: Remove query conditions
- [ ] Step 6: Update integration tests
- [ ] Step 7: Document the microservice
- [ ] Step 8: Versioning
```

#### Step 1: Update the type definition of the object

Find the type definition of the object in `object.go` in the API directory of the microservice.
Remove the appropriate fields from the type definition of the struct.
Remove the irrelevant code from the object's `Validate` method.

#### Step 2: Update the type definition of the query

Find the type definition of the query in `query.go` in the API directory of the microservice.
Remove the appropriate fields from the type definition of the struct.
Remove the irrelevant code from the object's `Validate` method.

#### Step 3: Update database schema

Create a new migration script file in `resources/sql` with an incremental file name. **IMPORTANT**: Do not edit an existing migration file.

Refer to the older `.sql` migration files to identify what if any indices were associated with the deprecated columns. Use `DROP INDEX` statements to drop these indices, if applicable.

```sql
-- DRIVER: mysql
DROP INDEX my_table_idx_deprecated_field ON my_table;

-- DRIVER: pgx
DROP INDEX my_table_idx_deprecated_field;

-- DRIVER: mssql
DROP INDEX my_table_idx_deprecated_field ON my_table;
```

Append `ALTER TABLE` statements to drop the columns.

```sql
-- DRIVER: mysql
ALTER TABLE my_table
	DROP deprecated_field,
	DROP unused_field;

-- DRIVER: pgx
ALTER TABLE my_table
	DROP COLUMN deprecated_field,
	DROP COLUMN unused_field;

-- DRIVER: mssql
ALTER TABLE my_table DROP COLUMN
	deprecated_field,
	unused_field;
```

#### Step 4: Remove mappings of column names to object fields

Update the mappings of the database column names to their corresponding object fields in `mapColumnsOnInsert`, `mapColumnsOnUpdate` and `mapColumnsOnSelect` in `service.go`. Remove mappings of deprecated columns to deprecated object fields.

#### Step 5: Remove query conditions

Remove the query conditions and searchable columns in `prepareWhereClauses` in `service.go` that correspond to the removed fields or columns.

#### Step 6: Update integration tests

Skip this step if integration tests were skipped for this microservice and there is no `service_test.go`, or if instructed to be "quick".

Remove references to the deprecated fields in `service_test.go`, including in the `NewObject` constructor.

Remove references to the deprecated fields in `query_test.go`, located in the API directory of the microservice.

#### Step 7: Document the microservice

Skip this step if instructed to be "quick".

Update the microservice's local `AGENTS.md` file to reflect the changes. Capture purpose, context, and design rationale. Focus on the reasons behind decisions rather than describing what the code does. Explain design choices, tradeoffs, and the context needed for someone to safely evolve this microservice in the future.

#### Step 8: Versioning

Run `go generate` to version the code.
