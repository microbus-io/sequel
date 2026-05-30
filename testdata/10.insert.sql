-- Sequence number 10 sorts before "2.alter-table.sql" lexicographically. It references the
-- "updated" column that 2.alter-table.sql adds, so it only succeeds when migrations run in
-- numeric (not filename) order.
INSERT INTO foo (id, str, updated) VALUES (10, 'j', 1)
