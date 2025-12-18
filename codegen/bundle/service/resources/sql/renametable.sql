-- DRIVER: mysql
RENAME TABLE old_table_name TO new_table_name;

-- DRIVER: pgx
ALTER TABLE old_table_name RENAME TO new_table_name;

-- DRIVER: mssql
EXEC sp_rename 'old_table_name', 'new_table_name';
