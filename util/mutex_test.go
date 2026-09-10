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
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"sync/atomic"
	"testing"
	"time"
)

func Test_ContextRWLocker(t *testing.T) {
	t.Run("lock, unlock, then lock again", func(t *testing.T) {
		l := ContextRWLocker{}
		err := l.LockContext(context.Background())
		l.Unlock()
		assert.NoError(t, err)

		err = l.LockContext(context.Background())
		assert.NoError(t, err)
	})
	t.Run("rlock, unlock, then lock again", func(t *testing.T) {
		l := ContextRWLocker{}
		err := l.RLockContext(context.Background())
		l.RUnlock()
		assert.NoError(t, err)

		err = l.RLockContext(context.Background())
		assert.NoError(t, err)
	})
	t.Run("wlock, rlock", func(t *testing.T) {
		l := &ContextRWLocker{}

		err := l.LockContext(context.Background())
		assert.NoError(t, err)
		l.Unlock()

		err = l.RLockContext(context.Background())
		assert.NoError(t, err)
		l.RUnlock()

		err = l.LockContext(context.Background())
		assert.NoError(t, err)
		l.Unlock()
	})
	t.Run("rlock, rlock, unlock", func(t *testing.T) {
		l := &ContextRWLocker{}

		err := l.RLockContext(context.Background())
		assert.NoError(t, err)

		err = l.RLockContext(context.Background())
		assert.NoError(t, err)

		l.RUnlock()
		l.RUnlock()

		err = l.LockContext(context.Background())
		assert.NoError(t, err)
		l.Unlock()
	})
	t.Run("context cancelled", func(t *testing.T) {
		l := ContextRWLocker{}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()

		err := l.LockContext(ctx)
		assert.ErrorIs(t, err, context.Canceled)
		assert.ErrorIs(t, err, stoabs.ErrDatabase{})
	})
	t.Run("context timeout", func(t *testing.T) {
		l := ContextRWLocker{}
		ctx, cancel := context.WithTimeout(context.Background(), time.Nanosecond)
		defer cancel()

		err := l.LockContext(ctx)
		assert.ErrorIs(t, err, context.DeadlineExceeded)
		assert.ErrorIs(t, err, stoabs.ErrDatabase{})
	})
}

// lockAcquiredContext is a cancelled context whose Done() only returns once the
// locking goroutine has acquired the lock. That forces the interleaving in which
// the lock is acquired concurrently with the context expiring, which used to
// deadlock lockWithCancel.
type lockAcquiredContext struct {
	context.Context
	lockAcquired chan struct{}
}

func (c lockAcquiredContext) Done() <-chan struct{} {
	<-c.lockAcquired
	// give the locking goroutine time to get past fnLock() and signal the caller
	time.Sleep(time.Millisecond)
	return c.Context.Done()
}

func Test_lockWithCancel(t *testing.T) {
	t.Run("lock acquired while context expires does not deadlock and releases the lock", func(t *testing.T) {
		var locks, unlocks atomic.Int32
		cancelled, cancel := context.WithCancel(context.Background())
		cancel()

		// Repeat: the caller picks randomly when both the lock and the expired context are ready.
		for i := 0; i < 100; i++ {
			lockAcquired := make(chan struct{})
			ctx := lockAcquiredContext{Context: cancelled, lockAcquired: lockAcquired}
			fnLock := func() {
				locks.Add(1)
				close(lockAcquired)
			}
			fnUnlock := func() {
				unlocks.Add(1)
			}

			done := make(chan error, 1)
			go func() {
				done <- lockWithCancel(ctx, fnLock, fnUnlock)
			}()

			select {
			case err := <-done:
				if err == nil {
					// caller got the lock before it noticed the expired context: it owns the lock
					fnUnlock()
				} else {
					assert.ErrorIs(t, err, context.Canceled)
				}
			case <-time.After(5 * time.Second):
				require.FailNowf(t, "deadlock", "lockWithCancel did not return (iteration %d)", i)
			}
		}

		// Every acquired lock must be released, either by the caller or by the locking goroutine.
		assert.Eventually(t, func() bool {
			return locks.Load() == unlocks.Load()
		}, 5*time.Second, 10*time.Millisecond, "locks=%d unlocks=%d", locks.Load(), unlocks.Load())
	})
}
