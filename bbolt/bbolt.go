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
	"bytes"
	"context"
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"os"
	"path"
	"time"

	"github.com/nuts-foundation/go-stoabs"
	"github.com/nuts-foundation/go-stoabs/util"
	"github.com/sirupsen/logrus"
	"go.etcd.io/bbolt"
)

var _ stoabs.ReadTx = (*bboltTx)(nil)
var _ stoabs.WriteTx = (*bboltTx)(nil)
var _ stoabs.Reader = (*bboltShelf)(nil)
var _ stoabs.Writer = (*bboltShelf)(nil)

const defaultFileTimeout = 5 * time.Second

var fileTimeout = defaultFileTimeout

// CreateBBoltStore creates a new BBolt-backed KV store.
func CreateBBoltStore(filePath string, opts ...stoabs.Option) (stoabs.KVStore, error) {
	cfg := stoabs.DefaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}

	bboltOpts := *bbolt.DefaultOptions
	if cfg.NoSync {
		bboltOpts.NoSync = true
		bboltOpts.NoFreelistSync = true
		bboltOpts.NoGrowSync = true
	}
	return createBBoltStore(filePath, &bboltOpts, cfg)
}

func createBBoltStore(filePath string, options *bbolt.Options, cfg stoabs.Config) (stoabs.KVStore, error) {
	err := os.MkdirAll(path.Dir(filePath), os.ModePerm) // TODO: Right permissions?
	if err != nil {
		return nil, stoabs.DatabaseError(err)
	}

	done := make(chan bool, 1)
	ticker := time.NewTicker(fileTimeout)
	go func() {
		for {
			select {
			case <-done:
				ticker.Stop()
				return
			case <-ticker.C:
				cfg.Log.Warnf("Trying to open %s, but file appears to be locked", filePath)
			}
		}
	}()

	db, err := bbolt.Open(filePath, os.FileMode(0640), options) // TODO: Right permissions?
	done <- true
	if err != nil {
		return nil, stoabs.DatabaseError(err)
	}

	return Wrap(db, cfg), nil
}

// Wrap creates a KVStore using an existing bbolt.db
func Wrap(db *bbolt.DB, cfg stoabs.Config) stoabs.KVStore {
	result := &store{
		db:   db,
		cfg:  cfg,
		log:  cfg.Log,
		lock: &util.ContextRWLocker{},
	}
	if cfg.PrometheusCollectorReceiver != nil {
		// Create following metrics:
		// - Histogram for transaction acquisition time (vector for read transaction and write transaction)
		// - Histogram for transaction commit time
		// - Histogram for write transaction duration
		result.promTXAcquisitionDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "bbolt_tx_acquisition_duration_seconds",
			Help:    "Duration of BBolt transaction acquisition in seconds (experimental, may be removed without notice)",
			Buckets: prometheus.DefBuckets,
		}, []string{"type"})
		result.promTXCommitDuration = prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "bbolt_tx_commit_duration_seconds",
			Help:    "Duration of BBolt transaction commit in seconds (experimental, may be removed without notice)",
			Buckets: prometheus.DefBuckets,
		})
		result.promTXDuration = prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "bbolt_tx_duration_seconds",
			Help:    "Duration of BBolt transaction in seconds (experimental, may be removed without notice)",
			Buckets: prometheus.DefBuckets,
		}, []string{"type"})
		cfg.PrometheusCollectorReceiver([]prometheus.Collector{
			result.promTXAcquisitionDuration,
			result.promTXCommitDuration,
			result.promTXDuration,
		})
	}
	return result
}

type store struct {
	db                        *bbolt.DB
	log                       *logrus.Logger
	lock                      *util.ContextRWLocker
	cfg                       stoabs.Config
	promTXAcquisitionDuration *prometheus.HistogramVec
	promTXCommitDuration      prometheus.Histogram
	promTXDuration            *prometheus.HistogramVec
}

func (b *store) Close(ctx context.Context) error {
	err := util.CallWithTimeout(ctx, b.db.Close, func() {
		b.log.Error("Closing of BBolt store timed out, store may not shut down correctly.")
	})
	if err != nil {
		return stoabs.DatabaseError(err)
	}
	return nil
}

