/*
Copyright (c) 2025-2026 Microbus LLC and various contributors

Licensed under the Apache License, Version 2.0 (the "License");
you may not use this file except in compliance with the License.
You may obtain a copy of the License at

	http://www.apache.org/licenses/LICENSE-2.0

Unless required by applicable law or agreed to in writing, software
distributed under the License is distributed on an "AS IS" BASIS,
WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
See the License for the specific language governing permissions and
limitations under the License.
*/

package sequel

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"io/fs"
	"math"
	"net/url"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	_ "github.com/denisenkom/go-mssqldb"
	"github.com/denisenkom/go-mssqldb/msdsn"
	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/microbus-io/errors"
	_ "modernc.org/sqlite"
)

var (
	singletonsMap      = map[string]*DB{}
	singletonMutex     sync.Mutex
	testingDSNs        = map[string]string{}
	testingGlobalMutex sync.Mutex
	testingMutexes     = map[string]*sync.Mutex{}

	valuesClausePattern = regexp.MustCompile(`(?i)\s+VALUES\s*`)
)

/*
DB is an enhanced database connection that
  - Limits the size of the connection pool to each server to approx the sqrt of the number of clients
  - Performs schema migration
  - Automatically creates and connects to a localhost database while testing
*/
type DB struct {
	*sql.DB
	driverName          string
	singletonKey        string
	refCount            int
	mutex               sync.Mutex
	dropTestingDatabase func() (err error)
}

/*
Open returns a database connection to the named data source.

If a driver name is not provided, it is inferred from the data source name on a best-effort basis.
Drivers currently supported: "mysql" (MySQL), "pgx" (Postgres), "mssql" (SQL Server) or "sqlite" (SQLite).

Example data source name for each of the supported drivers:
  - mysql: username:password@tcp(hostname:3306)/
  - pgx: postgres://username:password@hostname:5432/
  - mssql: sqlserver://username:password@hostname:1433
  - sqlite: file:path/to/database.sqlite
*/
func Open(driverName string, dataSourceName string) (db *DB, err error) {
	if dataSourceName == "" {
		return nil, errors.New("data source name is required")
	}
	if driverName == "mariadb" {
		driverName = "mysql"
	}
	if driverName == "sqlite3" {
		driverName = "sqlite"
	}
	if driverName == "" {
		driverName = inferDriverName(dataSourceName)
	}
	if driverName == "" {
		return nil, errors.New("driver name could not be inferred from data source name")
	}
	singletonKey := hashStr(driverName + "|" + dataSourceName)

	singletonMutex.Lock()
	singletonDB, ok := singletonsMap[singletonKey]
	if !ok {
		singletonDB = &DB{
			DB:           nil,
			driverName:   driverName,
			singletonKey: singletonKey,
			refCount:     0,
		}
		singletonsMap[singletonKey] = singletonDB
	}
	singletonMutex.Unlock()

	singletonDB.mutex.Lock()
	defer singletonDB.mutex.Unlock()
	if singletonDB.DB != nil {
		singletonDB.refCount++
		singletonDB.adjustConnectionLimits()
		return singletonDB, nil
	}

	var sqlDB *sql.DB
	switch driverName {
	case "mysql":
		cfg, err := mysql.ParseDSN(dataSourceName)
		if err != nil {
			return nil, errors.Trace(err)
		}
		if cfg.Params == nil {
			cfg.Params = map[string]string{}
		}
		// See https://github.com/go-sql-driver/mysql#dsn-data-source-name
		cfg.Params["parseTime"] = "true"
		cfg.Params["timeout"] = "4s"
		cfg.Params["readTimeout"] = "8s"
		cfg.Params["writeTimeout"] = "8s"
		sqlDB, err = sql.Open(driverName, cfg.FormatDSN())
		if err != nil {
			return nil, errors.Trace(err)
		}
		// Strict mode guards against errors
		// https://dev.mysql.com/doc/refman/5.7/en/sql-mode.html#sql-mode-strict
		// max_allowed_packet needs to be large enough to accommodate inserting large blobs.
		// max_allowed_packet can only be set globally.
		_, err = sqlDB.Exec(
			`SET
			GLOBAL sql_mode = 'STRICT_ALL_TABLES', SESSION sql_mode = 'STRICT_ALL_TABLES',
			GLOBAL max_allowed_packet = 134217728`, // 128MB
		)
		if err != nil {
			sqlDB.Close()
			return nil, errors.Trace(err)
		}
	case "sqlite":
		if !strings.Contains(dataSourceName, "busy_timeout") {
			// Set a busy_timeout so that concurrent writers retry on lock contention
			// instead of immediately returning SQLITE_BUSY. Without this, in-memory and
			// shared-cache SQLite databases serialize write transactions but bare writes
			// from concurrent connections fail rather than wait.
			if strings.Contains(dataSourceName, "?") {
				dataSourceName += "&_pragma=busy_timeout(1000)"
			} else {
				dataSourceName += "?_pragma=busy_timeout(1000)"
			}
		}
		sqlDB, err = sql.Open(driverName, dataSourceName)
		if err != nil {
			return nil, errors.Trace(err)
		}
	default:
		sqlDB, err = sql.Open(driverName, dataSourceName)
		if err != nil {
			return nil, errors.Trace(err)
		}
	}

	// Prepare the database struct
	singletonDB.DB = sqlDB
	singletonDB.refCount = 1
	singletonDB.adjustConnectionLimits()
	return singletonDB, nil
}

