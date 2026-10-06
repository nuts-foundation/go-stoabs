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

The `sql` package implements a `KVStore` on a SQL database through `database/sql`. Supported dialects: SQLite,
PostgreSQL, MySQL/MariaDB and SQL Server.

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
    "key"   BYTEA NOT NULL PRIMARY KEY, -- VARBINARY(128) on MySQL/SQL Server, BLOB on SQLite
    "value" BYTEA NOT NULL              -- LONGBLOB / VARBINARY(MAX) / BLOB
);
```

Keys are ordered bytewise, matching bbolt and the `stoabs.Key` types, so `Range()` and `Iterate()` behave the same.

### Transactions

Writable transactions are serialized per store with a process-level lock, like the bbolt backend. Readers are not
blocked. Writes are buffered per shelf and flushed as multi-row upserts/deletes before any read that needs them and
at commit, so reading a value written earlier in the same transaction works.

The database handle passed to `Wrap` is owned by the caller and is not closed by `Close()`.

