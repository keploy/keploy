package mockdb

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

// linkTo makes a relative link at path to target, which need not exist.
func linkTo(t *testing.T, target, path string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	rel, err := filepath.Rel(filepath.Dir(path), target)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(rel, path); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
}

// requireWriteRefused checks err says the file is changed only inside its
// recording.
func requireWriteRefused(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "only inside its recording") {
		t.Fatalf("%s: %v, want it refused as outside the recording", what, err)
	}
}

// requireFileIs checks the file at path holds want.
func requireFileIs(t *testing.T, what, path, want string) {
	t.Helper()
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if string(got) != want {
		t.Fatalf("%s: %s holds %d bytes %.40q, want %q", what, path, len(got), got, want)
	}
}

// requireAbsent checks nothing is at path, a link included.
func requireAbsent(t *testing.T, what, path string) {
	t.Helper()
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("%s: %s is there (%v), want nothing", what, path, err)
	}
}

// requireIsLink checks path is still a link.
func requireIsLink(t *testing.T, what, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	if err != nil {
		t.Fatalf("%s: %v", what, err)
	}
	if info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("%s: %s is no longer a link", what, path)
	}
}

func mockExt(format string) string {
	if format == mockFormatGob {
		return "gob"
	}
	return format
}

// A record into a set whose mock file links out of the recording is refused,
// with why, and writes nothing there: not onto a file with something in it,
// not onto an empty one (a gob writer begins a stream in it), and not through
// a link to nothing (a create would make the file the link names). A set
// folder that links out is refused the same way.
func TestARecordWritesASetsMockFileOnlyInsideTheRecording(t *testing.T) {
	for _, format := range []string{string(yaml.FormatYAML), string(yaml.FormatJSON), mockFormatGob} {
		for _, shape := range []struct {
			name    string
			content *string
		}{
			{"a file with something in it", ptr("KEEP ME\n")},
			{"an empty file", ptr("")},
			{"nothing", nil},
		} {
			t.Run(format+" link to "+shape.name, func(t *testing.T) {
				ctx := context.Background()
				base := t.TempDir()
				root := filepath.Join(base, "keploy")
				out := filepath.Join(base, "outside.txt")
				if shape.content != nil {
					if err := os.WriteFile(out, []byte(*shape.content), 0o644); err != nil {
						t.Fatal(err)
					}
				}
				file := filepath.Join(root, "set", "mocks."+mockExt(format))
				linkTo(t, out, file)
				ys := clockStore(root, format)
				err := errors.Join(ys.InsertMock(ctx, genericNoiseMock("mock-0"), "set"), ys.Close())
				requireWriteRefused(t, "InsertMock", err)
				requireIsLink(t, "InsertMock", file)
				if shape.content == nil {
					requireAbsent(t, "InsertMock", out)
					return
				}
				requireFileIs(t, "InsertMock", out, *shape.content)
			})
		}
		t.Run(format+" set folder links out", func(t *testing.T) {
			ctx := context.Background()
			base := t.TempDir()
			root := filepath.Join(base, "keploy")
			out := filepath.Join(base, "outside")
			if err := os.MkdirAll(out, 0o755); err != nil {
				t.Fatal(err)
			}
			linkTo(t, out, filepath.Join(root, "set"))
			ys := clockStore(root, format)
			err := errors.Join(ys.InsertMock(ctx, genericNoiseMock("mock-0"), "set"), ys.Close())
			requireWriteRefused(t, "InsertMock", err)
			entries, rerr := os.ReadDir(out)
			if rerr != nil {
				t.Fatal(rerr)
			}
			if len(entries) != 0 {
				t.Fatalf("InsertMock wrote %s outside the recording", entries[0].Name())
			}
		})
	}
}

func ptr[T any](v T) *T { return &v }

