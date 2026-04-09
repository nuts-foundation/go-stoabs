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

package util

import (
	"context"
	"github.com/nuts-foundation/go-stoabs"
	"sync"
	"sync/atomic"
)

// RWLocker defines the interface for a read-write lock like sync.RWMutex
type RWLocker interface {
	sync.Locker
	RLock()
	TryRLock() bool
	RUnlock()
	TryLock() bool
	RLocker() sync.Locker
}

var _ RWLocker = &sync.RWMutex{}

// ContextRWLocker returns a RWMutex that supports context cancellation.
type ContextRWLocker struct {
	mux sync.RWMutex
}

// LockContext locks the RWLocker with the given context.
// If the context is canceled the function will return immediately with the error from the context.
// It will still acquire the lock on the background, but it will be released immediately.
func (c *ContextRWLocker) LockContext(ctx context.Context) error {
	return lockWithCancel(ctx, c.mux.Lock, c.mux.Unlock)
}

// RLockContext locks the RWLocker with the given context.
// If the context is canceled the function will return immediately with the error from the context.
// It will still acquire the lock on the background, but it will be released immediately.
func (c *ContextRWLocker) RLockContext(ctx context.Context) error {
	return lockWithCancel(ctx, c.mux.RLock, c.mux.RUnlock)
}

func lockWithCancel(ctx context.Context, fnLock func(), fnUnlock func()) error {
	locked := make(chan bool)
	expired := &atomic.Value{}

	// We need an additional mutex to synchronize signalling the locking goroutine the context expired.
	// Otherwise, if the context cancellation handler and locking goroutine execute concurrently,
	// the context cancellation handler might return an error (which should cause an immediate unlock after acquiring the lock),
	// which (the signal) might be lost when the locking goroutine is already finished (or just finishing).
	m := &sync.Mutex{}

	go func() {
		fnLock()
		m.Lock()
		defer m.Unlock()
		if expired.Load() != nil {
			// Context expired, unlock immediately.
			fnUnlock()
		} else {
			locked <- true
		}
	}()

	select {
	case <-ctx.Done():
		m.Lock()
		defer m.Unlock()
		// context expired, signal to the locking goroutine to unlock immediately after acquiring the lock
		expired.Store(true)
		return stoabs.DatabaseError(ctx.Err())
	case <-locked:
		// we got the lock before the context expired
		return nil
	}
}

// Lock simply calls the underlying lock.
func (c *ContextRWLocker) Lock() {
	c.mux.Lock()
}

// RLock simply calls the underlying lock.
func (c *ContextRWLocker) RLock() {
	c.mux.RLock()
}

// TryLock simply calls the underlying lock.
func (c *ContextRWLocker) TryLock() bool {
	return c.mux.TryLock()
}

// RLocker simply calls the underlying lock.
func (c *ContextRWLocker) RLocker() sync.Locker {
	return c.mux.RLocker()
}

// TryRLock simply calls the underlying lock.
func (c *ContextRWLocker) TryRLock() bool {
	return c.mux.TryRLock()
}

// Unlock simply calls the underlying lock.
func (c *ContextRWLocker) Unlock() {
	c.mux.Unlock()
}

// RUnlock simply calls the underlying lock.
func (c *ContextRWLocker) RUnlock() {
	c.mux.RUnlock()
}

// ContextLocker provides a Mutex that supports context cancellation.
// Unlike ContextRWLocker, it only provides exclusive locking (no read lock).
// This is appropriate when only write serialization is needed and reads are handled natively by the underlying store.
type ContextLocker struct {
	mux sync.Mutex
}

// LockContext locks the Mutex with the given context.
// If the context is canceled the function will return immediately with the error from the context.
// It will still acquire the lock in the background, but it will be released immediately.
func (c *ContextLocker) LockContext(ctx context.Context) error {
	return lockWithCancel(ctx, c.mux.Lock, c.mux.Unlock)
}

// Unlock releases the Mutex.
func (c *ContextLocker) Unlock() {
	c.mux.Unlock()
}
