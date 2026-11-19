//go:build unix

package utils

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// The home folder stops the search when the folder is reached by another
// path than $HOME's, as through a symlinked /home.
func TestAppNameStopsAtAHomeFolderReachedByAnotherPath(t *testing.T) {
	home := newDotfilesHome(t, t.TempDir(), "someone")
	link := filepath.Join(t.TempDir(), "home-link")
	if err := os.Symlink(home, link); err != nil {
		t.Fatal(err)
	}
	setHome(t, link)
	wantAppName(t, filepath.Join(home, "proj", "a"), "a", AppNameFromDir)
	setHome(t, home)
	wantAppName(t, filepath.Join(link, "proj", "a"), "a", AppNameFromDir)
}

// A config that is a FIFO is not opened: the open would wait for a writer
// for good. The repository is named after its folder.
func TestAppNameForAConfigThatIsAFIFO(t *testing.T) {
	top := filepath.Join(t.TempDir(), "orders")
	if err := os.MkdirAll(filepath.Join(top, ".git"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := syscall.Mkfifo(filepath.Join(top, ".git", "config"), 0o644); err != nil {
		t.Fatal(err)
	}
	done := make(chan struct{})
	go func() {
		defer close(done)
		wantAppName(t, top, "orders", AppNameFromRepo)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		// Unblock the open, so the goroutine ends with the test.
		if f, err := os.OpenFile(filepath.Join(top, ".git", "config"), os.O_WRONLY, 0); err == nil {
			_ = f.Close()
		}
		t.Fatal("AppNameFor opened a FIFO named config and waited on it")
	}
}
