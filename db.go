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
	"log/slog"
	"math"
	"math/rand/v2"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	mssql "github.com/denisenkom/go-mssqldb"
	"github.com/denisenkom/go-mssqldb/msdsn"
	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/microbus-io/errors"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/metric"
	"go.opentelemetry.io/otel/trace"
	sqlite "modernc.org/sqlite"
)

var (
	singletonsMap      = map[string]*DB{}
	singletonMutex     sync.Mutex
	testingDSNs        = map[string]string{}
	testingGlobalMutex sync.Mutex
	testingMutexes     = map[string]*sync.Mutex{}
	testingStartedAt   = time.Now().UTC()

	// insertSourceClausePattern matches the start of an INSERT statement's source clause - either
	// VALUES (INSERT ... VALUES (...)) or SELECT (INSERT ... SELECT ...). MSSQL's OUTPUT INSERTED
	// clause is injected immediately before whichever appears, so InsertReturnID works for both forms.
	insertSourceClausePattern  = regexp.MustCompile(`(?i)\s+(VALUES|SELECT)\b`)
	testingDatabaseNamePattern = regexp.MustCompile(`^testing_\d{2}_`)
)

/*
DB is an enhanced database connection that
  - Limits the size of the connection pool to each server to approx the sqrt of the number of clients
  - Performs schema migration
  - Automatically creates and connects to a localhost database while testing
*/
type DB struct {
	*sql.DB
	driverName     string
	dataSourceName string
	singletonKey   string
	refCount       int
	mutex          sync.Mutex
	telemetry      atomic.Pointer[telemetry]
}

/*
Open returns a database connection to the named data source with a dedicated
connection pool. Each call returns a distinct *DB; sequel does not coalesce by DSN.
The caller is responsible for sizing the pool via SetMaxOpenConns / SetMaxIdleConns
if the database/sql defaults (unlimited open, 2 idle) don't fit.

Use [OpenSingleton] when multiple consumers in the same process share a DSN and
you want sequel to manage one pool across all of them.

If a driver name is not provided, it is inferred from the data source name on a
best-effort basis. Drivers currently supported: "mysql" (MySQL), "pgx" (Postgres),
"cockroachdb" (CockroachDB), "mssql" (SQL Server) or "sqlite" (SQLite).

Example data source name for each of the supported drivers:
  - mysql: username:password@tcp(hostname:3306)/
  - pgx: postgres://username:password@hostname:5432/
  - cockroachdb: postgres://username:password@hostname:26257/
  - mssql: sqlserver://username:password@hostname:1433
  - sqlite: file:path/to/database.sqlite
*/
func Open(driverName string, dataSourceName string) (db *DB, err error) {
	driverName, dataSourceName, err = normalizeDriverAndDSN(driverName, dataSourceName)
	if err != nil {
		return nil, errors.Trace(err)
	}
	sqlDB, err := sql.Open(physicalDriverName(driverName), dataSourceName)
	if err != nil {
		return nil, errors.Trace(err)
	}
	db = &DB{
		DB:             sqlDB,
		driverName:     driverName,
		dataSourceName: dataSourceName,
		refCount:       1,
	}
	db.telemetry.Store(newDefaultTelemetry(db))
	return db, nil
}

