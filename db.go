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
	crand "crypto/rand"
	"crypto/sha256"
	"database/sql"
	"database/sql/driver"
	"encoding/hex"
	"io"
	"io/fs"
	"log/slog"
	"math"
	"math/rand/v2"
	"net"
	"net/url"
	"os"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/go-sql-driver/mysql"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/microbus-io/errors"
	mssql "github.com/microsoft/go-mssqldb"
	"github.com/microsoft/go-mssqldb/msdsn"
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
	// testingProcessToken is part of every testing database name this process mints, so that two test processes -
	// go test ./... runs packages in parallel - never mint, and drop, each other's database.
	testingProcessToken = newTestingProcessToken()
	// testingDBRefs counts the live *DB handles on each generation of each testing database, keyed by
	// testingGenerationKey, so the auto-drop happens on the last close rather than on every one.
	testingDBRefs = map[string]int{}
	// testingGenerations counts the provisionings of each testing database, keyed by testingDatabaseKey. A handle
	// belongs to the generation it was opened on, and the last close of an older one drops nothing: its database
	// is gone already, and the name now belongs to a newer one.
	testingGenerations = map[string]int{}
	// testingLifecycles serializes, per testing database (testingDatabaseKey), its provisioning against its drop, so
	// a DROP that the last close decided on cannot run after the database was provisioned again.
	testingLifecycles = map[string]*sync.Mutex{}
	// testingDBKeys maps a testing database, by testingDatabaseKey, to its testingDSNs cache key, so dropping the
	// database can evict the DSN that named it.
	testingDBKeys = map[string]string{}
	// testingAnchors holds one connection, outside every handle's pool, to each live generation of each SQLite
	// testing database, keyed by testingGenerationKey. An in-memory database is dropped with its last connection, and a pool may close all of
	// its connections while its handle stays open.
	testingAnchors = map[string]*sql.DB{}
)

// maxTestingDatabaseName is the longest testing database name minted: PostgreSQL's identifier limit, one under MySQL's.
const maxTestingDatabaseName = 63

// newTestingProcessToken returns six random lower-case letters and digits.
func newTestingProcessToken() string {
	const alphabet = "abcdefghijklmnopqrstuvwxyz0123456789"
	b := make([]byte, 6)
	for i := range b {
		b[i] = alphabet[rand.IntN(len(alphabet))]
	}
	return string(b)
}

// testingDatabaseKey identifies a testing database across handles. Its name alone is not enough: one process may
// reach two servers under one test ID, and each has a database of that name.
func testingDatabaseKey(driverName, dsn, databaseName string) string {
	return physicalDriverName(driverName) + "|" + serverAddress(driverName, dsn) + "|" + databaseName
}

// testingGenerationKey identifies one generation of a testing database.
func testingGenerationKey(key string, generation int) string {
	return key + "#" + strconv.Itoa(generation)
}

// testingLifecycle returns the mutex that serializes the provisioning and the drop of a testing database.
func testingLifecycle(key string) *sync.Mutex {
	testingGlobalMutex.Lock()
	defer testingGlobalMutex.Unlock()
	mux, ok := testingLifecycles[key]
	if !ok {
		mux = &sync.Mutex{}
		testingLifecycles[key] = mux
	}
	return mux
}

// testingRetireHook runs after the last close of a testing database decided to drop it, before the drop, while its
// lifecycle is held. A variable, so that a test can hold a drop there.
var testingRetireHook = func() {}

// serverAddress returns the address of the server a DSN connects to, or "" for SQLite or a DSN that does not parse.
func serverAddress(driverName, dsn string) string {
	switch driverName {
	case "mysql", "mariadb":
		if cfg, err := mysql.ParseDSN(trimMySQLScheme(dsn)); err == nil {
			return cfg.Net + "(" + cfg.Addr + ")"
		}
	case "pgx", "cockroachdb":
		if cfg, err := pgx.ParseConfig(toPostgresScheme(dsn)); err == nil {
			return cfg.Host + ":" + strconv.Itoa(int(cfg.Port))
		}
	case "mssql":
		if cfg, err := msdsn.Parse(dsn); err == nil {
			return cfg.Host + ":" + strconv.Itoa(int(cfg.Port)) + "/" + cfg.Instance
		}
	}
	return ""
}

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
	// closed is set, under mutex, when the last handle closes. The embedded *sql.DB is never replaced once set,
	// since its readers take no lock: a closed singleton leaves the singleton map, and the next opener makes a
	// new *DB.
	closed bool
	// testingGeneration is the generation of the testing database this handle was opened on, if any.
	testingGeneration int
	mutex             sync.Mutex
	// pool mirrors the embedded *sql.DB for the pool-stats callback, which must not take mutex: OTel holds its
	// own lock while it runs the callback, and SetMeterProvider holds mutex while it takes that lock.
	pool      atomic.Pointer[sql.DB]
	telemetry atomic.Pointer[telemetry]
	// simulatedRTT is the artificial per-round-trip delay set by [DB.SimulateRTT], in nanoseconds. It is
	// read on every operation and may be changed while the pool is in use, so it is atomic rather than
	// guarded by mutex.
	simulatedRTT atomic.Int64
	// migrationsTableReady is set once Migrate has made sure sequel_migrations exists, so later calls skip
	// that round trip.
	migrationsTableReady atomic.Bool
}

