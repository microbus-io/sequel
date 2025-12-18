---
name: Renaming the database table in a SQL CRUD microservice
description: Renames the database table used by a SQL CRUS microservice to persist objects. Use when explicitly asked by the user to rename the table.
---

## Workflow

Copy this checklist and track your progress:

```
Renaming the database table:
- [ ] Step 1: Update table name in service.yaml
- [ ] Step 2: Generate boilerplate code
```

#### Step 1: Update table name in `service.yaml`

Update the table name in the `sql` section in `service.yaml` to the new table name. The table name must be in snake_case. If the noun representing the object has more than one word, use an underscore to separate words. For example, the table name for "sales order" should be `sales_order`.

```yaml
sql:
  table: new_table_name
  object: ObjectName
```

**IMPORTANT**: Do not make other changes to `service.yaml`.

#### Step 2: Regenerate boilerplate code

Run `go generate` to regenerate the boilerplate code.