/*
OpenSingleton returns a per-DSN coalesced *DB whose connection pool sequel manages
automatically based on the number of openers (sqrt-based growth, see
[DB.adjustConnectionLimits]). Multiple OpenSingleton calls with the same
(driverName, dataSourceName) return the same *DB and share its connection pool.
This is the right choice when many parts of the same process each access the
database occasionally.

Use [Open] when you want a dedicated pool with explicit caller-managed sizing.

Driver inference, DSN defaults, and supported drivers are the same as [Open].
*/
func OpenSingleton(driverName string, dataSourceName string) (db *DB, err error) {
	driverName, dataSourceName, err = normalizeDriverAndDSN(driverName, dataSourceName)
	if err != nil {
		return nil, errors.Trace(err)
	}
	singletonKey := hashStr(driverName + "|" + dataSourceName)

	singletonMutex.Lock()
	singletonDB, ok := singletonsMap[singletonKey]
	if !ok {
		singletonDB = &DB{
			driverName:     driverName,
			dataSourceName: dataSourceName,
			singletonKey:   singletonKey,
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

	sqlDB, err := sql.Open(physicalDriverName(driverName), dataSourceName)
	if err != nil {
		return nil, errors.Trace(err)
	}
	singletonDB.DB = sqlDB
	singletonDB.refCount = 1
	singletonDB.telemetry.Store(newDefaultTelemetry(singletonDB))
	singletonDB.adjustConnectionLimits()
	return singletonDB, nil
}

// normalizeDriverAndDSN normalizes driver name aliases, infers the driver name when
// missing, and applies driver-specific DSN tweaks (MySQL connection params, SQLite
// busy_timeout pragma).
func normalizeDriverAndDSN(driverName, dataSourceName string) (string, string, error) {
	if dataSourceName == "" {
		return "", "", errors.New("data source name is required")
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
		return "", "", errors.New("driver name could not be inferred from data source name")
	}
	switch driverName {
	case "mysql":
		cfg, err := mysql.ParseDSN(dataSourceName)
		if err != nil {
			return "", "", errors.Trace(err)
		}
		if cfg.Params == nil {
			cfg.Params = map[string]string{}
		}
		// See https://github.com/go-sql-driver/mysql#dsn-data-source-name
		cfg.Params["parseTime"] = "true"
		cfg.Params["timeout"] = "4s"
		cfg.Params["readTimeout"] = "8s"
		cfg.Params["writeTimeout"] = "8s"
		dataSourceName = cfg.FormatDSN()
	case "sqlite":
		if !strings.Contains(dataSourceName, "busy_timeout") {
			// Set a busy_timeout so that concurrent writers retry on lock contention
			// instead of immediately returning SQLITE_BUSY. Without this, in-memory and
			// shared-cache SQLite databases serialize write transactions but bare writes
			// from concurrent connections fail rather than wait.
			if strings.Contains(dataSourceName, "?") {
				dataSourceName += "&_pragma=busy_timeout(2000)"
			} else {
				dataSourceName += "?_pragma=busy_timeout(2000)"
			}
		}
	}
	return driverName, dataSourceName, nil
}

// IsLockContentionError returns true if the error indicates database lock contention or a deadlock.
// Such errors are transient and the operation can typically be retried.
// Recognizes lock errors from SQLite, MySQL, PostgreSQL, SQL Server, and CockroachDB.
//
// Classification prefers the driver's native error code (immune to message wording, localization, and
// user data appearing in error messages); a substring match is used as a fallback for errors whose
// driver type is not present in the chain (e.g. some wrapped or text-only CockroachDB retry errors).
func IsLockContentionError(err error) bool {
	if err == nil {
		return false
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// 40P01 deadlock_detected, 40001 serialization_failure (PostgreSQL and CockroachDB).
		if pgErr.Code == "40P01" || pgErr.Code == "40001" {
			return true
		}
	}
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		// 1213 ER_LOCK_DEADLOCK, 1205 ER_LOCK_WAIT_TIMEOUT.
		if myErr.Number == 1213 || myErr.Number == 1205 {
			return true
		}
	}
	var msErr mssql.Error
	if errors.As(err, &msErr) {
		// 1205 deadlock victim, 1222 lock request time out.
		if msErr.Number == 1205 || msErr.Number == 1222 {
			return true
		}
	}
	var liteErr *sqlite.Error
	if errors.As(err, &liteErr) {
		// The low byte is the primary result code: 5 SQLITE_BUSY, 6 SQLITE_LOCKED (also covers the
		// extended codes such as SQLITE_BUSY_SNAPSHOT and SQLITE_LOCKED_SHAREDCACHE).
		switch liteErr.Code() & 0xff {
		case 5, 6:
			return true
		}
	}

	return isLockContentionMessage(err.Error())
}

// isLockContentionMessage is the substring fallback for lock/deadlock errors whose native driver type
// is not in the error chain.
func isLockContentionMessage(msg string) bool {
	switch {
	case strings.Contains(msg, "SQLITE_BUSY"), strings.Contains(msg, "database is locked"),
		strings.Contains(msg, "SQLITE_LOCKED"), strings.Contains(msg, "database table is locked"),
		strings.Contains(msg, "database is deadlocked"): // SQLite (BUSY / LOCKED)
		return true
	case strings.Contains(msg, "Deadlock found"), strings.Contains(msg, "Lock wait timeout"): // MySQL
		return true
	case strings.Contains(msg, "deadlock detected"): // PostgreSQL
		return true
	case strings.Contains(msg, "deadlock victim"), strings.Contains(msg, "was deadlocked"),
		strings.Contains(msg, "Lock request time out"): // SQL Server
		return true
	case strings.Contains(msg, "restart transaction"), // CockroachDB
		strings.Contains(msg, "RETRY_SERIALIZABLE"),
		strings.Contains(msg, "RETRY_WRITE_TOO_OLD"):
		return true
	}
	return false
}

// Close closes the database connection.
//
// When the last reference closes and the underlying database name matches the
// testing pattern (testing_NN_…), sequel drops the database from the server as
// a best-effort cleanup. This makes [CreateTestingDatabase]-provisioned
// databases self-cleaning on test teardown.
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
		db.maybeDropTestingDatabase()
	} else {
		db.adjustConnectionLimits()
	}
	return errors.Trace(err)
}

