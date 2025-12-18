/*
Copyright (c) 2025 Microbus LLC and various contributors

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
	"database/sql"
	"errors"
	"fmt"
	"io/fs"
	"math"
	"os"
	"path"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	_ "github.com/denisenkom/go-mssqldb"
	"github.com/denisenkom/go-mssqldb/msdsn"
	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	_ "github.com/jackc/pgx/v5/stdlib"
)

var (
	singletonsMap  = map[string]*DB{}
	singletonMux   sync.Mutex
	testingCreated = map[string]bool{}
	testingMux     sync.Mutex
)

// FS is a file system that is used to read migration files.
type FS interface {
	fs.FS
	fs.ReadDirFS
	fs.ReadFileFS
}

/*
DB is an enhanced database connection that
  - Limits the size of the connection pool to each server to approx the sqrt of the number of clients
  - Performs schema migration
  - Automatically creates and connects to a localhost database while testing
*/
type DB struct {
	*sql.DB
	driver        string
	dataSource    string
	refCount      int
	mux           sync.Mutex
	testingDBName string
}

// Open a new database connection.
func Open(driverName string, dataSourceName string) (db *DB, err error) {
	if driverName == "mariadb" {
		driverName = "mysql"
	}

	if driverName == "" {
		return nil, errors.New("driver name cannot be empty")
	}
	testingDBName := ""
	if dataSourceName == "" && testing.Testing() {
		dataSourceName, testingDBName, err = createTestingDatabase(driverName)
		if err != nil {
			return nil, fmt.Errorf("failed to create testing database: %w", err)
		}
	}
	if dataSourceName == "" {
		return nil, errors.New("data source name cannot be empty")
	}

	singletonMux.Lock()
	defer singletonMux.Unlock()
	cached, ok := singletonsMap[driverName+"|"+dataSourceName]
	if ok {
		cached.mux.Lock()
		defer cached.mux.Unlock()
		cached.refCount++
		cached.adjustConnectionLimits()
		return cached, nil
	}

	var sqlDB *sql.DB
	switch driverName {
	case "mysql":
		cfg, err := mysql.ParseDSN(dataSourceName)
		if err != nil {
			return nil, err
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
			return nil, err
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
			return nil, err
		}
	default:
		sqlDB, err = sql.Open(driverName, dataSourceName)
		if err != nil {
			return nil, err
		}
	}
	err = sqlDB.Ping()
	if err != nil {
		sqlDB.Close()
		return nil, err
	}

	// Prepare the database struct
	db = &DB{
		DB:            sqlDB,
		driver:        driverName,
		dataSource:    dataSourceName,
		refCount:      1,
		testingDBName: testingDBName,
	}
	db.adjustConnectionLimits()
	singletonsMap[driverName+"|"+dataSourceName] = db
	return db, nil
}

// Close closes the database connection.
func (db *DB) Close() (err error) {
	singletonMux.Lock()
	defer singletonMux.Unlock()
	db.mux.Lock()
	defer db.mux.Unlock()
	if db.DB == nil || db.refCount == 0 {
		return nil
	}

	db.refCount--
	if db.refCount == 0 {
		if db.testingDBName != "" {
			db.Exec("DROP DATABASE " + db.testingDBName)
		}
		err = db.DB.Close()
		db.DB = nil
		delete(singletonsMap, db.driver+"|"+db.dataSource)
	} else {
		db.adjustConnectionLimits()
	}
	return err
}

