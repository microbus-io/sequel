package service

import (
	"context"
	"io/fs"
	"maps"
	"net/http"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"

	"github.com/microbus-io/errors"
	"github.com/microbus-io/sequel"
	"github.com/microbus-io/sequel/codegen/bundle/connector"
	"github.com/microbus-io/sequel/codegen/bundle/frame"
	"github.com/microbus-io/sequel/codegen/bundle/service/serviceapi"
)

const (
	tableName        = "table_name"
	bulkBatchSize    = 1000
	maxParamsInQuery = 2000 // SQL Server is limited to 2100 parameters
)

type Service struct {
	db *sequel.DB
}

// OnStartup is called when the microservice is started up.
func (svc *Service) OnStartup(ctx context.Context) (err error) {
	err = svc.openDatabase(ctx)
	if err != nil {
		return errors.Trace(err)
	}
	return nil
}

// OnShutdown is called when the microservice is shut down.
func (svc *Service) OnShutdown(ctx context.Context) (err error) {
	svc.closeDatabase(ctx)
	return nil
}

/*
mapColumnsOnInsert maps names of columns to their corresponding values, on creation of a new object.
*/
func (svc *Service) mapColumnsOnInsert(ctx context.Context, obj *serviceapi.Obj) (columnMapping map[string]any, err error) {
	_ = ctx
	/*
		HINT: Map columns names to their corresponding values.
		Do not include columns that the actor is unauthorized to write to.
		Wrap a value in sequel.Nullify to store NULL in the database for its zero value.
		Wrap a string in sequel.UnsafeSQL to use a SQL statement as value.
	*/
	columnMapping = map[string]any{
		"example": sequel.Nullify(obj.Example), // Do not remove the example
	}
	return columnMapping, nil
}

/*
mapColumnsOnUpdate maps names of columns to their corresponding values, on modification of an already existing object.
*/
func (svc *Service) mapColumnsOnUpdate(ctx context.Context, obj *serviceapi.Obj) (columnMapping map[string]any, err error) {
	_ = ctx
	/*
		HINT: Map columns names to their corresponding values.
		Do not include invariant columns.
		Do not include columns that the actor is unauthorized to write to.
		Wrap a value in sequel.Nullify to store NULL in the database for its zero value.
		Wrap a string in sequel.UnsafeSQL to use a SQL statement as value.
	*/
	columnMapping = map[string]any{
		"example": sequel.Nullify(obj.Example), // Do not remove the example
	}
	return columnMapping, nil
}

/*
mapColumnsOnSelect maps names of columns to their corresponding object fields, on reading of an object.
*/
func (svc *Service) mapColumnsOnSelect(ctx context.Context, obj *serviceapi.Obj) (columnMapping map[string]any, err error) {
	_ = ctx
	/*
		HINT: Map columns names to their object fields.
		Exclude columns that the actor is unauthorized to read.
		Wrap the reference to a field in sequel.Nullable if the corresponding database column is NULL-able.
		Use sequel.Bind to transform and apply the value manually to the object.
	*/
	columnMapping = map[string]any{
		"example": sequel.Nullable(&obj.Example), // Do not remove the example
	}
	return columnMapping, nil
}

/*
prepareWhereClauses prepares the conditions to add to the WHERE clause based on the query.
All conditions must be met for a record to match the query, that is, they are AND-ed together.
*/
func (svc *Service) prepareWhereClauses(ctx context.Context, query serviceapi.Query) (conditions []string, args []any, err error) {
	_ = ctx
	if strings.TrimSpace(query.Q) != "" {
		searchableColumns := []string{
			/*
				HINT: Add names of textual (VARCHAR, TEXT, etc.) columns that are searchable.
				Exclude columns that the actor is unauthorized to search on.
			*/
			"example", // Do not remove the example
		}
		q := strings.TrimSpace(regexp.MustCompile(`\s`).ReplaceAllString(query.Q, " ")) // Compress whitespaces
		for _, word := range strings.Split(q, " ") {
			conditions = append(conditions, svc.db.RegexpTextSearch(searchableColumns...))
			if len([]rune(word)) <= 3 {
				args = append(args, `(^|\b)`+regexp.QuoteMeta(word))
			} else {
				args = append(args, regexp.QuoteMeta(word))
			}
		}
	}
	/*
		HINT: Add WHERE conditions for each non-zero filtering option of the query.
		Exclude columns that the actor is unauthorized to filter on.
	*/
	query.Example = strings.TrimSpace(query.Example) // Do not remove the example
	if query.Example != "" {
		conditions = append(conditions, "example=?")
		args = append(args, query.Example)
	}
	return conditions, args, nil
}

/*
tenantOf returns the tenant claim of the actor, or 0 if not found.
*/
func (svc *Service) tenantOf(ctx context.Context) int {
	tid, _ := frame.Of(ctx).Tenant()
	return tid
}