// A record into a set whose mock file links to another set's file, inside the
// recording, goes on as it did.
func TestARecordFollowsALinkInsideTheRecording(t *testing.T) {
	ctx := context.Background()
	root := filepath.Join(t.TempDir(), "keploy")
	ys := clockStore(root, string(yaml.FormatYAML))
	if err := ys.InsertMock(ctx, genericNoiseMock("mock-0"), "other"); err != nil {
		t.Fatal(err)
	}
	linkTo(t, filepath.Join(root, "other", "mocks.yaml"), filepath.Join(root, "set", "mocks.yaml"))
	before, err := os.ReadFile(filepath.Join(root, "other", "mocks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ys.InsertMock(ctx, genericNoiseMock("mock-1"), "set"); err != nil {
		t.Fatal(err)
	}
	after, err := os.ReadFile(filepath.Join(root, "other", "mocks.yaml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(after) <= len(before) {
		t.Fatalf("the mock was not added through the link: %d bytes, then %d", len(before), len(after))
	}
}

// Clearing a set's mocks changes nothing outside the recording: a set folder
// that links out is refused, and the file it leads to is kept.
func TestClearingASetsMocksStaysInsideTheRecording(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root := filepath.Join(base, "keploy")
	out := filepath.Join(base, "outside")
	if err := os.MkdirAll(out, 0o755); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(out, "mocks.yaml")
	if err := os.WriteFile(kept, []byte("KEEP ME\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkTo(t, out, filepath.Join(root, "set"))
	ys := clockStore(root, string(yaml.FormatYAML))
	requireWriteRefused(t, "DeleteMocksForSet", ys.DeleteMocksForSet(ctx, "set"))
	requireFileIs(t, "DeleteMocksForSet", kept, "KEEP ME\n")
}

// Promoting a staged set stays inside the recording: a target file or folder
// that links out is refused and left as it was, and so is a staged folder
// that links out, whose file would be moved into the recording.
func TestPromotingAStagedSetStaysInsideTheRecording(t *testing.T) {
	stage := func(t *testing.T, ys *MockYaml) {
		t.Helper()
		if err := ys.InsertMock(context.Background(), genericNoiseMock("mock-0"), "staging"); err != nil {
			t.Fatal(err)
		}
	}
	t.Run("target file links out", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "keploy")
		out := filepath.Join(base, "outside.txt")
		if err := os.WriteFile(out, []byte("KEEP ME\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		ys := clockStore(root, string(yaml.FormatYAML))
		stage(t, ys)
		target := filepath.Join(root, "set", "mocks.yaml")
		linkTo(t, out, target)
		requireWriteRefused(t, "PromoteStagedSet", ys.PromoteStagedSet(context.Background(), "staging", "set"))
		requireIsLink(t, "PromoteStagedSet", target)
		requireFileIs(t, "PromoteStagedSet", out, "KEEP ME\n")
	})
	t.Run("target folder links out", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "keploy")
		out := filepath.Join(base, "outside")
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		ys := clockStore(root, string(yaml.FormatYAML))
		stage(t, ys)
		linkTo(t, out, filepath.Join(root, "set"))
		requireWriteRefused(t, "PromoteStagedSet", ys.PromoteStagedSet(context.Background(), "staging", "set"))
		requireAbsent(t, "PromoteStagedSet", filepath.Join(out, "mocks.yaml"))
	})
	t.Run("staged folder links out", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "keploy")
		out := filepath.Join(base, "outside")
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		kept := filepath.Join(out, "mocks.yaml")
		if err := os.WriteFile(kept, []byte("KEEP ME\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		linkTo(t, out, filepath.Join(root, "staging"))
		ys := clockStore(root, string(yaml.FormatYAML))
		requireWriteRefused(t, "PromoteStagedSet", ys.PromoteStagedSet(context.Background(), "staging", "set"))
		requireFileIs(t, "PromoteStagedSet", kept, "KEEP ME\n")
		requireAbsent(t, "PromoteStagedSet", filepath.Join(root, "set", "mocks.yaml"))
	})
}

// Discarding a staged set (a record clears one a crashed run left before it
// captures) removes it only inside the recording: through a keploy folder
// that links out of the project, a folder of that name where the link leads
// is kept, and the record is refused before it captures. A staged folder that
// is itself a link goes, not what it leads to; no keploy folder yet is
// nothing to discard.
func TestDiscardingAStagedSetStaysInsideTheRecording(t *testing.T) {
	const staging = "set" + yaml.MockStagingSuffix
	t.Run("keploy folder links out of the project", func(t *testing.T) {
		base := t.TempDir()
		out := filepath.Join(base, "outside")
		kept := filepath.Join(out, staging, "mocks.yaml")
		if err := os.MkdirAll(filepath.Dir(kept), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(kept, []byte("KEEP ME\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(base, "project", "keploy")
		linkTo(t, out, root)
		ys := clockStore(root, string(yaml.FormatYAML))
		requireWriteRefused(t, "DiscardStagedSet", ys.DiscardStagedSet(context.Background(), staging))
		requireFileIs(t, "DiscardStagedSet", kept, "KEEP ME\n")
	})
	t.Run("staged folder links out", func(t *testing.T) {
		base := t.TempDir()
		out := filepath.Join(base, "outside")
		kept := filepath.Join(out, "mocks.yaml")
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(kept, []byte("KEEP ME\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		root := filepath.Join(base, "keploy")
		linkTo(t, out, filepath.Join(root, staging))
		ys := clockStore(root, string(yaml.FormatYAML))
		if err := ys.DiscardStagedSet(context.Background(), staging); err != nil {
			t.Fatalf("DiscardStagedSet: %v", err)
		}
		requireAbsent(t, "DiscardStagedSet", filepath.Join(root, staging))
		requireFileIs(t, "DiscardStagedSet", kept, "KEEP ME\n")
	})
	t.Run("no keploy folder yet", func(t *testing.T) {
		ys := clockStore(filepath.Join(t.TempDir(), "keploy"), string(yaml.FormatYAML))
		if err := ys.DiscardStagedSet(context.Background(), staging); err != nil {
			t.Fatalf("DiscardStagedSet with no keploy folder: %v, want nothing to discard", err)
		}
	})
	t.Run("a staged set inside is removed", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "keploy")
		ys := clockStore(root, string(yaml.FormatYAML))
		if err := ys.InsertMock(context.Background(), genericNoiseMock("mock-0"), staging); err != nil {
			t.Fatal(err)
		}
		if err := ys.DiscardStagedSet(context.Background(), staging); err != nil {
			t.Fatalf("DiscardStagedSet: %v", err)
		}
		requireAbsent(t, "DiscardStagedSet", filepath.Join(root, staging))
	})
}

// The rewrites of a set's mock file stay inside the recording on their own:
// each is reached only after a read that refuses a link out, and the rule
// does not lean on that. A target that links out is refused where each one
// makes its change, and the file it leads to keeps what it held.
func TestAMockFileRewriteStaysInsideTheRecording(t *testing.T) {
	setup := func(t *testing.T, name string) (root, out, target string) {
		t.Helper()
		base := t.TempDir()
		root = filepath.Join(base, "keploy")
		out = filepath.Join(base, "outside.txt")
		if err := os.WriteFile(out, []byte("KEEP ME\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		target = filepath.Join(root, "set", name)
		linkTo(t, out, target)
		return root, out, target
	}
	t.Run("a text rewrite with mocks to keep", func(t *testing.T) {
		root, out, target := setup(t, "mocks.yaml")
		rw := newMockFileRewriter(zap.NewNop(), root, filepath.Join(root, "set"), "mocks", yaml.FormatYAML)
		defer rw.discard()
		if err := rw.open(); err != nil {
			t.Fatal(err)
		}
		requireWriteRefused(t, "commit", rw.commit())
		requireIsLink(t, "commit", target)
		requireFileIs(t, "commit", out, "KEEP ME\n")
	})
	t.Run("a text rewrite that keeps no mock", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "keploy")
		out := filepath.Join(base, "outside")
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		kept := filepath.Join(out, "mocks.yaml")
		if err := os.WriteFile(kept, []byte("KEEP ME\n"), 0o644); err != nil {
			t.Fatal(err)
		}
		linkTo(t, out, filepath.Join(root, "set"))
		rw := newMockFileRewriter(zap.NewNop(), root, filepath.Join(root, "set"), "mocks", yaml.FormatYAML)
		defer rw.discard()
		requireWriteRefused(t, "commit", rw.commit())
		requireFileIs(t, "commit", kept, "KEEP ME\n")
	})
	t.Run("a text rewrite in a folder that links out", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "keploy")
		out := filepath.Join(base, "outside")
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		linkTo(t, out, filepath.Join(root, "set"))
		rw := newMockFileRewriter(zap.NewNop(), root, filepath.Join(root, "set"), "mocks", yaml.FormatYAML)
		defer rw.discard()
		requireWriteRefused(t, "open", rw.open())
		if entries, err := os.ReadDir(out); err != nil || len(entries) != 0 {
			t.Fatalf("open left %v (%v) outside the recording", entries, err)
		}
	})
	t.Run("the incomplete document set aside", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "keploy")
		out := filepath.Join(base, "outside")
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		linkTo(t, out, filepath.Join(root, "set"))
		rw := newMockFileRewriter(zap.NewNop(), root, filepath.Join(root, "set"), "mocks", yaml.FormatYAML)
		rw.incomplete = []byte("half a mock")
		defer rw.discard()
		requireWriteRefused(t, "commit", rw.commit())
		if entries, err := os.ReadDir(out); err != nil || len(entries) != 0 {
			t.Fatalf("commit left %v (%v) outside the recording", entries, err)
		}
	})
	t.Run("a gob rewrite", func(t *testing.T) {
		root, out, target := setup(t, "mocks.gob")
		rw, err := newGobFileRewriter(root, target)
		if err != nil {
			t.Fatal(err)
		}
		defer rw.discard()
		requireWriteRefused(t, "commit", rw.commit())
		requireIsLink(t, "commit", target)
		requireFileIs(t, "commit", out, "KEEP ME\n")
	})
	t.Run("a gob rewrite in a folder that links out", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "keploy")
		out := filepath.Join(base, "outside")
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		linkTo(t, out, filepath.Join(root, "set"))
		_, err := newGobFileRewriter(root, filepath.Join(root, "set", "mocks.gob"))
		requireWriteRefused(t, "newGobFileRewriter", err)
		if entries, err := os.ReadDir(out); err != nil || len(entries) != 0 {
			t.Fatalf("the rewriter left %v (%v) outside the recording", entries, err)
		}
	})
	t.Run("a temp file beside a file in a folder that links out", func(t *testing.T) {
		base := t.TempDir()
		root := filepath.Join(base, "keploy")
		out := filepath.Join(base, "outside")
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		linkTo(t, out, filepath.Join(root, "set"))
		_, err := createTempBeside(root, filepath.Join(root, "set", "mocks.gob"), ".append.*.tmp")
		requireWriteRefused(t, "createTempBeside", err)
		if entries, err := os.ReadDir(out); err != nil || len(entries) != 0 {
			t.Fatalf("createTempBeside left %v (%v) outside the recording", entries, err)
		}
	})
	t.Run("what a mock file's mode and size are read from", func(t *testing.T) {
		root, _, target := setup(t, "mocks.gob")
		_, err := resolveMockFileMode(root, target)
		requireWriteRefused(t, "resolveMockFileMode", err)
		_, err = gobFileHoldsData(root, target)
		requireWriteRefused(t, "gobFileHoldsData", err)
	})
}

