package main

import (
	"context"
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