/*
openDatabase opens the database connection and runs schema migrations as needed.
*/
func (svc *Service) openDatabase(ctx context.Context) (err error) {
	_ = ctx
	const driverName = "" // The driver name is inferred from the data source name
	dataSourceName := svc.SQLDataSourceName()
	if svc.Deployment() == connector.TESTING {
		svc.db, err = sequel.OpenTesting(driverName, dataSourceName, svc.Plane())
	} else {
		svc.db, err = sequel.Open(driverName, dataSourceName)
	}
	if err != nil {
		return errors.Trace(err)
	}
	const sequenceName = `_SEQUENCE_` // Do not change
	dirFS, err := fs.Sub(svc.ResFS(), "sql")
	if err != nil {
		return errors.Trace(err)
	}
	err = svc.db.Migrate(sequenceName, dirFS)
	if err != nil {
		return errors.Trace(err)
	}
	return nil
}

/*
closeDatabase closes the database connection.
*/
func (svc *Service) closeDatabase(ctx context.Context) (err error) {
	_ = ctx
	if svc.db != nil {
		err = svc.db.Close()
	}
	return errors.Trace(err)
}

/*
Create creates a new object, returning its key.
*/
func (svc *Service) Create(ctx context.Context, obj *serviceapi.Obj) (objKey serviceapi.ObjKey, err error) {
	objKeys, err := svc.BulkCreate(ctx, []*serviceapi.Obj{obj})
	if err != nil {
		return serviceapi.ObjKey{}, errors.Trace(err)
	}
	return objKeys[0], nil
}

/*
Store updates the object.
*/
func (svc *Service) Store(ctx context.Context, obj *serviceapi.Obj) (stored bool, err error) {
	storedKeys, err := svc.BulkStore(ctx, []*serviceapi.Obj{obj})
	return len(storedKeys) > 0, errors.Trace(err)
}

/*
MustStore updates the object.
*/
func (svc *Service) MustStore(ctx context.Context, obj *serviceapi.Obj) (err error) {
	stored, err := svc.Store(ctx, obj)
	if err != nil {
		return errors.Trace(err)
	}
	if !stored {
		return errors.New("object not found", http.StatusNotFound)
	}
	return nil
}

/*
Revise updates the object only if the revision matches.
*/
func (svc *Service) Revise(ctx context.Context, obj *serviceapi.Obj) (revised bool, err error) {
	revisedKeys, err := svc.BulkRevise(ctx, []*serviceapi.Obj{obj})
	return len(revisedKeys) > 0, errors.Trace(err)
}

/*
MustRevise updates the object only if the revision matches.
*/
func (svc *Service) MustRevise(ctx context.Context, obj *serviceapi.Obj) (err error) {
	revised, err := svc.Revise(ctx, obj)
	if err != nil {
		return errors.Trace(err)
	}
	if !revised {
		return errors.New("revision conflict", http.StatusConflict)
	}
	return nil
}

/*
Delete deletes the object.
*/
func (svc *Service) Delete(ctx context.Context, objKey serviceapi.ObjKey) (deleted bool, err error) {
	if objKey.IsZero() {
		return false, nil
	}
	tenantID := svc.tenantOf(ctx)
	stmt := "DELETE FROM " + tableName + " WHERE tenant_id=? AND id=?"
	args := []any{
		tenantID,
		objKey,
	}
	stmt = svc.db.ConformArgPlaceholders(stmt)
	res, err := svc.db.ExecContext(ctx, stmt, args...)
	if err != nil {
		return false, errors.Trace(err)
	}
	ra, err := res.RowsAffected()
	if err != nil {
		return false, errors.Trace(err)
	}
	return ra > 0, nil
}

/*
MustDelete deletes the object.
*/
func (svc *Service) MustDelete(ctx context.Context, objKey serviceapi.ObjKey) (err error) {
	deleted, err := svc.Delete(ctx, objKey)
	if err != nil {
		return errors.Trace(err)
	}
	if !deleted {
		return errors.New("object not found", http.StatusNotFound)
	}
	return nil
}

