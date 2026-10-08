# Golang Storage Abstraction (go-stoabs)

## BBolt

## Redis

When creating a Redis `KVStore` it tests the connection using Redis' `PING` command.

Due to the simple API of the library, the Redis adapter only supports reading/writing byte arrays.
The behavior when reading any other Redis type (e.g. a list or set) is undefined.

### Transaction Isolation

Redis doesn't have actual transactions, so this library simulates them by using the `MULTI`/`EXEC`/`DISCARD` commands.
That way all commands are guaranteed to be executed atomically.

As a consequence, a value that hasn't been committed yet can't be read. In other words, don't try to read a value from a
key that was written to in the same transaction.
Subsequently, changes from other writers (from the same process or remote) are reflected immediately in the current
transaction: if a key is read twice, there's no guarantee the returned value will be equal.

If the application requires exclusive write access to a store it can lock the database, to assert there are no other
active writers:

```golang
store.Write(func (tx stoabs.WriteTx) error { 
	// do something with tx
}, stoabs.WithWriteLock())
```

The lock is released when the transaction is committed or rolled back.
The lock is subject to the prefix (`CreateRedisStore(prefix string, ...)`) the store was created with, meaning other stores with the same prefix will have the same lock.

Redis locks are implemented using (Redsync)[https://github.com/go-redsync/redsync].

### Unsupported features

* Clustering

## SQL

The `sql` package implements a `KVStore` on a SQL database server through `database/sql`. Supported dialects:
PostgreSQL, MySQL/MariaDB and SQL Server (`SQLServer()` also covers Azure SQL, which speaks the same T-SQL over the
same driver; it is verified against SQL Server 2022 in CI, not against Azure SQL itself). SQLite is deliberately not
supported: it is a file on disk like bbolt and shares its limitations.

Every shelf is a table with a binary `key` column (primary key) and a binary `value` column. The application owns the
schema: it creates the tables (e.g. with its migration tooling) and passes a function that maps a shelf name to a
table name. The store never executes DDL; operating on a shelf whose table does not exist returns a `stoabs.ErrDatabase`.

```golang
db, _ := sql.Open("pgx", dsn)
store, err := stoabssql.Wrap(db, stoabssql.Postgres(), stoabssql.PrefixTableName("kv_network_data"))
```

Expected table shape (types differ per database):

```sql
CREATE TABLE kv_network_data_documents (
    "key"   BYTEA NOT NULL PRIMARY KEY, -- VARBINARY(128) on MySQL/SQL Server
    "value" BYTEA NOT NULL              -- LONGBLOB / VARBINARY(MAX)
);
```

Keys are ordered bytewise, matching bbolt and the `stoabs.Key` types, so `Range()` and `Iterate()` behave the same.

### Transactions

Writable transactions are serialized per store, across processes: every writable transaction first takes an exclusive,
transaction-scoped lock on the single row of the store's lock table (the pseudo-shelf `stoabssql.LockShelf`, `_lock`,
with key `stoabssql.LockKey`). `SELECT ... FOR UPDATE` on PostgreSQL and MySQL, `UPDLOCK, HOLDLOCK` on SQL Server.
This is the equivalent of bbolt's exclusive file lock, but it also covers multiple
instances of the application on one database. The application creates and seeds the lock table with its other tables;
a missing lock row fails the transaction with `ErrLockRowMissing` rather than silently not locking. Readers never touch
the lock row and are not blocked.

```sql
CREATE TABLE kv_network_data__lock ("key" BYTEA NOT NULL PRIMARY KEY, "value" BYTEA NOT NULL);
INSERT INTO kv_network_data__lock ("key", "value") VALUES ('\x00', '');
```

Writes are buffered per shelf and flushed as multi-row upserts/deletes (at most 500 rows and 4 MB per statement)
before any read that needs them and at commit, so reading a value written earlier in the same transaction works.
`Iterate` and `Range` read in pages and invoke the callbacks between pages, so a callback may run further queries on
the same transaction.

Cancelling the caller's context aborts the transaction: queries run on that context, it is checked between pages and
before commit, and the transaction is then rolled back explicitly. The database transaction itself is started on a
context that cannot be cancelled, so `database/sql` never rolls it back from the background. That background rollback
is unsafe with go-mssqldb (microsoft/go-mssqldb#390), and this way the rollback always completes before the connection
is reused.

### Database notes

- SQL Server: readers run at the database's default isolation. Without `READ_COMMITTED_SNAPSHOT` a reader can briefly
  wait for a committing writer's row locks; it never waits for the lock row.
- Table names from `PrefixTableName` are lower-cased with non-alphanumerics replaced by `_`, so two shelf names that
  differ only in case or punctuation map to the same table. Use distinct names.

The database handle passed to `Wrap` is owned by the caller and is not closed by `Close()`.

### Running the tests

The `sql` package tests need database servers and skip databases whose DSN is unset. Locally:

```shell
docker run -d --rm --name stoabs-pg -e POSTGRES_PASSWORD=stoabs -e POSTGRES_USER=stoabs -e POSTGRES_DB=stoabs -p 5432:5432 postgres:16-alpine
docker run -d --rm --name stoabs-mysql -e MYSQL_ROOT_PASSWORD=stoabs -e MYSQL_DATABASE=stoabs -p 3306:3306 mysql:8
docker run -d --rm --name stoabs-mssql -e ACCEPT_EULA=Y -e MSSQL_SA_PASSWORD='Stoabs!Passw0rd' -p 1433:1433 mcr.microsoft.com/mssql/server:2022-latest
export STOABS_TEST_POSTGRES_DSN='postgres://stoabs:stoabs@localhost:5432/stoabs?sslmode=disable'
export STOABS_TEST_MYSQL_DSN='root:stoabs@tcp(localhost:3306)/stoabs'
export STOABS_TEST_SQLSERVER_DSN='sqlserver://sa:Stoabs!Passw0rd@localhost:1433?database=master'
go test ./sql/
```
