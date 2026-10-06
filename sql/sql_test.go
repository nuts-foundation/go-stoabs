/*
 * Copyright (C) 2026 Nuts community
 *
 * This program is free software: you can redistribute it and/or modify
 * it under the terms of the GNU General Public License as published by
 * the Free Software Foundation, either version 3 of the License, or
 * (at your option) any later version.
 *
 * This program is distributed in the hope that it will be useful,
 * but WITHOUT ANY WARRANTY; without even the implied warranty of
 * MERCHANTABILITY or FITNESS FOR A PARTICULAR PURPOSE.  See the
 * GNU General Public License for more details.
 *
 * You should have received a copy of the GNU General Public License
 * along with this program.  If not, see <https://www.gnu.org/licenses/>.
 *
 */

package sql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path"
	"testing"

	"github.com/nuts-foundation/go-stoabs"
	"github.com/nuts-foundation/go-stoabs/kvtests"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/microsoft/go-mssqldb"
	_ "modernc.org/sqlite"
)

// shelves used by the kvtests conformance suite; the application owns the schema, so the tests create them.
var testShelves = []string{"test", "other"}

const tablePrefix = "kv_unit"

// testDatabase describes how to connect to a database under test and which DDL creates a shelf table.
type testDatabase struct {
	name    string
	dialect Dialect
	// open returns a fresh database handle. For SQLite that is a new file per test; for the others, the shared
	// server from the environment variable.
	open func(t *testing.T) *sql.DB
	// createTable is the DDL template for a shelf table, with %s for the quoted table name.
	createTable string
}

var databases = []testDatabase{
	{
		name:    "sqlite",
		dialect: SQLite(),
		open: func(t *testing.T) *sql.DB {
			db, err := sql.Open("sqlite", "file:"+path.Join(t.TempDir(), "test.db")+"?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)")
			require.NoError(t, err)
			return db
		},
		createTable: `CREATE TABLE IF NOT EXISTS %s ("key" BLOB NOT NULL PRIMARY KEY, "value" BLOB NOT NULL)`,
	},
	{
		name:        "postgres",
		dialect:     Postgres(),
		open:        envDatabase("pgx", "STOABS_TEST_POSTGRES_DSN"),
		createTable: `CREATE TABLE IF NOT EXISTS %s ("key" BYTEA NOT NULL PRIMARY KEY, "value" BYTEA NOT NULL)`,
	},
	{
		name:        "mysql",
		dialect:     MySQL(),
		open:        envDatabase("mysql", "STOABS_TEST_MYSQL_DSN"),
		createTable: "CREATE TABLE IF NOT EXISTS %s (`key` VARBINARY(128) NOT NULL PRIMARY KEY, `value` LONGBLOB NOT NULL)",
	},
	{
		name:        "sqlserver",
		dialect:     SQLServer(),
		open:        envDatabase("sqlserver", "STOABS_TEST_SQLSERVER_DSN"),
		createTable: "IF OBJECT_ID(N'%[1]s', N'U') IS NULL CREATE TABLE %[1]s ([key] VARBINARY(128) NOT NULL PRIMARY KEY, [value] VARBINARY(MAX) NOT NULL)",
	},
}

// envDatabase returns an opener that uses the DSN from the environment variable, or skips the test if unset.
func envDatabase(driver string, envVar string) func(t *testing.T) *sql.DB {
	return func(t *testing.T) *sql.DB {
		dsn := os.Getenv(envVar)
		if dsn == "" {
			t.Skipf("%s not set, skipping", envVar)
		}
		db, err := sql.Open(driver, dsn)
		require.NoError(t, err)
		require.NoError(t, db.PingContext(context.Background()))
		return db
	}
}

// provider returns a kvtests.StoreProvider for the database. Each store gets its own table prefix so tests
// running against a shared server do not see each other's data, and the tables are dropped afterwards.
func (d testDatabase) provider(t *testing.T) kvtests.StoreProvider {
	var counter int
	return func(t *testing.T) (stoabs.KVStore, error) {
		db := d.open(t)
		counter++
		prefix := fmt.Sprintf("%s_%d_%d", tablePrefix, os.Getpid(), counter)
		tableName := PrefixTableName(prefix)
		for _, shelf := range testShelves {
			quoted := d.dialect.QuoteIdentifier(tableName(shelf))
			_, err := db.ExecContext(context.Background(), fmt.Sprintf(d.createTable, quoted))
			require.NoError(t, err)
		}
		t.Cleanup(func() {
			for _, shelf := range testShelves {
				quoted := d.dialect.QuoteIdentifier(tableName(shelf))
				_, _ = db.ExecContext(context.Background(), "DROP TABLE "+quoted)
			}
			_ = db.Close()
		})
		return Wrap(db, d.dialect, tableName)
	}
}

