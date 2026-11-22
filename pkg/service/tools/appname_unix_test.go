//go:build !windows

package tools

import (
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// A FIFO named keploy.yml is refused at once: reading it would wait for a
// writer for good.
func TestAddAppNameRefusesAFIFO(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keploy.yml")
	if err := syscall.Mkfifo(path, 0o644); err != nil {
		t.Skipf("no FIFOs here: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- AddAppName(path, "shop") }()
	select {
	case err := <-done:
		if err == nil || !strings.Contains(err.Error(), "not a regular file") {
			t.Errorf("AddAppName = %v, want a refusal that says it is not a regular file", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("AddAppName waited on a FIFO named keploy.yml")
	}
}
