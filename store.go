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

package stoabs

import (
	"context"
	"errors"
	"fmt"
	"github.com/prometheus/client_golang/prometheus"
	"time"

	"github.com/sirupsen/logrus"
)

// DatabaseError wraps the given error in ErrDatabase if it isn't already in the error chain.
func DatabaseError(err error) error {
	if errors.Is(err, ErrDatabase{}) {
		// Only wrap once to keep ErrDatabase closest to the actual database error
		return err
	}
	return ErrDatabase{err}
}

// ErrDatabase signals that the wrapped error is related to database access, or due to context cancellation/timeout.
// The action that resulted in this error may succeed when retried.
type ErrDatabase struct {
	error
}

func (e ErrDatabase) Error() string {
	// Use Sprintf to avoid dereferencing of wrapped nil error
	return fmt.Sprintf("database error: %s", e.error)
}

func (e ErrDatabase) Is(other error) bool {
	_, ok := other.(ErrDatabase)
	return ok
}
func (e ErrDatabase) Unwrap() error {
	return e.error
}

// ErrStoreIsClosed is returned when an operation is executed on a closed store. Is also a ErrDatabase.
var ErrStoreIsClosed = DatabaseError(errors.New("database not open"))

// ErrCommitFailed is returned when the commit of transaction fails. Is also a ErrDatabase.
var ErrCommitFailed = DatabaseError(errors.New("unable to commit transaction"))

// ErrKeyNotFound is returned when the requested key does not exist
var ErrKeyNotFound = errors.New("key not found")

const DefaultTransactionTimeout = 30 * time.Second

const defaultLockAcquisitionTimeout = 3 * time.Second

// KVStore defines the interface for a key-value store.
// Writing to it is done in callbacks passed to the Write-functions. If the callback returns an error, the transaction is rolled back.
// Methods return a ErrDatabase when the context has been cancelled or timed-out.
type KVStore interface {
	Store
	// Write starts a writable transaction and passes it to the given function.
	// Callers should not try to read values which are written in the same transactions, and thus haven't been committed yet.
	// The result when doing so depends on transaction isolation of the underlying database.
	// The passed context can be used to cancel long-running operations or the final commit of the transaction.
	Write(ctx context.Context, fn func(WriteTx) error, opts ...TxOption) error
	// Read starts a read-only transaction and passes it to the given function.
	// The passed context can be used to cancel long-running read operations.
	Read(ctx context.Context, fn func(ReadTx) error) error
	// WriteShelf starts a writable transaction, open a writer for the specified shelf and passes it to the given function.
	// If the shelf does not exist, it will be created.
	// The same semantics of Write apply.
	// The passed context can be used to cancel long-running operations or the final commit of the transaction.
	WriteShelf(ctx context.Context, shelfName string, fn func(Writer) error) error
	// ReadShelf starts a read-only transaction, open a reader for the specified shelf and passes it to the given function.
	// If the shelf does not exist, the function is not called.
	// The passed context can be used to cancel long-running read operations.
	ReadShelf(ctx context.Context, shelfName string, fn func(Reader) error) error
}

type Option func(config *Config)

// Config specifies the configuration for a KVStore.
type Config struct {
	Log                         *logrus.Logger
	NoSync                      bool
	LockAcquireTimeout          time.Duration
	PrometheusCollectorReceiver func(collectors []prometheus.Collector)
}

// DefaultConfig returns the default configuration.
func DefaultConfig() Config {
	return Config{
		Log:                logrus.StandardLogger(),
		LockAcquireTimeout: defaultLockAcquisitionTimeout,
	}
}

// WithLockAcquireTimeout overrides the default timeout for acquiring a lock.
func WithLockAcquireTimeout(value time.Duration) Option {
	return func(config *Config) {
		config.LockAcquireTimeout = value
	}
}

// WithNoSync specifies that the database should not flush its data to disk.
// Support depends on the underlying database.
func WithNoSync() Option {
	return func(config *Config) {
		config.NoSync = true
	}
}

// WithLogger overrides the default logger.
func WithLogger(log *logrus.Logger) Option {
	return func(config *Config) {
		config.Log = log
	}
}

func WithPrometheus(collectorReceiver func(collectors []prometheus.Collector)) Option {
	return func(config *Config) {
		config.PrometheusCollectorReceiver = collectorReceiver
	}
}

// ShelfStats contains statistics about a shelf.
type ShelfStats struct {
	// NumEntries holds the number of entries in the shelf.
	NumEntries uint
	// ShelfSize holds the current shelf size in bytes.
	ShelfSize uint
}

// CallerFn is the function type which is called for each key value pair when using Iterate() or Range()
type CallerFn func(key Key, value []byte) error

// Reader is used to read from a shelf.
type Reader interface {
	// Empty returns true if the shelf contains no data
	Empty() (bool, error)
	// Get returns the value for the given key.
	// If the key does not exist it returns ErrKeyNotFound.
	// Returns a ErrDatabase if unsuccessful.
	Get(key Key) ([]byte, error)
	// Iterate walks over all key/value pairs for this shelf. Ordering is not guaranteed.
	// The caller will have to supply the correct key type, such that the keys can be parsed.
	Iterate(callback CallerFn, keyType Key) error
	// Range calls the callback for each key/value pair on this shelf from (inclusive) and to (exclusive) given keys.
	// Ordering is guaranteed and determined by the type of Key given.
	// If stopAtNil is true the operation stops when a non-existing key is encountered.
	Range(from Key, to Key, callback CallerFn, stopAtNil bool) error
	// Stats returns statistics about the shelf.
	Stats() ShelfStats
}