// A link cannot be followed at the final step of a gob record either: one that
// takes the set's file's place while the writer carries the file's mocks over,
// as a process beside keploy could, is refused as the new stream is put in
// place, and left as it is.
func TestALinkThatAppearsDuringACarryOverIsRefused(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root := filepath.Join(base, "keploy")
	out := filepath.Join(base, "outside.txt")
	if err := os.WriteFile(out, []byte("KEEP ME\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	ys := clockStore(root, mockFormatGob)
	if err := ys.InsertMock(ctx, genericNoiseMock("mock-0"), "set"); err != nil {
		t.Fatal(err)
	}
	if err := ys.Close(); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "set", "mocks.gob")
	planted := false
	saved := gobFileOf
	t.Cleanup(func() { gobFileOf = saved })
	gobFileOf = func(f *os.File) io.Writer {
		return writerFunc(func(b []byte) (int, error) {
			if !planted {
				planted = true
				if err := os.Remove(file); err != nil {
					t.Error(err)
				}
				linkTo(t, out, file)
			}
			return f.Write(b)
		})
	}
	ys = clockStore(root, mockFormatGob)
	err := errors.Join(ys.InsertMock(ctx, genericNoiseMock("mock-1"), "set"), ys.Close())
	if !planted {
		t.Fatal("the writer wrote nothing, so the link never took the file's place")
	}
	requireWriteRefused(t, "InsertMock", err)
	requireIsLink(t, "InsertMock", file)
	requireFileIs(t, "InsertMock", out, "KEEP ME\n")
}
