//go:build !windows

package log

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The debug log's folder is, in keploy's agent container, the host's keploy
// folder, whose files come with the repository. A link at the debug file
// that leads out of that folder must not be followed: the file it leads to
// stays as it was, the link stays, and the error says why. A link that stays
// in the folder, and a plain file, keep working.
func TestOpenDebugFileALinkOutOfItsFolderIsRefused(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "keploy-host")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	outside := filepath.Join(base, "outside.txt")
	const kept = "a file of the user's\n"
	if err := os.WriteFile(outside, []byte(kept), 0o644); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct{ name, target string }{
		{"relative link out", filepath.Join("..", "outside.txt")},
		{"absolute link", outside},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(dir, "agent-debug.log")
			_ = os.Remove(path)
			if err := os.Symlink(tc.target, path); err != nil {
				t.Fatal(err)
			}
			f, err := OpenDebugFile(path)
			if err == nil {
				_, _ = f.WriteString("agent log\n")
				_ = f.Close()
				t.Fatalf("OpenDebugFile(%q -> %q) opened it; want it refused", path, tc.target)
			}
			if !strings.Contains(err.Error(), "agent-debug.log") {
				t.Errorf("the error %q does not name the file", err)
			}
			if got, _ := os.ReadFile(outside); string(got) != kept {
				t.Errorf("the file outside the folder is now %q; want it untouched %q", got, kept)
			}
			if fi, err := os.Lstat(path); err != nil || fi.Mode()&os.ModeSymlink == 0 {
				t.Errorf("the link was not kept as it was: %v, %v", fi, err)
			}
		})
	}

	t.Run("a link inside the folder and a plain file keep working", func(t *testing.T) {
		in := filepath.Join(dir, "real.log")
		if err := os.WriteFile(in, []byte("old\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		link := filepath.Join(dir, "agent-debug.log")
		_ = os.Remove(link)
		if err := os.Symlink("real.log", link); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{link, filepath.Join(dir, "fresh.log")} {
			f, err := OpenDebugFile(path)
			if err != nil {
				t.Fatalf("OpenDebugFile(%q): %v", path, err)
			}
			if f.Name() != path {
				t.Errorf("the file is named %q; want %q (RotateForScope takes its folder from it)", f.Name(), path)
			}
			if _, err := f.WriteString("new\n"); err != nil {
				t.Fatal(err)
			}
			_ = f.Close()
		}
		if got, _ := os.ReadFile(in); string(got) != "new\n" {
			t.Errorf("the file the inside link names is %q; want it truncated and written", got)
		}
	})
}
