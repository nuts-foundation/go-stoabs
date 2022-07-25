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
	"bytes"
	"context"
	"errors"
	"github.com/dgraph-io/badger/v3"
	"os"
	"path"
	"sync"
	"time"

	"github.com/nuts-foundation/go-stoabs"
	"github.com/nuts-foundation/go-stoabs/util"
	"github.com/sirupsen/logrus"
)

var _ stoabs.ReadTx = (*badgerTx)(nil)
var _ stoabs.WriteTx = (*badgerTx)(nil)
var _ stoabs.Reader = (*badgerShelf)(nil)
var _ stoabs.Writer = (*badgerShelf)(nil)

const defaultFileTimeout = 5 * time.Second

var fileTimeout = defaultFileTimeout

// CreateBadgerStore creates a new Badger-backed KV store.
func CreateBadgerStore(filePath string, opts ...stoabs.Option) (stoabs.KVStore, error) {
	cfg := stoabs.Config{}
	for _, opt := range opts {
		opt(&cfg)
	}

	badgerOpts := badger.DefaultOptions(filePath).WithLogger(getLogger(cfg))

	return createBadgerStore(filePath, badgerOpts, cfg)
}

func createBadgerStore(filePath string, options badger.Options, cfg stoabs.Config) (stoabs.KVStore, error) {
	err := os.MkdirAll(path.Dir(filePath), os.ModePerm)
	if err != nil {
		return nil, err
	}

	// log warning if file opening hangs
	cfg.Log = getLogger(cfg)
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

	db, err := badger.Open(options)
	done <- true
	if err != nil {
		return nil, err
	}

	return Wrap(db, cfg), nil
}

// Wrap creates a KVStore using an existing bbolt.DB
func Wrap(db *badger.DB, cfg stoabs.Config) stoabs.KVStore {
	return &store{
		db:  db,
		log: getLogger(cfg),
	}
}

func getLogger(cfg stoabs.Config) *logrus.Logger {
	if cfg.Log != nil {
		return cfg.Log
	}
	return stoabs.DefaultLogger()
}

type store struct {
	db    *badger.DB
	log   *logrus.Logger
	mutex sync.Mutex
}

func (b *store) Close(ctx context.Context) error {
	return util.CallWithTimeout(ctx, b.db.Close, func() {
		b.log.Error("Closing of BBolt store timed out, store may not shut down correctly.")
	})
}

func (b *store) Write(ctx context.Context, fn func(stoabs.WriteTx) error, opts ...stoabs.TxOption) error {
	return b.doTX(ctx, func(tx *badger.Txn) error {
		return fn(&badgerTx{tx: tx, store: b, ctx: ctx})
	}, true, opts)
}

func (b *store) Read(ctx context.Context, fn func(stoabs.ReadTx) error) error {
	return b.doTX(ctx, func(tx *badger.Txn) error {
		return fn(&badgerTx{tx: tx, store: b, ctx: ctx})
	}, false, nil)
}

func (b *store) WriteShelf(ctx context.Context, shelfName string, fn func(writer stoabs.Writer) error) error {
	return b.doTX(ctx, func(tx *badger.Txn) error {
		shelf, err := badgerTx{tx: tx, store: b, ctx: ctx}.GetShelfWriter(shelfName)
		if err != nil {
			return err
		}
		return fn(shelf)
	}, true, nil)
}

func (b *store) ReadShelf(ctx context.Context, shelfName string, fn func(reader stoabs.Reader) error) error {
	return b.doTX(ctx, func(tx *badger.Txn) error {
		shelf := badgerTx{tx: tx, store: b, ctx: ctx}.GetShelfReader(shelfName)
		return fn(shelf)
	}, false, nil)
}

