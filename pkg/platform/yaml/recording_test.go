package yaml

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// symlinkOrSkip makes link point at target, or skips the test where this
// user cannot make links (Windows without the privilege).
func symlinkOrSkip(t *testing.T, target, link string) {
	t.Helper()
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
}

func writeFileT(t *testing.T, path, text string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(text), 0o644); err != nil {
		t.Fatal(err)
	}
}

// requireRefused checks err refuses path, a file of the recording at root,
// with why (naming it relative to root, and root), and data is nothing.
func requireRefused(t *testing.T, data []byte, err error, root, path string) {
	t.Helper()
	if err == nil {
		t.Fatalf("read %q (%q), want it refused", path, data)
	}
	dir := root
	if dir == "" {
		dir = "."
	}
	rel, rerr := filepath.Rel(dir, path)
	if rerr != nil {
		t.Fatal(rerr)
	}
	if want := fmt.Sprintf("%q is read only inside its recording %q", rel, dir); !strings.Contains(err.Error(), want) {
		t.Fatalf("error %q does not say %s", err, want)
	}
	if errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("error %q reads as a missing file, which a caller passes over", err)
	}
	if len(data) != 0 {
		t.Fatalf("read %q", data)
	}
}

// recordingWithLinks is a recording at root with a test set "set" whose
// tests/ holds a file and links of each kind, and a secret beside the
// recording.
func recordingWithLinks(t *testing.T) (root, tests string) {
	t.Helper()
	base := t.TempDir()
	root = filepath.Join(base, "keploy")
	tests = filepath.Join(root, "set", "tests")
	writeFileT(t, filepath.Join(tests, "test-1.yaml"), "kind: Http\nname: test-1\n")
	writeFileT(t, filepath.Join(root, "other", "tests", "test-9.yaml"), "kind: Http\nname: test-9\n")
	writeFileT(t, filepath.Join(base, "secret.txt"), "SECRET-OUTSIDE\n")
	symlinkOrSkip(t, "test-1.yaml", filepath.Join(tests, "same-dir.yaml"))
	symlinkOrSkip(t, filepath.Join("..", "..", "other", "tests", "test-9.yaml"), filepath.Join(tests, "other-set.yaml"))
	symlinkOrSkip(t, filepath.Join("..", "..", "..", "secret.txt"), filepath.Join(tests, "relative-out.yaml"))
	symlinkOrSkip(t, filepath.Join(base, "secret.txt"), filepath.Join(tests, "absolute-out.yaml"))
	symlinkOrSkip(t, filepath.Join(root, "set", "tests", "test-1.yaml"), filepath.Join(tests, "absolute-in.yaml"))
	symlinkOrSkip(t, filepath.Join(base, "secret.txt"), filepath.Join(root, "set", "mocks.yaml"))
	return root, tests
}

// A recording's own file is read inside the recording: a link that stays in
// it is followed, and one that leads out, relative or absolute, is refused
// with why.
func TestReadRecordingFileReadsOnlyInsideTheRecording(t *testing.T) {
	ctx, log := context.Background(), zap.NewNop()
	root, tests := recordingWithLinks(t)
	for _, c := range []struct{ name, want string }{
		{"test-1", "name: test-1\n"},
		{"same-dir", "name: test-1\n"},
		{"other-set", "name: test-9\n"},
	} {
		data, err := ReadRecordingFileF(ctx, log, root, tests, c.name, FormatYAML)
		if err != nil || !strings.Contains(string(data), c.want) {
			t.Fatalf("%s: read %q, %v; want %q", c.name, data, err, c.want)
		}
	}
	for _, name := range []string{"relative-out", "absolute-out", "absolute-in"} {
		data, err := ReadRecordingFileF(ctx, log, root, tests, name, FormatYAML)
		requireRefused(t, data, err, root, filepath.Join(tests, name+".yaml"))
	}
	// A directory that is not under root: refused, and nothing outside root
	// is opened.
	out := filepath.Join(root, "..")
	data, err := ReadRecordingFileF(ctx, log, root, out, "secret", FormatYAML)
	requireRefused(t, data, err, root, filepath.Join(out, "secret.yaml"))
	if !strings.Contains(err.Error(), "path escapes from parent") {
		t.Fatalf("error %q does not say the path leads out of the recording", err)
	}
	// A path that is not under root at all (a relative one, for a root that
	// is absolute) is refused, naming it, and is not a missing file.
	_, err = ReadRecordingFileF(ctx, log, root, filepath.Join("set", "tests"), "test-1", FormatYAML)
	if want := fmt.Sprintf("%q is read only inside its recording %q", filepath.Join("set", "tests", "test-1.yaml"), root); err == nil || !strings.Contains(err.Error(), want) || errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("a path not under the recording: %v, want it refused (%s)", err, want)
	}
	// A file that is not there reads as not there.
	if _, err := ReadRecordingFileF(ctx, log, root, tests, "test-404", FormatYAML); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: %v, want fs.ErrNotExist", err)
	}
	// The unconfined reader, for contrast, follows the link out.
	if data, err := ReadFileF(ctx, log, tests, "relative-out", FormatYAML); err != nil || !strings.Contains(string(data), "SECRET-OUTSIDE") {
		t.Fatalf("ReadFileF: %q, %v", data, err)
	}
}

