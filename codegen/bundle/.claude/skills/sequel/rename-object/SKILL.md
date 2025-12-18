---
name: Renaming the type of the object persisted by a SQL CRUD microservice
description: Renames the type of the object used by a SQL CRUD microservice. Use when explicitly asked by the user to rename the object or its type definition.
---

## Workflow

Copy this checklist and track your progress:

```
Renaming the type of the object:
- [ ] Step 1: Update object name in service.yaml
- [ ] Step 2: Generate boilerplate code
```

#### Step 1: Update object name in `service.yaml`

Update the object name in the `sql` section in `service.yaml` to the new name. The object name must be in PascalCase. If the noun representing the object has more than one word, use a capital letter for each word. For example, the object name for "sales order" should be `SalesOrder`.

```yaml
sql:
  table: table_name
  object: NewObjectName
```

**IMPORTANT**: Do not make other changes to `service.yaml`. Do not update the name of the object in the function signatures.

#### Step 2: Regenerate boilerplate code

Run `go generate` to regenerate the boilerplate code.
