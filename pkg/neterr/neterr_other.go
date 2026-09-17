//go:build !windows

package neterr

import "syscall"

// Everywhere except Windows the portable syscall constants are the real errno
// values, so there is nothing extra to match.
var (
	connResetErrnos   []syscall.Errno
	connAbortedErrnos []syscall.Errno
	brokenPipeErrnos  []syscall.Errno
	connRefusedErrnos []syscall.Errno
)
