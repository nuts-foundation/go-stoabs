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
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nuts-foundation/go-stoabs"
	"github.com/nuts-foundation/go-stoabs/kvtests"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"

	_ "github.com/go-sql-driver/mysql"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "github.com/microsoft/go-mssqldb"
)

// shelves used by the kvtests conformance suite; the application owns the schema, so the tests create them.
// All tests need a database server: set STOABS_TEST_{POSTGRES,MYSQL,SQLSERVER}_DSN (see README), unset ones are skipped.
var testShelves = []string{"test", "other"}

const tablePrefix = "kv_unit"

// testDatabase describes how to connect to a database under test and which DDL creates a shelf table.
type testDatabase struct {
	name    string
	dialect Dialect
	// open returns a database handle to the server from the environment variable, or skips the test if it is unset.
	open func(t *testing.T) *sql.DB
	// createTable is the DDL template for a shelf table, with %s for the quoted table name.
	createTable string
}

var databases = []testDatabase{
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
// storeCounter makes table prefixes unique across all providers in the process (several tests share one server).
var storeCounter atomic.Int64

func (d testDatabase) provider(t *testing.T) kvtests.StoreProvider {
	return func(t *testing.T) (stoabs.KVStore, error) {
		db := d.open(t)
		prefix := fmt.Sprintf("%s_%d_%d", tablePrefix, os.Getpid(), storeCounter.Add(1))
		tableName := PrefixTableName(prefix)
		createTables(t, d, db, tableName)
		t.Cleanup(func() {
			dropTables(d, db, tableName)
			_ = db.Close()
		})
		return Wrap(db, d.dialect, tableName)
	}
}

// createTables creates the shelf tables and the seeded lock table, as the application's schema migration would.
func createTables(t *testing.T, d testDatabase, db *sql.DB, tableName TableNameFunc) {
	for _, shelf := range append([]string{LockShelf}, testShelves...) {
		quoted := d.dialect.QuoteIdentifier(tableName(shelf))
		_, err := db.ExecContext(context.Background(), fmt.Sprintf(d.createTable, quoted))
		require.NoError(t, err)
	}
	lockTable := d.dialect.QuoteIdentifier(tableName(LockShelf))
	_, err := db.ExecContext(context.Background(), fmt.Sprintf("INSERT INTO %s (%s, %s) VALUES (%s, %s)",
		lockTable, d.dialect.QuoteIdentifier(keyColumn), d.dialect.QuoteIdentifier(valueColumn),
		d.dialect.Placeholder(1), d.dialect.Placeholder(2)), LockKey, []byte{})
	require.NoError(t, err)
}

func dropTables(d testDatabase, db *sql.DB, tableName TableNameFunc) {
	for _, shelf := range append([]string{LockShelf}, testShelves...) {
		quoted := d.dialect.QuoteIdentifier(tableName(shelf))
		_, _ = db.ExecContext(context.Background(), "DROP TABLE "+quoted)
	}
}

// TestCrossProcessWriteLock simulates two node instances (two stores on two database handles, no shared Go state)
// writing to the same store: the second writer must wait for the first one's transaction to finish.
func TestCrossProcessWriteLock(t *testing.T) {
	ctx := context.Background()
	for _, d := range databases {
		t.Run(d.name, func(t *testing.T) {
			d.open(t).Close()
			tableName := PrefixTableName(fmt.Sprintf("%s_xp_%d_%d", tablePrefix, os.Getpid(), storeCounter.Add(1)))
			db1 := d.open(t)
			db2 := d.open(t)
			createTables(t, d, db1, tableName)
			t.Cleanup(func() {
				dropTables(d, db1, tableName)
				_ = db1.Close()
				_ = db2.Close()
			})
			store1, err := Wrap(db1, d.dialect, tableName, stoabs.WithLockAcquireTimeout(10*time.Second))
			require.NoError(t, err)
			store2, err := Wrap(db2, d.dialect, tableName, stoabs.WithLockAcquireTimeout(10*time.Second))
			require.NoError(t, err)

			t.Run("second writer waits for the first", func(t *testing.T) {
				const hold = 700 * time.Millisecond
				inFirst := make(chan struct{})
				var firstCommitted, secondStarted time.Time
				errs := make(chan error, 2)
				go func() {
					errs <- store1.WriteShelf(ctx, "test", func(writer stoabs.Writer) error {
						close(inFirst)
						time.Sleep(hold)
						firstCommitted = time.Now()
						return writer.Put(stoabs.BytesKey("a"), []byte{1})
					})
				}()
				<-inFirst
				go func() {
					errs <- store2.WriteShelf(ctx, "test", func(writer stoabs.Writer) error {
						secondStarted = time.Now()
						return writer.Put(stoabs.BytesKey("b"), []byte{2})
					})
				}()
				require.NoError(t, <-errs)
				require.NoError(t, <-errs)
				assert.False(t, secondStarted.Before(firstCommitted), "second writer ran while first held the lock")
			})
			t.Run("timeout while another writer holds the lock", func(t *testing.T) {
				impatient, err := Wrap(db2, d.dialect, tableName, stoabs.WithLockAcquireTimeout(300*time.Millisecond))
				require.NoError(t, err)
				inFirst := make(chan struct{})
				release := make(chan struct{})
				go func() {
					_ = store1.WriteShelf(ctx, "test", func(writer stoabs.Writer) error {
						close(inFirst)
						<-release
						return nil
					})
				}()
				<-inFirst
				err = impatient.WriteShelf(ctx, "test", func(writer stoabs.Writer) error { return nil })
				close(release)
				assert.ErrorIs(t, err, stoabs.ErrDatabase{})
				assert.ErrorContains(t, err, "unable to obtain SQL write lock")
			})
			t.Run("readers are not blocked by a writer", func(t *testing.T) {
				inFirst := make(chan struct{})
				release := make(chan struct{})
				go func() {
					_ = store1.WriteShelf(ctx, "test", func(writer stoabs.Writer) error {
						close(inFirst)
						<-release
						return nil
					})
				}()
				<-inFirst
				readCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
				defer cancel()
				err := store2.ReadShelf(readCtx, "test", func(reader stoabs.Reader) error {
					_, err := reader.Get(stoabs.BytesKey("a"))
					return err
				})
				close(release)
				assert.NoError(t, err)
			})
			t.Run("missing lock row fails loud", func(t *testing.T) {
				_, err := db1.ExecContext(ctx, "DELETE FROM "+d.dialect.QuoteIdentifier(tableName(LockShelf)))
				require.NoError(t, err)
				err = store1.WriteShelf(ctx, "test", func(writer stoabs.Writer) error { return nil })
				assert.ErrorIs(t, err, ErrLockRowMissing)
				assert.ErrorIs(t, err, stoabs.ErrDatabase{})
			})
		})
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

// TestNestedReads covers what the Nuts node does and the generic conformance suite does not: callbacks of Iterate
// and Range that query the same transaction again (nested Get), and iterating shelves larger than one page.
// A streaming implementation fails this on all three databases ("bad connection"/"commands out of sync").
func TestNestedReads(t *testing.T) {
	ctx := context.Background()
	for _, d := range databases {
		t.Run(d.name, func(t *testing.T) {
			d.open(t).Close()
			store, err := d.provider(t)(t)
			require.NoError(t, err)
			const n = pageSize + 5
			require.NoError(t, store.Write(ctx, func(tx stoabs.WriteTx) error {
				w := tx.GetShelfWriter("test")
				o := tx.GetShelfWriter("other")
				for i := 0; i < n; i++ {
					require.NoError(t, w.Put(stoabs.Uint32Key(i), stoabs.Uint32Key(i).Bytes()))
					require.NoError(t, o.Put(stoabs.Uint32Key(i), []byte{1}))
				}
				return nil
			}))
			t.Run("Iterate with nested Get on same and other shelf", func(t *testing.T) {
				var count int
				err := store.Read(ctx, func(tx stoabs.ReadTx) error {
					test := tx.GetShelfReader("test")
					other := tx.GetShelfReader("other")
					return test.Iterate(func(key stoabs.Key, value []byte) error {
						same, err := test.Get(key)
						if err != nil {
							return err
						}
						assert.Equal(t, value, same)
						if _, err := other.Get(key); err != nil {
							return err
						}
						count++
						return nil
					}, stoabs.Uint32Key(0))
				})
				require.NoError(t, err)
				assert.Equal(t, n, count)
			})
			t.Run("Range with nested Get crossing a page boundary", func(t *testing.T) {
				var keys []stoabs.Uint32Key
				err := store.ReadShelf(ctx, "test", func(reader stoabs.Reader) error {
					return reader.Range(stoabs.Uint32Key(3), stoabs.Uint32Key(n), func(key stoabs.Key, value []byte) error {
						if _, err := reader.Get(key); err != nil {
							return err
						}
						keys = append(keys, key.(stoabs.Uint32Key))
						return nil
					}, true)
				})
				require.NoError(t, err)
				require.Len(t, keys, n-3)
				assert.Equal(t, stoabs.Uint32Key(3), keys[0])
				assert.Equal(t, stoabs.Uint32Key(n-1), keys[len(keys)-1])
				// strictly ascending, no duplicates across pages
				for i := 1; i < len(keys); i++ {
					assert.Equal(t, keys[i-1]+1, keys[i])
				}
			})
			t.Run("pages are cut by size, iteration stays complete and ordered", func(t *testing.T) {
				previous := pageBytes
				pageBytes = 64 * 1024 // 64 KB: with 10 KB values a page holds 7 rows
				t.Cleanup(func() { pageBytes = previous })
				large, err := d.provider(t)(t)
				require.NoError(t, err)
				const rows = 100
				value := bytes.Repeat([]byte{1}, 10*1024)
				require.NoError(t, large.WriteShelf(ctx, "test", func(w stoabs.Writer) error {
					for i := 0; i < rows; i++ {
						require.NoError(t, w.Put(stoabs.Uint32Key(i), value))
					}
					return nil
				}))
				var seen []stoabs.Uint32Key
				require.NoError(t, large.ReadShelf(ctx, "test", func(r stoabs.Reader) error {
					return r.Iterate(func(key stoabs.Key, v []byte) error {
						assert.Len(t, v, len(value))
						// nested query on the same tx must still work between pages
						_, err := r.Get(key)
						seen = append(seen, key.(stoabs.Uint32Key))
						return err
					}, stoabs.Uint32Key(0))
				}))
				require.Len(t, seen, rows)
				for i := range seen {
					assert.Equal(t, stoabs.Uint32Key(i), seen[i])
				}
				var ranged int
				require.NoError(t, large.ReadShelf(ctx, "test", func(r stoabs.Reader) error {
					return r.Range(stoabs.Uint32Key(10), stoabs.Uint32Key(90), func(_ stoabs.Key, _ []byte) error {
						ranged++
						return nil
					}, true)
				}))
				assert.Equal(t, 80, ranged)
			})
			t.Run("Iterate inside a write transaction sees buffered writes", func(t *testing.T) {
				err := store.Write(ctx, func(tx stoabs.WriteTx) error {
					w := tx.GetShelfWriter("test")
					require.NoError(t, w.Put(stoabs.Uint32Key(n+1), []byte{9}))
					var count int
					if err := w.Iterate(func(_ stoabs.Key, _ []byte) error {
						count++
						return nil
					}, stoabs.Uint32Key(0)); err != nil {
						return err
					}
					assert.Equal(t, n+1, count)
					return errors.New("rollback")
				})
				assert.EqualError(t, err, "rollback")
			})
		})
	}
}

func TestStoreSpecifics(t *testing.T) {
	for _, d := range databases {
		t.Run(d.name, func(t *testing.T) {
			d.open(t).Close()
			testStoreSpecifics(t, d)
		})
	}
}

func testStoreSpecifics(t *testing.T, d testDatabase) {
	ctx := context.Background()
	newStore := func(t *testing.T) stoabs.KVStore {
		store, err := d.provider(t)(t)
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
	t.Run("large values are split over statements by size", func(t *testing.T) {
		store := newStore(t)
		// 12 values of 1 MB: batchBytes (4 MB) allows 4 per statement, so 3 statements; all must land.
		const n = 12
		value := bytes.Repeat([]byte{7}, 1024*1024)
		require.NoError(t, store.WriteShelf(ctx, "test", func(writer stoabs.Writer) error {
			for i := 0; i < n; i++ {
				require.NoError(t, writer.Put(stoabs.Uint32Key(i), value))
			}
			return nil
		}))
		var count int
		var size uint
		require.NoError(t, store.ReadShelf(ctx, "test", func(reader stoabs.Reader) error {
			stats := reader.Stats()
			count, size = int(stats.NumEntries), stats.ShelfSize
			return nil
		}))
		assert.Equal(t, n, count)
		assert.Equal(t, uint(n*len(value)), size)
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
		db := d.open(t)
		t.Cleanup(func() { _ = db.Close() })
		store, err := Wrap(db, d.dialect, func(shelf string) string { return "bad;name" })
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
	// sql.Open does not connect, so no server is needed here
	db, err := sql.Open("pgx", "postgres://127.0.0.1:1/none")
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	_, err = Wrap(nil, Postgres(), PrefixTableName("x"))
	assert.EqualError(t, err, "sql: db is nil")
	_, err = Wrap(db, nil, PrefixTableName("x"))
	assert.EqualError(t, err, "sql: dialect is nil")
	_, err = Wrap(db, Postgres(), nil)
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