// IsLockContentionError returns true if the error indicates database lock contention or a deadlock.
// Such errors are transient and the operation can typically be retried.
// Recognizes lock errors from SQLite, MySQL, and PostgreSQL.
func IsLockContentionError(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "SQLITE_BUSY"), strings.Contains(msg, "database is locked"): // SQLite
		return true
	case strings.Contains(msg, "Deadlock found"), strings.Contains(msg, "Lock wait timeout"): // MySQL
		return true
	case strings.Contains(msg, "deadlock detected"): // PostgreSQL
		return true
	case strings.Contains(msg, "deadlock victim"), strings.Contains(msg, "was deadlocked"), // SQL Server (1205)
		strings.Contains(msg, "Lock request time out"): // SQL Server (1222)
		return true
	}
	return false
}

// Close closes the database connection.
func (db *DB) Close() (err error) {
	if db == nil {
		return nil
	}
	db.mutex.Lock()
	defer db.mutex.Unlock()
	if db.DB == nil || db.refCount == 0 {
		return nil
	}
	db.refCount--
	if db.refCount == 0 {
		err = db.DB.Close()
		db.DB = nil
		if db.dropTestingDatabase != nil {
			db.dropTestingDatabase()
		}
	} else {
		db.adjustConnectionLimits()
	}
	return errors.Trace(err)
}

// Exec shadows sql.DB.Exec and conforms arg placeholders for the driver.
func (db *DB) Exec(query string, args ...any) (sql.Result, error) {
	query, err := db.UnpackQuery(query)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return db.DB.Exec(query, args...)
}

// ExecContext shadows sql.DB.ExecContext and conforms arg placeholders for the driver.
func (db *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	query, err := db.UnpackQuery(query)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return db.DB.ExecContext(ctx, query, args...)
}

// Query shadows sql.DB.Query and conforms arg placeholders for the driver.
func (db *DB) Query(query string, args ...any) (*sql.Rows, error) {
	query, err := db.UnpackQuery(query)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return db.DB.Query(query, args...)
}

// QueryContext shadows sql.DB.QueryContext and conforms arg placeholders for the driver.
func (db *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	query, err := db.UnpackQuery(query)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return db.DB.QueryContext(ctx, query, args...)
}

// QueryRow shadows sql.DB.QueryRow and conforms arg placeholders for the driver.
func (db *DB) QueryRow(query string, args ...any) *sql.Row {
	query, _ = db.UnpackQuery(query)
	return db.DB.QueryRow(query, args...)
}

// QueryRowContext shadows sql.DB.QueryRowContext and conforms arg placeholders for the driver.
func (db *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	query, _ = db.UnpackQuery(query)
	return db.DB.QueryRowContext(ctx, query, args...)
}

// Prepare shadows sql.DB.Prepare and conforms arg placeholders for the driver.
func (db *DB) Prepare(query string) (*sql.Stmt, error) {
	query, err := db.UnpackQuery(query)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return db.DB.Prepare(query)
}

// PrepareContext shadows sql.DB.PrepareContext and conforms arg placeholders for the driver.
func (db *DB) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	query, err := db.UnpackQuery(query)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return db.DB.PrepareContext(ctx, query)
}

// DriverName is the name of the driver: "mysql", "pgx", "mssql" or "sqlite".
func (db *DB) DriverName() string {
	return db.driverName
}

// UnpackQuery expands virtual functions (e.g. NOW_UTC(), REGEXP_TEXT_SEARCH()) into
// driver-specific SQL expressions, and conforms arg placeholders
// to the syntax expected by the driver (e.g. ? to $1, $2 for PostgreSQL).
func (db *DB) UnpackQuery(query string) (string, error) {
	return unpackQuery(db.driverName, query)
}