func (b *store) doTX(ctx context.Context, fn func(tx *badger.Txn) error, writable bool, optsSlice []stoabs.TxOption) error {
	opts := stoabs.TxOptions(optsSlice)

	if writable {
		b.mutex.Lock()
	}

	// Start transaction, retrieve/create shelf to operate on
	dbTX := b.db.NewTransaction(writable)
	defer dbTX.Discard()

	// Perform TX action(s)
	appError := fn(dbTX)

	// Writable TXs should be committed, non-writable TXs rolled back
	if !writable {
		rollbackTX(dbTX)
		return appError
	}
	// Observe result, commit/rollback
	var err error
	if appError == nil {
		b.log.Trace("Committing BBolt transaction")
		// Check context cancellation, if not cancelled/expired; commit.
		if ctx.Err() != nil {
			err = ctx.Err()
			rollbackTX(dbTX)
		} else if writable {
			err = dbTX.Commit()
		}
		b.mutex.Unlock()
		if err != nil {
			opts.InvokeOnRollback()
			return util.WrapError(stoabs.ErrCommitFailed, err)
		}

		opts.InvokeAfterCommit()
	} else {
		b.log.WithError(appError).Warn("Rolling back transaction application due to error")
		rollbackTX(dbTX)
		opts.InvokeOnRollback()
		return appError
	}

	return nil
}

func rollbackTX(dbTX *badger.Txn) {
	dbTX.Discard()
}

type badgerTx struct {
	store *store
	tx    *badger.Txn
	ctx   context.Context
}

func (b badgerTx) Unwrap() interface{} {
	return b.tx
}

func (b badgerTx) GetShelfReader(shelfName string) stoabs.Reader {
	return b.getBucket(shelfName)
}

func (b badgerTx) GetShelfWriter(shelfName string) (stoabs.Writer, error) {
	return &badgerShelf{name: shelfName, tx: b.tx, ctx: b.ctx}, nil
}

func (b badgerTx) getBucket(shelfName string) stoabs.Reader {
	return &badgerShelf{name: shelfName, tx: b.tx, ctx: b.ctx}
}

func (b badgerTx) Store() stoabs.KVStore {
	return b.store
}

type badgerShelf struct {
	name string
	tx   *badger.Txn
	ctx  context.Context
}

func (t badgerShelf) key(key stoabs.Key) stoabs.Key {
	myBytes := []byte(t.name)
	newKey := stoabs.BytesKey((append(myBytes, key.Bytes()...)))
	return newKey
}

func (t badgerShelf) Get(key stoabs.Key) ([]byte, error) {
	item, err := t.tx.Get(t.key(key).Bytes())
	if err != nil {
		if errors.Is(err, badger.ErrKeyNotFound) {
			return nil, nil
		}
		return nil, err
	}
	value := make([]byte, item.ValueSize())
	item.ValueCopy(value)
	return value, nil
}

func (t badgerShelf) Put(key stoabs.Key, value []byte) error {
	return t.tx.Set(t.key(key).Bytes(), value)
}

func (t badgerShelf) Delete(key stoabs.Key) error {
	return t.tx.Delete(t.key(key).Bytes())
}

func (t badgerShelf) Stats() stoabs.ShelfStats {
	return stoabs.ShelfStats{
		NumEntries: 0,
		ShelfSize:  0,
	}
}

func (t badgerShelf) Iterate(callback stoabs.CallerFn, keyType stoabs.Key) error {
	it := t.tx.NewIterator(badger.DefaultIteratorOptions)
	defer it.Close()
	prefix := []byte(t.name)
	for it.Seek(prefix); it.ValidForPrefix(prefix); it.Next() {
		item := it.Item()
		k := item.Key()
		return item.Value(func(v []byte) error {
			kt, err := keyType.FromBytes(k[len(prefix):])
			if err != nil {
				return err
			}
			return callback(kt, v)
		})
	}
	return nil
}

func (t badgerShelf) Range(from stoabs.Key, to stoabs.Key, callback stoabs.CallerFn, stopAtNil bool) error {
	it := t.tx.NewIterator(badger.DefaultIteratorOptions)
	defer it.Close()
	prefix := []byte(t.name)
	var prevKey stoabs.Key
	for it.Seek(prefix); it.ValidForPrefix(prefix) && bytes.Compare(it.Item().Key(), to.Bytes()) < 0; it.Next() {
		item := it.Item()
		k := item.Key()
		key, _ := from.FromBytes(k)
		if stopAtNil && prevKey != nil && !prevKey.Next().Equals(key) {
			// gap found, stop here
			return nil
		}
		err := item.Value(func(v []byte) error {
			kt, err := from.FromBytes(k[len(prefix):])
			if err != nil {
				return err
			}
			return callback(kt, v)
		})
		if err != nil {
			return err
		}
		prevKey = key
		return nil
	}
	return nil
}
