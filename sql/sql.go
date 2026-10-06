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

// Package sql implements a stoabs.KVStore on a SQL database (SQLite, PostgreSQL, MySQL/MariaDB, SQL Server).
//
// Every shelf is a table with two columns, "key" (binary, primary key) and "value" (binary blob).
// The application owns the schema: it creates the tables (e.g. through its migration tooling) and tells the
// store how a shelf name maps to a table name. The store never executes DDL. Operating on a shelf whose table
// does not exist returns a stoabs.ErrDatabase.
//
// Expected table shape (types differ per database):
//
//	CREATE TABLE <table> (
//	    "key"   VARBINARY(128) NOT NULL PRIMARY KEY,
//	    "value" BLOB           NOT NULL
//	);
//
// Keys are compared and ordered bytewise, which matches the ordering of bbolt and of the stoabs.Key types.
//
// Writable transactions are serialized per store with a process-level lock, like the bbolt backend.
// Writes are buffered per shelf and flushed as multi-row upserts/deletes before any read that needs them
// and at commit, to keep the number of round trips low.
package sql

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"sync/atomic"

	"github.com/nuts-foundation/go-stoabs"
	"github.com/nuts-foundation/go-stoabs/util"
	"github.com/sirupsen/logrus"
)

var _ stoabs.KVStore = (*store)(nil)
var _ stoabs.ReadTx = (*tx)(nil)
var _ stoabs.WriteTx = (*tx)(nil)
var _ stoabs.Reader = (*shelf)(nil)
var _ stoabs.Writer = (*shelf)(nil)

// batchSize is the maximum number of rows per upsert/delete statement.
// 2 parameters per row keeps it under SQL Server's limit of 2100 parameters per statement.
const batchSize = 500

var identifierPattern = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// TableNameFunc maps a shelf name to the name of the table holding it.
// The returned name must be a plain SQL identifier: letters, digits and underscores, not starting with a digit.
type TableNameFunc func(shelf string) string

var nonIdentifierChars = regexp.MustCompile(`[^a-z0-9_]`)

// PrefixTableName returns a TableNameFunc that names the table "<prefix>_<shelf>", lower-cased,
// with every character that is not a letter, digit or underscore replaced by an underscore.
func PrefixTableName(prefix string) TableNameFunc {
	return func(shelf string) string {
		return nonIdentifierChars.ReplaceAllString(strings.ToLower(prefix+"_"+shelf), "_")
	}
}

// Wrap creates a KVStore on an existing database handle. Connection pooling, authentication and the schema are
// the responsibility of the caller. The handle is not closed by Close().
func Wrap(db *sql.DB, dialect Dialect, tableName TableNameFunc, opts ...stoabs.Option) (stoabs.KVStore, error) {
	if db == nil {
		return nil, errors.New("sql: db is nil")
	}
	if dialect == nil {
		return nil, errors.New("sql: dialect is nil")
	}
	if tableName == nil {
		return nil, errors.New("sql: tableName is nil")
	}
	cfg := stoabs.DefaultConfig()
	for _, opt := range opts {
		opt(&cfg)
	}
	return &store{
		db:        db,
		dialect:   dialect,
		tableName: tableName,
		cfg:       cfg,
		log:       cfg.Log,
		writeLock: &util.ContextLocker{},
	}, nil
}

type store struct {
	db        *sql.DB
	dialect   Dialect
	tableName TableNameFunc
	cfg       stoabs.Config
	log       *logrus.Logger
	writeLock *util.ContextLocker
	closed    atomic.Bool
}

func (s *store) Close(ctx context.Context) error {
	if !s.closed.CompareAndSwap(false, true) {
		return nil
	}
	// The database handle is owned by the caller; there is nothing to release here.
	// Still honour the context so behaviour matches the other backends.
	if ctx.Err() != nil {
		return stoabs.DatabaseError(ctx.Err())
	}
	return nil
}

func (s *store) Write(ctx context.Context, fn func(stoabs.WriteTx) error, opts ...stoabs.TxOption) error {
	return s.doTX(ctx, func(t *tx) error {
		return fn(t)
	}, true, opts)
}

func (s *store) Read(ctx context.Context, fn func(stoabs.ReadTx) error) error {
	return s.doTX(ctx, func(t *tx) error {
		return fn(t)
	}, false, nil)
}

func (s *store) WriteShelf(ctx context.Context, shelfName string, fn func(stoabs.Writer) error) error {
	return s.doTX(ctx, func(t *tx) error {
		return fn(t.GetShelfWriter(shelfName))
	}, true, nil)
}

func (s *store) ReadShelf(ctx context.Context, shelfName string, fn func(stoabs.Reader) error) error {
	return s.doTX(ctx, func(t *tx) error {
		return fn(t.GetShelfReader(shelfName))
	}, false, nil)
}