// maybeDropTestingDatabase drops the database backing this *DB if its database
// name has the testing prefix produced by [CreateTestingDatabase]. Errors are
// swallowed: the leftover-DB sweep on the next test run is the safety net.
// No-op for SQLite (in-memory).
func (db *DB) maybeDropTestingDatabase() {
	if db.driverName == "sqlite" {
		return
	}
	dbName, err := databaseNameFromDataSourceName(db.driverName, db.dataSourceName)
	if err != nil || !testingDatabaseNamePattern.MatchString(dbName) {
		return
	}
	masterDSN, err := setDatabaseInDataSourceName(db.driverName, db.dataSourceName, "")
	if err != nil {
		return
	}
	masterDB, err := OpenSingleton(db.driverName, masterDSN)
	if err != nil {
		return
	}
	defer masterDB.Close()
	masterDB.Exec("DROP DATABASE IF EXISTS " + dbName)
}

/*
SetTracerProvider attaches an OpenTelemetry TracerProvider so sequel emits a client span around each query,
transaction, and migration. A freshly opened *DB already uses the process-wide otel.GetTracerProvider();
call this to override it, or pass nil to revert to that global provider (whose default is a no-op).

Observability is configured after Open/OpenSingleton (which keep the standard database/sql signature) rather
than at construction. This loses nothing: sql.Open does no I/O — it only prepares a lazy pool — so there is
no work inside Open worth a span; every operation that does real work happens later on the returned *DB.

Configure before the *DB is used concurrently. For an OpenSingleton-shared *DB the providers are process-
wide for that pool; the last setter wins, so configure once from the owning caller.
*/
func (db *DB) SetTracerProvider(tp trace.TracerProvider) {
	if tp == nil {
		tp = otel.GetTracerProvider()
	}
	db.updateTelemetry(func(t *telemetry) {
		t.tracer = tp.Tracer(instrumentationName)
	})
}

// SetMeterProvider attaches an OpenTelemetry MeterProvider so sequel emits sequel_ metrics (query and
// transaction duration, lock-contention count, migration count, and connection-pool gauges). A freshly
// opened *DB already uses the process-wide otel.GetMeterProvider(); call this to override it, or pass nil to
// revert to that global provider (whose default is a no-op). See [DB.SetTracerProvider] for when to call.
func (db *DB) SetMeterProvider(mp metric.MeterProvider) {
	if mp == nil {
		mp = otel.GetMeterProvider()
	}
	db.mutex.Lock()
	defer db.mutex.Unlock()
	nt := db.telemetry.Load().clone()
	nt.clearInstruments() // unregisters any prior pool callback
	nt.meter = mp.Meter(instrumentationName)
	nt.initInstruments(db)
	db.telemetry.Store(nt)
}

