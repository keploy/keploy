//go:build !windows

package supervisor

import "syscall"

// Off Windows the session keeps the errno itself (errnoCode).
func keptClientEndCode(errno syscall.Errno) uint32 { return errnoCode(errno) }

func clientEndErrorOf(code uint32) error { return errnoError(code) }