/*
List returns the objects matching the query, and the total count of matches regardless of the limit.
*/
func (svc *Service) List(ctx context.Context, query serviceapi.Query) (objs []*serviceapi.Obj, totalCount int, err error) {
	err = query.Validate(ctx)
	if err != nil {
		return nil, 0, errors.Trace(err, http.StatusBadRequest)
	}
	var obj serviceapi.Obj
	columnMapping, err := svc.mapColumnsOnSelect(ctx, &obj)
	if err != nil {
		return nil, 0, errors.Trace(err)
	}
	fuzzyColumnNames := map[string]string{"key": "id"}
	knownColumnNames := map[string]bool{}
	for k := range columnMapping {
		fuzzyColumnNames[strings.ToLower(strings.ReplaceAll(k, "_", ""))] = k
		knownColumnNames[k] = true
	}

	// -- SELECT --
	tenantID := svc.tenantOf(ctx)
	columnMapping["id"] = &obj.Key
	columnMapping["revision"] = &obj.Revision
	columnMapping["created_at"] = &obj.CreatedAt
	columnMapping["updated_at"] = &obj.UpdatedAt
	var columnsInOrder []string
	if query.Select != "" {
		for _, col := range strings.Split(query.Select, ",") {
			col = strings.TrimSpace(col)
			if _, ok := columnMapping[col]; !ok {
				// Fuzzy match to column_name
				col = fuzzyColumnNames[strings.ToLower(col)]
			}
			if _, ok := columnMapping[col]; ok && col != "" {
				columnsInOrder = append(columnsInOrder, col)
			}
		}
		sort.Strings(columnsInOrder)
	}
	if len(columnsInOrder) == 0 {
		columnsInOrder = slices.Sorted(maps.Keys(columnMapping))
	}
	var stmt strings.Builder
	stmt.WriteString("SELECT ")
	stmt.WriteString(strings.Join(columnsInOrder, ", "))
	stmt.WriteString(" FROM ")
	stmt.WriteString(tableName)
	stmt.WriteString(" WHERE tenant_id=?")
	args := []any{
		tenantID,
	}

	// -- WHERE --
	if !query.Key.IsZero() {
		stmt.WriteString(" AND id=?")
		args = append(args, query.Key)
	}
	if len(query.Keys) > 0 {
		stmt.WriteString(" AND id IN (")
		for i, k := range query.Keys {
			if i > 0 {
				stmt.WriteString(",")
			}
			stmt.WriteString(strconv.Itoa(k.ID))
		}
		stmt.WriteString(")")
	} else if query.Keys != nil {
		stmt.WriteString(" AND 1=0")
	}
	conditions, conditionArgs, err := svc.prepareWhereClauses(ctx, query)
	if err != nil {
		return nil, 0, errors.Trace(err)
	}
	for _, cond := range conditions {
		stmt.WriteString(" AND (")
		stmt.WriteString(cond)
		stmt.WriteString(")")
	}
	args = append(args, conditionArgs...)

	// -- ORDER BY --
	countOrderBy := 0
	orderedByID := false
	stmt.WriteString(" ORDER BY")
	for orderBy := range strings.SplitSeq(query.OrderBy, ",") {
		orderBy := strings.TrimSpace(strings.ToLower(orderBy))
		if orderBy == "" {
			continue
		}
		direction := "ASC"
		if strings.HasPrefix(orderBy, "-") {
			orderBy = orderBy[1:]
			direction = "DESC"
		}
		if _, ok := columnMapping[orderBy]; !ok {
			// Fuzzy match to column_name
			orderBy = fuzzyColumnNames[strings.ToLower(orderBy)]
		}
		if _, ok := columnMapping[orderBy]; !ok {
			continue
		}
		if orderBy == "example" && svc.Deployment() != connector.TESTING {
			continue
		}
		if orderBy == "id" {
			orderedByID = true
		}
		if countOrderBy > 0 {
			stmt.WriteString(",")
		}
		countOrderBy++
		stmt.WriteString(" ")
		stmt.WriteString(orderBy)
		stmt.WriteString(" ")
		stmt.WriteString(direction)
	}
	if !orderedByID {
		if countOrderBy > 0 {
			stmt.WriteString(",")
		}
		stmt.WriteString(" id ASC")
	}

	// -- OFFSET/LIMIT --
	countArgsBeforeOffsetLimit := len(args)
	if (query.Offset > 0 || query.Limit > 0) && query.Offset >= 0 && query.Limit >= 0 {
		switch svc.db.DriverName() {
		case "mysql":
			// LIMIT is required to use OFFSET
			stmt.WriteString(" LIMIT ?, ?")
			args = append(args, query.Offset)
			if query.Limit > 0 {
				args = append(args, query.Limit)
			} else {
				args = append(args, 1<<62) // Infinity
			}
		case "mssql":
			// OFFSET is required to use FETCH NEXT
			stmt.WriteString(" OFFSET ? ROWS")
			args = append(args, query.Offset)
			if query.Limit > 0 {
				stmt.WriteString(" FETCH NEXT ? ROWS ONLY")
				args = append(args, query.Limit)
			}
		case "pgx":
			if query.Offset > 0 {
				stmt.WriteString(" OFFSET ? ROWS")
				args = append(args, query.Offset)
			}
			if query.Limit > 0 {
				stmt.WriteString(" LIMIT ?")
				args = append(args, query.Limit)
			}
		}
	}

	// Query
	stmtStr := svc.db.ConformArgPlaceholders(stmt.String())
	f1 := func() (err error) {
		// Query for the objects
		rows, err := svc.db.QueryContext(ctx, stmtStr, args...)
		if err != nil {
			return errors.Trace(err)
		}
		defer rows.Close()
		scanArgs := []any{}
		for _, k := range columnsInOrder {
			scanArgs = append(scanArgs, columnMapping[k])
		}
		testing := svc.Deployment() == connector.TESTING
		for rows.Next() {
			err := rows.Scan(scanArgs...)
			if err != nil {
				return errors.Trace(err)
			}
			err = sequel.ApplyBindings(scanArgs...)
			if err != nil {
				return errors.Trace(err)
			}
			copy := obj
			if !testing {
				copy.Example = ""
			}
			objs = append(objs, &copy)
		}
		return nil
	}
	f2 := func() (err error) {
		// Query for the total count
		p := strings.Index(stmtStr, "FROM "+tableName+" WHERE ")
		q := strings.Index(stmtStr, "ORDER BY ")
		err = svc.db.QueryRowContext(ctx, "SELECT COUNT(*) "+stmtStr[p:q], args[:countArgsBeforeOffsetLimit]...).Scan(&totalCount)
		return errors.Trace(err)
	}
	if !query.Key.IsZero() || (query.Offset == 0 && query.Limit == 0) {
		// No need to count separately when fetching by key or when fetching the entire dataset
		err = f1()
		totalCount = len(objs)
	} else {
		err = svc.Parallel(f1, f2)
	}
	if err != nil {
		return nil, 0, errors.Trace(err)
	}
	return objs, totalCount, nil
}

