-- DRIVER: mysql
CREATE TABLE table_name (
	tenant_id INT NOT NULL,
	id BIGINT NOT NULL AUTO_INCREMENT,
	revision BIGINT NOT NULL DEFAULT 0,
	example VARCHAR(256) NULL,
	created_at DATETIME(3) NOT NULL,
	updated_at DATETIME(3) NOT NULL,

	CONSTRAINT table_name_pk PRIMARY KEY (tenant_id, id),
	UNIQUE INDEX table_name_idx_id (id),
	INDEX table_name_idx_created_at (tenant_id, created_at)
);


-- DRIVER: pgx
CREATE TABLE table_name (
	tenant_id INT NOT NULL,
	id BIGSERIAL,
	revision BIGINT NOT NULL DEFAULT 0,
	example VARCHAR(256) NULL,
	created_at TIMESTAMP(3) NOT NULL,
	updated_at TIMESTAMP(3) NOT NULL,

	CONSTRAINT table_name_pk PRIMARY KEY (tenant_id, id)
);
-- DRIVER: pgx
CREATE UNIQUE INDEX table_name_idx_id ON table_name USING btree (id);
-- DRIVER: pgx
CREATE INDEX table_name_idx_created_at ON table_name USING btree (tenant_id, created_at);


-- DRIVER: mssql
CREATE TABLE table_name (
	tenant_id INT NOT NULL,
	id BIGINT IDENTITY(1, 1),
	revision BIGINT NOT NULL DEFAULT 0,
	example NVARCHAR(256) NULL,
	created_at DATETIME2(3) NOT NULL,
	updated_at DATETIME2(3) NOT NULL,

	CONSTRAINT table_name_pk PRIMARY KEY NONCLUSTERED (id),
	CONSTRAINT table_name_idx_id UNIQUE CLUSTERED (tenant_id, id),
	INDEX table_name_idx_created_at (tenant_id, created_at)
);