func TestConformance(t *testing.T) {
	for _, d := range databases {
		t.Run(d.name, func(t *testing.T) {
			// Skip the whole database early if it is not configured.
			d.open(t).Close()
			provider := d.provider(t)
			kvtests.TestReadingAndWriting(t, provider)
			kvtests.TestRange(t, provider)
			kvtests.TestIterate(t, provider)
			kvtests.TestEmpty(t, provider)
			kvtests.TestClose(t, provider)
			kvtests.TestDelete(t, provider)
			kvtests.TestStats(t, provider)
			kvtests.TestWriteTransactions(t, provider)
			kvtests.TestTransactionWriteLock(t, provider)
		})
	}
}

func TestSQLite_Specifics(t *testing.T) {
	ctx := context.Background()
	newStore := func(t *testing.T) stoabs.KVStore {
		store, err := databases[0].provider(t)(t)
		require.NoError(t, err)
		return store
	}

	t.Run("Iterate is ordered bytewise", func(t *testing.T) {
		store := newStore(t)
		keys := []stoabs.Uint32Key{300, 2, 70000, 1}
		require.NoError(t, store.WriteShelf(ctx, "test", func(writer stoabs.Writer) error {
			for _, k := range keys {
				require.NoError(t, writer.Put(k, k.Bytes()))
			}
			return nil
		}))
		var actual []stoabs.Uint32Key
		require.NoError(t, store.ReadShelf(ctx, "test", func(reader stoabs.Reader) error {
			return reader.Iterate(func(key stoabs.Key, _ []byte) error {
				actual = append(actual, key.(stoabs.Uint32Key))
				return nil
			}, stoabs.Uint32Key(0))
		}))
		assert.Equal(t, []stoabs.Uint32Key{1, 2, 300, 70000}, actual)
	})
	t.Run("read-your-writes inside a write transaction", func(t *testing.T) {
		store := newStore(t)
		key := stoabs.BytesKey("k")
		err := store.Write(ctx, func(tx stoabs.WriteTx) error {
			w := tx.GetShelfWriter("test")
			require.NoError(t, w.Put(key, []byte("v1")))
			v, err := w.Get(key)
			require.NoError(t, err)
			assert.Equal(t, []byte("v1"), v)
			require.NoError(t, w.Delete(key))
			_, err = w.Get(key)
			assert.ErrorIs(t, err, stoabs.ErrKeyNotFound)
			require.NoError(t, w.Put(key, []byte("v2")))
			// Iterate forces a flush and reads back from the database within the transaction
			var seen [][]byte
			require.NoError(t, w.Iterate(func(_ stoabs.Key, value []byte) error {
				seen = append(seen, value)
				return nil
			}, stoabs.BytesKey{}))
			assert.Equal(t, [][]byte{[]byte("v2")}, seen)
			return nil
		})
		require.NoError(t, err)
	})
	t.Run("many writes in one transaction are batched", func(t *testing.T) {
		store := newStore(t)
		const n = 2*batchSize + 7
		require.NoError(t, store.WriteShelf(ctx, "test", func(writer stoabs.Writer) error {
			for i := 0; i < n; i++ {
				require.NoError(t, writer.Put(stoabs.Uint32Key(i), []byte{byte(i)}))
			}
			// and delete a slice of them again
			for i := 0; i < batchSize+3; i += 2 {
				require.NoError(t, writer.Delete(stoabs.Uint32Key(i)))
			}
			return nil
		}))
		var count int
		require.NoError(t, store.ReadShelf(ctx, "test", func(reader stoabs.Reader) error {
			count = int(reader.Stats().NumEntries)
			return nil
		}))
		assert.Equal(t, n-(batchSize+3+1)/2, count)
	})
	t.Run("unknown shelf (table missing) fails loud", func(t *testing.T) {
		store := newStore(t)
		err := store.WriteShelf(ctx, "does-not-exist", func(writer stoabs.Writer) error {
			return writer.Put(stoabs.BytesKey("k"), []byte("v"))
		})
		assert.ErrorIs(t, err, stoabs.ErrDatabase{})
		err = store.ReadShelf(ctx, "does-not-exist", func(reader stoabs.Reader) error {
			_, err := reader.Get(stoabs.BytesKey("k"))
			return err
		})
		assert.ErrorIs(t, err, stoabs.ErrDatabase{})
	})
	t.Run("invalid table name", func(t *testing.T) {
		db := databases[0].open(t)
		t.Cleanup(func() { _ = db.Close() })
		store, err := Wrap(db, SQLite(), func(shelf string) string { return "bad;name" })
		require.NoError(t, err)
		err = store.ReadShelf(ctx, "test", func(reader stoabs.Reader) error {
			_, err := reader.Get(stoabs.BytesKey("k"))
			return err
		})
		assert.ErrorIs(t, err, stoabs.ErrDatabase{})
		assert.ErrorContains(t, err, "not a valid identifier")
	})
	t.Run("writer in read-only transaction", func(t *testing.T) {
		store := newStore(t)
		err := store.Read(ctx, func(tx stoabs.ReadTx) error {
			return tx.(stoabs.WriteTx).GetShelfWriter("test").Put(stoabs.BytesKey("k"), []byte("v"))
		})
		assert.ErrorIs(t, err, stoabs.ErrDatabase{})
	})
	t.Run("closed store", func(t *testing.T) {
		store := newStore(t)
		require.NoError(t, store.Close(ctx))
		err := store.ReadShelf(ctx, "test", func(reader stoabs.Reader) error { return nil })
		assert.ErrorIs(t, err, stoabs.ErrStoreIsClosed)
	})
	t.Run("Unwrap returns *sql.Tx", func(t *testing.T) {
		store := newStore(t)
		require.NoError(t, store.Read(ctx, func(tx stoabs.ReadTx) error {
			_, ok := tx.Unwrap().(*sql.Tx)
			assert.True(t, ok)
			return nil
		}))
	})
	t.Run("commit failure invokes OnRollback", func(t *testing.T) {
		store := newStore(t)
		rolledBack := false
		err := store.Write(ctx, func(tx stoabs.WriteTx) error {
			// Finish the underlying transaction behind the store's back so the commit fails.
			return tx.Unwrap().(*sql.Tx).Rollback()
		}, stoabs.OnRollback(func() { rolledBack = true }))
		assert.ErrorIs(t, err, stoabs.ErrCommitFailed)
		assert.True(t, rolledBack)
	})
}

