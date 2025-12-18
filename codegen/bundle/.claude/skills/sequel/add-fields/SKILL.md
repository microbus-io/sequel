---
name: Modifying the fields of the object persisted by a SQL CRUD microservice
description: Adds, modifies or removes fields of an object that is persisted to a SQL database by a CRUD microservice. Use when explicitly asked by the user to add, modify or remove fields (or properties) of the object; or when explicitly asked by the user to add, modify or remove columns to the database table.
---

## Workflow

Copy this checklist and track your progress:

```
Modifying the fields of the object:
- [ ] Step 1: Update the type definition of the object
- [ ] Step 2: Update the type definition of the query
- [ ] Step 3: Update database schema
- [ ] Step 4: Map column names to object fields
- [ ] Step 5: Add query conditions
- [ ] Step 6: Update integration tests
- [ ] Step 7: Document the microservice
- [ ] Step 8: Versioning
```

#### Step 1: Update the type definition of the object

Find the type definition of the object in `object.go` in the API directory of the microservice.
Add the new fields to the type definition of the struct.
Fields can be primitive or complex types that serialize to JSON.
Be sure to include a JSON tag. Use camelCase for the JSON name, and specify to `omitzero`.

When referring to an object persisted by another microservices, use its respective key.

```go
type Object struct {
	Key ObjectKey `json:"key,omitzero"`

	// HINT: Define the fields of the object here
	MyFieldString   string             `json:"myFieldString,omitzero"`
	MyFieldInteger  int                `json:"myFieldInteger,omitzero"`
	MyFieldNullable string             `json:"myFieldNullable,omitzero"`
	MyFieldTime     time.Time          `json:"myFieldTime,omitzero"`
	MyFieldTags     map[string]string  `json:"myFieldTags,omitzero"`
	
	MyOtherObjectKey otherobjectapi.OtherObjectKey `json:"myOtherObjectKey,omitzero"`
}
```

Modify the object's `Validate` method appropriately to return an error if the values of the new fields do not meet the validation requirements. Be sure to strip strings of extra spaces using `strings.TrimSpace` if appropriate.

```go
// Validate validates the object before storing it.
func (obj *Object) Validate(ctx context.Context) error {
	// ...

	// HINT: Validate the fields of the object here as required
	obj.MyFieldString = strings.TrimSpace(obj.MyFieldString)
	if len([]rune(obj.MyFieldString)) > 256 {
		return errors.New("length of MyFieldString must not exceed 256 characters")
	}
	if obj.MyFieldInteger < 0 {
		return errors.New("MyFieldInteger must not be negative")
	}
	if obj.MyFieldTime.After(time.Now()) {
		return errors.New("MyFieldTime must not in the future")
	}
	if obj.MyFieldOtherObjectKey.IsZero() {
		return errors.New("MyFieldOtherObjectKey is required")
	}
	return nil
}
```

#### Step 2: Update the type definition of the query

Find the type definition of the `Query` in `object.go` in the API directory of the microservice.
To allow filtering by the new fields, add them to the type definition of the struct.
Fields can be primitive or complex types that serialize to JSON.
Be sure to include a JSON tag. Use camelCase for the JSON name, and specify to `omitzero`.

When referring to a parent object that is persisted by another SQL CRUD microservices, use its respective key as the field type.

```go
type Query struct {
	Key ObjectKey `json:"key,omitzero"`

	// HINT: Define the fields of the object here
	MyFieldInteger  int       `json:"myFieldInteger,omitzero"`
	MyFieldNullable string    `json:"myFieldNullable,omitzero"`
	MyFieldTimeGTE  time.Time `json:"myFieldTimeStart,omitzero"`
	MyFieldTimeLT   time.Time `json:"myFieldTimeEnd,omitzero"`
	
	ParentKey parentapi.ParentKey `json:"parentKey,omitzero"`
}
```

Modify the `Query`'s `Validate` method appropriately to return an error if the values of the new fields do not meet the validation requirements. Be sure to strip strings of extra spaces using `strings.TrimSpace` if appropriate.