/*
Open returns a database connection to the named data source with a dedicated
connection pool. Each call returns a distinct *DB; sequel does not coalesce by DSN.
The caller is responsible for sizing the pool via SetMaxOpenConns / SetMaxIdleConns
if the database/sql defaults (unlimited open, 2 idle) don't fit.

Use [OpenSingleton] when multiple consumers in the same process share a DSN and
you want sequel to manage one pool across all of them.

If a driver name is not provided, it is inferred from the data source name on a
best-effort basis. Drivers currently supported: "mysql" (MySQL), "mariadb" (MariaDB),
"pgx" (Postgres), "cockroachdb" (CockroachDB), "mssql" (SQL Server) or "sqlite" (SQLite).

Example data source name for each of the supported drivers:
  - mysql: username:password@tcp(hostname:3306)/ or mysql://username:password@tcp(hostname:3306)/
  - mariadb: mariadb://username:password@tcp(hostname:3306)/
  - pgx: postgres://username:password@hostname:5432/
  - cockroachdb: cockroachdb://username:password@hostname:26257/
  - mssql: sqlserver://username:password@hostname:1433
  - sqlite: file:path/to/database.sqlite

A URL scheme decides the driver ahead of anything else in the data source name, a port
included: mysql://, mariadb://, postgres:// or postgresql:// (pgx), cockroachdb://, sqlserver://
(mssql) and file: (sqlite). The mysql:// and mariadb:// prefixes go in front of a DSN in the
MySQL driver's own format, which names neither engine, and are stripped before the driver sees
it; without one, that format is taken for MySQL. A cockroachdb:// URL reaches the driver as
postgres://. A default port is the last guess, for a data source name with no scheme.

A PostgreSQL keyword DSN (host=... dbname=...) is inferred as pgx when its first key is a
PostgreSQL connection keyword and it has no ; outside a quoted value. The prefixes pgx+dsn:// and cockroachdb+dsn://
name the logical driver of a keyword DSN and are stripped before the Go driver parses it.

On "pgx" and "cockroachdb", every connection starts its session in the UTC time zone,
whatever the server, database or role default, so that NOW_UTC() stores the right instant
in TIMESTAMP and TIMESTAMPTZ columns alike. A time zone the caller chooses is kept instead:
a timezone parameter in the data source name or in a service file it names, an options
parameter that sets timezone, or the PGTZ or PGOPTIONS environment variable. A name from
the pgx stdlib's RegisterConnConfig is passed through unchanged; set timezone in its
RuntimeParams.
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
	generation, err := retainTestingDatabase(driverName, dataSourceName)
	if err != nil {
		sqlDB.Close()
		return nil, errors.Trace(err)
	}
	db = &DB{
		DB:                sqlDB,
		driverName:        driverName,
		dataSourceName:    dataSourceName,
		refCount:          1,
		testingGeneration: generation,
	}
	db.pool.Store(sqlDB)
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

	var singletonDB *DB
	for {
		singletonMutex.Lock()
		var ok bool
		singletonDB, ok = singletonsMap[singletonKey]
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
		if !singletonDB.closed {
			break
		}
		// Closed by its last holder after it was looked up; the map no longer holds it
		singletonDB.mutex.Unlock()
	}
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
	// Only on this branch, not the coalescing one above: the extra callers share this *DB.
	generation, err := retainTestingDatabase(driverName, dataSourceName)
	if err != nil {
		sqlDB.Close()
		return nil, errors.Trace(err)
	}
	singletonDB.testingGeneration = generation
	singletonDB.DB = sqlDB
	singletonDB.pool.Store(sqlDB)
	singletonDB.refCount = 1
	singletonDB.telemetry.Store(newDefaultTelemetry(singletonDB))
	singletonDB.adjustConnectionLimits()
	return singletonDB, nil
}

// normalizeDriverAndDSN normalizes driver name aliases, infers the driver name when
// missing, and applies driver-specific DSN tweaks (MySQL connection params, the
// PostgreSQL session time zone, SQLite busy_timeout pragma).
func normalizeDriverAndDSN(driverName, dataSourceName string) (string, string, error) {
	if dataSourceName == "" {
		return "", "", errors.New("data source name is required")
	}
	if driverName == "sqlite3" {
		driverName = "sqlite"
	}
	dataSourceName = lowerScheme(dataSourceName)
	if driverName == "" {
		driverName = inferDriverName(dataSourceName)
	}
	if driverName == "" {
		return "", "", errors.New("driver name could not be inferred from data source name")
	}
	switch driverName {
	case "mysql", "mariadb":
		cfg, err := mysql.ParseDSN(trimMySQLScheme(dataSourceName))
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
	case "pgx", "cockroachdb":
		dataSourceName = pinSessionTimeZoneUTC(toPostgresScheme(dataSourceName))
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

// pinSessionTimeZoneUTC adds timezone=UTC to a PostgreSQL DSN unless the caller already chose a session
// time zone. A name registered with the pgx stdlib's RegisterConnConfig is a lookup key, not a DSN, and is
// returned unchanged: rewriting it would break the lookup.
func pinSessionTimeZoneUTC(dsn string) string {
	if strings.HasPrefix(dsn, "registeredConnConfig") || callerChoseTimeZone(dsn) {
		return dsn
	}
	rest, isURL := strings.CutPrefix(dsn, "postgres://")
	if !isURL {
		rest, isURL = strings.CutPrefix(dsn, "postgresql://")
	}
	if !isURL {
		// Prepended, not appended: a keyword DSN may end in a backslash, which would escape the separator.
		return "timezone=UTC " + dsn
	}
	// The driver takes everything before an '@' that precedes the first '/' as userinfo, so a raw '?' in a
	// password does not start the query. Look for the query only after it.
	if i := strings.IndexAny(rest, "@/"); i >= 0 && rest[i] == '@' {
		rest = rest[i+1:]
	}
	switch {
	case !strings.Contains(rest, "?"):
		return dsn + "?timezone=UTC"
	case strings.HasSuffix(dsn, "?"), strings.HasSuffix(dsn, "&"):
		// The driver rejects an empty query pair, so "?&timezone=UTC" would not parse.
		return dsn + "timezone=UTC"
	default:
		return dsn + "&timezone=UTC"
	}
}

// optionsTimeZonePattern matches a timezone setting inside a PostgreSQL options value: -c timezone=...,
// -ctimezone=... or --timezone=..., in any letter case.
var optionsTimeZonePattern = regexp.MustCompile(`(?i)(^|\s)(-c\s*|--)timezone\s*=`)

// callerChoseTimeZone reports whether a PostgreSQL DSN, a service file it names, or the PGTZ / PGOPTIONS
// environment variables set the session time zone.
func callerChoseTimeZone(dsn string) bool {
	cfg, err := pgx.ParseConfig(dsn)
	if err != nil {
		// The driver parses again on every connection, and a file missing now (a certificate, a service
		// file) may be there by then. Judge by the text and the environment, erring towards the caller.
		return strings.Contains(strings.ToLower(dsn), "timezone") ||
			os.Getenv("PGTZ") != "" ||
			strings.Contains(strings.ToLower(os.Getenv("PGOPTIONS")), "timezone")
	}
	for k, v := range cfg.RuntimeParams {
		if strings.EqualFold(k, "timezone") {
			return true
		}
		if strings.EqualFold(k, "options") && optionsTimeZonePattern.MatchString(v) {
			return true
		}
	}
	return false
}

// IsLockContentionError returns true if the error indicates database lock contention or a deadlock.
// Such errors are transient and the operation can typically be retried.
// Recognizes lock errors from SQLite, MySQL, PostgreSQL, SQL Server, and CockroachDB.
//
// An error carrying a driver's native error type is classified by its code alone, immune to message wording,
// localization, and user data appearing in the message. A substring match is used only when no driver type is
// present in the chain (e.g. some wrapped or text-only CockroachDB retry errors).
func IsLockContentionError(err error) bool {
	if err == nil {
		return false
	}

	// A typed error decides by its code. Falling through to the message once took a duplicate key whose value
	// read "restart transaction" for contention, and Transact re-ran a closure that was not safe to repeat.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// 40P01 deadlock_detected, 40001 serialization_failure (PostgreSQL and CockroachDB).
		return pgErr.Code == "40P01" || pgErr.Code == "40001"
	}
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		// 1213 ER_LOCK_DEADLOCK, 1205 ER_LOCK_WAIT_TIMEOUT.
		return myErr.Number == 1213 || myErr.Number == 1205
	}
	var msErr mssql.Error
	if errors.As(err, &msErr) {
		// 1205 deadlock victim, 1222 lock request time out.
		return msErr.Number == 1205 || msErr.Number == 1222
	}
	var liteErr *sqlite.Error
	if errors.As(err, &liteErr) {
		// The low byte is the primary result code: 5 SQLITE_BUSY, 6 SQLITE_LOCKED (also covers the
		// extended codes such as SQLITE_BUSY_SNAPSHOT and SQLITE_LOCKED_SHAREDCACHE).
		switch liteErr.Code() & 0xff {
		case 5, 6:
			return true
		}
		return false
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

// IsUndefinedTableError returns true if the error indicates that a statement named a table that does not exist.
// Recognizes the error from SQLite, MySQL, MariaDB, PostgreSQL, SQL Server, and CockroachDB. A column qualified by
// a name that is not in the FROM clause is not a missing table, although PostgreSQL and CockroachDB report it with
// the same SQLSTATE 42P01. SQLite is the exception: it reports a qualified star whose qualifier names no table
// (SELECT f.* FROM t) as "no such table: f", and this returns true for it.
//
// As with [IsLockContentionError], the driver's native error code is preferred, and a substring match is the
// fallback for errors whose driver type is not in the chain.
func IsUndefinedTableError(err error) bool {
	if err == nil {
		return false
	}

	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		// 42P01 undefined_table (PostgreSQL and CockroachDB), which also reports a qualifier that names no
		// FROM-clause entry. The routine that raised it tells them apart, and is not localized; the message is
		// the fallback for a server whose routine names differ.
		return pgErr.Code == "42P01" && !columnResolutionRoutines[pgErr.Routine] && !isFromClauseEntryMessage(pgErr.Message)
	}
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		// 1146 ER_NO_SUCH_TABLE.
		return myErr.Number == 1146
	}
	var msErr mssql.Error
	if errors.As(err, &msErr) {
		// 208 invalid object name.
		return msErr.Number == 208
	}
	// SQLite reports a missing table with the generic SQLITE_ERROR result code, so only its message tells it apart.
	return isUndefinedTableMessage(err.Error())
}

// isUndefinedTableMessage is the substring match for missing-table errors whose native driver type is not in the
// error chain, and for SQLite's, which carry no distinct code.
func isUndefinedTableMessage(msg string) bool {
	switch {
	case isFromClauseEntryMessage(msg): // PostgreSQL, CockroachDB: a bad qualifier, under the same 42P01
		return false
	case strings.Contains(msg, "no such table"): // SQLite
		return true
	case strings.Contains(msg, "Error 1146"): // MySQL, MariaDB
		return true
	case strings.Contains(msg, "SQLSTATE 42P01"): // PostgreSQL, CockroachDB
		return true
	case strings.Contains(msg, "Invalid object name"): // SQL Server
		return true
	}
	return false
}

// columnResolutionRoutines are the server routines that report a qualifier naming no FROM-clause entry as 42P01
// undefined_table although no table is missing: PostgreSQL's, then CockroachDB's.
var columnResolutionRoutines = map[string]bool{
	"errorMissingRTE":           true,
	"ResolveColumnItem":         true,
	"ResolveAllColumnsSelector": true,
}

// isFromClauseEntryMessage matches PostgreSQL's "missing FROM-clause entry" and "invalid reference to FROM-clause
// entry" errors, and CockroachDB's "no data source matches prefix" and "no data source matches pattern", which
// report as 42P01 undefined_table although no table is missing.
func isFromClauseEntryMessage(msg string) bool {
	return strings.Contains(msg, "FROM-clause entry") || strings.Contains(msg, "no data source matches")
}

// IsConnectionError returns true if the error says the database could not be reached, or the connection to it was
// lost, rather than anything about the statement: a refused, reset or closed connection, a server shutting down or
// not accepting connections yet, a connection limit, or a server that refuses writes because it is read-only, as a
// primary demoted in a failover does. Such an error says nothing against the statement, which may succeed on a
// later attempt. Recognizes these errors from MySQL, MariaDB, PostgreSQL, SQL Server, and CockroachDB; SQLite has
// no connection to lose, and none of its errors is one.
//
// A read-only refusal counts whatever made the server or transaction read-only, so a caller that starts a
// read-only transaction and then writes in it gets true. A context cancellation or deadline is the caller's own,
// not the connection's, and returns false.
//
// A statement can itself end its connection: a MySQL server closes it on a packet larger than its
// max_allowed_packet, and the driver reports the lost connection. A caller that retries a statement on a
// connection error must therefore bound the retries.
//
// As with [IsLockContentionError], the driver's native error code is preferred, and a substring match is the
// fallback for errors whose driver type is not in the chain.
func IsConnectionError(err error) bool {
	if err == nil {
		return false
	}
	// A context deadline satisfies net.Error, so it must be decided before the network check below.
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, sql.ErrConnDone) ||
		errors.Is(err, io.EOF) || errors.Is(err, io.ErrUnexpectedEOF) ||
		errors.Is(err, mysql.ErrInvalidConn) {
		return true
	}

	// A typed error the server reported decides by its code.
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		switch pgErr.Code {
		case "08000", "08001", "08003", "08004", "08006", "08007", // connection_exception, except 08P01 protocol_violation
			"57P01", "57P02", "57P03", "57P05", // admin_shutdown, crash_shutdown, cannot_connect_now, idle_session_timeout
			"53300", // too_many_connections
			"25006": // read_only_sql_transaction
			return true
		}
		return false
	}
	var myErr *mysql.MySQLError
	if errors.As(err, &myErr) {
		switch myErr.Number {
		case 1040, // ER_CON_COUNT_ERROR, too many connections
			1053, // ER_SERVER_SHUTDOWN
			1290, // ER_OPTION_PREVENTS_STATEMENT, raised by read_only and super_read_only
			1836, // ER_READ_ONLY_MODE
			1927, // ER_CONNECTION_KILLED (MariaDB)
			4031: // ER_CLIENT_INTERACTION_TIMEOUT, disconnected for inactivity
			return true
		}
		return false
	}
	var msErr mssql.Error
	if errors.As(err, &msErr) {
		switch msErr.Number {
		case 233, // no process is on the other end of the pipe
			4060,     // cannot open the database requested by the login
			976, 983, // the database is an availability group secondary
			3906,                // the database is read-only
			40197, 40501, 40613, // Azure SQL: service error, service busy, database unavailable
			49918, 49919, 49920: // Azure SQL: not enough resources to process the request
			return true
		}
		return false
	}
	var liteErr *sqlite.Error
	if errors.As(err, &liteErr) {
		return false
	}

	// An error from below the protocol: the dial, the socket, or the TDS stream.
	var netErr net.Error
	var connectErr *pgconn.ConnectError
	var streamErr mssql.StreamError
	if errors.As(err, &netErr) || errors.As(err, &connectErr) || errors.As(err, &streamErr) {
		return true
	}
	return isConnectionMessage(err.Error())
}

// isConnectionMessage is the substring fallback for connection errors whose native driver type is not in the
// error chain.
func isConnectionMessage(msg string) bool {
	switch {
	case strings.Contains(msg, "connection refused"), strings.Contains(msg, "connection reset"),
		strings.Contains(msg, "broken pipe"), strings.Contains(msg, "bad connection"),
		strings.Contains(msg, "unexpected EOF"), strings.Contains(msg, "i/o timeout"): // the socket, any driver
		return true
	case strings.Contains(msg, "server closed the connection unexpectedly"), strings.Contains(msg, "conn closed"): // PostgreSQL
		return true
	case strings.Contains(msg, "invalid connection"), strings.Contains(msg, "server has gone away"),
		strings.Contains(msg, "Lost connection to"): // MySQL, MariaDB
		return true
	case strings.Contains(msg, "transport-level error"): // SQL Server
		return true
	}
	return false
}

// Close closes the database connection.
//
// When the underlying database name matches the testing pattern (testing_NN_...),
// the last handle to close drops the database from the server as a best-effort
// cleanup, making [CreateTestingDatabase]-provisioned databases self-cleaning on
// test teardown. Last across every handle on that database, not just this one:
// [Open] hands out a *DB per call, so several may share one testing database.
//
// Once the last handle is closed, operations on the *DB fail as they do on a closed [sql.DB].
func (db *DB) Close() (err error) {
	if db == nil {
		return nil
	}
	db.mutex.Lock()
	defer db.mutex.Unlock()
	if db.DB == nil || db.closed {
		return nil
	}
	db.refCount--
	if db.refCount == 0 {
		// Unregistered, or every Open would add a callback that keeps this *DB alive and reports a closed pool
		if reg := db.telemetry.Load().poolRegistration; reg != nil {
			_ = reg.Unregister()
		}
		db.pool.Store(nil)
		// The closed *sql.DB stays in place, so that a later call reports a closed database rather than
		// dereferencing nil; a reopened singleton is a new *DB
		db.closed = true
		if db.singletonKey != "" {
			singletonMutex.Lock()
			if singletonsMap[db.singletonKey] == db {
				delete(singletonsMap, db.singletonKey)
			}
			singletonMutex.Unlock()
		}
		err = db.DB.Close()
		db.maybeDropTestingDatabase()
	} else {
		db.adjustConnectionLimits()
	}
	return errors.Trace(err)
}

// testingDatabaseNameOf reports the testing database a DSN names, if any: a name CreateTestingDatabase could
// have minted, matched in full, is the only signal.
func testingDatabaseNameOf(driverName string, dsn string) (string, bool) {
	// Cheap reject before parsing: this runs on every Open, production ones included.
	if !strings.Contains(dsn, "testing_") {
		return "", false
	}
	databaseName, err := databaseNameFromDataSourceName(driverName, dsn)
	// The full pattern, not just the prefix: the name is spliced into a DROP, and anything CreateTestingDatabase
	// could not have minted is not sequel's to drop
	if err != nil || !leftoverTestingDatabasePattern.MatchString(databaseName) {
		return "", false
	}
	return databaseName, true
}

// retainTestingDatabase records one more handle on the current generation of the testing database the DSN names,
// and returns that generation. It pins a SQLite testing database with an anchor connection when this is the
// generation's first handle. Call it once per *DB that will eventually run maybeDropTestingDatabase.
func retainTestingDatabase(driverName string, dsn string) (generation int, err error) {
	databaseName, ok := testingDatabaseNameOf(driverName, dsn)
	if !ok {
		return 0, nil
	}
	key := testingDatabaseKey(driverName, dsn, databaseName)
	testingGlobalMutex.Lock()
	defer testingGlobalMutex.Unlock()
	generation = testingGenerations[key]
	refKey := testingGenerationKey(key, generation)
	if testingDBRefs[refKey] == 0 && driverName == "sqlite" {
		anchor, err := sql.Open(physicalDriverName(driverName), dsn)
		if err != nil {
			return 0, errors.Trace(err)
		}
		anchor.SetMaxOpenConns(1)
		anchor.SetMaxIdleConns(1)
		err = anchor.Ping()
		if err != nil {
			anchor.Close()
			return 0, errors.Trace(err)
		}
		testingAnchors[refKey] = anchor
	}
	testingDBRefs[refKey]++
	return generation, nil
}

// maybeDropTestingDatabase releases this handle on the database backing this *DB if its name has the testing prefix
// produced by [CreateTestingDatabase], and drops the database once the last handle on its generation is gone. The
// decision and the drop are made under the database's lifecycle, so that it cannot be provisioned again in between;
// the last handle on an older generation drops nothing. Errors are swallowed: the leftover-DB sweep on the next test
// run is the safety net. For SQLite (in-memory), closing the anchor connection is the drop.
func (db *DB) maybeDropTestingDatabase() {
	dbName, ok := testingDatabaseNameOf(db.driverName, db.dataSourceName)
	if !ok {
		return
	}
	key := testingDatabaseKey(db.driverName, db.dataSourceName, dbName)
	lifecycle := testingLifecycle(key)
	lifecycle.Lock()
	defer lifecycle.Unlock()

	refKey := testingGenerationKey(key, db.testingGeneration)
	testingGlobalMutex.Lock()
	testingDBRefs[refKey]--
	last := testingDBRefs[refKey] <= 0
	current := db.testingGeneration == testingGenerations[key]
	if last {
		delete(testingDBRefs, refKey)
		// Close under the mutex: a retainTestingDatabase that ran before the close would anchor
		// this database instead of minting a fresh one.
		if anchor, ok := testingAnchors[refKey]; ok {
			anchor.Close()
			delete(testingAnchors, refKey)
		}
		// The cached DSN names a database that is about to stop existing, so evict it at once, before a cache hit
		// can hand it out, and let the next CreateTestingDatabase caller mint it afresh - after the drop, since it
		// waits for the lifecycle.
		if cacheKey, ok := testingDBKeys[key]; ok && current {
			delete(testingDBKeys, key)
			delete(testingDSNs, cacheKey)
		}
	}
	testingGlobalMutex.Unlock()
	if !last || !current {
		return
	}
	testingRetireHook()
	if db.driverName == "sqlite" {
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
	// Bounded: a leaked handle can leave the DROP blocked server-side on a database still in use.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	masterDB.ExecContext(ctx, "DROP DATABASE IF EXISTS "+dbName)
}

/*
SetTracerProvider attaches an OpenTelemetry TracerProvider so sequel emits a client span around each query,
transaction, and migration. A freshly opened *DB already uses the process-wide otel.GetTracerProvider();
call this to override it, or pass nil to revert to that global provider (whose default is a no-op).

Observability is configured after Open/OpenSingleton (which keep the standard database/sql signature) rather
than at construction. This loses nothing: sql.Open does no I/O - it only prepares a lazy pool - so there is
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

// SetLogger attaches an slog.Logger. Errors are returned to the caller, who logs them, so nothing is logged above
// Debug for one; sequel logs one-off events such as schema migrations at Info, and - when the logger is enabled at
// Debug level - each query at Debug, with its statement text and any error message. Per-query logging is therefore
// controlled by the logger's own level, not a separate switch. A freshly opened *DB uses a discard logger; pass nil here to
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
	pool := db.pool.Load()
	if pool == nil {
		return sql.DBStats{}, false
	}
	return pool.Stats(), true
}

// Exec shadows sql.DB.Exec and conforms arg placeholders for the driver.
func (db *DB) Exec(query string, args ...any) (sql.Result, error) {
	return instrumentExec(db.telemetry.Load(), db.rtt(), context.Background(), lookupQuery(db.driverName, query),
		func(_ context.Context, q string) (sql.Result, error) {
			return db.DB.Exec(q, args...)
		})
}

// ExecContext shadows sql.DB.ExecContext and conforms arg placeholders for the driver.
func (db *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return instrumentExec(db.telemetry.Load(), db.rtt(), ctx, lookupQuery(db.driverName, query),
		func(ctx context.Context, q string) (sql.Result, error) {
			return db.DB.ExecContext(ctx, q, args...)
		})
}

// Query shadows sql.DB.Query and conforms arg placeholders for the driver. It returns a [Rows], which
// embeds *sql.Rows so existing rows.Next()/rows.Scan()/rows.Err() call sites are unchanged. A *DB query
// is not transactional, so Rows here is a pure passthrough (no error latching).
func (db *DB) Query(query string, args ...any) (*Rows, error) {
	rows, err := instrumentExec(db.telemetry.Load(), db.rtt(), context.Background(), lookupQuery(db.driverName, query),
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
	rows, err := instrumentExec(db.telemetry.Load(), db.rtt(), ctx, lookupQuery(db.driverName, query),
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
	return instrumentQueryRow(db.telemetry.Load(), db.rtt(), context.Background(), lookupQuery(db.driverName, query), nil,
		func(_ context.Context, q string) *sql.Row {
			return db.DB.QueryRow(q, args...)
		})
}

// QueryRowContext shadows sql.DB.QueryRowContext and conforms arg placeholders for the driver. It returns a
// [Row], which embeds *sql.Row so existing QueryRowContext(...).Scan(...) call sites are unchanged.
func (db *DB) QueryRowContext(ctx context.Context, query string, args ...any) *Row {
	return instrumentQueryRow(db.telemetry.Load(), db.rtt(), ctx, lookupQuery(db.driverName, query), nil,
		func(ctx context.Context, q string) *sql.Row {
			return db.DB.QueryRowContext(ctx, q, args...)
		})
}

// Prepare shadows sql.DB.Prepare and conforms arg placeholders for the driver. It returns a [Stmt], which
// embeds *sql.Stmt so existing stmt.Exec(...)/stmt.Close() call sites are unchanged; executions of the
// returned statement stay on sequel's instrumented path.
func (db *DB) Prepare(query string) (*Stmt, error) {
	// sql.DB.Prepare is defined as PrepareContext(context.Background(), ...), so this loses nothing and
	// keeps one instrumented path.
	return db.PrepareContext(context.Background(), query)
}

// PrepareContext shadows sql.DB.PrepareContext and conforms arg placeholders for the driver. It returns a
// [Stmt] (see Prepare).
func (db *DB) PrepareContext(ctx context.Context, query string) (*Stmt, error) {
	pq := lookupQuery(db.driverName, query)
	sqlStmt, err := instrumentExec(db.telemetry.Load(), db.rtt(), ctx, pq,
		func(ctx context.Context, q string) (*sql.Stmt, error) {
			return db.DB.PrepareContext(ctx, q)
		})
	if err != nil {
		return nil, err
	}
	return &Stmt{Stmt: sqlStmt, pq: pq, db: db}, nil
}

// Ping shadows sql.DB.Ping so a simulated round-trip delay ([DB.SimulateRTT]) applies to it: a ping is a
// wire operation like any statement. It is not otherwise instrumented - sequel has never traced a ping.
func (db *DB) Ping() error {
	_ = simulateRTT(context.Background(), db.rtt()) // an uncancellable context: the pause always elapses
	return traceErr(db.DB.Ping())
}

// PingContext shadows sql.DB.PingContext for the same reason as [DB.Ping]. A context that expires during
// the simulated latency fails the ping without reaching the database.
func (db *DB) PingContext(ctx context.Context) error {
	if err := simulateRTT(ctx, db.rtt()); err != nil {
		return errors.Trace(err)
	}
	return traceErr(db.DB.PingContext(ctx))
}

// DriverName is the name of the driver: "mysql", "mariadb", "pgx", "cockroachdb", "mssql" or "sqlite".
func (db *DB) DriverName() string {
	return db.driverName
}

// UnpackQuery expands virtual functions (e.g. NOW_UTC(), REGEXP_TEXT_SEARCH()) into
// driver-specific SQL expressions, and conforms arg placeholders
// to the syntax expected by the driver (e.g. ? to $1, $2 for PostgreSQL and SQL Server).
func (db *DB) UnpackQuery(query string) (string, error) {
	return unpackQuery(db.driverName, query)
}

// unpackQuery expands virtual functions and conforms arg placeholders for the driver, through the query cache.
func unpackQuery(driverName string, query string) (string, error) {
	pq := lookupQuery(driverName, query)
	if pq.err != nil {
		return "", errors.Trace(pq.err)
	}
	return pq.text, nil
}

// traceErr attaches a stack trace to an operation error at the API boundary, passing the database/sql
// sentinels through untouched: sql.ErrNoRows and sql.ErrTxDone are routine control flow, not failures, and
// callers are entitled to compare them with == exactly as they would against database/sql. The identity
// check (not errors.Is) is deliberate - only the bare sentinel is ==-comparable in the first place.
// Everything else - driver errors, context errors - gains the stack of the failing call site; the wrapper
// preserves Unwrap, so errors.Is/errors.As and IsLockContentionError see through it.
func traceErr(err error) error {
	if err == nil || err == sql.ErrNoRows || err == sql.ErrTxDone {
		return err
	}
	return errors.Trace(err)
}

// Begin starts a transaction and returns a sequel.Tx that applies virtual function
// expansion and placeholder conforming.
func (db *DB) Begin() (*Tx, error) {
	// sql.DB.Begin is defined as BeginTx(context.Background(), nil), so this loses nothing and keeps one
	// instrumented path. The transaction then has no caller context, so its lifecycle spans are roots.
	return db.BeginTx(context.Background(), nil)
}

// BeginTx starts a transaction with the given options and returns a sequel.Tx that
// applies virtual function expansion and placeholder conforming.
func (db *DB) BeginTx(ctx context.Context, opts *sql.TxOptions) (*Tx, error) {
	sqlTx, _, rtt, err := db.beginTx(ctx, nil, false, opts)
	if err != nil {
		return nil, errors.Trace(err)
	}
	return &Tx{Tx: sqlTx, driverName: db.driverName, rtt: rtt, ctx: ctx, t: db.telemetry.Load()}, nil
}

// beginTx opens the underlying transaction under a BEGIN span, returning the simulated delay captured for the
// transaction's lifetime alongside it, so every statement and the eventual COMMIT run at one latency. On a held
// connection it begins there and returns it. Otherwise, with hold set it begins on a connection it checks out and
// returns, so the caller can reach the driver connection under the transaction; the caller closes that Conn after
// the transaction ends. The checkout sits inside the BEGIN span, where BeginTx's own pool wait sits.
func (db *DB) beginTx(ctx context.Context, held *sql.Conn, hold bool, opts *sql.TxOptions) (*sql.Tx, *sql.Conn, time.Duration, error) {
	rtt := db.rtt()
	var conn *sql.Conn
	sqlTx, err := instrumentTxOp(db.telemetry.Load(), rtt, ctx, db.driverName, "BEGIN",
		func(ctx context.Context) (*sql.Tx, error) {
			if db.driverName == "mssql" {
				ctx = mssqlTxContext{ctx}
			}
			if held != nil {
				sqlTx, err := held.BeginTx(ctx, opts)
				if err != nil {
					return nil, plainCancellation(err)
				}
				conn = held
				return sqlTx, nil
			}
			if !hold {
				sqlTx, err := db.DB.BeginTx(ctx, opts)
				return sqlTx, plainCancellation(err)
			}
			c, err := db.DB.Conn(ctx)
			if err != nil {
				return nil, err
			}
			sqlTx, err := c.BeginTx(ctx, opts)
			if err != nil {
				_ = c.Close()
				return nil, err
			}
			conn = c
			return sqlTx, nil
		})
	return sqlTx, conn, rtt, err
}

// mssqlTxContext is the context a SQL Server transaction begins on. go-mssqldb v1.11.2 pools a connection whose
// rollback on a cancelled context returned with the server's reply unread, and the next request on it fails
// (https://github.com/microsoft/go-mssqldb/issues/390). It discards one after a deadline, a net.Error, so a
// cancellation is handed to it as a net.Error too. Remove once a driver release carries the fix
// (https://github.com/microsoft/go-mssqldb/pull/486).
type mssqlTxContext struct{ context.Context }

// Err returns the context's error, a cancellation wrapped as a net.Error.
func (c mssqlTxContext) Err() error {
	err := c.Context.Err()
	if errors.Is(err, context.Canceled) {
		return connCanceledError{err}
	}
	return err
}

// connCanceledError is a cancellation that the SQL Server driver takes for a network error. Its message and
// errors.Is are the cancellation's.
type connCanceledError struct{ error }

func (e connCanceledError) Unwrap() error { return e.error }
func (connCanceledError) Timeout() bool   { return false }
func (connCanceledError) Temporary() bool { return false }

// plainCancellation returns the cancellation a connCanceledError wraps, so that only the driver sees the wrapper:
// database/sql and the driver return the context's Err as is from BEGIN, COMMIT and ROLLBACK.
func plainCancellation(err error) error {
	if c, ok := err.(connCanceledError); ok {
		return c.error
	}
	return err
}

// transactMaxAttempts bounds how many times Transact reruns a transaction that keeps losing to lock
// contention before giving up and returning the last error.
const transactMaxAttempts = 8

// transactBackoff sleeps before retry attempt n, jittered so callers that lost the same deadlock do not retry
// in lockstep.
func transactBackoff(n int) {
	time.Sleep(time.Duration(n)*time.Millisecond + time.Duration(rand.IntN(3))*time.Millisecond)
}

// Transact runs fn inside a transaction, committing on success and rolling back on error. If the
// transaction fails on lock contention or a deadlock, it is retried with a short jittered backoff.
// Because a retry re-executes fn from the start in a new transaction, fn must be safe to run more than
// once; any non-transactional side effects it performs (in-memory changes, channel sends) may repeat.
//
// The Tx passed to fn records the first statement error and short-circuits the remaining statements, so
// fn cannot commit partial work even if it does not check every statement's error. For SQL Server,
// SET XACT_ABORT ON is applied so that any statement error aborts the whole transaction.
func (db *DB) Transact(ctx context.Context, fn func(tx *Tx) error) error {
	return db.transact(ctx, nil, fn)
}

// transact runs Transact's attempts, each on a connection from the pool, or every one on held when it is set.
func (db *DB) transact(ctx context.Context, held *Conn, fn func(tx *Tx) error) (err error) {
	op := db.telemetry.Load().beginTransact(ctx, db.driverName)
	ctx = op.ctx
	attempts := 0
	defer func() {
		// A panic leaves err nil, and would otherwise be reported as a commit
		if r := recover(); r != nil {
			op.finish(attempts, errors.New("panic in transaction: %v", r))
			panic(r)
		}
		op.finish(attempts, err)
	}()
	for attempt := range transactMaxAttempts {
		if attempt > 0 {
			transactBackoff(attempt)
		}
		attempts++
		err = db.transactOnce(ctx, held, fn)
		if err == nil || !IsLockContentionError(err) || (held != nil && held.lost.Load()) {
			return err
		}
	}
	err = errors.Trace(err)
	return err
}

// transactOnce executes one attempt of a Transact: begin, run fn, commit, rolling back on any failure. On a held
// connection, an attempt that does not commit marks it lost when the connection is gone or the context has ended.
func (db *DB) transactOnce(ctx context.Context, held *Conn, fn func(tx *Tx) error) (err error) {
	committed := false
	// connErr is an error of the transaction's own that may say its connection is gone: not fn's, which may carry
	// another connection's.
	var connErr error
	if held != nil {
		if err := ctx.Err(); err != nil {
			// Ended between attempts: nothing began, so the connection is untouched
			return errors.Trace(err)
		}
		// Deferred before the rollback below, so it runs after it. With the context ended, database/sql may still be
		// rolling back on another goroutine when this returns, and on SQL Server the driver discards the connection.
		defer func() {
			if !committed && (ctx.Err() != nil || connGone(connErr)) {
				held.lost.Store(true)
			}
		}()
	}
	// On the pgx drivers the transaction begins on a held connection, so a Batch can reach the driver
	// connection under it and pipeline. Checking one out is the same pool operation BeginTx performs.
	pipelined := physicalDriverName(db.driverName) == "pgx"
	var heldConn *sql.Conn
	if held != nil {
		heldConn = held.conn
	}
	sqlTx, conn, rtt, err := db.beginTx(ctx, heldConn, pipelined, nil)
	if err != nil {
		connErr = err
		return errors.Trace(err)
	}
	if conn != nil && held == nil {
		// Deferred before the rollback below, so it runs after it: closing a Conn waits for its transaction to
		// end.
		defer conn.Close()
	}
	if !pipelined {
		conn = nil // A Batch pipelines whenever its transaction has a connection
	}
	// ctx carries the transact span, so the COMMIT/ROLLBACK spans nest under it like the statements do.
	tx := &Tx{Tx: sqlTx, driverName: db.driverName, autoErr: true, rtt: rtt, ctx: ctx, t: db.telemetry.Load(), conn: conn}
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	// XACT_ABORT ON makes any statement error abort the whole transaction server-side, so a deadlock or constraint
	// failure cannot leave it half-applied and committable. It is a session setting: a held Conn turns it off before
	// its next statement outside a transaction, and needs no SET while it is still on.
	if db.driverName == "mssql" && (held == nil || !held.xactAbortOn.Load()) {
		if err := setXactAbort(ctx, sqlTx, rtt, true); err != nil {
			connErr = err
			return errors.Trace(err)
		}
		if held != nil {
			held.xactAbortOn.Store(true)
		}
	}
	if err := fn(tx); err != nil {
		connErr = tx.Err()
		return errors.Trace(err)
	}
	if err := tx.Err(); err != nil {
		connErr = err
		return errors.Trace(err)
	}
	if err := tx.batchPending(); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		connErr = err
		return errors.Trace(err)
	}
	committed = true
	return nil
}

// InsertReturnID executes an INSERT statement and returns the auto-generated ID for the named ID column.
// idColumn must be a plain identifier matching [A-Za-z_][A-Za-z0-9_]* - it is spliced into the statement
// on some drivers, so quoted or exotic column names are rejected rather than escaped.
func (db *DB) InsertReturnID(ctx context.Context, idColumn string, stmt string, args ...any) (int64, error) {
	return insertReturnID(ctx, db, db.driverName, idColumn, stmt, args...)
}

// identifierPattern is the charset accepted for an identifier that is spliced into SQL text, such as
// InsertReturnID's idColumn (RETURNING <col> on PostgreSQL/CockroachDB, OUTPUT INSERTED.<col> on SQL
// Server). Same principle as JSON_FIELD paths: the narrow charset is what makes the splice safe on every
// dialect - a quote or bracket is rejected up front, not escaped.
var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// insertReturnID executes an INSERT statement and returns the auto-generated ID for the named ID column.
// The column name is validated on every driver, not only the ones that splice it, so a bad name fails the
// same way everywhere.
func insertReturnID(ctx context.Context, qe Executor, driverName string, idColumn string, stmt string, args ...any) (int64, error) {
	if !identifierPattern.MatchString(idColumn) {
		return 0, errors.New("ID column must match [A-Za-z_][A-Za-z0-9_]*: %s", idColumn)
	}
	switch driverName {
	case "mysql", "mariadb", "sqlite":
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

// injectOutputInserted rewrites an INSERT statement to include an OUTPUT INSERTED clause before the source clause
// (VALUES, DEFAULT VALUES or SELECT) of its top-level INSERT, for use with MSSQL. It reads only code, at the
// outermost level: a VALUES or SELECT in a comment, a string, a quoted identifier, a column list or a CTE before the
// INSERT is not the source clause.
func injectOutputInserted(stmt string, idColumn string) (string, error) {
	inserting := false
	depth := 0
	prevStart, prev := -1, ""
	floor := 0 // The end of the last comment or string, which the clause must not be moved into
	for i := 0; i < len(stmt); {
		if j := skipNonCode("mssql", stmt, i, false); j > i {
			i = j
			floor = j
			continue
		}
		ch := stmt[i]
		switch {
		case ch == '(':
			depth++
		case ch == ')':
			depth--
		case isWordByte(ch) && (i == 0 || !isIdentByte(stmt[i-1])):
			j := i
			for j < len(stmt) && isWordByte(stmt[j]) {
				j++
			}
			if depth == 0 {
				word := strings.ToUpper(stmt[i:j])
				if !inserting {
					inserting = word == "INSERT"
				} else if word == "VALUES" || word == "SELECT" {
					at := i
					if word == "VALUES" && prev == "DEFAULT" {
						at = prevStart
					}
					for at > floor && isSQLSpace(stmt[at-1]) {
						at--
					}
					sep := ""
					if !isSQLSpace(stmt[at]) {
						sep = " "
					}
					return stmt[:at] + " OUTPUT INSERTED." + idColumn + sep + stmt[at:], nil
				}
				prevStart, prev = i, word
			}
			i = j
			continue
		}
		i++
	}
	return "", errors.New("VALUES or SELECT clause not found in INSERT statement")
}

// leftoverTestingDatabasePattern matches a database name that the leftover-cleanup sweep is allowed to
// DROP. Candidates come from the server's own catalog - input sequel does not control, since any principal
// with CREATE DATABASE rights can plant a name there - and the name is spliced into an unquoted DROP
// DATABASE statement executed with the testing credentials. The full-match anchor and the narrow charset
// (exactly what CreateTestingDatabase can mint, which sanitizes to [a-z0-9_]) are what make that splice
// safe: a name carrying a quote, space or semicolon - a batch injection on SQL Server, which executes
// multi-statement batches - fails the match and is left alone rather than escaped.
var leftoverTestingDatabasePattern = regexp.MustCompile(`^testing_[0-2][0-9]_[a-z0-9_]*$`)

// dsnParseError omits the DSN and parser error: either can contain credentials, and malformed input
// cannot be reliably redacted across the supported DSN grammars.
func dsnParseError(driverName string) error {
	return errors.New("error parsing %s data source name", driverName)
}

// databaseNameFromDataSourceName extracts the database name part of the data source name.
func databaseNameFromDataSourceName(driverName string, dsn string) (databaseName string, err error) {
	if dsn == "" {
		return "", errors.New("empty dsn")
	}
	switch driverName {
	case "mysql", "mariadb":
		cfg, err := mysql.ParseDSN(trimMySQLScheme(dsn))
		if err != nil {
			return "", dsnParseError(driverName)
		}
		return cfg.DBName, nil
	case "pgx", "cockroachdb":
		// The driver's own reading, which covers a URL, a keyword DSN and the environment alike
		cfg, err := pgx.ParseConfig(dsn)
		if err != nil {
			return "", dsnParseError(driverName)
		}
		return cfg.Database, nil
	case "mssql":
		// https://github.com/microsoft/go-mssqldb?tab=readme-ov-file#common-parameters
		cfg, err := msdsn.Parse(dsn)
		if err != nil {
			return "", dsnParseError(driverName)
		}
		return cfg.Database, nil
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
	case "mysql", "mariadb":
		cfg, err := mysql.ParseDSN(trimMySQLScheme(dsn))
		if err != nil {
			return "", dsnParseError(driverName)
		}
		cfg.DBName = databaseName
		alteredDSN = cfg.FormatDSN()
		return alteredDSN, nil
	case "pgx", "cockroachdb":
		before, err := pgx.ParseConfig(dsn)
		if err != nil {
			return "", dsnParseError(driverName)
		}
		if _, isURL := cutPostgresScheme(dsn); isURL {
			u, err := url.Parse(dsn)
			if err != nil {
				return "", dsnParseError(driverName)
			}
			u.Path = "/" + databaseName
			alteredDSN = u.String()
		} else {
			// A later keyword overrides an earlier one
			alteredDSN = dsn + " dbname='" + databaseName + "'"
		}
		after, err := pgx.ParseConfig(alteredDSN)
		if err != nil || after.Database != databaseName || after.Host != before.Host || after.Port != before.Port ||
			after.User != before.User || after.Password != before.Password {
			// A keyword DSN ending in a backslash, say, which escapes the separator
			return "", errors.New("unable to set the database in this %s data source name", driverName)
		}
		return alteredDSN, nil
	case "mssql":
		// https://github.com/microsoft/go-mssqldb?tab=readme-ov-file#common-parameters
		before, err := msdsn.Parse(dsn)
		if err != nil {
			return "", dsnParseError(driverName)
		}
		if strings.HasPrefix(dsn, "sqlserver://") {
			u, err := url.Parse(dsn)
			if err != nil {
				return "", dsnParseError(driverName)
			}
			q := u.Query()
			// The driver folds keys to lower case and refuses a key given twice, so a case variant left
			// beside the canonical key would make the DSN unparsable.
			for key := range q {
				if strings.EqualFold(key, "database") {
					q.Del(key)
				}
			}
			if databaseName != "" {
				q.Set("database", databaseName)
			}
			u.RawQuery = q.Encode()
			alteredDSN = u.String()
		} else {
			// An ADO or ODBC DSN, where a later key overrides an earlier one, synonyms (initial catalog) included
			sep := ";"
			if strings.HasSuffix(dsn, ";") {
				sep = ""
			}
			alteredDSN = dsn + sep + "database=" + databaseName
		}
		after, err := msdsn.Parse(alteredDSN)
		if err != nil || after.Database != databaseName || after.Host != before.Host || after.Port != before.Port ||
			after.Instance != before.Instance || after.User != before.User || after.Password != before.Password {
			return "", errors.New("unable to set the database in this %s data source name", driverName)
		}
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

// sqliteTestingBase rewrites a SQLite DSN as the file: URI of an in-memory database with a shared cache, which is what
// a testing database is: every connection of every handle reaches one database, and nothing reaches the disk. A bare
// path would lose its parameters when the testing name replaces it, and name a file. The other parameters are kept;
// a :memory: path is dropped, since the testing database name takes its place.
func sqliteTestingBase(dsn string) string {
	path, query, _ := strings.Cut(strings.TrimPrefix(dsn, "file:"), "?")
	if path == ":memory:" {
		path = ""
	}
	var params []string
	for p := range strings.SplitSeq(query, "&") {
		key, _, _ := strings.Cut(p, "=")
		if p != "" && key != "mode" && key != "cache" {
			params = append(params, p)
		}
	}
	params = append(params, "mode=memory", "cache=shared")
	return "file:" + path + "?" + strings.Join(params, "&")
}

// physicalDriverName maps a sequel driver name to the driver registered with database/sql.
// CockroachDB shares the pgx driver since it speaks the PostgreSQL wire protocol; sequel
// keeps "cockroachdb" as a distinct logical name for callers and dispatch but opens the
// underlying connection with "pgx".
func physicalDriverName(driverName string) string {
	switch driverName {
	case "cockroachdb":
		return "pgx"
	case "mariadb":
		return "mysql"
	}
	return driverName
}

// cockroachDBScheme is sequel's URL scheme for CockroachDB. A postgres:// URL names no driver, so it lets a
// bare DSN say it is CockroachDB on any port.
const cockroachDBScheme = "cockroachdb://"

// Keyword DSN prefixes name the driver without serializing or escaping the driver's keyword syntax.
const (
	pgxKeywordScheme       = "pgx+dsn://"
	cockroachKeywordScheme = "cockroachdb+dsn://"
)

// toPostgresScheme removes keyword DSN prefixes or rewrites a cockroachdb:// URL to postgres://.
func toPostgresScheme(dsn string) string {
	for _, prefix := range []string{pgxKeywordScheme, cockroachKeywordScheme} {
		if rest, ok := strings.CutPrefix(dsn, prefix); ok {
			return rest
		}
	}
	if rest, ok := strings.CutPrefix(dsn, cockroachDBScheme); ok {
		return "postgres://" + rest
	}
	return dsn
}

// mySQLScheme and mariaDBScheme name the MySQL and MariaDB drivers in front of a DSN in the MySQL driver's own format
// (mysql://user:pass@tcp(host:3306)/db), which names neither engine. They are prefixes, not URL schemes: sequel strips
// them and the driver parses the rest as it always has.
const (
	mySQLScheme   = "mysql://"
	mariaDBScheme = "mariadb://"
)

// trimMySQLScheme strips a mysql:// or mariadb:// prefix, and returns any other DSN unchanged.
func trimMySQLScheme(dsn string) string {
	if rest, ok := strings.CutPrefix(dsn, mySQLScheme); ok {
		return rest
	}
	return strings.TrimPrefix(dsn, mariaDBScheme)
}

// cutPostgresScheme returns what follows a postgres:// or postgresql:// scheme.
func cutPostgresScheme(dsn string) (rest string, ok bool) {
	if rest, ok = strings.CutPrefix(dsn, "postgres://"); ok {
		return rest, true
	}
	return strings.CutPrefix(dsn, "postgresql://")
}

// mySQLNetworkPattern matches the credentials and network of a DSN in the MySQL driver's own format:
// user:password@tcp(.
var mySQLNetworkPattern = regexp.MustCompile(`^[^/]*@(tcp[46]?|unix)\(`)

// knownSchemes are the URL schemes sequel recognizes, in lower case.
var knownSchemes = []string{cockroachDBScheme, pgxKeywordScheme, cockroachKeywordScheme, mariaDBScheme, mySQLScheme, "postgres://", "postgresql://", "sqlserver://"}

// lowerScheme lower-cases a known URL scheme, which is case-insensitive, so that the scheme checks and rewrites see
// one spelling.
func lowerScheme(dsn string) string {
	for _, scheme := range knownSchemes {
		// Only a known scheme: a MySQL user named Admin with a password starting with "//" is not a scheme
		if len(dsn) >= len(scheme) && strings.EqualFold(dsn[:len(scheme)], scheme) {
			return scheme + dsn[len(scheme):]
		}
	}
	return dsn
}

// libpqKeywords are the connection keywords of a PostgreSQL keyword DSN, one of which must come first for the DSN to be
// inferred as pgx.
var libpqKeywords = map[string]bool{
	"host": true, "hostaddr": true, "port": true, "dbname": true, "user": true, "password": true, "passfile": true,
	"require_auth": true, "channel_binding": true, "connect_timeout": true, "client_encoding": true, "options": true,
	"application_name": true, "fallback_application_name": true, "keepalives": true, "sslmode": true, "sslcert": true,
	"sslkey": true, "sslrootcert": true, "sslpassword": true, "sslnegotiation": true, "target_session_attrs": true,
	"service": true, "servicefile": true, "gssencmode": true, "krbsrvname": true, "load_balance_hosts": true,
	"min_protocol_version": true, "max_protocol_version": true, "timezone": true,
}

// isPostgresKeywordDSN reports whether a DSN is a PostgreSQL keyword DSN (host=... dbname=...): its first key is a
// libpq keyword, it has no ; outside a quoted value, and pgx parses it. pgx alone is not enough, since it takes any
// unknown key for a runtime parameter: an ADO SQL Server DSN (server=...;password=...) parses, and would have been
// opened as PostgreSQL with the SQL Server password. ADO separates its keys with ;, and a keyword DSN with spaces.
func isPostgresKeywordDSN(dsn string) bool {
	eq := strings.IndexByte(dsn, '=')
	if eq <= 0 || !libpqKeywords[strings.TrimSpace(dsn[:eq])] {
		return false
	}
	quoted := false
	for i := 0; i < len(dsn); i++ {
		switch {
		case quoted && dsn[i] == '\\':
			i++
		case dsn[i] == '\'':
			quoted = !quoted
		case dsn[i] == ';' && !quoted:
			return false
		}
	}
	_, err := pgx.ParseConfig(dsn)
	return err == nil
}

// inferDriverName tries to infer the driver name from the data source name.
func inferDriverName(dataSourceName string) (driverName string) {
	if dataSourceName == "" {
		return ""
	}
	dataSourceName = lowerScheme(dataSourceName)
	// A MySQL DSN in the driver's own format whose user is named "file" is not a SQLite URI
	if strings.HasPrefix(dataSourceName, "file:") && mySQLNetworkPattern.MatchString(dataSourceName) {
		return "mysql"
	}
	// A scheme names the driver, whatever the port
	for _, scheme := range []struct{ prefix, driver string }{
		{cockroachDBScheme, "cockroachdb"},
		{pgxKeywordScheme, "pgx"},
		{cockroachKeywordScheme, "cockroachdb"},
		{mariaDBScheme, "mariadb"},
		{mySQLScheme, "mysql"},
		{"postgres://", "pgx"},
		{"postgresql://", "pgx"},
		{"sqlserver://", "mssql"},
		{"file:", "sqlite"},
	} {
		if strings.HasPrefix(dataSourceName, scheme.prefix) {
			return scheme.driver
		}
	}
	if isPostgresKeywordDSN(dataSourceName) {
		return "pgx"
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
	// A default port is the last guess, for a DSN with neither a scheme nor a recognizable format
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
the last *DB referencing that database is Closed, sequel drops it automatically -
no separate cleanup call is required.

uniqueTestID scopes the database so that independent tests don't collide. Pass
t.Name() from a test, or an equivalent identifier from production startup code
that wants a per-run database. The name also carries a token of the calling process,
so tests of the same name in two processes (go test ./... runs packages in parallel)
get two databases; a name too long for the server is shortened with a hash:

	dsn := cfg.DSN
	if cfg.Testing {
	    dsn, err = sequel.CreateTestingDatabase("", cfg.DSN, cfg.TestID)
	    if err != nil { return err }
	}
	db, err := sequel.OpenSingleton("", dsn)

Within a single process, repeated calls with the same (driverName,
baseDataSourceName, uniqueTestID) reuse the same testing database - the
underlying DROP+CREATE only happens on the first call. Once every handle on that
database has been closed it is dropped, and a later call with the same triple
provisions it again.

If a driver name is not provided, it is inferred from the data source name on a
best-effort basis, as in [Open]. Drivers currently supported: "mysql" (MySQL), "mariadb" (MariaDB),
"pgx" (Postgres), "cockroachdb" (CockroachDB), "mssql" (SQL Server) or "sqlite" (SQLite). The
testing data source name names its driver when opened without one: a CockroachDB one is a
cockroachdb:// URL, and a MariaDB one carries the mariadb:// prefix.

If neither a driver name nor a base data source name is provided, it falls back to the SEQUEL_TESTING_DSN
environment variable. This lets any consumer that builds ephemeral test databases through sequel redirect
its entire suite at a real server without changing test code: leave SEQUEL_TESTING_DSN unset to keep the
SQLite default, or set it to a base DSN to run against that server instead, with the driver inferred from
it. Naming a driver - even with an empty DSN - opts out of the fallback, so a test that explicitly asks for
SQLite keeps running on SQLite regardless of the environment.

If neither the arguments nor SEQUEL_TESTING_DSN select a server, the following localhost defaults are used
based on the driver name:
  - (empty): SQLite in-memory database
  - sqlite: SQLite in-memory database
  - mysql: root:root@tcp(127.0.0.1:3306)/
  - mariadb: mariadb://root:root@tcp(127.0.0.1:3306)/
  - pgx: postgres://postgres:postgres@127.0.0.1:5432/
  - cockroachdb: cockroachdb://root@127.0.0.1:26257/?sslmode=disable
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
		case "mariadb":
			baseDataSourceName = "mariadb://root:root@tcp(127.0.0.1:3306)/"
		case "pgx":
			baseDataSourceName = "postgres://postgres:postgres@127.0.0.1:5432/"
		case "cockroachdb":
			baseDataSourceName = "cockroachdb://root@127.0.0.1:26257/?sslmode=disable"
		case "mssql":
			baseDataSourceName = "sqlserver://sa:Password123@127.0.0.1:1433"
		default:
			return "", errors.New("unsupported driver name: %s", driverName)
		}
	}
	baseDataSourceName = lowerScheme(baseDataSourceName)
	if driverName == "" {
		driverName = inferDriverName(baseDataSourceName)
	}
	// Parsed without its pseudo-scheme; the testing DSN gets one back below
	pseudoScheme := strings.HasPrefix(baseDataSourceName, mySQLScheme) || strings.HasPrefix(baseDataSourceName, mariaDBScheme) ||
		strings.HasPrefix(baseDataSourceName, cockroachDBScheme)
	baseDataSourceName = trimMySQLScheme(toPostgresScheme(baseDataSourceName))
	// A testing DSN is opened without a driver name, so it must name its driver: by the pseudo-scheme of the driver
	// asked for, where the driver's own format cannot, or where the base had one
	named := func(native string) string {
		switch driverName {
		case "pgx", "cockroachdb":
			if rest, ok := cutPostgresScheme(native); ok {
				if driverName == "cockroachdb" {
					return cockroachDBScheme + rest
				}
				return native
			}
			if driverName == "cockroachdb" {
				return cockroachKeywordScheme + native
			}
			return pgxKeywordScheme + native
		case "mariadb":
			return mariaDBScheme + native
		case "mysql":
			if pseudoScheme {
				return mySQLScheme + native
			}
		}
		return native
	}
	if driverName == "sqlite" {
		baseDataSourceName = sqliteTestingBase(baseDataSourceName)
	}

	// Keyed on the physical target, not the driver name: MySQL and MariaDB, or PostgreSQL and CockroachDB, asked for
	// the same server and test share one database rather than drop each other's
	cacheKey := hashStr(physicalDriverName(driverName) + "|" + baseDataSourceName + "|" + uniqueTestID)

	testingGlobalMutex.Lock()
	testingMux, ok := testingMutexes[cacheKey]
	if !ok {
		testingMux = &sync.Mutex{}
		testingMutexes[cacheKey] = testingMux
	}
	cachedDSN, hasCached := testingDSNs[cacheKey]
	testingGlobalMutex.Unlock()
	if hasCached {
		return named(cachedDSN), nil
	}

	testingMux.Lock()
	defer testingMux.Unlock()

	// Re-check after taking the per-test mutex in case another caller raced ahead.
	testingGlobalMutex.Lock()
	cachedDSN, hasCached = testingDSNs[cacheKey]
	testingGlobalMutex.Unlock()
	if hasCached {
		return named(cachedDSN), nil
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
	prefix := "testing_" + now.Format("15") + "_" + testingProcessToken + "_"
	readable := regexp.MustCompile(`[^a-z0-9_]`).ReplaceAllString(baseDatabaseName+strings.ToLower(uniqueTestID), "_")
	// The readable part is lossy (Review/a and review_a both read review_a), so the name ends in a hash of the cache
	// key, which tells apart every target and test ID the cache does
	suffix := "_" + cacheKey[:8]
	if room := maxTestingDatabaseName - len(prefix) - len(suffix); len(readable) > room {
		readable = readable[:max(room, 0)]
	}
	testingDatabaseName := prefix + readable + suffix
	testingDSN, err := setDatabaseInDataSourceName(driverName, baseDataSourceName, testingDatabaseName)
	if err != nil {
		return "", errors.Trace(err)
	}
	// Held until the DSN is published: a drop decided by the last close of the previous generation finishes first
	dbKey := testingDatabaseKey(driverName, testingDSN, testingDatabaseName)
	lifecycle := testingLifecycle(dbKey)
	lifecycle.Lock()
	defer lifecycle.Unlock()

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
		case "mysql", "mariadb":
			stmt = "SHOW DATABASES"
		case "pgx", "cockroachdb":
			stmt = "SELECT datname FROM pg_database"
		case "mssql":
			stmt = "SELECT name FROM sys.databases"
		}
		rows, err := masterDB.Query(stmt)
		if err == nil {
			defer rows.Close()
			var leftoverDatabaseNames []string
			h14 := now.Add(-time.Hour).Format("15")
			h15 := now.Format("15")
			h16 := now.Add(time.Hour).Format("15")
			for rows.Next() {
				var databaseName string
				rows.Scan(&databaseName)
				if leftoverTestingDatabasePattern.MatchString(databaseName) &&
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

	// Cache for other openers in the same test, as a new generation: handles left on the previous one drop nothing
	testingGlobalMutex.Lock()
	testingGenerations[dbKey]++
	testingDSNs[cacheKey] = testingDSN
	testingDBKeys[dbKey] = cacheKey
	testingGlobalMutex.Unlock()

	return named(testingDSN), nil
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

// createMigrationsTable creates the sequel_migrations table if it does not exist.
func (db *DB) createMigrationsTable() error {
	stmt := ""
	switch db.driverName {
	case "mysql", "mariadb":
		stmt = `
		CREATE TABLE IF NOT EXISTS sequel_migrations (
			seq_name VARCHAR(256) NOT NULL,
			seq_num INT NOT NULL,
			completed BOOL NOT NULL DEFAULT FALSE,
			completed_on DATETIME(3),
			locked_before DATETIME(3) NOT NULL DEFAULT NOW_UTC(),
			locked_by VARCHAR(64),
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
			locked_by VARCHAR(64),
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
				locked_by VARCHAR(64),
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
			locked_by TEXT,
			PRIMARY KEY (seq_name, seq_num)
		)`
	default:
		return errors.New("unsupported driver name: %s", db.driverName)
	}
	_, err := db.Exec(stmt)
	if err != nil {
		// Concurrent first runs race this CREATE: the existence check precedes the catalog insert, so a loser
		// fails on a duplicate although the table now exists. Proceed if it does.
		var n int
		if db.QueryRow(`SELECT COUNT(*) FROM sequel_migrations WHERE 1=0`).Scan(&n) != nil {
			return errors.Trace(err)
		}
	}
	// A table made by an earlier version has no locked_by column. Concurrent runners race to add it, as they race to
	// make the table, so a failure is taken as the column being there if it now is
	const hasOwner = `SELECT COUNT(locked_by) FROM sequel_migrations WHERE 1=0`
	var n int
	if db.QueryRow(hasOwner).Scan(&n) == nil {
		return nil
	}
	switch db.driverName {
	case "mysql", "mariadb", "pgx", "cockroachdb":
		stmt = `ALTER TABLE sequel_migrations ADD COLUMN locked_by VARCHAR(64)`
	case "mssql":
		stmt = `ALTER TABLE sequel_migrations ADD locked_by VARCHAR(64) NULL`
	case "sqlite":
		stmt = `ALTER TABLE sequel_migrations ADD COLUMN locked_by TEXT`
	}
	_, err = db.Exec(stmt)
	if err != nil && db.QueryRow(hasOwner).Scan(&n) != nil {
		return errors.Trace(err)
	}
	return nil
}

// Migrate reads all #.sql files from the FS, and executes any new migrations in order of their file name.
// The order of execution is guaranteed only within the context of a sequence name.
func (db *DB) Migrate(sequenceName string, fileSys fs.FS) (err error) {
	ctx, finishMigrate := db.telemetry.Load().beginMigrate(context.Background(), db.driverName, sequenceName)
	defer func() { finishMigrate(err) }()

	// Query for the high watermark
	var nullableWatermark sql.NullInt32
	stmt := ""
	switch db.driverName {
	case "mysql", "mariadb", "pgx", "cockroachdb":
		stmt = `SELECT MAX(seq_num) FROM sequel_migrations WHERE seq_name=? AND completed=TRUE`
	case "mssql", "sqlite":
		stmt = `SELECT MAX(seq_num) FROM sequel_migrations WHERE seq_name=? AND completed=1`
	default:
		return errors.New("unsupported driver name: %s", db.driverName)
	}
	// The migrations table is made once per DB. The table can still go away under an open DB - the database
	// recreated (an in-memory SQLite, a testing database), or the table dropped - so a read that fails after the
	// table was taken as made makes it again and reads once more.
	assumed := db.migrationsTableReady.Load()
	for {
		if !db.migrationsTableReady.Load() {
			err = db.createMigrationsTable()
			if err != nil {
				return errors.Trace(err)
			}
			db.migrationsTableReady.Store(true)
		}
		err = db.QueryRow(stmt, sequenceName).Scan(&nullableWatermark)
		if err == nil || !assumed {
			break
		}
		assumed = false
		db.migrationsTableReady.Store(false)
	}
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
		if other, ok := fileNames[seqNum]; ok {
			// Only one of the two would run, and the number would be recorded as done
			return errors.New("migrations %s and %s have the same sequence number %d", other, file.Name(), seqNum)
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

	// The owner token makes the lock this runner's: renewing, releasing and completing a migration succeed only
	// while the lock still names it, so a runner whose lease lapsed and was taken finds out
	owner := newMigrationOwner()
	var lease *DB
	defer func() {
		if lease != nil && lease != db {
			lease.DB.Close()
		}
	}()

	// Execute the migrations
	for len(sequenceNumbersToRun) > 0 {
		seqNum := sequenceNumbersToRun[0]

		// Insert new migrations into the database first
		// Ignore duplicate key violations
		switch db.driverName {
		case "mysql", "mariadb":
			stmt = `INSERT IGNORE INTO sequel_migrations (seq_name, seq_num) VALUES (?, ?)`
		case "pgx", "cockroachdb":
			stmt = `INSERT INTO sequel_migrations (seq_name, seq_num) VALUES (?, ?) ON CONFLICT DO NOTHING`
		case "mssql":
			// The existence check spares the duplicate-key error, and its error span, in the common case of a row
			// already there; two runners that both pass it race to the INSERT, and the loser's duplicate is ignored
			// below. A MERGE WITH (HOLDLOCK) took key-range locks that deadlocked concurrent first runs, each one
			// found only by the 5s deadlock monitor.
			stmt = `IF NOT EXISTS (SELECT 1 FROM sequel_migrations WHERE seq_name=? AND seq_num=?)
				INSERT INTO sequel_migrations (seq_name, seq_num) VALUES (?, ?)`
		case "sqlite":
			stmt = `INSERT OR IGNORE INTO sequel_migrations (seq_name, seq_num) VALUES (?, ?)`
		default:
			return errors.New("unsupported driver name: %s", db.driverName)
		}
		args := []any{sequenceName, seqNum}
		if db.driverName == "mssql" {
			args = append(args, sequenceName, seqNum)
		}
		_, err = db.execRetryingContention(stmt, args...)
		if err != nil && !isDuplicateKeyError(db.driverName, err) {
			return errors.Trace(err)
		}

		// See if completed by another process
		stmt = `SELECT completed FROM sequel_migrations WHERE seq_name=? AND seq_num=?`
		var completed bool
		err := retryContention(func() error {
			return db.QueryRow(stmt, sequenceName, seqNum).Scan(&completed)
		})
		if err != nil {
			return errors.Trace(err)
		}
		if completed {
			sequenceNumbersToRun = sequenceNumbersToRun[1:]
			continue
		}

		// Try to obtain a lock. A row just inserted, or just released, holds the moment it was written, so the
		// comparison must include it: with a strict < a lock taken within that millisecond fails and sleeps.
		switch db.driverName {
		case "mysql", "mariadb", "pgx", "cockroachdb":
			stmt = `UPDATE sequel_migrations SET locked_before=DATE_ADD_MILLIS(NOW_UTC(), 15000), locked_by=?
					WHERE seq_name=? AND seq_num=? AND locked_before<=NOW_UTC() AND completed=FALSE`
		case "mssql", "sqlite":
			stmt = `UPDATE sequel_migrations SET locked_before=DATE_ADD_MILLIS(NOW_UTC(), 15000), locked_by=?
					WHERE seq_name=? AND seq_num=? AND locked_before<=NOW_UTC() AND completed=0`
		default:
			return errors.New("unsupported driver name: %s", db.driverName)
		}
		// The lease runs from before the statement, so the deadline kept here is never later than the server's
		leasedAt := time.Now()
		res, err := db.execRetryingContention(stmt, owner, sequenceName, seqNum)
		if err != nil {
			return errors.Trace(err)
		}
		affected, err := res.RowsAffected()
		if err != nil {
			return errors.Trace(err)
		}
		if affected == 0 {
			// Another runner holds the lock: poll until it completes the migration or its lease lapses.
			time.Sleep(5 * time.Millisecond)
			continue
		}

		// Obtained lock, execute migration in a goroutine
		db.telemetry.Load().logMigrationAttempt(ctx, db.driverName, sequenceName, fileNames[seqNum])
		if lease == nil {
			lease, err = db.openLeasePool()
			if err != nil {
				_, _ = db.releaseMigrationLock(sequenceName, seqNum, owner)
				return errors.Trace(err)
			}
		}
		statements := splitMigration(db.driverName, migrations[seqNum])

		// Buffered, so the goroutine never blocks on a receiver that has gone; cancelled if the lock is lost
		runCtx, cancelRun := context.WithCancel(ctx)
		done := make(chan error, 1)
		go func() {
			for _, stmt := range statements {
				if drivers, ok := migrationDriverDirective(db.driverName, stmt); ok && !slices.Contains(drivers, db.driverName) {
					continue
				}
				stmt = strings.TrimSpace(stripLineComments(db.driverName, stmt))
				if stmt == "" {
					// Empty after stripping comments (e.g. a comment-only segment); skip to avoid erroring out
					continue
				}
				_, e := db.ExecContext(runCtx, stmt)
				if e != nil {
					done <- e
					return
				}
			}
			done <- nil
		}()

		// Wait for it to finish, extending the lock while it runs. Each extension must succeed before the lease it
		// extends lapses, or another runner may take the lock: one that cannot is given up on, and the run stopped
		exit := false
		for !exit {
			select {
			case err = <-done:
				exit = true
			case <-time.After(migrationLockRenewal):
				extendCtx, cancelExtend := context.WithDeadline(ctx, leasedAt.Add(migrationLease-migrationLeaseMargin))
				extendedAt := time.Now()
				extendErr := extendMigrationLock(extendCtx, lease, sequenceName, seqNum, owner)
				cancelExtend()
				if extendErr == nil {
					leasedAt = extendedAt
					continue
				}
				// The lock will lapse, and another runner may take it. Stop this run before giving it up, and
				// wait for it, so that the two do not run the same migration at once
				cancelRun()
				if runErr := <-done; runErr != nil {
					err = errors.New("unable to extend the migration lock", extendErr)
				}
				exit = true
			}
		}
		cancelRun()

		db.telemetry.Load().recordMigration(ctx, db.driverName, sequenceName, err)

		if err != nil {
			_, _ = db.releaseMigrationLock(sequenceName, seqNum, owner)
			return errors.New("error running migration %s", fileNames[seqNum], err)
		}

		// Mark as complete, only while the lock is still this runner's
		switch db.driverName {
		case "mysql", "mariadb", "pgx", "cockroachdb":
			stmt = `UPDATE sequel_migrations SET locked_before=NOW_UTC(), completed_on=NOW_UTC(), completed=TRUE WHERE seq_name=? AND seq_num=? AND locked_by=?`
		case "mssql", "sqlite":
			stmt = `UPDATE sequel_migrations SET locked_before=NOW_UTC(), completed_on=NOW_UTC(), completed=1 WHERE seq_name=? AND seq_num=? AND locked_by=?`
		default:
			return errors.New("unsupported driver name: %s", db.driverName)
		}
		res, err = db.execRetryingContention(stmt, sequenceName, seqNum, owner)
		if err == nil {
			err = migrationLockHeld(ctx, db, res, sequenceName, seqNum, owner)
		}
		if err != nil {
			return errors.New("unable to mark migration %s as complete", fileNames[seqNum], err)
		}
		sequenceNumbersToRun = sequenceNumbersToRun[1:]
	}
	return nil
}

// isDuplicateKeyError reports whether err is SQL Server's duplicate-key error: 2627 for a primary key or unique
// constraint, 2601 for a unique index. The other drivers ignore duplicates in the statement itself.
func isDuplicateKeyError(driverName string, err error) bool {
	var msErr mssql.Error
	return driverName == "mssql" && errors.As(err, &msErr) && (msErr.Number == 2627 || msErr.Number == 2601)
}

// migrationLease is how long a migration lock lasts unless extended, as set by the statements that take and extend
// it, and migrationLeaseMargin how long before it lapses an extension must have succeeded.
const (
	migrationLease       = 15 * time.Second
	migrationLeaseMargin = 2 * time.Second
)

// migrationLockRenewal is how often a running migration extends its lock.
var migrationLockRenewal = 5 * time.Second

// errMigrationLockLost reports a migration lock that no longer names its owner: its lease lapsed and another runner
// took it.
var errMigrationLockLost = errors.New("the migration lock was lost to another runner")

// newMigrationOwner returns a random token that identifies one Migrate call as the owner of the locks it takes.
func newMigrationOwner() string {
	b := make([]byte, 16)
	_, _ = crand.Read(b)
	return hex.EncodeToString(b)
}

// openLeasePool opens the pool that extends a running migration's lock. It is apart from the caller's pool, which
// the migration may be holding every connection of - a pool of one, say - so that an extension never waits for the
// migration it extends. A private in-memory SQLite database cannot be reached from another pool, and no other
// runner can reach it either, so its lock is extended on the caller's pool.
func (db *DB) openLeasePool() (*DB, error) {
	dsn := db.dataSourceName
	if db.driverName == "sqlite" && (strings.Contains(dsn, ":memory:") || strings.Contains(dsn, "mode=memory")) &&
		!strings.Contains(dsn, "cache=shared") {
		return db, nil
	}
	sqlDB, err := sql.Open(physicalDriverName(db.driverName), dsn)
	if err != nil {
		return nil, errors.Trace(err)
	}
	sqlDB.SetMaxOpenConns(1)
	sqlDB.SetMaxIdleConns(1)
	// Not opened with Open: closing it must not release a testing database, and it registers no pool metrics
	lease := &DB{DB: sqlDB, driverName: db.driverName, refCount: 1}
	lease.pool.Store(sqlDB)
	lease.telemetry.Store(db.telemetry.Load())
	lease.simulatedRTT.Store(db.simulatedRTT.Load())
	return lease, nil
}

// extendMigrationLock extends the lock of a running migration, as long as it is still owned by owner. A variable, so
// that a test can make it fail.
var extendMigrationLock = func(ctx context.Context, db *DB, sequenceName string, seqNum int, owner string) error {
	return retryContention(func() error {
		res, err := db.ExecContext(ctx,
			`UPDATE sequel_migrations SET locked_before=DATE_ADD_MILLIS(NOW_UTC(), 15000) WHERE seq_name=? AND seq_num=? AND locked_by=?`,
			sequenceName, seqNum, owner)
		if err != nil {
			return err
		}
		return migrationLockHeld(ctx, db, res, sequenceName, seqNum, owner)
	})
}

// releaseMigrationLock gives up a migration lock held by owner, so that another runner can take it at once.
func (db *DB) releaseMigrationLock(sequenceName string, seqNum int, owner string) (sql.Result, error) {
	return db.execRetryingContention(
		`UPDATE sequel_migrations SET locked_before=NOW_UTC() WHERE seq_name=? AND seq_num=? AND locked_by=?`,
		sequenceName, seqNum, owner)
}

// migrationLockHeld reports errMigrationLockLost unless the update that res reports, made on the condition that the
// lock names owner, found it. MySQL counts only the rows an update changed, so no rows affected is confirmed by
// reading the lock.
func migrationLockHeld(ctx context.Context, db *DB, res sql.Result, sequenceName string, seqNum int, owner string) error {
	if affected, err := res.RowsAffected(); err != nil {
		return err
	} else if affected > 0 {
		return nil
	}
	var n int
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM sequel_migrations WHERE seq_name=? AND seq_num=? AND locked_by=?`,
		sequenceName, seqNum, owner).Scan(&n)
	if err != nil {
		return err
	}
	if n == 0 {
		return errMigrationLockLost
	}
	return nil
}

// splitMigration splits a migration into its statements, at each ; that ends a line, which it drops. A ; inside a
// string literal, a quoted identifier, a block comment or a PostgreSQL dollar-quoted body does not end a statement.
// One at the end of a -- comment does, as it always has.
func splitMigration(driverName, migration string) []string {
	var statements []string
	start := 0
	for i := 0; i < len(migration); {
		if j := skipNonCode(driverName, migration, i, true); j > i {
			if (strings.HasPrefix(migration[i:], "--") || migration[i] == '#') && strings.HasSuffix(strings.TrimRight(migration[i:j], " \t\r\n"), ";") {
				statements = append(statements, migration[start:j])
				start = j
			}
			i = j
			continue
		}
		if migration[i] == ';' {
			j := i + 1
			for j < len(migration) && (migration[j] == ' ' || migration[j] == '\t' || migration[j] == '\r') {
				j++
			}
			if j == len(migration) || migration[j] == '\n' {
				statements = append(statements, migration[start:i])
				start = j
				i = j
				continue
			}
		}
		i++
	}
	return append(statements, migration[start:])
}

// migrationDriverDirective returns the drivers named by the first -- DRIVER: comment of a migration statement, and
// whether it has one. Only a comment counts: the same text inside a string literal is data.
func migrationDriverDirective(driverName, stmt string) (drivers []string, ok bool) {
	for i := 0; i < len(stmt); {
		j := skipNonCode(driverName, stmt, i, true)
		if j == i {
			i++
			continue
		}
		if directive, found := strings.CutPrefix(stmt[i:j], "-- DRIVER:"); found {
			return strings.Fields(directive), true
		}
		i = j
	}
	return nil, false
}

// stripLineComments removes the -- comments of a migration statement (and # on MySQL and MariaDB), up to the end of
// each line, and leaves a "--" inside a string literal, a quoted identifier or a PostgreSQL dollar-quoted body alone.
func stripLineComments(driverName, stmt string) string {
	var b strings.Builder
	b.Grow(len(stmt))
	for i := 0; i < len(stmt); {
		j := skipNonCode(driverName, stmt, i, true)
		if j == i {
			b.WriteByte(stmt[i])
			i++
			continue
		}
		if !strings.HasPrefix(stmt[i:], "--") && stmt[i] != '#' {
			b.WriteString(stmt[i:j])
		} else if stmt[j-1] == '\n' {
			b.WriteByte('\n')
		}
		i = j
	}
	return b.String()
}

// retryContention runs fn again, with Transact's jittered backoff, while it fails on lock contention or a
// deadlock. fn must be safe to repeat.
func retryContention(fn func() error) (err error) {
	for attempt := range transactMaxAttempts {
		if attempt > 0 {
			transactBackoff(attempt)
		}
		err = fn()
		if err == nil || !IsLockContentionError(err) {
			return err
		}
	}
	return err
}

// execRetryingContention executes a statement that is safe to repeat, retrying it on lock contention or a deadlock.
// Migrate's bookkeeping statements are: concurrent first runs deadlock on SQL Server's key-range locks, and the
// victim's statement was rolled back.
func (db *DB) execRetryingContention(query string, args ...any) (res sql.Result, err error) {
	err = retryContention(func() error {
		res, err = db.Exec(query, args...)
		return err
	})
	return res, err
}

// conformPlaceholders replaces the ? arg placeholders in a SQL statement with the ordinals $1, $2 etc. for the
// Postgres and SQL Server drivers. Question marks inside quoted strings (single or double quotes), comments and,
// on SQL Server, bracketed identifiers are left as-is.
func conformPlaceholders(driverName string, stmt string) string {
	if driverName != "pgx" && driverName != "cockroachdb" && driverName != "mssql" {
		return stmt
	}
	n := strings.Count(stmt, "?")
	if n == 0 {
		return stmt
	}
	// Fast path: nothing a ? could hide in
	if !strings.ContainsAny(stmt, `'"[$`) && !strings.Contains(stmt, "--") && !strings.Contains(stmt, "/*") {
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
	// Slow path: skip what the driver skips - string literals, quoted identifiers, comments, and on PostgreSQL
	// dollar-quoted bodies, whose ? is an operator (jsonb's) rather than a parameter
	var sb strings.Builder
	sb.Grow(len(stmt) + n*3)
	argIndex := 1
	for i := 0; i < len(stmt); {
		if j := skipNonCode(driverName, stmt, i, true); j > i {
			sb.WriteString(stmt[i:j])
			i = j
			continue
		}
		if stmt[i] == '?' {
			sb.WriteString("$")
			sb.WriteString(strconv.Itoa(argIndex))
			argIndex++
		} else {
			sb.WriteByte(stmt[i])
		}
		i++
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
	s, _ := vfNowUTC(db.driverName, "")
	return s
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
	case "mysql", "mariadb":
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