// unpackQuery expands virtual functions and conforms arg placeholders for the driver.
func unpackQuery(driverName string, query string) (string, error) {
	query, err := expandVirtualFuncs(driverName, query)
	if err != nil {
		return "", errors.Trace(err)
	}
	query = conformPlaceholders(driverName, query)
	return query, nil
}

// Begin starts a transaction and returns a sequel.Tx that applies virtual function
// expansion and placeholder conforming.
func (db *DB) Begin() (*Tx, error) {
	sqlTx, err := db.DB.Begin()
	if err != nil {
		return nil, errors.Trace(err)
	}
	return &Tx{Tx: sqlTx, driverName: db.driverName}, nil
}

// BeginTx starts a transaction with the given options and returns a sequel.Tx that
// applies virtual function expansion and placeholder conforming.
func (db *DB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*Tx, error) {
	sqlTx, err := db.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return &Tx{Tx: sqlTx, driverName: db.driverName}, nil
}

// InsertReturnID executes an INSERT statement and returns the auto-generated ID for the named ID column.
func (db *DB) InsertReturnID(ctx context.Context, idColumn string, stmt string, args ...any) (int64, error) {
	return insertReturnID(ctx, db, db.driverName, idColumn, stmt, args...)
}

// insertReturnID executes an INSERT statement and returns the auto-generated ID for the named ID column.
func insertReturnID(ctx context.Context, qe Executor, driverName string, idColumn string, stmt string, args ...any) (int64, error) {
	switch driverName {
	case "mysql", "sqlite":
		res, err := qe.ExecContext(ctx, stmt, args...)
		if err != nil {
			return 0, errors.Trace(err)
		}
		id, err := res.LastInsertId()
		if err != nil {
			return 0, errors.Trace(err)
		}
		return id, nil
	case "pgx":
		var id int64
		err := qe.QueryRowContext(ctx, stmt+" RETURNING "+idColumn, args...).Scan(&id)
		if err != nil {
			return 0, errors.Trace(err)
		}
		return id, nil
	case "mssql":
		var id int64
		stmt, err := injectOutputInserted(stmt, idColumn)
		if err != nil {
			return 0, errors.Trace(err)
		}
		err = qe.QueryRowContext(ctx, stmt, args...).Scan(&id)
		if err != nil {
			return 0, errors.Trace(err)
		}
		return id, nil
	}
	return 0, errors.New("unsupported driver name: %s", driverName)
}

// injectOutputInserted rewrites an INSERT statement to include an OUTPUT INSERTED clause
// before the VALUES keyword, for use with MSSQL.
func injectOutputInserted(stmt string, idColumn string) (string, error) {
	loc := valuesClausePattern.FindStringIndex(stmt)
	if loc == nil {
		return "", errors.New("VALUES clause not found in INSERT statement")
	}
	return stmt[:loc[0]] + " OUTPUT INSERTED." + idColumn + stmt[loc[0]:], nil
}

// databaseNameFromDataSourceName extracts the database name part of the data source name.
func databaseNameFromDataSourceName(driverName string, dsn string) (databaseName string, err error) {
	if dsn == "" {
		return "", errors.New("empty dsn")
	}
	switch driverName {
	case "mysql":
		cfg, err := mysql.ParseDSN(dsn)
		if err != nil {
			return "", errors.New("error parsing data source name %s", dsn, err)
		}
		return cfg.DBName, nil
	case "pgx":
		_, err = pgx.ParseConfig(dsn)
		if err != nil {
			return "", errors.New("error parsing data source name %s", dsn, err)
		}
		u, err := url.Parse(dsn)
		if err != nil {
			return "", errors.New("error parsing data source name %s", dsn, err)
		}
		return strings.TrimPrefix(u.Path, "/"), nil
	case "mssql":
		// https://github.com/microsoft/go-mssqldb?tab=readme-ov-file#common-parameters
		_, _, err = msdsn.Parse(dsn)
		if err != nil {
			return "", errors.New("error parsing data source name %s", dsn, err)
		}
		u, err := url.Parse(dsn)
		if err != nil {
			return "", errors.New("error parsing data source name %s", dsn, err)
		}
		return u.Query().Get("database"), nil
	case "sqlite":
		// The DSN is the file path, optionally prefixed with "file:" and with query params
		path := dsn
		path = strings.TrimPrefix(path, "file:")
		if i := strings.Index(path, "?"); i >= 0 {
			path = path[:i]
		}
		return path, nil
	default:
		return "", errors.New("unsupported driver name %s", driverName)
	}
}