/*
Lookup returns the single object matching the query. It errors if more than one object matches the query.
*/
func (svc *Service) Lookup(ctx context.Context, query serviceapi.Query) (obj *serviceapi.Obj, found bool, err error) {
	err = query.Validate(ctx)
	if err != nil {
		return nil, false, errors.Trace(err, http.StatusBadRequest)
	}
	query.Offset = 0
	query.Limit = 2
	objs, _, err := svc.List(ctx, query)
	if err != nil {
		return nil, false, errors.Trace(err)
	}
	switch len(objs) {
	case 0:
		return nil, false, nil
	case 1:
		return objs[0], true, nil
	default:
		return nil, false, errors.New("more than one object matched the query")
	}
}

/*
MustLookup returns the single object matching the query. It errors unless exactly one object matches the query.
*/
func (svc *Service) MustLookup(ctx context.Context, query serviceapi.Query) (obj *serviceapi.Obj, err error) {
	obj, found, err := svc.Lookup(ctx, query)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if !found {
		return nil, errors.New("object not found", http.StatusNotFound)
	}
	return obj, nil
}

/*
Load returns the object associated with the key.
*/
func (svc *Service) Load(ctx context.Context, objKey serviceapi.ObjKey) (obj *serviceapi.Obj, found bool, err error) {
	if objKey.IsZero() {
		return nil, false, nil
	}
	obj, found, err = svc.Lookup(ctx, serviceapi.Query{Key: objKey})
	return obj, found, errors.Trace(err)
}

/*
MustLoad returns the object associated with the key. It errors if the object is not found.
*/
func (svc *Service) MustLoad(ctx context.Context, objKey serviceapi.ObjKey) (obj *serviceapi.Obj, err error) {
	obj, ok, err := svc.Load(ctx, objKey)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if !ok {
		return nil, errors.New("object not found", http.StatusNotFound)
	}
	return obj, nil
}

/*
BulkLoad returns the objects matching the keys.
*/
func (svc *Service) BulkLoad(ctx context.Context, objKeys []serviceapi.ObjKey) (objs []*serviceapi.Obj, err error) {
	if len(objKeys) == 0 {
		return nil, nil
	}
	sort.Slice(objKeys, func(i, j int) bool {
		return objKeys[i].ID < objKeys[j].ID
	})
	for len(objKeys) > 0 {
		n := len(objKeys)
		var batch []*serviceapi.Obj
		if n <= bulkBatchSize {
			batch, _, err = svc.List(ctx, serviceapi.Query{
				Keys: objKeys,
			})
			objKeys = nil
		} else {
			batch, _, err = svc.List(ctx, serviceapi.Query{
				Keys: objKeys[:bulkBatchSize],
			})
			objKeys = objKeys[bulkBatchSize:]
		}
		if err != nil {
			return nil, errors.Trace(err)
		}
		objs = append(objs, batch...)
	}
	return objs, nil
}

