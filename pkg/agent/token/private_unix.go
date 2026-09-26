//go:build !windows

package token

import (
	"os"
	"syscall"
)

// isPrivate reports whether a directory can be entered by this user alone:
// mode 0700 and owned by this process's effective user. os.MkdirTemp creates
// exactly that, and a directory another user put in its place cannot be both.
func isPrivate(fi os.FileInfo) bool {
	st, ok := fi.Sys().(*syscall.Stat_t)
	return ok && fi.Mode().Perm() == 0o700 && int(st.Uid) == os.Geteuid()
}