func (s *store) doTX(ctx context.Context, fn func(t *tx) error, writable bool, opts []stoabs.TxOption) error {
	if s.closed.Load() {
		return stoabs.ErrStoreIsClosed
	}
	if ctx.Err() != nil {
		return stoabs.DatabaseError(ctx.Err())
	}

	// Serialize writers per store, like bbolt. Readers are not blocked: the database handles concurrent reads.
	unlock := func() {}
	if writable {
		lockCtx, cancel := context.WithTimeout(ctx, s.cfg.LockAcquireTimeout)
		defer cancel()
		if err := s.writeLock.LockContext(lockCtx); err != nil {
			return fmt.Errorf("unable to obtain SQL write lock: %w", err)
		}
		unlock = s.writeLock.Unlock
	}

	var txOpts *sql.TxOptions
	if writable {
		txOpts = s.dialect.WriteTxOptions()
	} else {
		txOpts = s.dialect.ReadTxOptions()
	}
	dbTX, err := s.db.BeginTx(ctx, txOpts)
	if err != nil {
		unlock()
		return stoabs.DatabaseError(err)
	}
	t := &tx{store: s, tx: dbTX, ctx: ctx, writable: writable, shelves: map[string]*shelf{}}

	appError := fn(t)

	if !writable {
		s.rollback(dbTX)
		return appError
	}
	if appError != nil {
		s.log.WithError(appError).Warn("Rolling back SQL transaction due to error")
		s.rollback(dbTX)
		unlock()
		stoabs.OnRollbackOption{}.Invoke(opts)
		return appError
	}
	// Flush buffered writes, then commit; unless the context expired in the meantime.
	if ctx.Err() != nil {
		err = ctx.Err()
	} else {
		err = t.flushAll()
	}
	if err == nil {
		s.log.Trace("Committing SQL transaction")
		err = dbTX.Commit()
	}
	if err != nil {
		s.rollback(dbTX)
		unlock()
		stoabs.OnRollbackOption{}.Invoke(opts)
		return util.WrapError(stoabs.ErrCommitFailed, err)
	}
	unlock()
	stoabs.AfterCommitOption{}.Invoke(opts)
	return nil
}

func (s *store) rollback(dbTX *sql.Tx) {
	err := dbTX.Rollback()
	if err != nil && !errors.Is(err, sql.ErrTxDone) {
		s.log.WithError(err).Error("Could not rollback SQL transaction")
	}
}

// table returns the quoted table name for the shelf, or an error if the mapped name is not a safe identifier.
func (s *store) table(shelfName string) (string, error) {
	name := s.tableName(shelfName)
	if !identifierPattern.MatchString(name) {
		return "", fmt.Errorf("sql: table name for shelf %q is not a valid identifier: %q", shelfName, name)
	}
	return s.dialect.QuoteIdentifier(name), nil
}

type tx struct {
	store    *store
	tx       *sql.Tx
	ctx      context.Context
	writable bool
	shelves  map[string]*shelf
}

func (t *tx) Unwrap() interface{} {
	return t.tx
}

func (t *tx) Store() stoabs.KVStore {
	return t.store
}

func (t *tx) GetShelfReader(shelfName string) stoabs.Reader {
	sh, err := t.getShelf(shelfName)
	if err != nil {
		return stoabs.NewErrorWriter(err)
	}
	return sh
}

func (t *tx) GetShelfWriter(shelfName string) stoabs.Writer {
	sh, err := t.getShelf(shelfName)
	if err != nil {
		return stoabs.NewErrorWriter(err)
	}
	if !t.writable {
		return stoabs.NewErrorWriter(errors.New("sql: shelf writer requested in read-only transaction"))
	}
	return sh
}

func (t *tx) getShelf(shelfName string) (*shelf, error) {
	if sh, ok := t.shelves[shelfName]; ok {
		return sh, nil
	}
	table, err := t.store.table(shelfName)
	if err != nil {
		return nil, err
	}
	sh := &shelf{tx: t, table: table, pending: map[string]pendingWrite{}}
	t.shelves[shelfName] = sh
	return sh, nil
}

// flushAll writes the buffered changes of every shelf to the database, in a deterministic shelf order.
func (t *tx) flushAll() error {
	names := make([]string, 0, len(t.shelves))
	for name := range t.shelves {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if err := t.shelves[name].flush(); err != nil {
			return err
		}
	}
	return nil
}

type pendingWrite struct {
	value   []byte
	deleted bool
}

type shelf struct {
	tx    *tx
	table string
	// pending holds buffered writes of this transaction, keyed by the raw key bytes.
	pending map[string]pendingWrite
}

func (s *shelf) q(format string, args ...interface{}) string {
	return fmt.Sprintf(format, args...)
}

