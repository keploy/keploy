package utils

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// answer is a Getwd that says dir, as macOS's does of a removed directory.
func answer(dir string) func() (string, error) {
	return func() (string, error) { return dir, nil }
}

// removeWorkingDirectory leaves the test in a directory that was removed, and
// returns the path it had, which macOS's Getwd goes on answering.
func removeWorkingDirectory(t *testing.T) string {
	t.Helper()
	gone := filepath.Join(t.TempDir(), "app")
	if err := os.Mkdir(gone, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Chdir(gone)
	if err := os.Remove(gone); err != nil {
		t.Skipf("this system keeps a working directory from being removed: %v", err)
	}
	return gone
}

// wantGone fails unless err says, in the words every platform gives, that the
// working directory is gone and what to do about it.
func wantGone(t *testing.T, err error) {
	t.Helper()
	for _, want := range []string{"the working directory is gone: ", "; cd into a directory that exists and run keploy again"} {
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Fatalf("err = %v, want it to say %q", err, want)
		}
	}
}

func TestGetLastDirectory(t *testing.T) {
	t.Run("the directory it runs in", func(t *testing.T) {
		dir := filepath.Join(t.TempDir(), "app")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		got, err := GetLastDirectory()
		if err != nil || got != "app" {
			t.Fatalf("GetLastDirectory() = %q, %v, want app", got, err)
		}
	})

	t.Run("Getwd's own refusal is the one it gives", func(t *testing.T) {
		refused := errors.New("getwd refused")
		got, err := lastDirectory(func() (string, error) { return "", refused })
		if !errors.Is(err, refused) || strings.Contains(err.Error(), "gone") {
			t.Fatalf("lastDirectory() = %q, %v, want %v", got, err, refused)
		}
	})

	t.Run("a link it was reached through keeps its name", func(t *testing.T) {
		base := t.TempDir()
		dir := filepath.Join(base, "real")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(base, "link")
		if err := os.Symlink(dir, link); err != nil {
			t.Skipf("this system makes no link here: %v", err)
		}
		t.Chdir(dir)
		got, err := lastDirectory(answer(link))
		if err != nil || got != "link" {
			t.Fatalf("lastDirectory() = %q, %v, want link", got, err)
		}
	})

	t.Run("a directory moved away is where it is now", func(t *testing.T) {
		base := t.TempDir()
		before, after := filepath.Join(base, "before"), filepath.Join(base, "after")
		if err := os.Mkdir(before, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(before)
		if err := os.Rename(before, after); err != nil {
			t.Skipf("this system keeps a working directory from moving: %v", err)
		}
		got, err := GetLastDirectory()
		if err != nil || got != "after" {
			t.Fatalf("GetLastDirectory() = %q, %v, want after", got, err)
		}
	})

	t.Run("a directory removed is refused, $PWD or not", func(t *testing.T) {
		removeWorkingDirectory(t)
		_, err := GetLastDirectory()
		wantGone(t, err)
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("err = %v with $PWD on a removed directory, want it to be os.ErrNotExist", err)
		}
		t.Setenv("PWD", "")
		_, err = GetLastDirectory()
		wantGone(t, err)
		if !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("err = %v with no $PWD, want it to be os.ErrNotExist", err)
		}
	})

	t.Run("a new directory at the old path is not the one it runs in", func(t *testing.T) {
		gone := removeWorkingDirectory(t)
		if err := os.Mkdir(gone, 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := GetLastDirectory()
		wantGone(t, err)
	})

	// What macOS's Getwd answers, which no real directory makes Linux's answer:
	// there Getwd itself fails, and the rows above end before the check.
	t.Run("Getwd's answer leads nowhere", func(t *testing.T) {
		gone := removeWorkingDirectory(t)
		_, err := lastDirectory(answer(gone))
		wantGone(t, err)
		if !errors.Is(err, os.ErrNotExist) || !strings.Contains(err.Error(), gone) {
			t.Fatalf("err = %v, want os.ErrNotExist on %s", err, gone)
		}
	})
	t.Run("Getwd's answer leads to another directory", func(t *testing.T) {
		gone := removeWorkingDirectory(t)
		if err := os.Mkdir(gone, 0o755); err != nil {
			t.Fatal(err)
		}
		_, err := lastDirectory(answer(gone))
		wantGone(t, err)
		if !strings.Contains(err.Error(), gone+" is another directory now") {
			t.Fatalf("err = %v, want it to say %s is another directory now", err, gone)
		}
	})

	t.Run("a path this user may not walk is not a directory that is gone", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root walks every path")
		}
		private := t.TempDir()
		dir := filepath.Join(private, "app")
		if err := os.Mkdir(dir, 0o755); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		if err := os.Chmod(private, 0); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = os.Chmod(private, 0o755) })
		if _, err := os.Stat(dir); !os.IsPermission(err) {
			t.Skipf("this system lets this user walk the path: %v", err)
		}
		got, err := lastDirectory(answer(dir))
		if err != nil || got != "app" {
			t.Fatalf("lastDirectory() = %q, %v, want app", got, err)
		}
	})
}