// setDatabaseInDataSourceName alters the database in the data source name and returns the new data source name.
func setDatabaseInDataSourceName(driverName string, dsn string, databaseName string) (alteredDSN string, err error) {
	if dsn == "" {
		return "", errors.New("empty dsn")
	}
	switch driverName {
	case "mysql":
		cfg, err := mysql.ParseDSN(dsn)
		if err != nil {
			return "", errors.New("error parsing data source name %s", dsn, err)
		}
		cfg.DBName = databaseName
		alteredDSN = cfg.FormatDSN()
		return alteredDSN, nil
	case "pgx":
		_, err = pgx.ParseConfig(dsn)
		if err != nil {
			return "", errors.New("error parsing data source name %s", dsn, err)
		}
		u, err := url.Parse(dsn)
		if err != nil {
			return "", errors.New("error parsing data source name %s", dsn, err)
		}
		u.Path = "/" + databaseName
		alteredDSN = u.String()
		return alteredDSN, nil
	case "mssql":
		// https://github.com/microsoft/go-mssqldb?tab=readme-ov-file#common-parameters
		_, _, err = msdsn.Parse(dsn)
		if err != nil {
			return "", errors.New("error parsing data source name %s", dsn, err)
		}
		u, err := url.Parse(dsn)
		if err != nil {
			return "", errors.New("error parsing data source name %s", dsn, err)
		}
		q := u.Query()
		if databaseName == "" {
			q.Del("database")
		} else {
			q.Set("database", databaseName)
		}
		u.RawQuery = q.Encode()
		alteredDSN = u.String()
		return alteredDSN, nil
	case "sqlite":
		// Replace the file path in the DSN, preserving any "file:" prefix and query params
		if strings.HasPrefix(dsn, "file:") {
			rest := dsn[5:]
			if i := strings.Index(rest, "?"); i >= 0 {
				return "file:" + databaseName + rest[i:], nil
			}
			return "file:" + databaseName, nil
		}
		return databaseName, nil
	default:
		return "", errors.New("unsupported driver name %s", driverName)
	}
}

// inferDriverName tries to infer the driver name from the data source name.
func inferDriverName(dataSourceName string) (driverName string) {
	if dataSourceName == "" {
		return ""
	}
	if strings.HasPrefix(dataSourceName, "postgres://") {
		return "pgx"
	}
	if strings.HasPrefix(dataSourceName, "sqlserver://") {
		return "mssql"
	}
	if strings.HasPrefix(dataSourceName, "file:") {
		return "sqlite"
	}
	if dataSourceName == ":memory:" {
		return "sqlite"
	}
	if strings.HasSuffix(dataSourceName, ".db") || strings.HasSuffix(dataSourceName, ".sqlite") || strings.HasSuffix(dataSourceName, ".sqlite3") {
		return "sqlite"
	}
	if strings.Contains(dataSourceName, "tcp(") {
		return "mysql"
	}
	if strings.Contains(dataSourceName, ":3306") {
		return "mysql"
	}
	if strings.Contains(dataSourceName, ":5432") {
		return "pgx"
	}
	if strings.Contains(dataSourceName, ":1433") {
		return "mssql"
	}
	return ""
}

