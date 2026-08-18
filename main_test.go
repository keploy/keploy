package main

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.keploy.io/server/v3/utils"
)

func TestStart_LoggerInitFailureSetsErrCode(t *testing.T) {
	tmpDir := t.TempDir()

	// Creating a directory with the log file's name causes os.OpenFile
	// (O_WRONLY|O_CREATE) in log.New() to fail with EISDIR on all platforms.
	err := os.Mkdir(filepath.Join(tmpDir, "keploy-logs.txt"), 0755)
	if err != nil {
		t.Fatalf("failed to create dummy directory: %v", err)
	}

	origWd, err := os.Getwd()
	if err != nil {
		t.Fatalf("failed to get current working directory: %v", err)
	}

	if err := os.Chdir(tmpDir); err != nil {
		t.Fatalf("failed to chdir to tmpDir: %v", err)
	}
	t.Cleanup(func() {
		_ = os.Chdir(origWd)
		utils.ErrCode = 0
	})

	utils.ErrCode = 0

	start(context.Background())

	if utils.ErrCode != 1 {
		t.Fatalf("expected utils.ErrCode = 1 on logger failure, got %d", utils.ErrCode)
	}
}