```go
// Validate validates the filtering options of the query.
func (q *Query) Validate(ctx context.Context) error {
	// ...

	// HINT: Validate filtering options here as required
	if obj.MyFieldInteger < 0 {
		return errors.New("MyFieldInteger must not be negative")
	}
	obj.MyFieldNullable = strings.TrimSpace(obj.MyFieldNullable)
	if len([]rune(obj.MyFieldNullable)) > 256 {
		return errors.New("length of MyFieldNullable must not exceed 256 characters")
	}
	if obj.MyFieldTimeGTE.After(time.Now()) {
		return errors.New("MyFieldTimeGTE must not in the future")
	}
	if obj.MyFieldTimeLT.After(time.Now()) {
		return errors.New("MyFieldTimeLT must not in the future")
	}
	if obj.MyFieldTimeGTE.After(obj.MyFieldTimeLT) {
		return errors.New("MyFieldTimeGTE must not be after MyFieldTimeLT")
	}
	if obj.ParentKey.IsZero() {
		return errors.New("ParentKey is required")
	}
	return nil
}
```

#### Step 3: Update database schema

Create a new migration script file in `resources/sql` with an incremental file name. **IMPORTANT**: Do not edit an existing migration file.

Append `ALTER TABLE` statements to define the schema of the new columns. Define a `DEFAULT` value for all new columns that are `NOT NULL` in order to avoid the migration from failing on tables already populated with data. Columns holding IDs of parent objects should be named after the table they refer to with an `_id` suffix, e.g. `parent_table_id`.

```sql
-- DRIVER: mysql
ALTER TABLE my_table
	ADD my_field_integer BIGINT NOT NULL DEFAULT 0,
	ADD my_field_nullable TEXT NULL,
	ADD my_field_time DATETIME NULL,
	ADD my_field_tags MEDIUMBLOB NULL,
	ADD parent_table_id BIGINT NOT NULL DEFAULT 0;

-- DRIVER: pgx
ALTER TABLE my_table
	ADD COLUMN my_field_integer BIGINT NOT NULL DEFAULT 0,
	ADD COLUMN my_field_nullable TEXT NULL,
	ADD COLUMN my_field_time TIMESTAMP WITH TIME ZONE NULL,
	ADD my_field_tags BYTEA NULL,
	ADD COLUMN parent_table_id BIGINT NOT NULL DEFAULT 0;

-- DRIVER: mssql
ALTER TABLE my_table ADD
	my_field_integer BIGINT NOT NULL DEFAULT 0,
	my_field_nullable NVARCHAR(MAX) NULL,
	my_field_time DATETIME2 NULL,
	my_field_tags VARBINARY(MAX) NULL,
	parent_table_id BIGINT NOT NULL DEFAULT 0;
```

Append `CREATE INDEX` or `CREATE UNIQUE INDEX` statements to add indices for columns that will be heavily searchable. Always include the `tenant_id` as the first column in a composite index. Name the index by concatenating the name of the table, followed by `idx` and the columns it includes (excluding the `tenant_id` column). For example, `my_table_idx_my_field_integer` is a composite index of `(tenant_id, my_field_integer)` in the `my_table` table. If you are not sure what columns are worth indexing, ask the user for guidance.

```sql
CREATE INDEX my_table_idx_my_field_integer ON my_table (tenant_id, my_field_integer);
```

#### Step 4: Map column names to object fields

Update the mapping of the database column names to their corresponding object fields in `service.go`.
**IMPORTANT** : Do not remove the mappings of the `example` column to the `Example` field since they are required by various tests.

In `mapColumnsOnInsert`, map the column names that can be set during the initial insertion of the object.
For nullable columns, wrap the value in `sequel.Nullify` to store the Go zero value as `NULL` in the database. To use a SQL statement as value, wrap a string in `sequel.UnsafeSQL`.

```go
func (svc *Service) mapColumnsOnInsert(ctx context.Context, obj *serviceapi.Obj) (columnMapping map[string]any, err error) {
	tags, err := json.Marshal(obj.Tags)
	if err != nil {
		return errors.Trace(err)
	}
	columnMapping := map[string]any{
		"my_field_integer":  obj.MyFieldInteger,
		"my_field_nullable": sequel.Nullify(obj.MyFieldNullable),
		"my_field_time":     sequel.UnsafeSQL(svc.db.NowUTC()),
		"my_field_tags":     tags,
		"parent_table_id":   obj.ParentKey,
	}
	return columnMapping, nil
}
```

