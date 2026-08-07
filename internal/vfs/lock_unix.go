//go:build aix || darwin || dragonfly || freebsd || hurd || illumos || ios || linux || netbsd || openbsd || solaris

package vfs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"
)

func (r *Reconciler) acquireLock(ctx context.Context) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if _, err := os.Stat(r.config.Root); err != nil {
		if os.IsNotExist(err) {
			// The first dry-run is deliberately non-mutating. A real reconcile
			// creates the root before any link or state write, so there is no
			// cross-process state to lock at this point.
			return func() {}, nil
		}
		return nil, fmt.Errorf("vfs: inspect lock root: %w", err)
	}
	file, err := os.OpenFile(r.lockPath, os.O_RDWR|os.O_CREATE, 0o600)
	if err != nil {
		return nil, fmt.Errorf("vfs: open lock: %w", err)
	}
	for {
		err = syscall.Flock(int(file.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				_ = file.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) {
			_ = file.Close()
			return nil, fmt.Errorf("vfs: acquire lock: %w", err)
		}
		select {
		case <-ctx.Done():
			_ = file.Close()
			return nil, ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}
