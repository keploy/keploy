//go:build !windows

package log

import (
	"fmt"
	"os"
	"path/filepath"
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
			// Said plainly: which file, the link, and where it leads, not
			// in os.Root's words.
			if want := fmt.Sprintf("the debug log %q is written only inside %q: it is a link that leads out of it, to %q; replace the link with what it leads to", "agent-debug.log", dir, outside); err.Error() != want {
				t.Errorf("the error is %q; want %q", err, want)
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
		// The earlier log is longer than what is written now, so a file
		// that was overwritten but not truncated keeps its tail and fails.
		const earlier = "an earlier agent run's debug log, longer than the new one\n"
		in := filepath.Join(dir, "real.log")
		if err := os.WriteFile(in, []byte(earlier), 0o644); err != nil {
			t.Fatal(err)
		}
		plain := filepath.Join(dir, "plain.log")
		if err := os.WriteFile(plain, []byte(earlier), 0o644); err != nil {
			t.Fatal(err)
		}
		fresh := filepath.Join(dir, "fresh.log")
		link := filepath.Join(dir, "agent-debug.log")
		_ = os.Remove(link)
		if err := os.Symlink("real.log", link); err != nil {
			t.Fatal(err)
		}
		for _, path := range []string{link, plain, fresh} {
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
		for _, path := range []string{in, plain, fresh} {
			if got, err := os.ReadFile(path); err != nil || string(got) != "new\n" {
				t.Errorf("%s is %q (%v); want it truncated and written as %q", path, got, err, "new\n")
			}
		}
	})
}