func (b *store) Write(ctx context.Context, fn func(stoabs.WriteTx) error, opts ...stoabs.TxOption) error {
	return b.doTX(ctx, func(tx *bbolt.Tx) error {
		return fn(&bboltTx{tx: tx, store: b, ctx: ctx})
	}, true, opts)
}

func (b *store) Read(ctx context.Context, fn func(stoabs.ReadTx) error) error {
	return b.doTX(ctx, func(tx *bbolt.Tx) error {
		return fn(&bboltTx{tx: tx, store: b, ctx: ctx})
	}, false, nil)
}

func (b *store) WriteShelf(ctx context.Context, shelfName string, fn func(writer stoabs.Writer) error) error {
	return b.doTX(ctx, func(tx *bbolt.Tx) error {
		shelf := bboltTx{tx: tx, store: b, ctx: ctx}.GetShelfWriter(shelfName)
		return fn(shelf)
	}, true, nil)
}

func (b *store) ReadShelf(ctx context.Context, shelfName string, fn func(reader stoabs.Reader) error) error {
	return b.doTX(ctx, func(tx *bbolt.Tx) error {
		shelf := bboltTx{tx: tx, store: b, ctx: ctx}.GetShelfReader(shelfName)
		return fn(shelf)
	}, false, nil)
}

func (b *store) doTX(ctx context.Context, fn func(tx *bbolt.Tx) error, writable bool, opts []stoabs.TxOption) error {
	var unlock func()
	lockCtx, lockCtxCancel := context.WithTimeout(ctx, b.cfg.LockAcquireTimeout)
	defer lockCtxCancel()
	txAcquisitionStartTime := time.Now()
	var txType string
	if writable {
		txType = "write"
		err := b.lock.LockContext(lockCtx)
		if err != nil {
			return fmt.Errorf("unable to obtain BBolt write lock: %w", err)
		}
		unlock = b.lock.Unlock
	} else {
		txType = "read"
		err := b.lock.RLockContext(lockCtx)
		if err != nil {
			return fmt.Errorf("unable to obtain BBolt read lock: %w", err)
		}
		unlock = b.lock.RUnlock
	}

	// Start transaction, retrieve/create shelf to operate on
	dbTX, err := b.db.Begin(writable)
	if err != nil {
		unlock()
		if err == bbolt.ErrDatabaseNotOpen {
			return stoabs.ErrStoreIsClosed
		}
		return stoabs.DatabaseError(err)
	}
	if b.promTXAcquisitionDuration != nil {
		b.promTXAcquisitionDuration.WithLabelValues(txType).Observe(time.Since(txAcquisitionStartTime).Seconds())
	}

	// Perform TX action(s)
	var txDurationStartTime = time.Now()
	appError := fn(dbTX)
	if b.promTXDuration != nil {
		b.promTXDuration.WithLabelValues(txType).Observe(time.Since(txDurationStartTime).Seconds())
	}

	// Writable TXs should be committed, non-writable TXs rolled back
	if !writable {
		rollbackTX(dbTX, b.log)
		unlock()
		return appError
	}
	// Observe result, commit/rollback
	if appError != nil {
		b.log.WithError(appError).Warn("Rolling back transaction application due to error")
		rollbackTX(dbTX, b.log)
		unlock()
		stoabs.OnRollbackOption{}.Invoke(opts)
		return appError
	}

	b.log.Trace("Committing BBolt transaction")
	// Check context cancellation, if not cancelled/expired; commit.
	var txCommitStartTime time.Time
	if ctx.Err() != nil {
		err = ctx.Err()
		rollbackTX(dbTX, b.log)
	} else {
		txCommitStartTime = time.Now()
		err = dbTX.Commit()
	}
	if err != nil {
		unlock()
		stoabs.OnRollbackOption{}.Invoke(opts)
		return util.WrapError(stoabs.ErrCommitFailed, err)
	}
	if b.promTXCommitDuration != nil {
		b.promTXCommitDuration.Observe(time.Since(txCommitStartTime).Seconds())
	}

	unlock()
	stoabs.AfterCommitOption{}.Invoke(opts)
	return nil
}

