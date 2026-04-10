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

package bbolt

import (
	"context"
	"errors"
	"fmt"
	"github.com/nuts-foundation/go-stoabs/kvtests"
	"github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"go.etcd.io/bbolt"
	"path"
	"path/filepath"
	"testing"
	"time"

	"github.com/nuts-foundation/go-stoabs"
	"github.com/nuts-foundation/go-stoabs/util"
	"github.com/stretchr/testify/assert"
)

var key = []byte{1, 2, 3}
var value = []byte{4, 5, 6}

const shelf = "test"

func TestBBolt(t *testing.T) {
	provider := func(t *testing.T) (stoabs.KVStore, error) {
		return CreateBBoltStore(path.Join(util.TestDirectory(t), "bbolt.db"), stoabs.WithNoSync())
	}

	kvtests.TestReadingAndWriting(t, provider)
	kvtests.TestRange(t, provider)
	kvtests.TestIterate(t, provider)
	kvtests.TestEmpty(t, provider)
	kvtests.TestClose(t, provider)
	kvtests.TestDelete(t, provider)
	kvtests.TestStats(t, provider)
	kvtests.TestWriteTransactions(t, provider)
	kvtests.TestTransactionWriteLock(t, provider)
}

func TestBBolt_Unwrap(t *testing.T) {
	store, _ := createStore(t)

	var tx interface{}
	_ = store.Read(context.Background(), func(innerTx stoabs.ReadTx) error {
		tx = innerTx.Unwrap()
		return nil
	})
	_, ok := tx.(*bbolt.Tx)
	assert.True(t, ok)
}

func TestBBolt_WriteShelf(t *testing.T) {
	ctx := context.Background()

	t.Run("rollback on application error", func(t *testing.T) {
		store, _ := createStore(t)

		err := store.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
			err := writer.Put(stoabs.BytesKey(key), value)
			if err != nil {
				panic(err)
			}
			return errors.New("failed")
		})
		assert.EqualError(t, err, "failed")

		// Now assert the TX was rolled back
		var actual []byte
		err = store.ReadShelf(ctx, shelf, func(reader stoabs.Reader) error {
			actual, err = reader.Get(stoabs.BytesKey(key))
			return err
		})
		assert.ErrorIs(t, err, stoabs.ErrKeyNotFound)
		assert.Nil(t, actual)
	})
}

func createStore(t *testing.T) (stoabs.KVStore, error) {
	store, err := CreateBBoltStore(path.Join(util.TestDirectory(t), "bbolt.db"), stoabs.WithNoSync())
	t.Cleanup(func() {
		_ = store.Close(context.Background())
	})
	return store, err
}

func TestBBolt_CreateBBoltStore(t *testing.T) {
	t.Run("opening locked file logs warning", func(t *testing.T) {
		fileTimeout = 10 * time.Millisecond
		defer func() {
			fileTimeout = defaultFileTimeout
		}()
		filename := filepath.Join(util.TestDirectory(t), "test-store")
		logger, hook := test.NewNullLogger()

		// create first store
		store1, err := CreateBBoltStore(filename)
		if !assert.NoError(t, err) {
			return
		}
		defer store1.Close(context.Background())

		// create second store
		go func() {
			store2, _ := CreateBBoltStore(filename, stoabs.WithLogger(logger)) // hangs while store1 is open
			_ = store2.Close(context.Background())
		}()

		// wait for logger
		var lastEntry *logrus.Entry
		util.WaitFor(t, func() (bool, error) {
			lastEntry = hook.LastEntry()
			return lastEntry != nil, nil
		}, 100*fileTimeout, "time-out while waiting for log message")

		assert.Equal(t, fmt.Sprintf("Trying to open %s, but file appears to be locked", filename), lastEntry.Message)
		assert.Equal(t, logrus.WarnLevel, lastEntry.Level)
	})
}

func TestBBolt_SyncInterval(t *testing.T) {
	ctx := context.Background()
	const numEntries = 100
	data := make([]byte, 1024)

	t.Run("data is flushed on close when dirty", func(t *testing.T) {
		dbPath := path.Join(util.TestDirectory(t), "bbolt.db")
		store, _ := CreateBBoltStore(dbPath, stoabs.WithSyncInterval(time.Second))

		for i := 0; i < numEntries; i++ {
			err := store.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
				return writer.Put(stoabs.BytesKey(fmt.Sprintf("%d", i)), data)
			})
			if !assert.NoError(t, err) {
				return
			}
		}
		assert.NoError(t, store.Close(ctx))

		// Reopen and verify all entries survived the close
		store, _ = CreateBBoltStore(dbPath, stoabs.WithSyncInterval(time.Second))
		defer store.Close(ctx)
		err := store.ReadShelf(ctx, shelf, func(reader stoabs.Reader) error {
			assert.Equal(t, uint(numEntries), reader.Stats().NumEntries)
			return nil
		})
		assert.NoError(t, err)
	})

	t.Run("data is flushed periodically", func(t *testing.T) {
		dbPath := path.Join(util.TestDirectory(t), "bbolt.db")
		kvStore, _ := CreateBBoltStore(dbPath, stoabs.WithSyncInterval(50*time.Millisecond))

		err := kvStore.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
			return writer.Put(stoabs.BytesKey("key"), data)
		})
		assert.NoError(t, err)

		// Wait for the periodic sync to fire
		util.WaitFor(t, func() (bool, error) {
			return !kvStore.(*store).mustSync.Load(), nil
		}, time.Second, "timed out waiting for periodic sync")

		assert.NoError(t, kvStore.Close(ctx))
	})
}

func BenchmarkBBolt_Flush(b *testing.B) {
	ctx := context.Background()
	const numEntries = 10
	data := make([]byte, 1024)

	b.Run("sync on every commit", func(b *testing.B) {
		dbPath := path.Join(b.TempDir(), "bbolt.db")
		kvStore, _ := CreateBBoltStore(dbPath)
		b.ResetTimer()
		for x := 0; x < b.N; x++ {
			for i := 0; i < numEntries; i++ {
				_ = kvStore.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
					return writer.Put(stoabs.BytesKey(fmt.Sprintf("%d", i)), data)
				})
			}
		}
		_ = kvStore.Close(ctx)
	})

	b.Run("sync at interval", func(b *testing.B) {
		dbPath := path.Join(b.TempDir(), "bbolt.db")
		kvStore, _ := CreateBBoltStore(dbPath, stoabs.WithSyncInterval(time.Second))
		b.ResetTimer()
		for x := 0; x < b.N; x++ {
			for i := 0; i < numEntries; i++ {
				_ = kvStore.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
					return writer.Put(stoabs.BytesKey(fmt.Sprintf("%d", i)), data)
				})
			}
		}
		_ = kvStore.Close(ctx)
	})
}

func TestBBolt_Close(t *testing.T) {
	ctx := context.Background()
	var bytesKey = stoabs.BytesKey([]byte{1, 2, 3})
	var bytesValue = bytesKey.Next().Bytes()
	store, _ := CreateBBoltStore(path.Join(util.TestDirectory(t), "bbolt.db"), stoabs.WithNoSync())

	t.Run("Close()", func(t *testing.T) {
		t.Run("write to closed store", func(t *testing.T) {
			assert.NoError(t, store.Close(context.Background()))
			err := store.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
				return writer.Put(bytesKey, bytesValue)
			})
			assert.Equal(t, stoabs.ErrStoreIsClosed, err)
		})
	})
}