// SetLogger attaches an slog.Logger. The library does not log operation errors (they are returned to the
// caller, who logs them); it logs one-off events such as schema migrations at Info, and — when the logger
// is enabled at Debug level — each query at Debug. Per-query logging is therefore controlled by the
// logger's own level, not a separate switch. A freshly opened *DB uses a discard logger; pass nil here to
// revert to that discard logger (disabling logging).
func (db *DB) SetLogger(logger *slog.Logger) {
	if logger == nil {
		logger = slog.New(slog.DiscardHandler)
	}
	db.updateTelemetry(func(t *telemetry) {
		t.logger = logger
	})
}

// updateTelemetry applies a mutation to a fresh copy of the current telemetry and swaps it in atomically,
// so the hot read path stays lock-free. Used for the setters that do not touch metric instruments.
func (db *DB) updateTelemetry(mutate func(*telemetry)) {
	db.mutex.Lock()
	defer db.mutex.Unlock()
	nt := db.telemetry.Load().clone()
	mutate(nt)
	db.telemetry.Store(nt)
}

// snapshotStats returns a connection-pool stats snapshot, or ok=false if the pool has been closed. Used by
// the pool-gauge callback, which must tolerate a singleton *DB whose underlying pool was closed.
func (db *DB) snapshotStats() (sql.DBStats, bool) {
	db.mutex.Lock()
	defer db.mutex.Unlock()
	if db.DB == nil {
		return sql.DBStats{}, false
	}
	return db.DB.Stats(), true
}

// Exec shadows sql.DB.Exec and conforms arg placeholders for the driver.
func (db *DB) Exec(query string, args ...any) (sql.Result, error) {
	return instrumentExec(db.telemetry.Load(), context.Background(), db.driverName, query,
		func(_ context.Context, q string) (sql.Result, error) {
			return db.DB.Exec(q, args...)
		})
}

// ExecContext shadows sql.DB.ExecContext and conforms arg placeholders for the driver.
func (db *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return instrumentExec(db.telemetry.Load(), ctx, db.driverName, query,
		func(ctx context.Context, q string) (sql.Result, error) {
			return db.DB.ExecContext(ctx, q, args...)
		})
}

// Query shadows sql.DB.Query and conforms arg placeholders for the driver. It returns a [Rows], which
// embeds *sql.Rows so existing rows.Next()/rows.Scan()/rows.Err() call sites are unchanged. A *DB query
// is not transactional, so Rows here is a pure passthrough (no error latching).
func (db *DB) Query(query string, args ...any) (*Rows, error) {
	rows, err := instrumentExec(db.telemetry.Load(), context.Background(), db.driverName, query,
		func(_ context.Context, q string) (*sql.Rows, error) {
			return db.DB.Query(q, args...)
		})
	if err != nil {
		return nil, err
	}
	return &Rows{Rows: rows}, nil
}

// QueryContext shadows sql.DB.QueryContext and conforms arg placeholders for the driver. It returns a
// [Rows] (see Query); a *DB query does not latch errors into any transaction.
func (db *DB) QueryContext(ctx context.Context, query string, args ...any) (*Rows, error) {
	rows, err := instrumentExec(db.telemetry.Load(), ctx, db.driverName, query,
		func(ctx context.Context, q string) (*sql.Rows, error) {
			return db.DB.QueryContext(ctx, q, args...)
		})
	if err != nil {
		return nil, err
	}
	return &Rows{Rows: rows}, nil
}

// QueryRow shadows sql.DB.QueryRow and conforms arg placeholders for the driver. It returns a [Row], which
// embeds *sql.Row so existing QueryRow(...).Scan(...) call sites are unchanged.
func (db *DB) QueryRow(query string, args ...any) *Row {
	return instrumentQueryRow(db.telemetry.Load(), context.Background(), db.driverName, query,
		func(_ context.Context, q string) *sql.Row {
			return db.DB.QueryRow(q, args...)
		})
}