func rollbackTX(dbTX *bbolt.Tx, log *logrus.Logger) {
	err := dbTX.Rollback()
	if err != nil {
		log.WithError(err).Error("Could not rollback BBolt transaction")
	}
}

type bboltTx struct {
	store *store
	tx    *bbolt.Tx
	ctx   context.Context
}

func (b bboltTx) Unwrap() interface{} {
	return b.tx
}

func (b bboltTx) GetShelfReader(shelfName string) stoabs.Reader {
	return b.getBucket(shelfName)
}

func (b bboltTx) GetShelfWriter(shelfName string) stoabs.Writer {
	bucket, err := b.tx.CreateBucketIfNotExists([]byte(shelfName))
	if err != nil {
		return stoabs.NewErrorWriter(err)
	}
	return &bboltShelf{bucket: bucket, ctx: b.ctx}
}

func (b bboltTx) getBucket(shelfName string) stoabs.Reader {
	bucket := b.tx.Bucket([]byte(shelfName))
	if bucket == nil {
		return stoabs.NilReader{}
	}
	return &bboltShelf{bucket: bucket, ctx: b.ctx}
}

func (b bboltTx) Store() stoabs.KVStore {
	return b.store
}

type bboltShelf struct {
	bucket *bbolt.Bucket
	ctx    context.Context
}

func (t bboltShelf) Empty() (bool, error) {
	// bbolt statistics can be used since they are accurate
	stats := t.Stats()
	return stats.NumEntries == 0, nil
}

func (t bboltShelf) Get(key stoabs.Key) ([]byte, error) {
	value := t.bucket.Get(key.Bytes())
	if value == nil {
		return nil, stoabs.ErrKeyNotFound
	}

	// Because things will go terribly wrong when you use a []byte returned by BBolt outside its transaction,
	// we want to make sure to work with a copy.
	//
	// This seems to be the best (and shortest) way to copy a byte slice:
	// https://github.com/go101/go101/wiki/How-to-perfectly-clone-a-slice%3F
	return append(value[:0:0], value...), nil
}

func (t bboltShelf) Put(key stoabs.Key, value []byte) error {
	if err := t.bucket.Put(key.Bytes(), value); err != nil {
		return stoabs.DatabaseError(err)
	}
	return nil
}

func (t bboltShelf) Delete(key stoabs.Key) error {
	if err := t.bucket.Delete(key.Bytes()); err != nil {
		return stoabs.DatabaseError(err)
	}
	return nil
}

func (t bboltShelf) Stats() stoabs.ShelfStats {
	return stoabs.ShelfStats{
		NumEntries: uint(t.bucket.Stats().KeyN),
		ShelfSize:  uint(t.bucket.Tx().Size()),
	}
}

func (t bboltShelf) Iterate(callback stoabs.CallerFn, keyType stoabs.Key) error {
	cursor := t.bucket.Cursor()
	for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
		// Potentially long-running operation, check context for cancellation
		if t.ctx.Err() != nil {
			return stoabs.DatabaseError(t.ctx.Err())
		}
		// return a copy to avoid data manipulation
		vCopy := append(v[:0:0], v...)
		key, err := keyType.FromBytes(k)
		if err != nil {
			// should never happen
			return err
		}
		if err := callback(key, vCopy); err != nil {
			return err
		}
	}
	return nil
}

func (t bboltShelf) Range(from stoabs.Key, to stoabs.Key, callback stoabs.CallerFn, stopAtNil bool) error {
	cursor := t.bucket.Cursor()
	var prevKey stoabs.Key
	for k, v := cursor.Seek(from.Bytes()); k != nil && bytes.Compare(k, to.Bytes()) < 0; k, v = cursor.Next() {
		// Potentially long-running operation, check context for cancellation
		if t.ctx.Err() != nil {
			return stoabs.DatabaseError(t.ctx.Err())
		}
		key, err := from.FromBytes(k)
		if err != nil {
			return err
		}
		if stopAtNil && prevKey != nil && !prevKey.Next().Equals(key) {
			// gap found, stop here
			return nil
		}
		// return a copy to avoid data manipulation
		vCopy := append(v[:0:0], v...)
		if err := callback(key, vCopy); err != nil {
			return err
		}
		prevKey = key
	}
	return nil
}