/*
OpenTesting opens a connection to a uniquely named database for testing purposes.
A database is created for each unique test at the database instance pointed to by the input DSN.

If a driver name is not provided, it is inferred from the data source name on a best-effort basis.
Drivers currently supported: "mysql" (MySQL), "pgx" (Postgres), "mssql" (SQL Server) or "sqlite" (SQLite).

If a data source name is not provided, the following defaults are used based on the driver name:
  - (empty): SQLite in-memory database
  - sqlite: SQLite in-memory database
  - mysql: root:root@tcp(127.0.0.1:3306)/
  - pgx: postgres://postgres:postgres@127.0.0.1:5432/
  - mssql: sqlserver://sa:Password123@127.0.0.1:1433
*/
func OpenTesting(driverName string, dataSourceName string, uniqueTestID string) (db *DB, err error) {
	// Set default connection to localhost
	if dataSourceName == "" {
		switch driverName {
		case "", "sqlite":
			dataSourceName = "file:?mode=memory&cache=shared"
		case "mysql":
			dataSourceName = "root:root@tcp(127.0.0.1:3306)/"
		case "pgx":
			dataSourceName = "postgres://postgres:postgres@127.0.0.1:5432/"
		case "mssql":
			dataSourceName = "sqlserver://sa:Password123@127.0.0.1:1433"
		default:
			return nil, errors.New("unsupported driver name: %s", driverName)
		}
	}
	if driverName == "" {
		driverName = inferDriverName(dataSourceName)
	}

	cacheKey := hashStr(driverName + "|" + dataSourceName + "|" + uniqueTestID)

	// Obtain the mutex for the unique test
	testingGlobalMutex.Lock()
	testingMux, ok := testingMutexes[cacheKey]
	if !ok {
		testingMux = &sync.Mutex{}
		testingMutexes[cacheKey] = testingMux
	}
	testingDataSourceName, ok := testingDSNs[cacheKey]
	testingGlobalMutex.Unlock()

	// Check if a database was previously created for this test
	if ok {
		db, err = Open(driverName, testingDataSourceName)
		return db, errors.Trace(err)
	}

	testingMux.Lock()
	defer testingMux.Unlock()

	// Generate a database name
	baseDatabaseName, err := databaseNameFromDataSourceName(driverName, dataSourceName)
	if err != nil {
		return nil, errors.Trace(err)
	}
	if baseDatabaseName != "" {
		baseDatabaseName = strings.ToLower(baseDatabaseName) + "_"
	}
	now := time.Now().UTC()
	testingDatabaseName := "testing_" + now.Format("15") + "_" + baseDatabaseName + strings.ToLower(uniqueTestID)
	testingDatabaseName = regexp.MustCompile(`[^a-z0-9_]`).ReplaceAllString(testingDatabaseName, "_")

	// For server-based drivers, open the master database and CREATE/DROP DATABASE.
	// SQLite uses in-memory databases for testing, so this step is skipped.
	if driverName != "sqlite" {
		masterDataSourceName, err := setDatabaseInDataSourceName(driverName, dataSourceName, "")
		if err != nil {
			return nil, errors.Trace(err)
		}
		masterDB, err := Open(driverName, masterDataSourceName)
		if err != nil {
			return nil, errors.New("failed to open master database", err)
		}
		defer masterDB.Close()

		// Create the testing database
		_, err = masterDB.Exec("DROP DATABASE IF EXISTS " + testingDatabaseName)
		if err != nil {
			return nil, errors.New("failed to drop database %s", testingDatabaseName, err)
		}
		_, err = masterDB.Exec("CREATE DATABASE " + testingDatabaseName)
		if err != nil {
			return nil, errors.New("failed to create database %s", testingDatabaseName, err)
		}

		// Cleanup leftover testing databases, on a best-effort basis.
		// A testing database is considered leftover if it is more than 1 to 2 hours old.
		stmt := ""
		switch driverName {
		case "mysql":
			stmt = "SHOW DATABASES"
		case "pgx":
			stmt = "SELECT datname FROM pg_database"
		case "mssql":
			stmt = "SELECT name FROM sys.databases"
		}
		rows, err := masterDB.Query(stmt)
		if err == nil {
			defer rows.Close()
			re := regexp.MustCompile(`^testing_[0-2][0-9]_`)
			var leftoverDatabaseNames []string
			h14 := now.Add(-time.Hour).Format("15")
			h15 := now.Format("15")
			h16 := now.Add(time.Hour).Format("15")
			for rows.Next() {
				var databaseName string
				rows.Scan(&databaseName)
				if re.MatchString(databaseName) &&
					!strings.HasPrefix(databaseName, "testing_"+h14+"_") &&
					!strings.HasPrefix(databaseName, "testing_"+h15+"_") &&
					!strings.HasPrefix(databaseName, "testing_"+h16+"_") {
					leftoverDatabaseNames = append(leftoverDatabaseNames, databaseName)
				}
			}
			for _, databaseName := range leftoverDatabaseNames {
				masterDB.Exec("DROP DATABASE IF EXISTS " + databaseName)
			}
		}
		masterDB.Close()
	}

	// Open the new database
	testingDataSourceName, err = setDatabaseInDataSourceName(driverName, dataSourceName, testingDatabaseName)
	if err != nil {
		return nil, errors.Trace(err)
	}
	testingDB, err := Open(driverName, testingDataSourceName)
	if err != nil {
		return nil, errors.New("failed to open testing database", err)
	}

	// Drop the testing database when it's no longer used.
	// SQLite uses in-memory databases that are automatically freed when all connections close.
	if driverName != "sqlite" {
		testingDB.dropTestingDatabase = func() (err error) {
			masterDataSourceName, err := setDatabaseInDataSourceName(driverName, dataSourceName, "")
			if err != nil {
				return errors.Trace(err)
			}
			masterDB, err := Open(driverName, masterDataSourceName)
			if err != nil {
				return errors.New("failed to open master database", err)
			}
			defer masterDB.Close()
			_, err = masterDB.Exec("DROP DATABASE IF EXISTS " + testingDatabaseName)
			if err != nil {
				return errors.New("failed to drop database %s", testingDatabaseName, err)
			}
			return nil
		}
	}

	// Cache for other microservices running in the same test
	testingGlobalMutex.Lock()
	testingDSNs[cacheKey] = testingDataSourceName
	testingGlobalMutex.Unlock()

	return testingDB, nil
}