In `mapColumnsOnUpdate`, map the columns that can be modified after the initial insertion of the object.
For nullable columns, wrap the value in `sequel.Nullify` to store the Go zero value as `NULL` in the database. To use a SQL statement as value, wrap a string in `sequel.UnsafeSQL`.

```go
func (svc *Service) mapColumnsOnUpdate(ctx context.Context, obj *serviceapi.Obj) (columnMapping map[string]any, err error) {
	tags, err := json.Marshal(obj.Tags)
	if err != nil {
		return errors.Trace(err)
	}
	columnMapping := map[string]any{
		"my_field_integer":  obj.MyFieldInteger,
		"my_field_nullable": sequel.Nullify(obj.MyFieldNullable),
		"my_field_time":     sequel.UnsafeSQL(svc.db.NowUTC()),
		"my_field_tags":     tags,
		"parent_table_id":   obj.ParentKey,
	}
	return columnMapping, nil
}
```

In `mapColumnsOnSelect`, map the columns that can be read.
For nullable columns, wrap the reference to the variable in `sequel.Nullable` in order to interpret a database `NULL` value as the zero value of the Go data type. Use `sequel.Bind` to transform and apply the value manually to the object.

```go
func (svc *Service) mapColumnsOnSelect(ctx context.Context, obj *serviceapi.Obj) (columnMapping map[string]any, err error) {
	columnMapping := map[string]any{
		"my_field_integer":  &obj.MyFieldInteger,
		"my_field_nullable": sequel.Nullable(&obj.MyFieldNullable),
		"my_field_time":     &obj.MyFieldTime,
		"my_field_tags": sequel.Bind(func(value []byte) (err error) {
			return json.Unmarshal(value, &obj.Tags)
		}),
		"parent_table_id": &obj.ParentKey.ID,
	}
	return columnMapping, nil
}
```

#### Step 5: Add query conditions

Prepare appropriate query conditions in `prepareWhereClauses` in `service.go` for new `Query` fields. Only add a condition if the `Query` field is not its zero value. Add the names of any textual and searchable columns to the `searchableColumns` array.

```go
func (svc *Service) prepareWhereClauses(ctx context.Context, query serviceapi.Query) (conditions []string, args []any, err error) {
	if strings.TrimSpace(query.Q) != "" {
		searchableColumns := []string{
			"my_field_nullable",
		}
		// ...
	}
	if query.MyFieldInteger != 0 {
		conditions = append(conditions,"my_field_integer=?")
		args = append(args, query.MyFieldInteger)
	}
	query.MyFieldNullable = strings.TrimSpace(query.MyFieldNullable)
	if query.MyFieldNullable != "" {
		conditions = append(conditions,"my_field_nullable=?")
		args = append(args, query.MyFieldNullable)
	}
	if !query.MyFieldTimeGTE.IsZero() {
		conditions = append(conditions,"my_field_time>=?")
		args = append(args, query.MyFieldTimeGTE)
	}
	if !query.MyFieldTimeLT.IsZero() {
		conditions = append(conditions,"my_field_time<?")
		args = append(args, query.MyFieldTimeLT)
	}
	if !query.ParentKey.IsZero() {
		conditions = append(conditions,"parent_table_id=?")
		args = append(args, query.ParentKey.ID)
	}
	return conditions, args, nil
}
```

#### Step 6: Update integration tests

Skip this step if integration tests were skipped for this microservice and there is no `service_test.go`, or if instructed to be "quick".

The `NewObject` function in `service_test.go` is used by tests to construct a new object to pass to `Create`. Adjust the constructor function to initialize all required fields so that they pass validation. You may introduce a measure of randomness.

Extend the integration tests to take into account the schema changes. Look for the `HINT`s to guide you. In particular:
- Set, modify and verify the new fields in the `create_and_store` test case in `service_test.go`
- Add validation test cases for the new fields in `TestQuery_ValidateObject` in `query_test.go` in the API directory of the microservice

#### Step 7: Document the microservice

Skip this step if instructed to be "quick".

Update the microservice's local `AGENTS.md` file to reflect the changes. Capture purpose, context, and design rationale. Focus on the reasons behind decisions rather than describing what the code does. Explain design choices, tradeoffs, and the context needed for someone to safely evolve this microservice in the future.

#### Step 8: Versioning

Run `go generate` to version the code.
