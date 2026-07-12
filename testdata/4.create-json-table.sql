-- A JSON document table for the JSON_FIELD() fixtures. The column type is the engine's native JSON type
-- where it has one, and its widest text type where it does not, so JSON_FIELD is exercised against both
-- shapes it meets in the wild.

-- DRIVER: mysql
CREATE TABLE jsonbag (
    id  INT NOT NULL,
    doc JSON NOT NULL
);

-- DRIVER: pgx cockroachdb
CREATE TABLE jsonbag (
    id  INT NOT NULL,
    doc JSONB NOT NULL
);

-- DRIVER: mssql
CREATE TABLE jsonbag (
    id  INT NOT NULL,
    doc NVARCHAR(MAX) NOT NULL
);

-- DRIVER: sqlite
CREATE TABLE jsonbag (
    id  INT NOT NULL,
    doc TEXT NOT NULL
);
