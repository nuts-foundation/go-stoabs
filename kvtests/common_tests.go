/*
 * Copyright (C) 2022 Nuts community
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

package kvtests

import (
	"context"
	"database/sql"
	"errors"
	"github.com/dgraph-io/badger/v4"
	"github.com/nuts-foundation/go-stoabs"
	"github.com/sirupsen/logrus"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.etcd.io/bbolt"
	"sync"
	"testing"
	"time"
)

var bytesKey = stoabs.BytesKey([]byte{1, 2, 3})
var bytesValue = bytesKey.Next().Bytes()
var largerBytesKey = stoabs.BytesKey([]byte{4, 5, 6})
var largerBytesValue = []byte{100, 101, 102}

const stringKey = "message"
const stringValue = "Hello, World!"

const shelf = "test"

type StoreProvider func(t *testing.T) (stoabs.KVStore, error)

func TestReadingAndWriting(t *testing.T, storeProvider StoreProvider) {
	ctx := context.Background()

	t.Run("read/write", func(t *testing.T) {
		t.Run("write, then read ([]byte])", func(t *testing.T) {
			store := createStore(t, storeProvider)

			err := store.Write(ctx, func(tx stoabs.WriteTx) error {
				writer := tx.GetShelfWriter(shelf)
				return writer.Put(bytesKey, bytesValue)
			})
			if !assert.NoError(t, err) {
				return
			}

			var actual []byte
			err = store.ReadShelf(ctx, shelf, func(reader stoabs.Reader) error {
				actual, err = reader.Get(bytesKey)
				return err
			})
			assert.NoError(t, err)
			assert.Equal(t, bytesValue, actual)
		})
		t.Run("write, then read (string)", func(t *testing.T) {
			store := createStore(t, storeProvider)

			err := store.Write(ctx, func(tx stoabs.WriteTx) error {
				writer := tx.GetShelfWriter(shelf)
				return writer.Put(stoabs.BytesKey(stringKey), []byte(stringValue))
			})
			if !assert.NoError(t, err) {
				return
			}

			var actual []byte
			err = store.ReadShelf(ctx, shelf, func(reader stoabs.Reader) error {
				actual, err = reader.Get(stoabs.BytesKey(stringKey))
				return err
			})
			assert.NoError(t, err)
			assert.Equal(t, stringValue, string(actual))
		})

		t.Run("ReadShelf for non-existing shelf", func(t *testing.T) {
			store := createStore(t, storeProvider)

			called := false
			err := store.ReadShelf(ctx, shelf, func(reader stoabs.Reader) error {
				called = true
				return nil
			})

			assert.NoError(t, err)
			assert.True(t, called)
		})

		t.Run("GetShelfReader for non-existing shelf", func(t *testing.T) {
			store := createStore(t, storeProvider)

			err := store.Read(ctx, func(tx stoabs.ReadTx) error {
				shelf := tx.GetShelfReader(shelf)
				value, err := shelf.Get(stoabs.BytesKey("key"))
				if err != nil {
					return err
				}
				if value == nil {
					// OK, NilReader should return nil
					return nil
				}
				t.Fatal()
				return nil
			})
			assert.ErrorIs(t, err, stoabs.ErrKeyNotFound)
		})

		t.Run("read non-existing key", func(t *testing.T) {
			store := createStore(t, storeProvider)

			err := store.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
				return writer.Put(stoabs.BytesKey(stringKey), []byte(stringValue))
			})
			if !assert.NoError(t, err) {
				return
			}

			var actual []byte
			err = store.ReadShelf(ctx, shelf, func(reader stoabs.Reader) error {
				actual, err = reader.Get(bytesKey)
				return err
			})
			assert.ErrorIs(t, err, stoabs.ErrKeyNotFound)
			assert.Nil(t, actual)
		})
	})
}

func TestRange(t *testing.T, storeProvider StoreProvider) {
	ctx := context.Background()

	t.Run("Range()", func(t *testing.T) {
		// b - b+1 - gap - b+3
		type entry struct {
			key   stoabs.Key
			value []byte
		}
		input := []entry{
			// 1, 2, 3 => 1, 2, 4
			{key: bytesKey, value: bytesValue},
		}
		// 1, 2, 4 => 1, 2, 4
		input = append(input, entry{key: input[0].key.Next(), value: bytesValue})
		// Here's a gap
		// 1, 2, 5 => 1, 2, 4
		input = append(input, entry{key: input[1].key.Next().Next(), value: bytesValue})

		t.Run("range over values (with gaps, stop at gaps)", func(t *testing.T) {
			store := createStore(t, storeProvider)
			from := bytesKey     // inclusive
			to := largerBytesKey // exclusive

			// Write some data
			_ = store.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
				for _, e := range input {
					_ = writer.Put(e.key, e.value)
				}
				return nil
			})

			var actual []entry
			err := store.ReadShelf(ctx, shelf, func(reader stoabs.Reader) error {
				err := reader.Range(from, to, func(key stoabs.Key, value []byte) error {
					actual = append(actual, entry{
						key:   key,
						value: value,
					})
					return nil
				}, true)

				return err
			})
			assert.NoError(t, err)
			require.Len(t, actual, 2)
			assert.Equal(t, input[0], actual[0])
			assert.Equal(t, input[1], actual[1])
		})
		t.Run("range over values (with gaps, but skip over gaps)", func(t *testing.T) {
			store := createStore(t, storeProvider)
			from := bytesKey     // inclusive
			to := largerBytesKey // exclusive

			// Write some data
			_ = store.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
				for _, e := range input {
					_ = writer.Put(e.key, e.value)
				}
				return nil
			})

			var actual []entry
			err := store.ReadShelf(ctx, shelf, func(reader stoabs.Reader) error {
				err := reader.Range(from, to, func(key stoabs.Key, value []byte) error {
					actual = append(actual, entry{
						key:   key,
						value: value,
					})
					return nil
				}, false)

				return err
			})
			assert.NoError(t, err)
			assert.Equal(t, input, actual)
		})

		t.Run("many results (2000 in store, want 1500)", func(t *testing.T) {
			store := createStore(t, storeProvider)
			from := bytesKey
			var to stoabs.Key

			var currentKey stoabs.Key = from
			_ = store.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
				for i := 1; i <= 2000; i++ {
					if i == 1501 { // end is exclusive
						to = currentKey
					}
					_ = writer.Put(currentKey, bytesValue)
					currentKey = currentKey.Next()
				}
				return nil
			})

			// Range from..to
			var keys, values [][]byte
			err := store.ReadShelf(ctx, shelf, func(reader stoabs.Reader) error {
				err := reader.Range(from, to, func(key stoabs.Key, value []byte) error {
					keys = append(keys, key.Bytes())
					values = append(values, value)
					return nil
				}, false)
				return err
			})

			// Just assert number of entries (correctness is verified in other test)
			assert.NoError(t, err)
			assert.Len(t, keys, 1500)
			assert.Len(t, values, 1500)
		})

		t.Run("error", func(t *testing.T) {
			store := createStore(t, storeProvider)
			from := bytesKey     // inclusive
			to := largerBytesKey // exclusive

			// Write some data
			_ = store.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
				return writer.Put(bytesKey, bytesValue)
			})

			err := store.ReadShelf(ctx, shelf, func(reader stoabs.Reader) error {
				err := reader.Range(from, to, func(key stoabs.Key, value []byte) error {
					return errors.New("failure")
				}, false)

				return err
			})

			assert.EqualError(t, err, "failure")
			assert.NotErrorIs(t, err, stoabs.ErrDatabase{})
		})

		t.Run("TX context cancelled", func(t *testing.T) {
			store := createStore(t, storeProvider)

			// Write some data
			_ = store.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
				for _, e := range input {
					_ = writer.Put(e.key, e.value)
				}
				return nil
			})

			// Cancel read context
			ctx, cancel := context.WithCancel(ctx)

			err := store.ReadShelf(ctx, shelf, func(reader stoabs.Reader) error {
				return reader.Range(bytesKey, largerBytesKey, func(key stoabs.Key, value []byte) error {
					// cancel within Range to make sure the context cancellation is caught during Range()
					cancel()
					return nil
				}, false)
			})

			assert.ErrorIs(t, err, context.Canceled)
			assert.ErrorIs(t, err, stoabs.ErrDatabase{})
		})
	})
}

func TestEmpty(t *testing.T, storeProvider StoreProvider) {
	ctx := context.Background()

	t.Run("Empty()", func(t *testing.T) {
		t.Run("returns true for an empty shelf", func(t *testing.T) {
			store := createStore(t, storeProvider)

			err := store.ReadShelf(ctx, shelf, func(reader stoabs.Reader) error {
				empty, err := reader.Empty()
				assert.NoError(t, err)
				assert.True(t, empty)
				return nil
			})
			assert.NoError(t, err)
		})

		t.Run("returns true for an empty shelf when other data is available", func(t *testing.T) {
			store := createStore(t, storeProvider)

			// Write some data
			_ = store.WriteShelf(ctx, "other", func(writer stoabs.Writer) error {
				_ = writer.Put(stoabs.BytesKey{0}, []byte{0})
				return nil
			})

			err := store.ReadShelf(ctx, shelf, func(reader stoabs.Reader) error {
				empty, err := reader.Empty()
				assert.NoError(t, err)
				assert.True(t, empty)
				return nil
			})
			assert.NoError(t, err)
		})

		t.Run("returns false for a non-empty shelf", func(t *testing.T) {
			store := createStore(t, storeProvider)

			// Write some data
			_ = store.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
				_ = writer.Put(stoabs.BytesKey{0}, []byte{0})
				return nil
			})

			err := store.ReadShelf(ctx, shelf, func(reader stoabs.Reader) error {
				empty, err := reader.Empty()
				assert.NoError(t, err)
				assert.False(t, empty)
				return nil
			})
			assert.NoError(t, err)
		})
	})
}

func TestIterate(t *testing.T, storeProvider StoreProvider) {
	ctx := context.Background()

	t.Run("Iterate()", func(t *testing.T) {
		t.Run("iterates over all keys", func(t *testing.T) {
			store := createStore(t, storeProvider)

			// Write some data
			_ = store.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
				_ = writer.Put(bytesKey, bytesValue)
				return writer.Put(largerBytesKey, largerBytesValue)
			})

			var keys, values [][]byte
			err := store.ReadShelf(ctx, shelf, func(reader stoabs.Reader) error {
				err := reader.Iterate(func(key stoabs.Key, value []byte) error {
					keys = append(keys, key.Bytes())
					values = append(values, value)
					return nil
				}, stoabs.BytesKey{})

				return err
			})
			if !assert.NoError(t, err) {
				return
			}

			if !assert.Len(t, keys, 2) {
				return
			}
			if !assert.Len(t, values, 2) {
				return
			}

			assert.Contains(t, keys, bytesKey.Bytes())
			assert.Contains(t, keys, largerBytesKey.Bytes())
			assert.Contains(t, values, bytesValue)
			assert.Contains(t, values, largerBytesValue)
		})

		t.Run("iterates over int keys", func(t *testing.T) {
			expectedKeys := []stoabs.Uint32Key{
				1,
				2,
				3,
				10,
				11,
			}
			store := createStore(t, storeProvider)

			// Write some data
			_ = store.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
				for _, key := range expectedKeys {
					_ = writer.Put(key, (key + 1000).Bytes())
				}
				return nil
			})

			var actualKeys []stoabs.Key
			err := store.ReadShelf(ctx, shelf, func(reader stoabs.Reader) error {
				err := reader.Iterate(func(key stoabs.Key, value []byte) error {
					actualKeys = append(actualKeys, key)
					return nil
				}, stoabs.Uint32Key(0))

				return err
			})
			assert.NoError(t, err)
			assert.Len(t, actualKeys, len(expectedKeys))
			for _, key := range expectedKeys {
				assert.Contains(t, actualKeys, key)
			}
		})

		t.Run("iterate over empty store", func(t *testing.T) {
			store := createStore(t, storeProvider)

			var keys, values [][]byte
			err := store.ReadShelf(ctx, shelf, func(reader stoabs.Reader) error {
				err := reader.Iterate(func(key stoabs.Key, value []byte) error {
					keys = append(keys, key.Bytes())
					values = append(values, value)
					return nil
				}, stoabs.BytesKey{})

				return err
			})
			assert.NoError(t, err)
			assert.Empty(t, keys)
			assert.Empty(t, values)
		})

		t.Run("error", func(t *testing.T) {
			store := createStore(t, storeProvider)

			// Write some data otherwise shelf is empty and no error can be returned
			err := store.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
				return writer.Put(bytesKey, bytesValue)
			})
			if !assert.NoError(t, err) {
				return
			}

			err = store.ReadShelf(ctx, shelf, func(reader stoabs.Reader) error {
				err := reader.Iterate(func(key stoabs.Key, value []byte) error {
					return errors.New("failure")
				}, stoabs.BytesKey{})

				return err
			})
			assert.EqualError(t, err, "failure")
			assert.NotErrorIs(t, err, stoabs.ErrDatabase{})
		})

		t.Run("TX context cancelled", func(t *testing.T) {
			store := createStore(t, storeProvider)

			// Write some data
			_ = store.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
				return writer.Put(bytesKey, bytesValue)
			})

			// Cancel read context
			ctx, cancel := context.WithCancel(ctx)
			// TODO: move cancellation inside iterator. Currently returns before iterator is reached
			cancel()

			err := store.ReadShelf(ctx, shelf, func(reader stoabs.Reader) error {
				return reader.Iterate(func(key stoabs.Key, value []byte) error {
					return nil
				}, stoabs.BytesKey{})
			})

			assert.ErrorIs(t, err, context.Canceled)
			assert.ErrorIs(t, err, stoabs.ErrDatabase{})
		})
	})
}

func TestWriteTransactions(t *testing.T, storeProvider StoreProvider) {
	ctx := context.Background()

	t.Run("write transactions", func(t *testing.T) {
		t.Run("onRollback called, afterCommit not called when commit fails", func(t *testing.T) {
			store := createStore(t, storeProvider)

			var rollbackCalled = false
			var afterCommitCalled = false
			err := store.Write(ctx, func(tx stoabs.WriteTx) error {
				switch dbTX := tx.Unwrap().(type) {
				case *bbolt.Tx:
					_ = dbTX.Rollback()
				case *badger.Txn:
					dbTX.Discard()
				case *sql.Tx:
					_ = dbTX.Rollback()
				default:
					// Not supported
					t.SkipNow()
				}
				return errors.New("rollbacked")
			}, stoabs.OnRollback(func() {
				rollbackCalled = true
			}), stoabs.AfterCommit(func() {
				afterCommitCalled = true
			}))
			assert.Error(t, err)
			assert.True(t, rollbackCalled)
			assert.False(t, afterCommitCalled)
		})

		t.Run("rollback does not commit changes", func(t *testing.T) {
			store := createStore(t, storeProvider)
			firstKey := bytesKey
			secondKey := firstKey.Next()
			thirdKey := secondKey.Next()

			// First write some data
			_ = store.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
				_ = writer.Put(firstKey, bytesValue)
				return nil
			})

			// Then write some data in a TX that is rolled back
			var actualValue []byte
			err := store.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
				_ = writer.Put(secondKey, bytesValue)
				_ = writer.Put(thirdKey, bytesValue)
				// Also read a value to assert it doesn't accidentally commit the writes above (Redis)
				actualValue, _ = writer.Get(firstKey)
				return errors.New("failure")
			})
			assert.Error(t, err)
			assert.NotErrorIs(t, err, stoabs.ErrDatabase{}) // user error
			assert.Equal(t, bytesValue, actualValue)

			// Assert that the first key can be read, but the second and third keys not
			var actual []stoabs.Key
			err = store.ReadShelf(ctx, shelf, func(reader stoabs.Reader) error {
				return reader.Iterate(func(key stoabs.Key, _ []byte) error {
					actual = append(actual, key)
					return nil
				}, stoabs.BytesKey{})
			})
			if !assert.NoError(t, err) {
				return
			}
			assert.Len(t, actual, 1)
			assert.Contains(t, actual, firstKey)
		})

		t.Run("afterCommit and onRollback after commit", func(t *testing.T) {
			store := createStore(t, storeProvider)

			var actual []byte
			var innerError error
			var onRollbackCalled bool

			err := store.Write(ctx, func(tx stoabs.WriteTx) error {
				writer := tx.GetShelfWriter(shelf)
				return writer.Put(bytesKey, bytesValue)
			}, stoabs.AfterCommit(func() {
				// Happens after commit, so we should be able to read the data now
				innerError = store.ReadShelf(ctx, shelf, func(reader stoabs.Reader) error {
					actual, innerError = reader.Get(bytesKey)
					return innerError
				})
				if innerError != nil {
					t.Fatal(innerError)
				}
			}), stoabs.OnRollback(func() {
				onRollbackCalled = true
			}))

			assert.NoError(t, err)
			assert.Equal(t, bytesValue, actual)
			assert.False(t, onRollbackCalled)
		})
		t.Run("afterCommit and onRollback on rollback", func(t *testing.T) {
			store := createStore(t, storeProvider)

			var afterCommitCalled bool
			var onRollbackCalled bool

			_ = store.Write(ctx, func(tx stoabs.WriteTx) error {
				return errors.New("failed")
			}, stoabs.AfterCommit(func() {
				afterCommitCalled = true
			}), stoabs.OnRollback(func() {
				onRollbackCalled = true
			}))

			assert.False(t, afterCommitCalled)
			assert.True(t, onRollbackCalled)
		})
		t.Run("store is set on transaction", func(t *testing.T) {
			store := createStore(t, storeProvider)
			_ = store.Write(ctx, func(tx stoabs.WriteTx) error {
				assert.True(t, tx.Store() == store)
				return nil
			})
		})
		t.Run("context cancellation prevents commit", func(t *testing.T) {
			store := createStore(t, storeProvider)
			callbackWaiter := &sync.Mutex{}
			callbackWaiter.Lock()

			errs := make(chan error)

			ctx, cancel := context.WithCancel(ctx)
			go func() {
				errs <- store.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
					// Now write something that should never be committed
					err := writer.Put(bytesKey, bytesValue)
					callbackWaiter.Unlock()
					// Wait until context is cancelled, blocking TX commit
					<-ctx.Done()
					return err
				})
			}()

			// Wait until WriteShelf callback is active
			callbackWaiter.Lock()
			cancel()

			// Transaction should now have been aborted because context was cancelled
			err := <-errs
			assert.ErrorIs(t, err, context.Canceled)
			assert.ErrorIs(t, err, stoabs.ErrDatabase{})
			// Assert value hasn't been ommitted
			var actual []byte
			err = store.ReadShelf(context.Background(), shelf, func(reader stoabs.Reader) error {
				var err error
				actual, err = reader.Get(bytesKey)
				return err
			})
			assert.ErrorIs(t, err, stoabs.ErrKeyNotFound)
			assert.Nil(t, actual)
		})
	})
}

func TestTransactionWriteLock(t *testing.T, storeProvider StoreProvider) {
	ctx := context.Background()

	t.Run("Transaction-Level Write Lock", func(t *testing.T) {
		t.Run("Multiple routines try to lock", func(t *testing.T) {
			store := createStore(t, storeProvider)

			const numTXs = 10
			// We use a Mutex.TryLock() to detect if write transactions are executed concurrently.
			// If we can't acquire the lock, it means there's another TX in flight.
			assertionLock := &sync.Mutex{}
			failures := make(chan error, numTXs)
			wg := sync.WaitGroup{}

			for i := 0; i < numTXs; i++ {
				wg.Add(1)
				go func(mux *sync.Mutex, idx int) {
					err := store.Write(ctx, func(tx stoabs.WriteTx) error {
						logrus.Infof("starting %d", idx)
						if !mux.TryLock() {
							return errors.New("concurrent write transactions detected")
						}
						defer mux.Unlock()
						writer := tx.GetShelfWriter(shelf)
						logrus.Infof("end of %d", idx)
						return writer.Put(bytesKey, bytesValue)
					}, stoabs.WithWriteLock())
					if err != nil {
						failures <- err
					}
					wg.Done()
				}(assertionLock, i)
			}

			// Wait for all TXs to finish
			wg.Wait()

			// Check for failures
			assert.Len(t, failures, 0)
		})

		t.Run("context expired", func(t *testing.T) {
			store := createStore(t, storeProvider)

			ctx, cancel := context.WithTimeout(ctx, 500*time.Millisecond)
			defer cancel()

			err := store.Write(ctx, func(tx stoabs.WriteTx) error {
				time.Sleep(time.Second)
				return nil
			}, stoabs.WithWriteLock())

			assert.ErrorIs(t, err, context.DeadlineExceeded)
			assert.ErrorIs(t, err, stoabs.ErrDatabase{})
		})
	})
}

func TestDelete(t *testing.T, storeProvider StoreProvider) {
	ctx := context.Background()

	t.Run("Delete()", func(t *testing.T) {
		t.Run("write, delete, read", func(t *testing.T) {
			store := createStore(t, storeProvider)
			// Write
			err := store.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
				_ = writer.Put(bytesKey, bytesValue)
				return writer.Put(largerBytesKey, bytesValue)
			})
			assert.NoError(t, err)
			// Delete
			err = store.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
				return writer.Delete(bytesKey)
			})
			assert.NoError(t, err)
			// Read, assert
			var actual []byte
			err = store.ReadShelf(ctx, shelf, func(reader stoabs.Reader) error {
				actual, err = reader.Get(bytesKey)
				return err
			})
			assert.ErrorIs(t, err, stoabs.ErrKeyNotFound)
			assert.Nil(t, actual)
		})
		t.Run("delete non-existing entry", func(t *testing.T) {
			store := createStore(t, storeProvider)
			err := store.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
				return writer.Delete(bytesKey)
			})
			assert.NoError(t, err)
		})
	})
}

func TestClose(t *testing.T, storeProvider StoreProvider) {
	t.Run("Close()", func(t *testing.T) {
		t.Run("close closed store", func(t *testing.T) {
			store := createStore(t, storeProvider)
			assert.NoError(t, store.Close(context.Background()))
			assert.NoError(t, store.Close(context.Background()))
		})
		t.Run("timeout", func(t *testing.T) {
			store := createStore(t, storeProvider)
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			err := store.Close(ctx)
			assert.Equal(t, stoabs.DatabaseError(context.Canceled), err)
		})
	})
}

func TestStats(t *testing.T, storeProvider StoreProvider) {
	t.Run("stats", func(t *testing.T) {
		ctx := context.Background()

		store := createStore(t, storeProvider)
		getStats := func(store stoabs.KVStore, shelf string) stoabs.ShelfStats {
			var stats stoabs.ShelfStats
			_ = store.ReadShelf(ctx, shelf, func(reader stoabs.Reader) error {
				stats = reader.Stats()
				return nil
			})
			return stats
		}

		t.Run("empty", func(t *testing.T) {
			stats := getStats(store, shelf)
			assert.Equal(t, uint(0), stats.NumEntries)
			assert.Equal(t, uint(0), stats.ShelfSize)
		})

		t.Run("non-empty", func(t *testing.T) {
			err := store.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
				return writer.Put(stoabs.Uint32Key(2), []byte("test value"))
			})
			if err != nil {
				t.Fatal(err)
			}
			stats := getStats(store, shelf)

			assert.Equal(t, uint(1), stats.NumEntries)
			assert.Less(t, uint(0), stats.ShelfSize)
		})
	})
}

func createStore(t *testing.T, provider StoreProvider) stoabs.KVStore {
	store, err := provider(t)
	if !assert.NoError(t, err) {
		t.Fatalf("Unable to create store: %s", err)
	}
	t.Cleanup(func() {
		_ = store.Close(context.Background())
	})
	return store
}

// TODO: Write in other shelf with same key name, make sure they don't overwrite