// QueryRowContext shadows sql.DB.QueryRowContext and conforms arg placeholders for the driver. It returns a
// [Row], which embeds *sql.Row so existing QueryRowContext(...).Scan(...) call sites are unchanged.
func (db *DB) QueryRowContext(ctx context.Context, query string, args ...any) *Row {
	return instrumentQueryRow(db.telemetry.Load(), ctx, db.driverName, query,
		func(ctx context.Context, q string) *sql.Row {
			return db.DB.QueryRowContext(ctx, q, args...)
		})
}

// Prepare shadows sql.DB.Prepare and conforms arg placeholders for the driver.
func (db *DB) Prepare(query string) (*sql.Stmt, error) {
	return instrumentExec(db.telemetry.Load(), context.Background(), db.driverName, query,
		func(_ context.Context, q string) (*sql.Stmt, error) {
			return db.DB.Prepare(q)
		})
}

// PrepareContext shadows sql.DB.PrepareContext and conforms arg placeholders for the driver.
func (db *DB) PrepareContext(ctx context.Context, query string) (*sql.Stmt, error) {
	return instrumentExec(db.telemetry.Load(), ctx, db.driverName, query,
		func(ctx context.Context, q string) (*sql.Stmt, error) {
			return db.DB.PrepareContext(ctx, q)
		})
}

// DriverName is the name of the driver: "mysql", "pgx", "cockroachdb", "mssql" or "sqlite".
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
	return &Tx{Tx: sqlTx, driverName: db.driverName, t: db.telemetry.Load()}, nil
}

// BeginTx starts a transaction with the given options and returns a sequel.Tx that
// applies virtual function expansion and placeholder conforming.
func (db *DB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*Tx, error) {
	sqlTx, err := db.DB.BeginTx(ctx, opts)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return &Tx{Tx: sqlTx, driverName: db.driverName, t: db.telemetry.Load()}, nil
}

// transactMaxAttempts bounds how many times Transact reruns a transaction that keeps losing to lock
// contention before giving up and returning the last error.
const transactMaxAttempts = 8

// Transact runs fn inside a transaction, committing on success and rolling back on error. If the
// transaction fails on lock contention or a deadlock, it is retried with a short jittered backoff.
// Because a retry re-executes fn from the start in a new transaction, fn must be safe to run more than
// once; any non-transactional side effects it performs (in-memory changes, channel sends) may repeat.
//
// The Tx passed to fn records the first statement error and short-circuits the remaining statements, so
// fn cannot commit partial work even if it does not check every statement's error. For SQL Server,
// SET XACT_ABORT ON is applied so that any statement error aborts the whole transaction.
func (db *DB) Transact(ctx context.Context, fn func(tx *Tx) error) (err error) {
	ctx, finish := db.telemetry.Load().beginTransact(ctx, db.driverName)
	attempts := 0
	defer func() { finish(attempts, err) }()
	for attempt := range transactMaxAttempts {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt)*time.Millisecond + time.Duration(rand.IntN(3))*time.Millisecond)
		}
		attempts++
		err = db.transactOnce(ctx, fn)
		if err == nil || !IsLockContentionError(err) {
			return err
		}
	}
	err = errors.Trace(err)
	return err
}

