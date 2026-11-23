//go:build !linux

package tls

import "os"

// storeLockHolder says who holds the lock on f's file: here, no process.
// Windows does not tell who holds a LockFileEx lock, and macOS has no table
// of flock holders like Linux's /proc/locks.
func storeLockHolder(*os.File) string { return storeLockHolderUnknown }