func (s *shelf) key() string   { return s.tx.store.dialect.QuoteIdentifier(keyColumn) }
func (s *shelf) value() string { return s.tx.store.dialect.QuoteIdentifier(valueColumn) }
func (s *shelf) ph(n int) string {
	return s.tx.store.dialect.Placeholder(n)
}

func (s *shelf) Empty() (bool, error) {
	if err := s.flush(); err != nil {
		return false, err
	}
	var exists int
	row := s.tx.tx.QueryRowContext(s.tx.ctx,
		s.q("SELECT CASE WHEN EXISTS (SELECT 1 FROM %s) THEN 1 ELSE 0 END", s.table))
	if err := row.Scan(&exists); err != nil {
		return false, stoabs.DatabaseError(err)
	}
	return exists == 0, nil
}

func (s *shelf) Get(key stoabs.Key) ([]byte, error) {
	if p, ok := s.pending[string(key.Bytes())]; ok {
		if p.deleted {
			return nil, stoabs.ErrKeyNotFound
		}
		return bytes.Clone(p.value), nil
	}
	var value []byte
	row := s.tx.tx.QueryRowContext(s.tx.ctx,
		s.q("SELECT %s FROM %s WHERE %s = %s", s.value(), s.table, s.key(), s.ph(1)), key.Bytes())
	err := row.Scan(&value)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, stoabs.ErrKeyNotFound
	}
	if err != nil {
		return nil, stoabs.DatabaseError(err)
	}
	if value == nil {
		value = []byte{}
	}
	return value, nil
}

func (s *shelf) Put(key stoabs.Key, value []byte) error {
	if value == nil {
		value = []byte{}
	}
	s.pending[string(key.Bytes())] = pendingWrite{value: bytes.Clone(value)}
	return nil
}

func (s *shelf) Delete(key stoabs.Key) error {
	s.pending[string(key.Bytes())] = pendingWrite{deleted: true}
	return nil
}

func (s *shelf) Stats() stoabs.ShelfStats {
	if err := s.flush(); err != nil {
		s.tx.store.log.WithError(err).Error("Could not flush writes before reading shelf stats")
		return stoabs.ShelfStats{}
	}
	var numEntries, size uint
	row := s.tx.tx.QueryRowContext(s.tx.ctx,
		s.q("SELECT COUNT(*), COALESCE(SUM(%s), 0) FROM %s", s.tx.store.dialect.ByteLength(s.value()), s.table))
	if err := row.Scan(&numEntries, &size); err != nil {
		s.tx.store.log.WithError(err).Error("Could not read shelf stats")
		return stoabs.ShelfStats{}
	}
	return stoabs.ShelfStats{NumEntries: numEntries, ShelfSize: size}
}

// pageSize is the number of rows fetched per query by Iterate and Range.
const pageSize = 1000

type row struct {
	key   []byte
	value []byte
}

// scanPage runs a query that returns (key, value) rows and reads them all, closing the result set before returning.
// Callbacks must never run while a result set is open: a nested query on the same transaction (e.g. a Get from
// inside an Iterate callback) fails on Postgres, MySQL and SQL Server while rows are pending on the connection.
func (s *shelf) scanPage(query string, args ...interface{}) ([]row, error) {
	rows, err := s.tx.tx.QueryContext(s.tx.ctx, query, args...)
	if err != nil {
		return nil, stoabs.DatabaseError(err)
	}
	defer rows.Close()
	page := make([]row, 0, pageSize)
	for rows.Next() {
		var r row
		if err := rows.Scan(&r.key, &r.value); err != nil {
			return nil, stoabs.DatabaseError(err)
		}
		if r.value == nil {
			r.value = []byte{}
		}
		page = append(page, r)
	}
	if err := rows.Err(); err != nil {
		return nil, stoabs.DatabaseError(err)
	}
	return page, nil
}

func (s *shelf) Iterate(callback stoabs.CallerFn, keyType stoabs.Key) error {
	if err := s.flush(); err != nil {
		return err
	}
	// Keyset pagination in key order: the first page has no lower bound, later pages continue after the last key seen.
	first := s.q("SELECT %s, %s FROM %s ORDER BY %s %s",
		s.key(), s.value(), s.table, s.key(), s.tx.store.dialect.LimitSuffix(pageSize))
	next := s.q("SELECT %s, %s FROM %s WHERE %s > %s ORDER BY %s %s",
		s.key(), s.value(), s.table, s.key(), s.ph(1), s.key(), s.tx.store.dialect.LimitSuffix(pageSize))
	var lastKey []byte
	for {
		if s.tx.ctx.Err() != nil {
			return stoabs.DatabaseError(s.tx.ctx.Err())
		}
		var page []row
		var err error
		if lastKey == nil {
			page, err = s.scanPage(first)
		} else {
			page, err = s.scanPage(next, lastKey)
		}
		if err != nil {
			return err
		}
		for _, r := range page {
			if s.tx.ctx.Err() != nil {
				return stoabs.DatabaseError(s.tx.ctx.Err())
			}
			key, err := keyType.FromBytes(r.key)
			if err != nil {
				return err
			}
			if err := callback(key, r.value); err != nil {
				return err
			}
		}
		if len(page) < pageSize {
			return nil
		}
		lastKey = page[len(page)-1].key
	}
}

