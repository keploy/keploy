//go:build !windows

package replay

import (
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestCopyDirContentsDoesNotLeakFileHandles(t *testing.T) {
	// RLIMIT_NOFILE is process-wide; keep this test non-parallel.
	var old syscall.Rlimit
	require.NoError(t, syscall.Getrlimit(syscall.RLIMIT_NOFILE, &old))
	limited := old
	limited.Cur = 128
	if limited.Cur > old.Max {
		limited.Cur = old.Max
	}
	require.NoError(t, syscall.Setrlimit(syscall.RLIMIT_NOFILE, &limited))
	t.Cleanup(func() { _ = syscall.Setrlimit(syscall.RLIMIT_NOFILE, &old) })

	src, dst := t.TempDir(), t.TempDir()
	for i := 0; i < 200; i++ {
		require.NoError(t, os.WriteFile(filepath.Join(src, fmt.Sprintf("tc-%d.yaml", i)), []byte("x"), 0644))
	}

	require.NoError(t, (&Replayer{}).copyDirContents(src, dst))
}
