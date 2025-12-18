---
name: Adding a new SQL CRUD microservice
description: Creates and initializes a new microservice that provides CRUD operations to a SQL database such as MySQL, Postgres or Microsoft SQL Server. Use when explicitly asked by the user to create a new SQL or CRUD microservice to persist an object.
---

## Workflow

Copy this checklist and track your progress:

```
Creating a new SQL CRUD microservice:
- [ ] Step 1: Create a directory for the new microservice
- [ ] Step 2: Create doc.go with code generation directives
- [ ] Step 3: Generate service.yaml
- [ ] Step 4: Update service.yaml
- [ ] Step 5: Generate the microservice file structure
- [ ] Step 6: Propose object fields
```

#### Step 1: Create a directory for the new microservice

Create a new directory for the new microservice.
For the directory name, use the singular form of the noun representing the object being persisted.
Use only lowercase letters `a` through `z`, for example, `user`, `notebook` or `salesorder`.

In smaller projects, place the new directory under the root directory of the project.

```bash
mkdir -p myservice
cd myservice
```

In larger projects, consider using a nested directory structure to group similar microservices together.

```bash
mkdir -p mydomain/myservice
cd mydomain/myservice
```

#### Step 2: Create `doc.go` with code generation directives

Create `doc.go` with the `go:generate` directive to trigger the code generator. Name the package the same as the directory.

```go
package myservice

//go:generate go run github.com/microbus-io/fabric/codegen
//go:generate go run github.com/microbus-io/sequel/codegen

```

#### Step 3: Generate microservice file structure

Run `go generate` to generate `service.yaml`.

**Important** Do not attempt to create `service.yaml` yourself from scratch. Always let the code generator initialize it.

#### Step 4: Update `service.yaml`

The `service.yaml` file is the blueprint of the microservice.

Update the `general` section as needed:
- The `host` defines the host name under which this microservice will be addressable. It must be unique across the application. Use reverse domain notation, e.g. `myservice.myproject.mycompany`.
- The `description` should explain what this microservice is about.
- Set `integrationTests` to `false` if instructed to skip integration tests, or if instructed to be "quick".

```yaml
general:
  host: myservice.myproject.mycompany
  description: My microservice manages the persistence of X.
  integrationTests: true
```

Update the `sql` section as needed:
- The `table` is the name of the database table where the data is stored. The table name must be in snake_case. If the noun representing the object has more than one words, separate them with underscores. For example, the table name for "sales order" should be `sales_order`
- The `object` is the name of the Go object type of the object. The object name must be in PascalCase. If the noun representing the object has more than one word, use a capital letter for each word. For example, the object name for "sales order" should be `SalesOrder`

```yaml
sql:
  table: my_object
  object: MyObject
```

**Important**: Do not update any other sections of `service.yaml` unless explicitly asked to do so by the user.

#### Step 5: Generate the microservice file structure

Run `go generate` to generate the file structure of the microservice.

#### Step 6: Propose object fields

Skip this step if instructed to be "quick".

If you have an idea what fields the object should include, prepare a proposal that describes for each proposed field:
- Name
- Description
- Go data type, e.g. `string`, `integer`, `time.Time`, etc.
- Validation rules such as whether or not the field is required, maximum length (if string), acceptable range (if numeric), etc.
- Whether or not it is a unique identifier of the object

Save the proposal in `AGENTS.md`, then show it to the user and seek additional instructions.
Do not implement without explicit approval from the user.