/*
BulkDelete deletes the objects matching the keys, returning the keys of the deleted objects.
*/
func (svc *Service) BulkDelete(ctx context.Context, objKeys []serviceapi.ObjKey) (deletedKeys []serviceapi.ObjKey, err error) {
	if len(objKeys) == 0 {
		return nil, nil
	}
	// Sort by ID to optimize disk access
	sort.Slice(objKeys, func(i, j int) bool {
		return objKeys[i].ID < objKeys[j].ID
	})
	tenantID := svc.tenantOf(ctx)
	for len(objKeys) > 0 {
		var batch []serviceapi.ObjKey
		if len(objKeys) <= bulkBatchSize {
			batch = objKeys
			objKeys = nil
		} else {
			batch = objKeys[:bulkBatchSize]
			objKeys = objKeys[bulkBatchSize:]
		}
		writeIDList := func(stmt *strings.Builder) {
			for i, k := range batch {
				if i > 0 {
					stmt.WriteString(",")
				}
				stmt.WriteString(strconv.Itoa(k.ID))
			}
		}
		switch svc.db.DriverName() {
		case "mysql":
			// MySQL doesn't support RETURNING; use a transaction with SELECT FOR UPDATE
			tx, err := svc.db.BeginTx(ctx, nil)
			if err != nil {
				return deletedKeys, errors.Trace(err)
			}
			var selectStmt strings.Builder
			selectStmt.WriteString("SELECT id FROM ")
			selectStmt.WriteString(tableName)
			selectStmt.WriteString(" WHERE tenant_id=? AND id IN (")
			writeIDList(&selectStmt)
			selectStmt.WriteString(") FOR UPDATE")
			selectStmtStr := svc.db.ConformArgPlaceholders(selectStmt.String())
			rows, err := tx.QueryContext(ctx, selectStmtStr, tenantID)
			if err != nil {
				tx.Rollback()
				return deletedKeys, errors.Trace(err)
			}
			var foundKeys []serviceapi.ObjKey
			for rows.Next() {
				var key serviceapi.ObjKey
				err = rows.Scan(&key)
				if err != nil {
					rows.Close()
					tx.Rollback()
					return deletedKeys, errors.Trace(err)
				}
				foundKeys = append(foundKeys, key)
			}
			rows.Close()
			if len(foundKeys) > 0 {
				var deleteStmt strings.Builder
				deleteStmt.WriteString("DELETE FROM ")
				deleteStmt.WriteString(tableName)
				deleteStmt.WriteString(" WHERE tenant_id=? AND id IN (")
				writeIDList(&deleteStmt)
				deleteStmt.WriteString(")")
				deleteStmtStr := svc.db.ConformArgPlaceholders(deleteStmt.String())
				_, err = tx.ExecContext(ctx, deleteStmtStr, tenantID)
				if err != nil {
					tx.Rollback()
					return deletedKeys, errors.Trace(err)
				}
			}
			err = tx.Commit()
			if err != nil {
				return deletedKeys, errors.Trace(err)
			}
			deletedKeys = append(deletedKeys, foundKeys...)
		case "pgx":
			// PostgreSQL supports RETURNING
			var stmt strings.Builder
			stmt.WriteString("DELETE FROM ")
			stmt.WriteString(tableName)
			stmt.WriteString(" WHERE tenant_id=? AND id IN (")
			writeIDList(&stmt)
			stmt.WriteString(") RETURNING id")
			stmtStr := svc.db.ConformArgPlaceholders(stmt.String())
			rows, err := svc.db.QueryContext(ctx, stmtStr, tenantID)
			if err != nil {
				return deletedKeys, errors.Trace(err)
			}
			for rows.Next() {
				var key serviceapi.ObjKey
				err = rows.Scan(&key)
				if err != nil {
					rows.Close()
					return deletedKeys, errors.Trace(err)
				}
				deletedKeys = append(deletedKeys, key)
			}
			rows.Close()
		case "mssql":
			// SQL Server supports OUTPUT DELETED
			var stmt strings.Builder
			stmt.WriteString("DELETE FROM ")
			stmt.WriteString(tableName)
			stmt.WriteString(" OUTPUT DELETED.id")
			stmt.WriteString(" WHERE tenant_id=? AND id IN (")
			writeIDList(&stmt)
			stmt.WriteString(")")
			stmtStr := svc.db.ConformArgPlaceholders(stmt.String())
			rows, err := svc.db.QueryContext(ctx, stmtStr, tenantID)
			if err != nil {
				return deletedKeys, errors.Trace(err)
			}
			for rows.Next() {
				var key serviceapi.ObjKey
				err = rows.Scan(&key)
				if err != nil {
					rows.Close()
					return deletedKeys, errors.Trace(err)
				}
				deletedKeys = append(deletedKeys, key)
			}
			rows.Close()
		}
	}
	return deletedKeys, nil
}

/*
BulkStore updates multiple objects, returning the keys of the stored objects.
*/
func (svc *Service) BulkStore(ctx context.Context, objs []*serviceapi.Obj) (storedKeys []serviceapi.ObjKey, err error) {
	storedKeys, err = svc.bulkUpdate(ctx, objs, false)
	return storedKeys, errors.Trace(err)
}

/*
BulkRevise updates multiple objects, returning the number of rows affected.
Only rows with matching revisions are updated.
*/
func (svc *Service) BulkRevise(ctx context.Context, objs []*serviceapi.Obj) (revisedKeys []serviceapi.ObjKey, err error) {
	revisedKeys, err = svc.bulkUpdate(ctx, objs, true)
	return revisedKeys, errors.Trace(err)
}