func TestWrap_Validation(t *testing.T) {
	db := databases[0].open(t)
	t.Cleanup(func() { _ = db.Close() })
	_, err := Wrap(nil, SQLite(), PrefixTableName("x"))
	assert.EqualError(t, err, "sql: db is nil")
	_, err = Wrap(db, nil, PrefixTableName("x"))
	assert.EqualError(t, err, "sql: dialect is nil")
	_, err = Wrap(db, SQLite(), nil)
	assert.EqualError(t, err, "sql: tableName is nil")
}

func TestPrefixTableName(t *testing.T) {
	fn := PrefixTableName("kv_network_data")
	assert.Equal(t, "kv_network_data__nats_jobs", fn("_nats_jobs"))
	assert.Equal(t, "kv_network_data_xorbucket", fn("xorBucket"))
	assert.Equal(t, "kv_vdr_didstore_latestv2", PrefixTableName("kv_vdr_didstore")("latestV2"))
	assert.Equal(t, "kv_a_b_c_d", PrefixTableName("kv_a-b")("c.d"))
}

func TestDialects_Upsert(t *testing.T) {
	// Only shape checks; execution is covered by the conformance tests per database.
	assert.Equal(t,
		`INSERT INTO "t" ("key", "value") VALUES (?, ?), (?, ?) ON CONFLICT("key") DO UPDATE SET "value" = excluded."value"`,
		SQLite().Upsert(`"t"`, 2))
	assert.Equal(t,
		`INSERT INTO "t" ("key", "value") VALUES ($1, $2) ON CONFLICT ("key") DO UPDATE SET "value" = EXCLUDED."value"`,
		Postgres().Upsert(`"t"`, 1))
	assert.Equal(t,
		"INSERT INTO `t` (`key`, `value`) VALUES (?, ?) ON DUPLICATE KEY UPDATE `value` = VALUES(`value`)",
		MySQL().Upsert("`t`", 1))
	assert.Contains(t, SQLServer().Upsert("[t]", 2), "USING (VALUES (@p1, @p2), (@p3, @p4)) AS src (k, v)")
}

func TestErrorIsDatabase(t *testing.T) {
	// sanity check on the error semantics the DAG relies on: ErrKeyNotFound is not a database error
	assert.False(t, errors.Is(stoabs.ErrKeyNotFound, stoabs.ErrDatabase{}))
}