// adjustConnectionLimits adjusts the size of the connection pool based on the ref count.
// It should be called under mutex lock.
//
//	n	maxIdle	maxOpen
//	1	1	4
//	2	2	6
//	5	3	8
//	10	4	10
//	17	5	12
//	26	6	14
//	37	7	16
//	50	8	18
//	65	9	20
//	82	10	22
//	101	11	24
//	...
//	1025	33	68
//	...
func (db *DB) adjustConnectionLimits() {
	maxIdle := math.Ceil(math.Sqrt(float64(db.refCount)))
	maxOpen := math.Ceil(maxIdle*2) + 2
	db.DB.SetMaxOpenConns(int(maxOpen))
	db.DB.SetMaxIdleConns(int(maxIdle))
}

// Migrate reads all #.sql files from the FS, and executes any new migrations in order of their file name.
// The order of execution is guaranteed only within the context of a sequence name.
func (db *DB) Migrate(sequenceName string, fileSys fs.FS) (err error) {
	// Init the schema migration table
	stmt := ""
	switch db.driverName {
	case "mysql":
		stmt = `
		CREATE TABLE IF NOT EXISTS sequel_migrations (
			seq_name VARCHAR(256) NOT NULL,
			seq_num INT NOT NULL,
			completed BOOL NOT NULL DEFAULT FALSE,
			completed_on DATETIME(3),
			locked_before DATETIME(3) NOT NULL DEFAULT (UTC_TIMESTAMP(3)),
			PRIMARY KEY (seq_name, seq_num)
		)`
	case "pgx":
		stmt = `
		CREATE TABLE IF NOT EXISTS sequel_migrations (
			seq_name VARCHAR(256) NOT NULL,
			seq_num INT NOT NULL,
			completed BOOL NOT NULL DEFAULT FALSE,
			completed_on TIMESTAMP(3),
			locked_before TIMESTAMP(3) NOT NULL DEFAULT (NOW() AT TIME ZONE 'UTC'),
			PRIMARY KEY (seq_name, seq_num)
		)`
	case "mssql":
		stmt = `
		IF OBJECT_ID(N'dbo.sequel_migrations', N'U') IS NULL BEGIN
			CREATE TABLE sequel_migrations (
				seq_name VARCHAR(256) NOT NULL,
				seq_num INT NOT NULL,
				completed BIT NOT NULL DEFAULT 0,
				completed_on DATETIME2(3),
				locked_before DATETIME2(3) NOT NULL DEFAULT (SYSUTCDATETIME()),
				PRIMARY KEY (seq_name, seq_num)
			)
		END`
	case "sqlite":
		stmt = `
		CREATE TABLE IF NOT EXISTS sequel_migrations (
			seq_name TEXT NOT NULL,
			seq_num INTEGER NOT NULL,
			completed INTEGER NOT NULL DEFAULT 0,
			completed_on TEXT,
			locked_before TEXT NOT NULL DEFAULT (STRFTIME('%Y-%m-%d %H:%M:%f', 'now')),
			PRIMARY KEY (seq_name, seq_num)
		)`
	default:
		return errors.New("unsupported driver name: %s", db.driverName)
	}
	_, err = db.Exec(stmt)
	if err != nil {
		return errors.Trace(err)
	}

	// Query for the high watermark
	var nullableWatermark sql.NullInt32
	switch db.driverName {
	case "mysql", "pgx":
		stmt = `SELECT MAX(seq_num) FROM sequel_migrations WHERE seq_name=? AND completed=TRUE`
	case "mssql", "sqlite":
		stmt = `SELECT MAX(seq_num) FROM sequel_migrations WHERE seq_name=? AND completed=1`
	default:
		return errors.New("unsupported driver name: %s", db.driverName)
	}
	row := db.QueryRow(stmt, sequenceName)
	err = row.Scan(&nullableWatermark)
	if err != nil {
		return errors.Trace(err)
	}
	watermark := 0
	if nullableWatermark.Valid {
		watermark = int(nullableWatermark.Int32)
	}

	// Read migrations from FS
	files, err := fs.ReadDir(fileSys, ".")
	if err != nil {
		return errors.New("unable to read directory", err)
	}
	var sequenceNumbersToRun []int
	migrations := map[int]string{}
	fileNames := map[int]string{}
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".sql") {
			continue
		}
		seqStr, _, _ := strings.Cut(file.Name(), ".")
		seqNum, err := strconv.Atoi(seqStr)
		if err != nil {
			continue
		}
		if seqNum <= watermark {
			// Already migrated
			continue
		}
		sequenceNumbersToRun = append(sequenceNumbersToRun, seqNum)
		content, err := fs.ReadFile(fileSys, file.Name())
		if err != nil {
			return errors.New("unable to read file %s", file.Name(), err)
		}
		migrations[seqNum] = string(content)
		fileNames[seqNum] = file.Name()
	}

	// Execute the migrations
	for len(sequenceNumbersToRun) > 0 {
		seqNum := sequenceNumbersToRun[0]

		// Insert new migrations into the database first
		// Ignore duplicate key violations
		switch db.driverName {
		case "mysql":
			stmt = `INSERT IGNORE INTO sequel_migrations (seq_name, seq_num) VALUES (?, ?)`
		case "pgx":
			stmt = `INSERT INTO sequel_migrations (seq_name, seq_num) VALUES (?, ?) ON CONFLICT DO NOTHING`
		case "mssql":
			stmt = `
			MERGE sequel_migrations AS tgt
			USING (SELECT ? AS seq_name, ? AS seq_num) AS src
				ON tgt.seq_name = src.seq_name AND tgt.seq_num = src.seq_num
			WHEN NOT MATCHED BY TARGET THEN
				INSERT (seq_name, seq_num)
				VALUES (src.seq_name, src.seq_num);`
		case "sqlite":
			stmt = `INSERT OR IGNORE INTO sequel_migrations (seq_name, seq_num) VALUES (?, ?)`
		default:
			return errors.New("unsupported driver name: %s", db.driverName)
		}
		_, err = db.Exec(stmt, sequenceName, seqNum)
		if err != nil {
			return errors.Trace(err)
		}

		// See if completed by another process
		stmt = `SELECT completed FROM sequel_migrations WHERE seq_name=? AND seq_num=?`
		row := db.QueryRow(stmt, sequenceName, seqNum)
		var completed bool
		err := row.Scan(&completed)
		if err != nil {
			return errors.Trace(err)
		}
		if completed {
			sequenceNumbersToRun = sequenceNumbersToRun[1:]
			continue
		}

		// Try to obtain a lock
		switch db.driverName {
		case "mysql", "pgx":
			stmt = `UPDATE sequel_migrations SET locked_before=DATE_ADD_MILLIS(NOW_UTC(), 15000)
					WHERE seq_name=? AND seq_num=? AND locked_before<NOW_UTC() AND completed=FALSE`
		case "mssql", "sqlite":
			stmt = `UPDATE sequel_migrations SET locked_before=DATE_ADD_MILLIS(NOW_UTC(), 15000)
					WHERE seq_name=? AND seq_num=? AND locked_before<NOW_UTC() AND completed=0`
		default:
			return errors.New("unsupported driver name: %s", db.driverName)
		}
		res, err := db.Exec(stmt, sequenceName, seqNum)
		if err != nil {
			return errors.Trace(err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return errors.Trace(err)
		}
		if affected == 0 {
			time.Sleep(20 * time.Millisecond)
			continue
		}

		// Obtained lock, execute migration in a goroutine
		statement := migrations[seqNum]
		lines := strings.Split(statement, "\n")
		for i := range lines {
			lines[i] = strings.TrimRight(lines[i], " \t\r")
		}
		statement = strings.Join(lines, "\n")

		done := make(chan error)
		go func() {
			for _, stmt := range strings.Split(statement, ";\n") {
				stmt = strings.TrimSpace(stmt)
				if stmt == "" {
					continue
				}
				p := strings.Index(stmt, "-- DRIVER:")
				if p >= 0 {
					driver, _, _ := strings.Cut(stmt[p+10:], "\n")
					if !strings.Contains(driver, db.driverName) {
						continue
					}
				}
				lines := strings.Split(stmt, "\n")
				for i := range lines {
					lines[i], _, _ = strings.Cut(lines[i], "--")
					lines[i] = strings.TrimSpace(lines[i])
				}
				stmt = strings.Join(lines, "\n")
				stmt = strings.TrimSpace(stmt)
				_, e := db.Exec(stmt)
				if e != nil {
					done <- e
					return
				}
			}
			done <- nil
		}()

		// Wait for it to finish
		exit := false
		for !exit {
			select {
			case err = <-done:
				exit = true
			case <-time.After(5 * time.Second):
				// Extend the lock while the migration is in progress
				stmt = `UPDATE sequel_migrations SET locked_before=DATE_ADD_MILLIS(NOW_UTC(), 15000) WHERE seq_name=? AND seq_num=?`
				_, err = db.Exec(stmt, sequenceName, seqNum)
				if err != nil {
					exit = true
				}
			}
		}

		if err != nil {
			// Release the lock
			stmt = `UPDATE sequel_migrations SET locked_before=NOW_UTC() WHERE seq_name=? AND seq_num=?`
			_, _ = db.Exec(stmt, sequenceName, seqNum)
			return errors.New("error running migration %s", fileNames[seqNum], err)
		}

		// Mark as complete
		switch db.driverName {
		case "mysql", "pgx":
			stmt = `UPDATE sequel_migrations SET locked_before=NOW_UTC(), completed_on=NOW_UTC(), completed=TRUE WHERE seq_name=? AND seq_num=?`
		case "mssql", "sqlite":
			stmt = `UPDATE sequel_migrations SET locked_before=NOW_UTC(), completed_on=NOW_UTC(), completed=1 WHERE seq_name=? AND seq_num=?`
		default:
			return errors.New("unsupported driver name: %s", db.driverName)
		}
		_, err = db.Exec(stmt, sequenceName, seqNum)
		if err != nil {
			return errors.Trace(err)
		}
		sequenceNumbersToRun = sequenceNumbersToRun[1:]
	}
	return nil
}

