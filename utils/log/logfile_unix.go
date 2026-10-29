//go:build !windows

package log

import "syscall"

// oNoFollow makes ReadLogFile's open refuse a symlink at the log's name, and
// oNonBlock keeps it from waiting forever on a named pipe put there.
const (
	oNoFollow = syscall.O_NOFOLLOW
	oNonBlock = syscall.O_NONBLOCK
)