// An empty root is the working directory: a relative path is read in it,
// and a link out of it is refused.
func TestReadRecordingFileEmptyRootIsTheWorkingDirectory(t *testing.T) {
	root, _ := recordingWithLinks(t)
	t.Chdir(root)
	ctx, log := context.Background(), zap.NewNop()
	tests := filepath.Join("set", "tests")
	if data, err := ReadRecordingFileF(ctx, log, "", tests, "other-set", FormatYAML); err != nil || !strings.Contains(string(data), "name: test-9") {
		t.Fatalf("read %q, %v", data, err)
	}
	data, err := ReadRecordingFileF(ctx, log, "", tests, "relative-out", FormatYAML)
	requireRefused(t, data, err, "", filepath.Join(tests, "relative-out.yaml"))
}

// ReadRecordingFileAny passes to the other format only when the preferred
// one's file is not there: a preferred file that links out is refused, not
// passed over.
func TestReadRecordingFileAnyRefusesALinkOutRatherThanPassingIt(t *testing.T) {
	ctx, log := context.Background(), zap.NewNop()
	root, tests := recordingWithLinks(t)
	writeFileT(t, filepath.Join(tests, "only-json.json"), `{"kind":"Http"}`)
	data, f, err := ReadRecordingFileAny(ctx, log, root, tests, "only-json", FormatYAML)
	if err != nil || f != FormatJSON || string(data) != `{"kind":"Http"}` {
		t.Fatalf("read %q as %q, %v", data, f, err)
	}
	writeFileT(t, filepath.Join(tests, "relative-out.json"), `{"kind":"Http"}`)
	data, _, err = ReadRecordingFileAny(ctx, log, root, tests, "relative-out", FormatYAML)
	requireRefused(t, data, err, root, filepath.Join(tests, "relative-out.yaml"))
	if _, _, err := ReadRecordingFileAny(ctx, log, root, tests, "test-404", FormatYAML); !errors.Is(err, fs.ErrNotExist) {
		t.Fatalf("missing file: %v, want fs.ErrNotExist", err)
	}
}

// The mock readers open a recording's mock file inside the recording too.
func TestRecordingMockReaderOpensOnlyInsideTheRecording(t *testing.T) {
	ctx, log := context.Background(), zap.NewNop()
	root, tests := recordingWithLinks(t)
	set := filepath.Join(root, "set")
	for _, open := range []struct {
		name string
		fn   func(dir, name string) (*MockReader, error)
	}{
		{"F", func(dir, name string) (*MockReader, error) {
			return NewRecordingMockReaderF(ctx, log, root, dir, name, FormatYAML)
		}},
		{"Any", func(dir, name string) (*MockReader, error) {
			return NewRecordingMockReaderAny(ctx, log, root, dir, name, FormatYAML)
		}},
	} {
		r, err := open.fn(set, "mocks")
		if r != nil {
			_ = r.Close()
		}
		requireRefused(t, nil, err, root, filepath.Join(set, "mocks.yaml"))
		r, err = open.fn(tests, "other-set")
		if err != nil {
			t.Fatalf("%s: a link inside the recording: %v", open.name, err)
		}
		doc, err := r.ReadNextDocument()
		_ = r.Close()
		if (err != nil && err != io.EOF) || !strings.Contains(string(doc), "name: test-9") {
			t.Fatalf("%s: read %q, %v", open.name, doc, err)
		}
		if r, err := open.fn(tests, "test-404"); !errors.Is(err, fs.ErrNotExist) {
			if r != nil {
				_ = r.Close()
			}
			t.Fatalf("%s: missing file: %v, want fs.ErrNotExist", open.name, err)
		}
	}
	// Any passes to the other format's file when the preferred one is not
	// there.
	writeFileT(t, filepath.Join(tests, "only-json.json"), `{"kind":"Http"}`+"\n")
	r, err := NewRecordingMockReaderAny(ctx, log, root, tests, "only-json", FormatYAML)
	if err != nil {
		t.Fatalf("Any: the other format's file: %v", err)
	}
	_ = r.Close()
	writeFileT(t, filepath.Join(set, "mocks.json"), "{}\n")
	r, err = NewRecordingMockReaderAny(ctx, log, root, set, "mocks", FormatYAML)
	if r != nil {
		_ = r.Close()
	}
	requireRefused(t, nil, err, root, filepath.Join(set, "mocks.yaml"))
}
