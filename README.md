# Sequel

### Motivation

`sequel.DB` is an enhancement to Go's standard `sql.DB` that facilitates the use of SQL databases in Microbus solutions.

#### Connection Pool Limit

Since every microservice is potentially a client, a solution with many microservices can easily overwhelm the database and run it out of memory.
`sequel.DB` limits the size of the connection pool on a global basis to no more than approximately the sqrt of the number of clients.

#### Schema Migration

`sequel.DB` can perform schema migration of the database given a `fs.FS` that contains migrations files at `sql/1.create-table-foo.sql`, `sql/2.update-table-foo.sql` etc.
Migration is executed in a thread-safe manner to handle microservices with more than one replica attempting to perform migrations concurrently.

#### Creation of Testing Database

`sequel.DB` detects when it is run in a unit test under `go test` and creates a unique temporary database for each test. If a `dataSourceName` is not given as argument to `Open` nor found in the `TESTING_DATA_SOURCE_NAME` envar, `sequel.DB` defaults to connecting to `127.0.0.1` port `:3306` (MySQL), `:5432` (Postgres) or `:1433` (SQL Server), with the user ID `root` and the password `secret1234`.

### Example

This examples shows how to use a `sequel.DB` to open and migrate a database in a Microbus microservice.

```go
import "github.com/microbus-io/sequel"

type Service struct {
	*intermediate.Intermediate // DO NOT REMOVE
    db *sequel.DB
}

func (svc *Service) OnStartup(ctx context.Context) (err error) {
    svc.db, err = sequel.Open("mysql", svc.SQLDataSourceName())
    if err == nil {
        err = svc.db.Migrate("myservice", svc.ResFS())
    }
    if err != nil {
        return errors.Trace(err)
    }
    return nil
}

func (svc *Service) OnShutdown(ctx context.Context) (err error) {
    if svc.db != nil {
        svc.db.Close()
    }
    return nil
}
```

### Supported Drivers

`sequel.DB` supports the `mysql`, `pgx` and `mssql` driver names.

### Legal

`Sequel` is the copyrighted work of various contributors. It is licensed to you free of charge by `Microbus LLC` - a Delaware limited liability company formed to hold rights to the combined intellectual property of all contributors - under the [Apache License 2.0](http://www.apache.org/licenses/LICENSE-2.0).