// transactOnce executes one attempt of a Transact: begin, run fn, commit, rolling back on any failure.
func (db *DB) transactOnce(ctx context.Context, fn func(tx *Tx) error) error {
	sqlTx, err := db.DB.BeginTx(ctx, nil)
	if err != nil {
		return errors.Trace(err)
	}
	tx := &Tx{Tx: sqlTx, driverName: db.driverName, autoErr: true, t: db.telemetry.Load()}
	committed := false
	defer func() {
		if !committed {
			_ = sqlTx.Rollback()
		}
	}()
	if db.driverName == "mssql" {
		// XACT_ABORT ON makes any statement error abort the whole transaction server-side, so a deadlock
		// or constraint failure cannot leave the transaction in a half-applied, committable state.
		if _, err := sqlTx.ExecContext(ctx, "SET XACT_ABORT ON"); err != nil {
			return errors.Trace(err)
		}
	}
	if err := fn(tx); err != nil {
		return errors.Trace(err)
	}
	if tx.err != nil {
		return errors.Trace(tx.err)
	}
	if err := sqlTx.Commit(); err != nil {
		return errors.Trace(err)
	}
	committed = true
	return nil
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
	case "pgx", "cockroachdb":
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
// before the source clause (VALUES or SELECT), for use with MSSQL. This makes InsertReturnID
// work for both INSERT ... VALUES (...) and INSERT ... SELECT ... forms.
func injectOutputInserted(stmt string, idColumn string) (string, error) {
	loc := insertSourceClausePattern.FindStringIndex(stmt)
	if loc == nil {
		return "", errors.New("VALUES or SELECT clause not found in INSERT statement")
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
	case "pgx", "cockroachdb":
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
	case "pgx", "cockroachdb":
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

// physicalDriverName maps a sequel driver name to the driver registered with database/sql.
// CockroachDB shares the pgx driver since it speaks the PostgreSQL wire protocol; sequel
// keeps "cockroachdb" as a distinct logical name for callers and dispatch but opens the
// underlying connection with "pgx".
func physicalDriverName(driverName string) string {
	if driverName == "cockroachdb" {
		return "pgx"
	}
	return driverName
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
	if strings.Contains(dataSourceName, ":26257") {
		return "cockroachdb"
	}
	if strings.Contains(dataSourceName, ":1433") {
		return "mssql"
	}
	return ""
}

/*
CreateTestingDatabase provisions a uniquely-named database (or returns a SQLite
in-memory DSN) for testing and returns the resolved data source name. Pass the
result to [Open] or [OpenSingleton] to open a connection.

The returned DSN points at a database whose name has the testing_NN_ prefix. When
the last *DB referencing that database is Closed, sequel drops it automatically —
no separate cleanup call is required.

uniqueTestID scopes the database so that independent tests don't collide. Pass
t.Name() from a test, or an equivalent identifier from production startup code
that wants a per-run database:

	dsn := cfg.DSN
	if cfg.Testing {
	    dsn, err = sequel.CreateTestingDatabase("", cfg.DSN, cfg.TestID)
	    if err != nil { return err }
	}
	db, err := sequel.OpenSingleton("", dsn)

Within a single process, repeated calls with the same (driverName,
baseDataSourceName, uniqueTestID) reuse the same testing database — the
underlying DROP+CREATE only happens on the first call.

If a driver name is not provided, it is inferred from the data source name on a
best-effort basis. Drivers currently supported: "mysql" (MySQL), "pgx" (Postgres),
"cockroachdb" (CockroachDB), "mssql" (SQL Server) or "sqlite" (SQLite).

If neither a driver name nor a base data source name is provided, it falls back to the SEQUEL_TESTING_DSN
environment variable. This lets any consumer that builds ephemeral test databases through sequel redirect
its entire suite at a real server without changing test code: leave SEQUEL_TESTING_DSN unset to keep the
SQLite default, or set it to a base DSN to run against that server instead, with the driver inferred from
it. Naming a driver — even with an empty DSN — opts out of the fallback, so a test that explicitly asks for
SQLite keeps running on SQLite regardless of the environment.

If neither the arguments nor SEQUEL_TESTING_DSN select a server, the following localhost defaults are used
based on the driver name:
  - (empty): SQLite in-memory database
  - sqlite: SQLite in-memory database
  - mysql: root:root@tcp(127.0.0.1:3306)/
  - pgx: postgres://postgres:postgres@127.0.0.1:5432/
  - cockroachdb: postgres://root@127.0.0.1:26257/?sslmode=disable
  - mssql: sqlserver://sa:Password123@127.0.0.1:1433
*/
func CreateTestingDatabase(driverName string, baseDataSourceName string, uniqueTestID string) (dsn string, err error) {
	// Only fall back to SEQUEL_TESTING_DSN when the caller named neither a driver nor a DSN;
	// naming a driver is intent that must be honored.
	if driverName == "" && baseDataSourceName == "" {
		baseDataSourceName = os.Getenv("SEQUEL_TESTING_DSN")
	}
	// Set default connection to localhost
	if baseDataSourceName == "" {
		switch driverName {
		case "", "sqlite":
			baseDataSourceName = "file:?mode=memory&cache=shared"
		case "mysql":
			baseDataSourceName = "root:root@tcp(127.0.0.1:3306)/"
		case "pgx":
			baseDataSourceName = "postgres://postgres:postgres@127.0.0.1:5432/"
		case "cockroachdb":
			baseDataSourceName = "postgres://root@127.0.0.1:26257/?sslmode=disable"
		case "mssql":
			baseDataSourceName = "sqlserver://sa:Password123@127.0.0.1:1433"
		default:
			return "", errors.New("unsupported driver name: %s", driverName)
		}
	}
	if driverName == "" {
		driverName = inferDriverName(baseDataSourceName)
	}
	if driverName == "sqlite" && !strings.Contains(baseDataSourceName, "mode=memory") && !strings.Contains(baseDataSourceName, "cache=shared") {
		if strings.Contains(baseDataSourceName, "?") {
			baseDataSourceName += "&mode=memory&cache=shared"
		} else {
			baseDataSourceName += "?mode=memory&cache=shared"
		}
	}

	cacheKey := hashStr(driverName + "|" + baseDataSourceName + "|" + uniqueTestID)

	testingGlobalMutex.Lock()
	testingMux, ok := testingMutexes[cacheKey]
	if !ok {
		testingMux = &sync.Mutex{}
		testingMutexes[cacheKey] = testingMux
	}
	cachedDSN, hasCached := testingDSNs[cacheKey]
	testingGlobalMutex.Unlock()
	if hasCached {
		return cachedDSN, nil
	}

	testingMux.Lock()
	defer testingMux.Unlock()

	// Re-check after taking the per-test mutex in case another caller raced ahead.
	testingGlobalMutex.Lock()
	cachedDSN, hasCached = testingDSNs[cacheKey]
	testingGlobalMutex.Unlock()
	if hasCached {
		return cachedDSN, nil
	}

	// Generate a database name. The testing_NN_ prefix is the contract that
	// (*DB).maybeDropTestingDatabase uses to auto-drop on Close.
	baseDatabaseName, err := databaseNameFromDataSourceName(driverName, baseDataSourceName)
	if err != nil {
		return "", errors.Trace(err)
	}
	if baseDatabaseName != "" {
		baseDatabaseName = strings.ToLower(baseDatabaseName) + "_"
	}
	now := testingStartedAt // Avoid hour change mid-run
	testingDatabaseName := "testing_" + now.Format("15") + "_" + baseDatabaseName + strings.ToLower(uniqueTestID)
	testingDatabaseName = regexp.MustCompile(`[^a-z0-9_]`).ReplaceAllString(testingDatabaseName, "_")

	// For server-based drivers, open the master database and CREATE/DROP DATABASE.
	// SQLite uses in-memory databases for testing, so this step is skipped.
	if driverName != "sqlite" {
		masterDataSourceName, err := setDatabaseInDataSourceName(driverName, baseDataSourceName, "")
		if err != nil {
			return "", errors.Trace(err)
		}
		masterDB, err := OpenSingleton(driverName, masterDataSourceName)
		if err != nil {
			return "", errors.New("failed to open master database", err)
		}
		defer masterDB.Close()

		// Create the testing database
		_, err = masterDB.Exec("DROP DATABASE IF EXISTS " + testingDatabaseName)
		if err != nil {
			return "", errors.New("failed to drop database %s", testingDatabaseName, err)
		}
		_, err = masterDB.Exec("CREATE DATABASE " + testingDatabaseName)
		if err != nil {
			return "", errors.New("failed to create database %s", testingDatabaseName, err)
		}

		// Cleanup leftover testing databases, on a best-effort basis. This is the
		// safety net for tests that exited before Close had a chance to fire
		// maybeDropTestingDatabase. A testing database is considered leftover if
		// it's more than 1 to 2 hours old.
		stmt := ""
		switch driverName {
		case "mysql":
			stmt = "SHOW DATABASES"
		case "pgx", "cockroachdb":
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
	}

	testingDSN, err := setDatabaseInDataSourceName(driverName, baseDataSourceName, testingDatabaseName)
	if err != nil {
		return "", errors.Trace(err)
	}

	// Cache for other openers in the same test
	testingGlobalMutex.Lock()
	testingDSNs[cacheKey] = testingDSN
	testingGlobalMutex.Unlock()

	return testingDSN, nil
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
	ctx, finishMigrate := db.telemetry.Load().beginMigrate(context.Background(), db.driverName, sequenceName)
	defer func() { finishMigrate(err) }()

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
			locked_before DATETIME(3) NOT NULL DEFAULT NOW_UTC(),
			PRIMARY KEY (seq_name, seq_num)
		)`
	case "pgx", "cockroachdb":
		stmt = `
		CREATE TABLE IF NOT EXISTS sequel_migrations (
			seq_name VARCHAR(256) NOT NULL,
			seq_num INT NOT NULL,
			completed BOOL NOT NULL DEFAULT FALSE,
			completed_on TIMESTAMP(3),
			locked_before TIMESTAMP(3) NOT NULL DEFAULT NOW_UTC(),
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
				locked_before DATETIME2(3) NOT NULL DEFAULT NOW_UTC(),
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
			locked_before TEXT NOT NULL DEFAULT NOW_UTC(),
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
	case "mysql", "pgx", "cockroachdb":
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
	slices.Sort(sequenceNumbersToRun)

	// Execute the migrations
	for len(sequenceNumbersToRun) > 0 {
		seqNum := sequenceNumbersToRun[0]

		// Insert new migrations into the database first
		// Ignore duplicate key violations
		switch db.driverName {
		case "mysql":
			stmt = `INSERT IGNORE INTO sequel_migrations (seq_name, seq_num) VALUES (?, ?)`
		case "pgx", "cockroachdb":
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
		case "mysql", "pgx", "cockroachdb":
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
		db.telemetry.Load().logMigrationAttempt(ctx, db.driverName, sequenceName, fileNames[seqNum])
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
					driverLine, _, _ := strings.Cut(stmt[p+10:], "\n")
					if !slices.Contains(strings.Fields(driverLine), db.driverName) {
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
				if stmt == "" {
					// Empty after stripping comments (e.g. a comment-only segment); skip to avoid erroring out
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
				stmt = `UPDATE sequel_migrations SET locked_before=DATE_ADD_MILLIS(NOW_UTC(), 15000) WHERE seq_name=? AND seq_num=?`
				_, err = db.Exec(stmt, sequenceName, seqNum)
				if err != nil {
					exit = true
				}
			}
		}

		db.telemetry.Load().recordMigration(ctx, db.driverName, sequenceName, err)

		if err != nil {
			// Release the lock
			stmt = `UPDATE sequel_migrations SET locked_before=NOW_UTC() WHERE seq_name=? AND seq_num=?`
			_, _ = db.Exec(stmt, sequenceName, seqNum)
			return errors.New("error running migration %s", fileNames[seqNum], err)
		}

		// Mark as complete
		switch db.driverName {
		case "mysql", "pgx", "cockroachdb":
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
	if driverName != "pgx" && driverName != "cockroachdb" {
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
	case "pgx", "cockroachdb":
		return "(NOW() AT TIME ZONE 'UTC')"
	case "mssql":
		return "(CONVERT(DATETIME2(3), SYSUTCDATETIME()))"
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
	case "pgx", "cockroachdb":
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