func (s *shelf) Range(from stoabs.Key, to stoabs.Key, callback stoabs.CallerFn, stopAtNil bool) error {
	if err := s.flush(); err != nil {
		return err
	}
	// Keyset pagination: the first page starts at from (inclusive), later pages continue strictly after the last key
	// seen. The continuation must be a strict comparison on the last key itself: SQL Server pads the shorter of two
	// binary values with zero bytes when comparing, so "lastKey + 0x00" would compare equal to lastKey.
	first := s.q("SELECT %s, %s FROM %s WHERE %s >= %s AND %s < %s ORDER BY %s %s",
		s.key(), s.value(), s.table, s.key(), s.ph(1), s.key(), s.ph(2), s.key(), s.tx.store.dialect.LimitSuffix(pageSize))
	next := s.q("SELECT %s, %s FROM %s WHERE %s > %s AND %s < %s ORDER BY %s %s",
		s.key(), s.value(), s.table, s.key(), s.ph(1), s.key(), s.ph(2), s.key(), s.tx.store.dialect.LimitSuffix(pageSize))
	var lastKey []byte
	var prevKey stoabs.Key
	for {
		if s.tx.ctx.Err() != nil {
			return stoabs.DatabaseError(s.tx.ctx.Err())
		}
		var page []row
		var err error
		if lastKey == nil {
			page, err = s.scanPage(first, from.Bytes(), to.Bytes())
		} else {
			page, err = s.scanPage(next, lastKey, to.Bytes())
		}
		if err != nil {
			return err
		}
		for _, r := range page {
			if s.tx.ctx.Err() != nil {
				return stoabs.DatabaseError(s.tx.ctx.Err())
			}
			key, err := from.FromBytes(r.key)
			if err != nil {
				return err
			}
			if stopAtNil && prevKey != nil && !prevKey.Next().Equals(key) {
				// gap found, stop here
				return nil
			}
			if err := callback(key, r.value); err != nil {
				return err
			}
			prevKey = key
		}
		if len(page) < pageSize {
			return nil
		}
		lastKey = page[len(page)-1].key
	}
}

// flush writes the buffered puts and deletes of this shelf to the database within the transaction.
func (s *shelf) flush() error {
	if len(s.pending) == 0 {
		return nil
	}
	// Deterministic order: sort keys bytewise.
	keys := make([]string, 0, len(s.pending))
	for k := range s.pending {
		keys = append(keys, k)
	}
	sort.Strings(keys)

	var puts [][]byte // key, value, key, value, ...
	var deletes [][]byte
	for _, k := range keys {
		p := s.pending[k]
		if p.deleted {
			deletes = append(deletes, []byte(k))
		} else {
			puts = append(puts, []byte(k), p.value)
		}
	}
	s.pending = map[string]pendingWrite{}

	for start := 0; start < len(deletes); start += batchSize {
		end := min(start+batchSize, len(deletes))
		if err := s.deleteBatch(deletes[start:end]); err != nil {
			return err
		}
	}
	for start := 0; start < len(puts); start += 2 * batchSize {
		end := min(start+2*batchSize, len(puts))
		if err := s.upsertBatch(puts[start:end]); err != nil {
			return err
		}
	}
	return nil
}

func (s *shelf) upsertBatch(kv [][]byte) error {
	args := make([]interface{}, len(kv))
	for i, b := range kv {
		args[i] = b
	}
	stmt := s.tx.store.dialect.Upsert(s.table, len(kv)/2)
	if _, err := s.tx.tx.ExecContext(s.tx.ctx, stmt, args...); err != nil {
		return stoabs.DatabaseError(fmt.Errorf("upsert failed: %w", err))
	}
	return nil
}

func (s *shelf) deleteBatch(keys [][]byte) error {
	args := make([]interface{}, len(keys))
	placeholders := make([]string, len(keys))
	for i, k := range keys {
		args[i] = k
		placeholders[i] = s.ph(i + 1)
	}
	stmt := s.q("DELETE FROM %s WHERE %s IN (%s)", s.table, s.key(), strings.Join(placeholders, ", "))
	if _, err := s.tx.tx.ExecContext(s.tx.ctx, stmt, args...); err != nil {
		return stoabs.DatabaseError(fmt.Errorf("delete failed: %w", err))
	}
	return nil
}
