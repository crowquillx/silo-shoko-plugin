//go:build !aix && !darwin && !dragonfly && !freebsd && !hurd && !illumos && !ios && !linux && !netbsd && !openbsd && !solaris

package vfs

import "context"

// Non-Unix hosts do not have the production plugin's advisory flock support.
// The in-process mutex still protects ordinary tests and local use.
func (r *Reconciler) acquireLock(context.Context) (func(), error) {
	return func() {}, nil
}
