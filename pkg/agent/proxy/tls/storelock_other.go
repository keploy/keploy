//go:build !linux

package tls

import "os"

// storeLockHolder says who holds the lock on f's file: here, no process.
// Windows does not tell who holds a LockFileEx lock, and on macOS the store
// lock is never taken (its directory under /run cannot be made, so the
// legacy sweep's changes run without it).
func storeLockHolder(*os.File) string { return storeLockHolderUnknown }
