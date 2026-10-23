//go:build !windows

package tls

import (
	"os"
	"syscall"
)

// syscallMkfifo makes a FIFO at path, for the fake keytool's hold.
func syscallMkfifo(path string) error { return syscall.Mkfifo(path, 0o600) }

// sameOwner reports whether a and b have the same owner and group.
func sameOwner(a, b os.FileInfo) bool {
	sa, oka := a.Sys().(*syscall.Stat_t)
	sb, okb := b.Sys().(*syscall.Stat_t)
	return oka && okb && sa.Uid == sb.Uid && sa.Gid == sb.Gid
}
