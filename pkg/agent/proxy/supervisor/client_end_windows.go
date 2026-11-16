//go:build windows

package supervisor

import "syscall"

// On Windows the session keeps the client socket's reset and pipe errors
// (winsockCode), which no byte of errnos could hold.
func keptClientEndCode(errno syscall.Errno) uint32 { return winsockCode(errno) }

func clientEndErrorOf(code uint32) error { return winsockError(code) }
