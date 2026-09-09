package main

import (
	"context"
	"errors"
	"strings"
	"time"

	"github.com/gofrs/flock"
	"github.com/rs/zerolog/log"
)

type withLockFn func()
type ctxLockFn func(context.Context, time.Duration) (bool, error)
type ctxLockFnGen func(*flock.Flock) ctxLockFn

const retryDelay = 300 * time.Millisecond

var ErrAlreadyLocked = errors.New("the file is already locked")

// Do something under a file lock, so that other queues (and other webcomic2cbz
// processes) cannot do so at the same time.
//
// The given file does not have to exist.
//
// acquireTimeout dictates how long to retry acquiring the lock before
// giving up and returning ErrAlreadyLocked.
//
// Any returned error will indicate the lock could not be acquired, and is unrelated
// to fn execution.
func WithLockFile(path string, acquireTimeout time.Duration, fn withLockFn) error {
	lockFn := func(f *flock.Flock) ctxLockFn { return f.TryLockContext }
	return withLockFile(path, acquireTimeout, fn, lockFn)
}

// Similar to WithLockFile, but only to read the file. This has the convenience that
// multiple items that just need to read the file can do so concurrently.
func WithRLockFile(path string, acquireTimeout time.Duration, fn withLockFn) error {
	lockFn := func(f *flock.Flock) ctxLockFn { return f.TryLockContext }
	return withLockFile(path, acquireTimeout, fn, lockFn)
}

func withLockFile(path string, acquireTimeout time.Duration, fn withLockFn, lockFn ctxLockFnGen) error {
	// Setup
	lockPath := strings.Join([]string{path, "lock"}, ".")
	f := flock.New(lockPath)

	ctx, cancel := context.WithTimeout(context.Background(), acquireTimeout)
	defer cancel()

	// Try acquire
	locked, err := lockFn(f)(ctx, retryDelay)
	if err != nil {
		return err
	}
	if !locked {
		return ErrAlreadyLocked
	}
	defer func() {
		// Try unlock. If that fails, then we don't need to return an error -
		// but we can log it.
		unlockErr := f.Unlock()
		if unlockErr != nil {
			log.Debug().
				Str("path", path).
				Str("unlock_msg", unlockErr.Error()).
				Msg("could not unlock, but carrying on.")
		}
	}()

	// Do the work under the lock
	fn()
	return nil
}