// createTestingDatabase creates a database for testing automatically.
func createTestingDatabase(driverName string) (dataSourceName string, dbName string, err error) {
	// Identify the top-most test function
	testFunctionName := ""
	for lvl := 0; ; lvl++ {
		pc, _, _, ok := runtime.Caller(lvl + 1)
		if !ok {
			break
		}
		runtimeFunc := runtime.FuncForPC(pc)
		if runtimeFunc != nil {
			functionName := runtimeFunc.Name()
			p := strings.LastIndex(functionName, "/")
			if p >= 0 {
				functionName = functionName[p+1:]
			}
			// github.com/microbus-io/sequel.Test_Example
			// github.com/microbus-io/sequel.Test_Example.func1
			pkgName, functionName, ok := strings.Cut(functionName, ".")
			if ok && strings.HasPrefix(functionName, "Test") {
				functionName, _, _ = strings.Cut(functionName, ".") // Subtests share the same database
				testFunctionName = pkgName + "_" + functionName
			}
		}
	}
	if testFunctionName == "" {
		return "", "", nil
	}

	testingMux.Lock()
	defer testingMux.Unlock()

	testingDataSourceName := strings.TrimSpace(os.Getenv("TESTING_DATA_SOURCE_NAME"))
	dbName = regexp.MustCompile(`[^a-z0-9]`).ReplaceAllString(strings.ToLower(testFunctionName), "_")

	switch driverName {
	case "mysql":
		if testingDataSourceName == "" {
			testingDataSourceName = "root:secret1234@tcp(127.0.0.1:3306)/"
		}
		cfg, err := mysql.ParseDSN(testingDataSourceName)
		if err != nil {
			return "", "", fmt.Errorf("error parsing data source name %s: %w", testingDataSourceName, err)
		}
		cfg.DBName = "" // Remove the database name, if any
		testingDataSourceName = cfg.FormatDSN()
		cfg.DBName = dbName // Set the actual datasource name
		dataSourceName = cfg.FormatDSN()
	case "pgx":
		if testingDataSourceName == "" {
			testingDataSourceName = "postgres://root:secret1234@127.0.0.1:5432/"
		}
		cfg, err := pgx.ParseConfig(testingDataSourceName)
		if err != nil {
			return "", "", fmt.Errorf("error parsing data source name %s: %w", testingDataSourceName, err)
		}
		cfg.Database = "" // Remove the database name, if any
		testingDataSourceName = cfg.ConnString()
		cfg.Database = dbName // Set the actual datasource name
		dataSourceName = cfg.ConnString()
	case "mssql":
		// https://github.com/microsoft/go-mssqldb?tab=readme-ov-file#common-parameters
		if testingDataSourceName == "" {
			testingDataSourceName = "user id=root;password=secret1234;server=127.0.0.1;port=1433"
		}
		_, cfg, err := msdsn.Parse(testingDataSourceName)
		if err != nil {
			return "", "", fmt.Errorf("error parsing data source name %s: %w", testingDataSourceName, err)
		}
		delete(cfg, "database") // Remove the database name, if any
		testingDataSourceName = ""
		for k, v := range cfg {
			testingDataSourceName += k + "=" + v + ";"
		}
		dataSourceName = testingDataSourceName + "database=" + dbName + ";" // Set the actual datasource name
	default:
		return "", "", fmt.Errorf("unsupported driver name: %s", driverName)
	}
	if !testingCreated[driverName+"|"+testFunctionName] {
		// Create the database
		rootDB, err := sql.Open(driverName, testingDataSourceName)
		if err != nil {
			return "", "", fmt.Errorf("failed to connect to database on localhost: %w", err)
		}
		defer rootDB.Close()
		_, err = rootDB.Exec("DROP DATABASE IF EXISTS " + dbName)
		if err != nil {
			return "", "", fmt.Errorf("failed to drop database %s: %w", dbName, err)
		}
		_, err = rootDB.Exec("CREATE DATABASE " + dbName)
		if err != nil {
			return "", "", fmt.Errorf("failed to create database %s: %w", dbName, err)
		}
		testingCreated[driverName+"|"+testFunctionName] = true
	}
	return dataSourceName, dbName, nil
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

// Migrate reads all sql/#.*.sql files from the FS, and executes any new migrations in order of their sequence number.
func (db *DB) Migrate(sequenceName string, fs FS) (err error) {
	// Init the schema migration table
	stmt := ""
	switch db.driver {
	case "mysql":
		stmt = `
		CREATE TABLE IF NOT EXISTS sequel_migrations (
			seq_name VARCHAR(256) NOT NULL,
			seq_num INT NOT NULL,
			completed BOOL NOT NULL DEFAULT FALSE,
			completed_on DATETIME(6),
			locked_until DATETIME(6) NOT NULL DEFAULT UTC_TIMESTAMP(6),
			PRIMARY KEY (seq_name, seq_num)
		)`
	case "pgx":
		stmt = `
		CREATE TABLE IF NOT EXISTS sequel_migrations (
			seq_name VARCHAR(256) NOT NULL,
			seq_num INT NOT NULL,
			completed BOOL NOT NULL DEFAULT FALSE,
			completed_on TIMESTAMP,
			locked_until TIMESTAMP NOT NULL DEFAULT (NOW() AT TIME ZONE 'UTC'),
			PRIMARY KEY (seq_name, seq_num)
		)`
	case "mssql":
		stmt = `
		IF OBJECT_ID(N'dbo.sequel_migrations', N'U') IS NULL BEGIN
			CREATE TABLE sequel_migrations (
				seq_name VARCHAR(256) NOT NULL,
				seq_num INT NOT NULL,
				completed BIT NOT NULL DEFAULT 0,
				completed_on DATETIME2,
				locked_until DATETIME2 NOT NULL DEFAULT SYSUTCDATETIME(),
				PRIMARY KEY (seq_name, seq_num)
			)
		END`
	default:
		return fmt.Errorf("unsupported driver name: %s", db.driver)
	}
	_, err = db.Exec(stmt)
	if err != nil {
		return err
	}

	// Query for the high watermark
	var nullableWatermark sql.NullInt32
	switch db.driver {
	case "mysql":
		stmt = `SELECT MAX(seq_num) FROM sequel_migrations WHERE seq_name=? AND completed=TRUE`
	case "pgx":
		stmt = `SELECT MAX(seq_num) FROM sequel_migrations WHERE seq_name=$1 AND completed=TRUE`
	case "mssql":
		stmt = `SELECT MAX(seq_num) FROM sequel_migrations WHERE seq_name=? AND completed=1`
	default:
		return fmt.Errorf("unsupported driver name: %s", db.driver)
	}
	row := db.QueryRow(stmt, sequenceName)
	err = row.Scan(&nullableWatermark)
	if err != nil {
		return err
	}
	watermark := 0
	if nullableWatermark.Valid {
		watermark = int(nullableWatermark.Int32)
	}

	// Read migrations from FS
	files, err := fs.ReadDir("sql")
	if err != nil {
		return fmt.Errorf("unable to read sql directory: %w", err)
	}
	var sequenceNumbersToRun []int
	migrations := map[int]string{}
	fileNames := map[int]string{}
	for _, file := range files {
		if !strings.HasSuffix(file.Name(), ".sql") {
			continue
		}
		seqStr, _, ok := strings.Cut(file.Name(), ".")
		if !ok {
			return fmt.Errorf("file name should start with a number followed by a dot: %s", file.Name())
		}
		seqNum, err := strconv.Atoi(seqStr)
		if err != nil {
			return fmt.Errorf("file name should start with a number followed by a dot: %s", file.Name())
		}
		if seqNum <= watermark {
			// Already migrated
			continue
		}
		sequenceNumbersToRun = append(sequenceNumbersToRun, seqNum)
		content, err := fs.ReadFile(path.Join("sql", file.Name()))
		if err != nil {
			return fmt.Errorf("unable to read file %s: %w", path.Join("sql", file.Name()), err)
		}
		migrations[seqNum] = string(content)
		fileNames[seqNum] = file.Name()
	}

	// Execute the migrations
	for len(sequenceNumbersToRun) > 0 {
		seqNum := sequenceNumbersToRun[0]

		// Insert new migrations into the database first
		// Ignore duplicate key violations
		switch db.driver {
		case "mysql":
			stmt = `INSERT IGNORE INTO sequel_migrations (seq_name, seq_num) VALUES (?, ?)`
		case "pgx":
			stmt = `INSERT INTO sequel_migrations (seq_name, seq_num) VALUES ($1, $2) ON CONFLICT DO NOTHING`
		case "mssql":
			stmt = `
			MERGE sequel_migrations AS tgt
			USING (SELECT ? AS seq_name, ? AS seq_num) AS src
				ON tgt.seq_name = src.seq_name AND tgt.seq_num = src.seq_num
			WHEN NOT MATCHED BY TARGET THEN
				INSERT (seq_name, seq_num)
				VALUES (src.seq_name, src.seq_num);`
		default:
			return fmt.Errorf("unsupported driver name: %s", db.driver)
		}
		_, err = db.Exec(stmt, sequenceName, seqNum)
		if err != nil {
			return err
		}

		// See if completed by another process
		switch db.driver {
		case "mysql":
			stmt = `SELECT completed FROM sequel_migrations WHERE seq_name=? AND seq_num=?`
		case "pgx":
			stmt = `SELECT completed FROM sequel_migrations WHERE seq_name=$1 AND seq_num=$2`
		case "mssql":
			stmt = `SELECT completed FROM sequel_migrations WHERE seq_name=? AND seq_num=?`
		default:
			return fmt.Errorf("unsupported driver name: %s", db.driver)
		}
		row := db.QueryRow(stmt, sequenceName, seqNum)
		var completed bool
		err := row.Scan(&completed)
		if err != nil {
			return err
		}
		if completed {
			sequenceNumbersToRun = sequenceNumbersToRun[1:]
			continue
		}

		// Try to obtain a lock
		switch db.driver {
		case "mysql":
			stmt = `UPDATE sequel_migrations SET locked_until=DATE_ADD(UTC_TIMESTAMP(6), INTERVAL 15 SECOND)
					WHERE seq_name=? AND seq_num=? AND locked_until<UTC_TIMESTAMP(6) AND completed=FALSE`
		case "pgx":
			stmt = `UPDATE sequel_migrations SET locked_until=((NOW() + INTERVAL '15 seconds') AT TIME ZONE 'UTC')
					WHERE seq_name=$1 AND seq_num=$2 AND locked_until<(NOW() AT TIME ZONE 'UTC') AND completed=FALSE`
		case "mssql":
			stmt = `UPDATE sequel_migrations SET locked_until=DATEADD(second, 15, SYSUTCDATETIME())
					WHERE seq_name=? AND seq_num=? AND locked_until<SYSUTCDATETIME() AND completed=0`
		default:
			return fmt.Errorf("unsupported driver name: %s", db.driver)
		}
		res, err := db.Exec(stmt, sequenceName, seqNum)
		if err != nil {
			return err
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			time.Sleep(20 * time.Millisecond)
			continue
		}

		// Obtained lock, execute migration in a goroutine
		statement := migrations[seqNum]
		lines := strings.Split(statement, "\n")
		for i := range lines {
			lines[i], _, _ = strings.Cut(lines[i], "--")
			lines[i] = strings.TrimRight(lines[i], "\r\t ")
		}
		statement = strings.Join(lines, "\n")
		statement = strings.TrimSpace(statement)

		done := make(chan error)
		go func() {
			for _, stmt := range strings.Split(statement, ";\n") {
				stmt = strings.TrimSpace(stmt)
				if stmt == "" {
					continue
				}
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
				switch db.driver {
				case "mysql":
					stmt = `UPDATE sequel_migrations SET locked_until=DATE_ADD(UTC_TIMESTAMP(6), INTERVAL 15 SECOND) WHERE seq_name=? AND seq_num=?`
				case "pgx":
					stmt = `UPDATE sequel_migrations SET locked_until=((NOW() + INTERVAL '15 seconds') AT TIME ZONE 'UTC') WHERE seq_name=$1 AND seq_num=$2`
				case "mssql":
					stmt = `UPDATE sequel_migrations SET locked_until=DATEADD(second, 15, SYSUTCDATETIME()) WHERE seq_name=? AND seq_num=?`
				default:
					return fmt.Errorf("unsupported driver name: %s", db.driver)
				}
				_, err = db.Exec(stmt, sequenceName, seqNum)
				if err != nil {
					exit = true
				}
			}
		}

		if err != nil {
			// Release the lock
			switch db.driver {
			case "mysql":
				stmt = `UPDATE sequel_migrations SET locked_until=UTC_TIMESTAMP(6) WHERE seq_name=? AND seq_num=?`
			case "pgx":
				stmt = `UPDATE sequel_migrations SET locked_until=(NOW() AT TIME ZONE 'UTC') WHERE seq_name=$1 AND seq_num=$2`
			case "mssql":
				stmt = `UPDATE sequel_migrations SET locked_until=SYSUTCDATETIME() WHERE seq_name=? AND seq_num=?`
			default:
				return fmt.Errorf("unsupported driver name: %s", db.driver)
			}
			_, _ = db.Exec(stmt, sequenceName, seqNum)
			return fmt.Errorf("error running migration %s: %w", fileNames[seqNum], err)
		}

		// Mark as complete
		switch db.driver {
		case "mysql":
			stmt = `UPDATE sequel_migrations SET locked_until=UTC_TIMESTAMP(6), completed_on=UTC_TIMESTAMP(6), completed=TRUE WHERE seq_name=? AND seq_num=?`
		case "pgx":
			stmt = `UPDATE sequel_migrations SET locked_until=(NOW() AT TIME ZONE 'UTC'), completed_on=(NOW() AT TIME ZONE 'UTC'), completed=TRUE WHERE seq_name=$1 AND seq_num=$2`
		case "mssql":
			stmt = `UPDATE sequel_migrations SET locked_until=SYSUTCDATETIME(), completed_on=SYSUTCDATETIME(), completed=1 WHERE seq_name=? AND seq_num=?`
		default:
			return fmt.Errorf("unsupported driver name: %s", db.driver)
		}
		_, err = db.Exec(stmt, sequenceName, seqNum)
		if err != nil {
			return err
		}
		sequenceNumbersToRun = sequenceNumbersToRun[1:]
	}
	return nil
}

// ArgPlaceholdersToPGX replaces the ? arg placeholders in a SQL statement to $1, $2 etc which is the format expected by Postgres.
func ArgPlaceholdersToPGX(stmt string) string {
	parts := strings.Split(stmt, "?")
	for i := 0; i < len(parts)-1; i++ {
		parts[i] += fmt.Sprintf("$%d", i+1)
	}
	return strings.Join(parts, "")
}