// conformPlaceholders replaces the ? arg placeholders in a SQL statement to $1, $2 etc. for a Postgres driver.
// Question marks inside quoted strings (single or double quotes) are left as-is.
func conformPlaceholders(driverName string, stmt string) string {
	if driverName != "pgx" {
		return stmt
	}
	n := strings.Count(stmt, "?")
	if n == 0 {
		return stmt
	}
	// Fast path: no quotes means no risk of replacing inside strings
	if !strings.ContainsAny(stmt, `'"`) {
		var sb strings.Builder
		sb.Grow(len(stmt) + n*3)
		argIndex := 1
		for {
			i := strings.Index(stmt, "?")
			if i < 0 {
				sb.WriteString(stmt)
				break
			}
			sb.WriteString(stmt[:i])
			sb.WriteString("$")
			sb.WriteString(strconv.Itoa(argIndex))
			argIndex++
			stmt = stmt[i+1:]
		}
		return sb.String()
	}
	// Slow path: scan character by character to skip quoted regions
	var sb strings.Builder
	sb.Grow(len(stmt) + n*3)
	argIndex := 1
	for i := 0; i < len(stmt); i++ {
		ch := stmt[i]
		if ch == '\'' || ch == '"' {
			// Copy everything up to and including the closing quote
			quote := ch
			sb.WriteByte(ch)
			i++
			for i < len(stmt) {
				sb.WriteByte(stmt[i])
				if stmt[i] == quote {
					break
				}
				i++
			}
		} else if ch == '?' {
			sb.WriteString("$")
			sb.WriteString(strconv.Itoa(argIndex))
			argIndex++
		} else {
			sb.WriteByte(ch)
		}
	}
	return sb.String()
}

