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

package badger

import (
	"context"
	"errors"
	"fmt"
	"github.com/dgraph-io/badger/v4"
	"github.com/nuts-foundation/go-stoabs"
	"github.com/nuts-foundation/go-stoabs/kvtests"
	"github.com/nuts-foundation/go-stoabs/util"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/assert"
	"path"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
)

var key = []byte{1, 2, 3}
var value = []byte{4, 5, 6}

const shelf = "test"

func TestBadger(t *testing.T) {
	provider := func(t *testing.T) (stoabs.KVStore, error) {
		return CreateBadgerStore(path.Join(util.TestDirectory(t), "badger.db"), stoabs.WithNoSync())
	}

	kvtests.TestReadingAndWriting(t, provider)
	kvtests.TestRange(t, provider)
	kvtests.TestEmpty(t, provider)
	kvtests.TestIterate(t, provider)
	kvtests.TestClose(t, provider)
	kvtests.TestDelete(t, provider)
	//kvtests.TestStats(t, provider) //not yet completed
	kvtests.TestWriteTransactions(t, provider)
	// Badger supports parallel transactions
	//kvtests.TestTransactionWriteLock(t, provider)
}

func TestBadger_Unwrap(t *testing.T) {
	store, _ := createStore(t)

	var tx interface{}
	_ = store.Read(context.Background(), func(innerTx stoabs.ReadTx) error {
		tx = innerTx.Unwrap()
		return nil
	})
	_, ok := tx.(*badger.Txn)
	assert.True(t, ok)
}

// TestBadger_IteratorClose tests that iterators are closed and panics are avoided
func TestBadger_IteratorClose(t *testing.T) {
	ctx := context.Background()

	tests := []struct {
		ctx    context.Context
		name   string
		err    error
		assert func(error)
	}{
		{
			ctx,
			"before rollback",
			errors.New("failed"),
			func(err error) {
				assert.EqualError(t, err, "failed")
			},
		},
		{
			ctx,
			"before commit",
			nil,
			func(err error) {
				assert.NoError(t, err)
			},
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store, _ := createStore(t)
			check := atomic.Bool{}
			group := sync.WaitGroup{}
			group.Add(1)
			err := store.WriteShelf(test.ctx, shelf, func(writer stoabs.Writer) error {
				err := writer.Put(stoabs.BytesKey(key), value)
				if err != nil {
					panic(err)
				}
				go func() {
					writer.Iterate(func(key stoabs.Key, value []byte) error {
						check.Store(true)
						group.Done()
						return nil
					}, stoabs.BytesKey{})
				}()
				group.Wait()
				return test.err
			})
			assert.True(t, check.Load())
			test.assert(err)
		})
	}
}

func TestBadger_CreateBadgerStore(t *testing.T) {
	t.Run("opening locked file logs warning", func(t *testing.T) {
		filename := filepath.Join(util.TestDirectory(t), "test-store")
		logger, _ := test.NewNullLogger()

		// create first store
		store1, err := CreateBadgerStore(filename)
		if !assert.NoError(t, err) {
			return
		}
		defer store1.Close(context.Background())

		_, err = CreateBadgerStore(filename, stoabs.WithLogger(logger)) // hangs while store1 is open

		assert.EqualError(t, err, fmt.Sprintf("Cannot acquire directory lock on \"%s\".  Another process is using this Badger database. err: resource temporarily unavailable", filename))
	})
}

func TestBadger_Close(t *testing.T) {
	ctx := context.Background()
	var bytesKey = stoabs.BytesKey([]byte{1, 2, 3})
	var bytesValue = bytesKey.Next().Bytes()
	store, _ := createStore(t)

	t.Run("Close()", func(t *testing.T) {
		t.Run("write to closed store", func(t *testing.T) {
			assert.NoError(t, store.Close(context.Background()))
			err := store.WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
				return writer.Put(bytesKey, bytesValue)
			})
			assert.Contains(t, err.Error(), "Writes are blocked, possibly due to DropAll or Close")
		})
	})
}

func createStore(t *testing.T) (stoabs.KVStore, error) {
	store, err := CreateBadgerStore("", stoabs.WithNoSync())
	t.Cleanup(func() {
		_ = store.Close(context.Background())
	})
	return store, err
}
