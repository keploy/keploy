package testnet

import (
	"os"
	"testing"
)

// SocketDir returns a directory for a test's unix sockets, made under
// os.TempDir and removed when the test ends. It is not the test's temporary
// directory: a unix socket's path holds at most 107 bytes (103 on macOS), and
// t.TempDir's path is GOTMPDIR (else TMPDIR), up to 64 bytes of the test's
// name, a random suffix and /001. Under a GOTMPDIR of a few dozen bytes a
// socket there no longer fits: bind refuses its path (EINVAL), and docker's
// API client refuses it as a DOCKER_HOST ("unix socket path ... is too
// long"). A socket's path in SocketDir is os.TempDir's, at most 17 bytes
// more, and the socket's name.
func SocketDir(t testing.TB) string {
	t.Helper()
	dir, err := os.MkdirTemp("", "ksock")
	if err != nil {
		t.Fatalf("a directory for the test's unix sockets: %v", err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(dir) })
	return dir
}