// bulkUpdate implements [Service.BulkStore] and [Service.BulkRevise].
func (svc *Service) bulkUpdate(ctx context.Context, objs []*serviceapi.Obj, checkRevision bool) (storedKeys []serviceapi.ObjKey, err error) {
	if len(objs) == 0 {
		return nil, nil
	}
	// Validate all objects before updating any
	for i, obj := range objs {
		if obj == nil {
			return nil, errors.New("nil object", http.StatusBadRequest, "index", i)
		}
		if obj.Key.IsZero() {
			return nil, errors.New("zero key", http.StatusBadRequest, "index", i)
		}
		err = obj.Validate(ctx)
		if err != nil {
			return nil, errors.Trace(err, http.StatusBadRequest, "index", i)
		}
	}
	// Sort by ID to optimize disk access
	sort.Slice(objs, func(i, j int) bool {
		return objs[i].Key.ID < objs[j].Key.ID
	})
	testing := svc.Deployment() == connector.TESTING
	tenantID := svc.tenantOf(ctx)

	// Determine column order and params per row from the first object
	if !testing {
		objs[0].Example = ""
	}
	firstMapping, err := svc.mapColumnsOnUpdate(ctx, objs[0])
	if err != nil {
		return nil, errors.Trace(err)
	}
	delete(firstMapping, "tenant_id")
	delete(firstMapping, "revision")
	delete(firstMapping, "created_at")
	firstMapping["updated_at"] = sequel.UnsafeSQL(svc.db.NowUTC())
	columnsInOrder := slices.Sorted(maps.Keys(firstMapping))
	paramsPerRow := 0
	for _, k := range columnsInOrder {
		if _, ok := firstMapping[k].(sequel.UnsafeSQL); ok {
			continue
		}
		if _, ok := firstMapping[k].(int); ok {
			continue
		}
		paramsPerRow++
	}
	if paramsPerRow == 0 {
		paramsPerRow = 1 // Avoid division by zero
	}
	batchSize := max(maxParamsInQuery/paramsPerRow, 1)
	batchSize = min(batchSize, bulkBatchSize)

	// Process in batches
	for batchStart := 0; batchStart < len(objs); batchStart += batchSize {
		batchEnd := min(batchStart+batchSize, len(objs))
		batch := objs[batchStart:batchEnd]

		// Build column mappings for this batch
		batchMappings := make([]map[string]any, len(batch))
		for i, obj := range batch {
			if batchStart == 0 && i == 0 {
				batchMappings[i] = firstMapping
			} else {
				if !testing {
					obj.Example = ""
				}
				batchMappings[i], err = svc.mapColumnsOnUpdate(ctx, obj)
				if err != nil {
					return nil, errors.Trace(err)
				}
				delete(batchMappings[i], "tenant_id")
				delete(batchMappings[i], "revision")
				delete(batchMappings[i], "created_at")
				batchMappings[i]["updated_at"] = sequel.UnsafeSQL(svc.db.NowUTC())
			}
		}

		// Build CASE UPDATE statement
		var stmt strings.Builder
		stmt.WriteString("UPDATE ")
		stmt.WriteString(tableName)
		stmt.WriteString(" SET ")
		args := []any{}
		for _, col := range columnsInOrder {
			stmt.WriteString(col)
			stmt.WriteString("=")
			if len(batchMappings) > 1 {
				stmt.WriteString("CASE")
				for i, mapping := range batchMappings {
					stmt.WriteString(" WHEN id=")
					stmt.WriteString(strconv.Itoa(batch[i].Key.ID))
					stmt.WriteString(" THEN ")
					v := mapping[col]
					if unsafeSQL, ok := v.(sequel.UnsafeSQL); ok {
						stmt.WriteString("(")
						stmt.WriteString(string(unsafeSQL))
						stmt.WriteString(")")
					} else if vint, ok := v.(int); ok {
						stmt.WriteString(strconv.Itoa(vint))
					} else {
						stmt.WriteString("?")
						args = append(args, v)
					}
				}
				stmt.WriteString(" END")
			} else {
				v := batchMappings[0][col]
				if unsafeSQL, ok := v.(sequel.UnsafeSQL); ok {
					stmt.WriteString("(")
					stmt.WriteString(string(unsafeSQL))
					stmt.WriteString(")")
				} else if vint, ok := v.(int); ok {
					stmt.WriteString(strconv.Itoa(vint))
				} else {
					stmt.WriteString("?")
					args = append(args, v)
				}
			}
			stmt.WriteString(",")
		}
		stmt.WriteString("revision=(1+revision)")
		if svc.db.DriverName() == "mssql" {
			stmt.WriteString(" OUTPUT INSERTED.id")
		}
		stmt.WriteString(" WHERE tenant_id=")
		stmt.WriteString(strconv.Itoa(tenantID))
		if !checkRevision {
			stmt.WriteString(" AND id IN (")
			for i, obj := range batch {
				if i > 0 {
					stmt.WriteString(",")
				}
				stmt.WriteString(strconv.Itoa(obj.Key.ID))
			}
			stmt.WriteString(")")
		} else {
			stmt.WriteString(" AND (")
			for i, obj := range batch {
				if i > 0 {
					stmt.WriteString(" OR ")
				}
				stmt.WriteString("id=")
				stmt.WriteString(strconv.Itoa(obj.Key.ID))
				stmt.WriteString(" AND revision=")
				stmt.WriteString(strconv.Itoa(obj.Revision))
			}
			stmt.WriteString(")")
		}
		if svc.db.DriverName() == "pgx" {
			stmt.WriteString(" RETURNING id")
		}
		stmtStr := svc.db.ConformArgPlaceholders(stmt.String())
		switch svc.db.DriverName() {
		case "mysql":
			// MySQL doesn't support RETURNING; use a transaction with SELECT FOR UPDATE
			tx, err := svc.db.BeginTx(ctx, nil)
			if err != nil {
				return storedKeys, errors.Trace(err)
			}
			var selectStmt strings.Builder
			selectStmt.WriteString("SELECT id FROM ")
			selectStmt.WriteString(tableName)
			selectStmt.WriteString(" WHERE tenant_id=?")
			if !checkRevision {
				selectStmt.WriteString(" AND id IN (")
				for i, obj := range batch {
					if i > 0 {
						selectStmt.WriteString(",")
					}
					selectStmt.WriteString(strconv.Itoa(obj.Key.ID))
				}
				selectStmt.WriteString(")")
			} else {
				selectStmt.WriteString(" AND (")
				for i, obj := range batch {
					if i > 0 {
						selectStmt.WriteString(" OR ")
					}
					selectStmt.WriteString("id=")
					selectStmt.WriteString(strconv.Itoa(obj.Key.ID))
					selectStmt.WriteString(" AND revision=")
					selectStmt.WriteString(strconv.Itoa(obj.Revision))
				}
				selectStmt.WriteString(")")
			}
			selectStmt.WriteString(" FOR UPDATE")
			selectStmtStr := svc.db.ConformArgPlaceholders(selectStmt.String())
			rows, err := tx.QueryContext(ctx, selectStmtStr, tenantID)
			if err != nil {
				tx.Rollback()
				return storedKeys, errors.Trace(err)
			}
			var foundKeys []serviceapi.ObjKey
			for rows.Next() {
				var key serviceapi.ObjKey
				err = rows.Scan(&key)
				if err != nil {
					rows.Close()
					tx.Rollback()
					return storedKeys, errors.Trace(err)
				}
				foundKeys = append(foundKeys, key)
			}
			rows.Close()
			if len(foundKeys) > 0 {
				_, err = tx.ExecContext(ctx, stmtStr, args...)
				if err != nil {
					tx.Rollback()
					return storedKeys, errors.Trace(err)
				}
			}
			err = tx.Commit()
			if err != nil {
				return storedKeys, errors.Trace(err)
			}
			storedKeys = append(storedKeys, foundKeys...)
		default:
			// pgx and mssql support RETURNING/OUTPUT
			rows, err := svc.db.QueryContext(ctx, stmtStr, args...)
			if err != nil {
				return storedKeys, errors.Trace(err)
			}
			for rows.Next() {
				var key serviceapi.ObjKey
				err = rows.Scan(&key)
				if err != nil {
					rows.Close()
					return storedKeys, errors.Trace(err)
				}
				storedKeys = append(storedKeys, key)
			}
			rows.Close()
		}
	}
	return storedKeys, nil
}