func hashStr(x string) string {
	h := sha256.New()
	h.Write([]byte(x))
	sum := h.Sum(nil)
	return hex.EncodeToString(sum)
}

// Deprecated: ConformArgPlaceholders is applied automatically by the query shadow methods.
// Use ? placeholders directly in queries passed to Exec, Query, QueryRow, and Prepare.
func (db *DB) ConformArgPlaceholders(stmt string) string {
	return conformPlaceholders(db.driverName, stmt)
}

// Deprecated: Use the NOW_UTC() virtual function directly in queries instead.
func (db *DB) NowUTC() string {
	switch db.driverName {
	case "mysql":
		return "UTC_TIMESTAMP(3)"
	case "pgx":
		return "(NOW() AT TIME ZONE 'UTC')"
	case "mssql":
		return "SYSUTCDATETIME()"
	case "sqlite":
		return "STRFTIME('%Y-%m-%d %H:%M:%f', 'now')"
	default:
		return ""
	}
}

// Deprecated: Use the REGEXP_TEXT_SEARCH() virtual function directly in queries instead.
func (db *DB) RegexpTextSearch(searchableColumns ...string) string {
	concatenated := ""
	switch len(searchableColumns) {
	case 0:
		concatenated = "''"
	case 1:
		concatenated = searchableColumns[0]
	default:
		concatenated = "CONCAT_WS(' '," + strings.Join(searchableColumns, ",") + ")"
	}
	switch db.DriverName() {
	case "mysql":
		return concatenated + " REGEXP ?"
	case "pgx":
		return "REGEXP_LIKE(" + concatenated + ", ?, 'i')"
	case "mssql":
		// The database compatibility level must be set to 170 or higher
		return "REGEXP_LIKE(" + concatenated + ", ?, 'i')"
	case "sqlite":
		return concatenated + " LIKE ('%' || ? || '%')"
	default:
		return ""
	}
}