// Writer is used to write to a shelf.
type Writer interface {
	Reader

	// Put stores the given key and value in the shelf.
	// Returns a ErrDatabase if unsuccessful.
	Put(key Key, value []byte) error
	// Delete removes the given key from the shelf.
	// Returns a ErrDatabase if unsuccessful.
	Delete(key Key) error
}

type Store interface {
	// Close releases all resources associated with the store. It is safe to call multiple (subsequent) times.
	// The context being passed can be used to specify a timeout for the close operation.
	// Returns a ErrDatabase if unsuccessful,
	Close(ctx context.Context) error
}

// TxOption holds options for store transactions.
type TxOption interface{}

// WriteLockOption see WithWriteLock
type WriteLockOption struct {
}

// Enabled returns whether the WithWriteLock option was specified.
func (w WriteLockOption) Enabled(opts []TxOption) bool {
	for _, opt := range opts {
		if _, ok := opt.(WriteLockOption); ok {
			return true
		}
	}
	return false
}

// WithWriteLock is a transaction option that acquires a write lock for the entire store, making sure there are no concurrent writeable transactions.
// The lock is released when the transaction finishes in any way (commit/rollback).
func WithWriteLock() TxOption {
	return WriteLockOption{}
}

// AfterCommitOption see AfterCommit
type AfterCommitOption struct {
	fn func()
}

// Invoke calls all functions registered with the AfterCommitOption.
func (o AfterCommitOption) Invoke(opts []TxOption) {
	for _, opt := range opts {
		if ar, ok := opt.(*AfterCommitOption); ok {
			ar.fn()
		}
	}
}

// AfterCommit specifies a function that will be called after a transaction is successfully committed.
// There can be multiple AfterCommit functions, which will be called in order.
func AfterCommit(fn func()) TxOption {
	return &AfterCommitOption{fn: fn}
}

// OnRollbackOption see OnRollback
type OnRollbackOption struct {
	fn func()
}

// Invoke calls all functions registered with the OnRollbackOption.
func (o OnRollbackOption) Invoke(opts []TxOption) {
	for _, opt := range opts {
		if ar, ok := opt.(*OnRollbackOption); ok {
			ar.fn()
		}
	}
}

// OnRollback specifies a function that will be called after a transaction is successfully rolled back.
// There can be multiple OnRollback functions, which will be called in order.
func OnRollback(fn func()) TxOption {
	return &OnRollbackOption{fn: fn}
}

// WriteTx is used to write to a KVStore.
type WriteTx interface {
	ReadTx
	// GetShelfWriter returns the specified shelf for writing. If it doesn't exist, it will be created.
	// To keep the API easy to use it doesn't return an error when it fails, but any call to the returned Writer will return the error that occurred.
	GetShelfWriter(shelfName string) Writer
}

// ReadTx is used to read from a KVStore.
type ReadTx interface {
	// GetShelfReader returns the specified shelf for reading. If it doesn't exist, a NilReader is returned that will return nil for all read operations.
	GetShelfReader(shelfName string) Reader
	// Store returns the KVStore on which the transaction is started
	Store() KVStore
	// Unwrap returns the underlying, database specific transaction object. If not supported, it returns nil.
	Unwrap() interface{}
}

// NilReader is a shelfReader that always returns nil. It can be used when shelves do not exist.
type NilReader struct{}

func (n NilReader) Empty() (bool, error) {
	return true, nil
}

func (n NilReader) Get(_ Key) ([]byte, error) {
	return nil, ErrKeyNotFound
}

func (n NilReader) Iterate(_ CallerFn, _ Key) error {
	return nil
}

func (n NilReader) Range(_ Key, to Key, _ CallerFn, _ bool) error {
	return nil
}

func (n NilReader) Stats() ShelfStats {
	return ShelfStats{
		NumEntries: 0,
		ShelfSize:  0,
	}
}

// NewErrorWriter returns a Writer that will return the error for every method
func NewErrorWriter(err error) Writer {
	return errWriter{err: DatabaseError(err)}
}

// errWriter is a shelfWriter that already failed, but to reduce the error cases to be handled, it'll fail on all operations
type errWriter struct {
	err error
}

func (e errWriter) Empty() (bool, error) {
	return false, e.err
}

func (e errWriter) Get(_ Key) ([]byte, error) {
	return nil, e.err
}

func (e errWriter) Iterate(_ CallerFn, _ Key) error {
	return e.err
}

func (e errWriter) Range(_ Key, _ Key, _ CallerFn, _ bool) error {
	return e.err
}

func (e errWriter) Stats() ShelfStats {
	return ShelfStats{
		NumEntries: 0,
		ShelfSize:  0,
	}
}

func (e errWriter) Put(_ Key, _ []byte) error {
	return e.err
}

func (e errWriter) Delete(_ Key) error {
	return e.err
}
