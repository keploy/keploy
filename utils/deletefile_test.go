package utils

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

// TestDeleteFileIfExists pins the behaviour the rename in #4444 describes.
// The function was called DeleteFileIfNotExists while deleting the file when
// it DID exist (#4321); nothing exercised it, so the name and the code were
// free to disagree. These cases fail if the condition is ever inverted again.
func TestDeleteFileIfExists(t *testing.T) {
	logger := zap.NewNop()

	t.Run("removes a file that exists", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "keploy-logs.txt")
		if err := os.WriteFile(path, []byte("a log line\n"), 0o600); err != nil {
			t.Fatalf("failed to seed file: %v", err)
		}

		if err := DeleteFileIfExists(logger, path); err != nil {
			t.Fatalf("DeleteFileIfExists returned %v, want nil", err)
		}
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("file is still present after deletion (stat error: %v)", err)
		}
	})

	t.Run("is a no-op when the file is absent", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "never-created.txt")

		if err := DeleteFileIfExists(logger, path); err != nil {
			t.Fatalf("DeleteFileIfExists returned %v, want nil for a missing file", err)
		}
	})

	t.Run("returns the error when the path cannot be removed", func(t *testing.T) {
		// os.Remove refuses to delete a non-empty directory on every platform,
		// which is the portable way to reach the error branch without relying
		// on file permissions (root ignores those, and Windows models them
		// differently).
		dir := filepath.Join(t.TempDir(), "not-empty")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatalf("failed to create directory: %v", err)
		}
		if err := os.WriteFile(filepath.Join(dir, "child"), nil, 0o600); err != nil {
			t.Fatalf("failed to seed child file: %v", err)
		}

		if err := DeleteFileIfExists(logger, dir); err == nil {
			t.Error("DeleteFileIfExists returned nil for a non-removable path, want an error")
		}
	})
}
