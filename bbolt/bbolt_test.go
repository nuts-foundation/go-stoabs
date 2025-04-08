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
	"github.com/prometheus/client_golang/prometheus"
	"github.com/sirupsen/logrus"
	"github.com/sirupsen/logrus/hooks/test"
	"github.com/stretchr/testify/require"
	"go.etcd.io/bbolt"
	"os"
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

func TestBBolt_PrometheusMetrics(t *testing.T) {
	createStoreWithStats := func(t *testing.T) stoabs.KVStore {
		var collectors []prometheus.Collector
		store, _ := createStore(t, stoabs.WithPrometheus(func(c []prometheus.Collector) {
			collectors = c
		}), stoabs.WithNoSync())
		for _, collector := range collectors {
			t.Cleanup(func() {
				prometheus.Unregister(collector)
			})
			require.NoError(t, prometheus.Register(collector))
		}
		return store
	}

	ctx := context.Background()
	t.Run("write TX metrics", func(t *testing.T) {
		err := createStoreWithStats(t).WriteShelf(ctx, shelf, func(writer stoabs.Writer) error {
			return writer.Put(stoabs.BytesKey(key), value)
		})
		assert.NoError(t, err)
		stats := getPrometheusStats(t)
		assert.Contains(t, stats, "bbolt_tx_acquisition_duration_seconds_count{type=\"write\"} 1")
		assert.Contains(t, stats, "bbolt_tx_commit_duration_seconds_count 1")
		assert.Contains(t, stats, "bbolt_tx_duration_seconds_count{type=\"write\"} 1")
		assert.NotContains(t, stats, "bbolt_tx_acquisition_duration_seconds_count{type=\"read\"}")
	})
	t.Run("read TX metrics", func(t *testing.T) {
		_ = createStoreWithStats(t).ReadShelf(ctx, shelf, func(reader stoabs.Reader) error {
			_, err := reader.Get(stoabs.BytesKey(key))
			return err
		})
		stats := getPrometheusStats(t)
		assert.Contains(t, stats, "bbolt_tx_acquisition_duration_seconds_count{type=\"read\"} 1")
		assert.Contains(t, stats, "bbolt_tx_duration_seconds_count{type=\"read\"} 1")
	})
}

func getPrometheusStats(t *testing.T) string {
	fileName := path.Join(t.TempDir(), "prometheus.txt")
	err := prometheus.WriteToTextfile(fileName, prometheus.Gatherers{prometheus.DefaultGatherer})
	require.NoError(t, err)
	data, err := os.ReadFile(fileName)
	require.NoError(t, err)
	return string(data)
}

func createStore(t *testing.T, opts ...stoabs.Option) (stoabs.KVStore, error) {
	if opts == nil {
		opts = []stoabs.Option{stoabs.WithNoSync()}
	}
	store, err := CreateBBoltStore(path.Join(util.TestDirectory(t), "bbolt.db"), opts...)
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