/*
BulkCreate creates multiple objects, returning their keys.
*/
func (svc *Service) BulkCreate(ctx context.Context, objs []*serviceapi.Obj) (objKeys []serviceapi.ObjKey, err error) {
	if len(objs) == 0 {
		return nil, nil
	}
	// Validate all objects before inserting any
	for i, obj := range objs {
		if obj == nil {
			return nil, errors.New("nil object", http.StatusBadRequest, "index", i)
		}
		err = obj.Validate(ctx)
		if err != nil {
			return nil, errors.Trace(err, http.StatusBadRequest, "index", i)
		}
	}
	testing := svc.Deployment() == connector.TESTING
	tenantID := svc.tenantOf(ctx)

	// Determine column order and params per row from the first object
	if !testing {
		objs[0].Example = ""
	}
	firstMapping, err := svc.mapColumnsOnInsert(ctx, objs[0])
	if err != nil {
		return nil, errors.Trace(err)
	}
	firstMapping["tenant_id"] = tenantID
	firstMapping["revision"] = 1
	firstMapping["created_at"] = sequel.UnsafeSQL(svc.db.NowUTC())
	firstMapping["updated_at"] = sequel.UnsafeSQL(svc.db.NowUTC())
	columnsInOrder := slices.Sorted(maps.Keys(firstMapping))
	paramsPerRow := 0
	for _, k := range columnsInOrder {
		if _, ok := firstMapping[k].(sequel.UnsafeSQL); ok {
			continue
		}
		if _, ok := firstMapping[k].(int); ok {
			continue
		}
		paramsPerRow++
	}
	if paramsPerRow == 0 {
		paramsPerRow = 1 // Avoid division by zero
	}
	batchSize := max(maxParamsInQuery/paramsPerRow, 1)
	batchSize = min(batchSize, bulkBatchSize)

	// Process in batches
	objKeys = make([]serviceapi.ObjKey, 0, len(objs))
	for batchStart := 0; batchStart < len(objs); batchStart += batchSize {
		batchEnd := min(batchStart+batchSize, len(objs))

		// Build column mappings and multi-row INSERT statement for this batch
		var stmt strings.Builder
		stmt.WriteString("INSERT INTO ")
		stmt.WriteString(tableName)
		stmt.WriteString(" (")
		stmt.WriteString(strings.Join(columnsInOrder, ","))
		stmt.WriteString(")")
		args := []any{}
		switch svc.db.DriverName() {
		case "mssql":
			stmt.WriteString(" OUTPUT INSERTED.id")
		}
		stmt.WriteString(" VALUES ")
		for i, obj := range objs[batchStart:batchEnd] {
			// The first object was already mapped above
			var mapping map[string]any
			if batchStart == 0 && i == 0 {
				mapping = firstMapping
			} else {
				if !testing {
					obj.Example = ""
				}
				mapping, err = svc.mapColumnsOnInsert(ctx, obj)
				if err != nil {
					return nil, errors.Trace(err)
				}
				mapping["tenant_id"] = tenantID
				mapping["revision"] = 1
				mapping["created_at"] = sequel.UnsafeSQL(svc.db.NowUTC())
				mapping["updated_at"] = sequel.UnsafeSQL(svc.db.NowUTC())
			}
			if i > 0 {
				stmt.WriteString(",")
			}
			stmt.WriteString("(")
			for j, k := range columnsInOrder {
				if j > 0 {
					stmt.WriteString(",")
				}
				v := mapping[k]
				if unsafeSQL, ok := v.(sequel.UnsafeSQL); ok {
					stmt.WriteString("(")
					stmt.WriteString(string(unsafeSQL))
					stmt.WriteString(")")
				} else if vint, ok := v.(int); ok {
					stmt.WriteString(strconv.Itoa(vint))
				} else {
					stmt.WriteString("?")
					args = append(args, v)
				}
			}
			stmt.WriteString(")")
		}
		switch svc.db.DriverName() {
		case "pgx":
			stmt.WriteString(" RETURNING id")
		}
		stmtStr := svc.db.ConformArgPlaceholders(stmt.String())

		// Execute and retrieve generated IDs
		switch svc.db.DriverName() {
		case "mysql":
			res, err := svc.db.ExecContext(ctx, stmtStr, args...)
			if err != nil {
				return nil, errors.Trace(err)
			}
			firstID, err := res.LastInsertId()
			if err != nil {
				return nil, errors.Trace(err)
			}
			for i := range batchEnd - batchStart {
				objKeys = append(objKeys, serviceapi.ObjKey{ID: int(firstID) + i})
			}
		default:
			rows, err := svc.db.QueryContext(ctx, stmtStr, args...)
			if err != nil {
				return nil, errors.Trace(err)
			}
			for rows.Next() {
				var key serviceapi.ObjKey
				err = rows.Scan(&key)
				if err != nil {
					rows.Close()
					return nil, errors.Trace(err)
				}
				objKeys = append(objKeys, key)
			}
			rows.Close()
		}
	}
	return objKeys, nil
}

/*
Purge deletes all objects matching the query, returning the keys of the deleted objects.
*/
func (svc *Service) Purge(ctx context.Context, query serviceapi.Query) (deletedKeys []serviceapi.ObjKey, err error) {
	query.Select = "id"
	objs, _, err := svc.List(ctx, query)
	keys := make([]serviceapi.ObjKey, len(objs))
	for i, obj := range objs {
		keys[i] = obj.Key
	}
	deletedKeys, err = svc.BulkDelete(ctx, keys)
	return deletedKeys, errors.Trace(err)
}

/*
Count returns the number of objects matching the query, disregarding pagination.
*/
func (svc *Service) Count(ctx context.Context, query serviceapi.Query) (count int, err error) {
	query.Offset = 0
	query.Limit = 1
	query.Select = "id"
	_, count, err = svc.List(ctx, query)
	return count, errors.Trace(err)
}
