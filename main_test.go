package main

import (
	"context"
	"github.com/spf13/cobra"
	"os"
	"path/filepath"
	"testing"

	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/log"
)

func TestStart_LoggerInitFailureSetsErrCode(t *testing.T) {
	// t.TempDir() has to come before t.Chdir: cleanups run LIFO, so
	// registering the temp dir first means its RemoveAll runs after the
	// working directory has been restored. Windows cannot remove a directory
	// that is still the process cwd.
	tmpDir := t.TempDir()

	// Creating a directory with the log file's name makes the os.OpenFile
	// (O_WRONLY|O_CREATE) in log.New() fail on every platform - EISDIR on
	// Unix, ERROR_ACCESS_DENIED on Windows.
	if err := os.Mkdir(filepath.Join(tmpDir, "keploy-logs.txt"), 0755); err != nil {
		t.Fatalf("failed to create dummy directory: %v", err)
	}

	// t.Chdir restores the working directory when the test ends. It also
	// panics if the test is ever marked parallel, which this one can never
	// be: it chdirs the whole process and writes the package-level
	// utils.ErrCode.
	t.Chdir(tmpDir)

	// start() only returns early while log.New() keeps failing here. log.New()
	// opens the hardcoded relative path "keploy-logs.txt", so making that path
	// configurable or absolute would break this setup - and start() would then
	// fall through into the real CLI and reach an os.Exit, killing the whole
	// test binary with nothing pointing back at this test. Assert the
	// precondition instead of debugging that later.
	if _, logFile, err := log.New(); err == nil {
		if logFile != nil {
			_ = logFile.Close()
		}
		t.Fatal("log.New() unexpectedly succeeded; start() would run the whole CLI inside the test binary")
	}

	utils.ErrCode = 0
	t.Cleanup(func() { utils.ErrCode = 0 })

	start(context.Background())

	if utils.ErrCode != 1 {
		t.Fatalf("expected utils.ErrCode = 1 on logger failure, got %d", utils.ErrCode)
	}
}

// A chown -R of the working directory is a destructive thing to do on the
// strength of a guess.
//
// It used to be gated on conf.Path being non-empty, which is true only once
// ValidateFlags has run -- an incidental signal, and one that turned `sudo
// keploy --version` into a recursive chown of the whole tree the moment the
// config default for path stopped being the empty string. Only the commands
// that can CREATE files under that path have anything to restore.
func TestWritesKeployFolder(t *testing.T) {
	// A real command tree, because argv cannot be read by eye: a root flag
	// that takes a separate value puts a non-flag word before the command.
	root := func() *cobra.Command {
		r := &cobra.Command{Use: "keploy"}
		r.PersistentFlags().String("storage-format", "yaml", "")
		r.PersistentFlags().Bool("debug", false, "")
		for _, n := range []string{"record", "test", "normalize", "templatize", "contract", "import", "export", "sanitize", "diff", "config", "login", "update"} {
			c := &cobra.Command{Use: n, Run: func(*cobra.Command, []string) {}}
			r.AddCommand(c)
		}
		mock := &cobra.Command{Use: "mock", Run: func(*cobra.Command, []string) {}}
		mock.AddCommand(&cobra.Command{Use: "record", Run: func(*cobra.Command, []string) {}})
		r.AddCommand(mock)
		return r
	}
	for _, c := range []struct {
		args []string
		want bool
	}{
		// A root flag with a separate value used to be read as the command.
		{[]string{"keploy", "--storage-format", "json", "record", "-c", "npm test"}, true},
		{[]string{"keploy", "--debug", "--storage-format", "json", "test"}, true},
		{[]string{"keploy", "--storage-format", "json", "config", "--generate"}, false},
		{[]string{"keploy", "mock", "record", "-c", "npm test"}, true},
		{[]string{"keploy", "record", "-c", "npm test"}, true},
		{[]string{"keploy", "test"}, true},
		{[]string{"keploy", "mock", "record", "-c", "npm test"}, true},
		{[]string{"keploy", "--debug", "record"}, true},
		{[]string{"keploy", "--version"}, false},
		{[]string{"keploy", "--help"}, false},
		{[]string{"keploy"}, false},
		{[]string{"keploy", "config", "--generate"}, false},
		{[]string{"keploy", "config", "defaults"}, false},
		{[]string{"keploy", "login"}, false},
		{[]string{"keploy", "update"}, false},
	} {
		if got := writesKeployFolder(root(), c.args[1:]); got != c.want {
			t.Errorf("writesKeployFolder(%v) = %v, want %v", c.args[1:], got, c.want)
		}
	}
}
