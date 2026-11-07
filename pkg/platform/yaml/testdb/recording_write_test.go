package testdb

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
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

func requireWriteRefused(t *testing.T, what string, err error) {
	t.Helper()
	if err == nil || !strings.Contains(err.Error(), "only inside its recording") {
		t.Fatalf("%s: %v, want it refused as outside the recording", what, err)
	}
}

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

func requireEmptyDir(t *testing.T, what, dir string) {
	t.Helper()
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("%s wrote %s outside the recording", what, entries[0].Name())
	}
}

func simpleCase(name string) *models.TestCase {
	return &models.TestCase{Version: models.GetVersion(), Kind: models.HTTP, Name: name,
		HTTPReq:  models.HTTPReq{Method: "GET", URL: "http://127.0.0.1:9/x"},
		HTTPResp: models.HTTPResp{StatusCode: 200, Body: "ok"}}
}

// A record into a test set whose folders lead out of the recording is
// refused, with why, and creates nothing there.
func TestARecordWritesATestCaseOnlyInsideTheRecording(t *testing.T) {
	for _, c := range []struct{ name, link string }{
		{"the set folder links out", "set"},
		{"the tests folder links out", filepath.Join("set", "tests")},
	} {
		for _, named := range []bool{true, false} {
			t.Run(c.name, func(t *testing.T) {
				base := t.TempDir()
				root := filepath.Join(base, "keploy")
				out := filepath.Join(base, "outside")
				if err := os.MkdirAll(out, 0o755); err != nil {
					t.Fatal(err)
				}
				linkTo(t, out, filepath.Join(root, c.link))
				tc := simpleCase("test-1")
				if !named {
					tc.Name = ""
				}
				db := New(zap.NewNop(), root)
				requireWriteRefused(t, "InsertTestCase", db.InsertTestCase(context.Background(), tc, "set", false))
				requireEmptyDir(t, "InsertTestCase", out)
			})
		}
	}
}

// A write over a test case whose file links out of the recording is refused,
// with why: the link and the file it leads to stay as they were. A link to a
// file inside the recording is written as it was.
func TestAWriteOverATestCaseStaysInsideTheRecording(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root := filepath.Join(base, "keploy")
	out := filepath.Join(base, "outside.yaml")
	if err := os.WriteFile(out, []byte("KEEP ME\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(root, "set", "tests", "test-1.yaml")
	linkTo(t, out, file)
	db := New(zap.NewNop(), root)
	requireWriteRefused(t, "UpdateTestCase", db.UpdateTestCase(ctx, simpleCase("test-1"), "set", false))
	requireFileIs(t, "UpdateTestCase", out, "KEEP ME\n")
	if info, err := os.Lstat(file); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("the link is gone: %v, %v", info, err)
	}

	if err := db.InsertTestCase(ctx, simpleCase("test-2"), "other", false); err != nil {
		t.Fatal(err)
	}
	linkTo(t, filepath.Join(root, "other", "tests", "test-2.yaml"), filepath.Join(root, "set", "tests", "test-2.yaml"))
	if err := db.UpdateTestCase(ctx, simpleCase("test-2"), "set", false); err != nil {
		t.Fatalf("a link inside the recording: %v", err)
	}
}

// A test case's assets are written only inside the recording: a request body,
// a large form value or a form file whose place links out is refused, with
// why, and the file it leads to keeps what it held.
func TestATestCasesAssetsAreWrittenOnlyInsideTheRecording(t *testing.T) {
	large := strings.Repeat("x", LargeBodyThreshold+1)
	upload := func(t *testing.T) string {
		t.Helper()
		p := filepath.Join(t.TempDir(), "upload.txt")
		if err := os.WriteFile(p, []byte("uploaded"), 0o644); err != nil {
			t.Fatal(err)
		}
		return p
	}
	for _, c := range []struct {
		name  string
		link  string
		apply func(t *testing.T, tc *models.TestCase)
	}{
		{"a body", filepath.Join("test-1", "body.txt"), func(_ *testing.T, tc *models.TestCase) { tc.HTTPReq.Body = large }},
		{"a large form value", filepath.Join("test-1", "field", "value_0.txt"), func(_ *testing.T, tc *models.TestCase) {
			tc.HTTPReq.Form = []models.FormData{{Key: "field", Values: []string{large}}}
		}},
		{"a form file from a temp file", filepath.Join("test-1", "field", "a.txt"), func(t *testing.T, tc *models.TestCase) {
			tc.HTTPReq.Form = []models.FormData{{Key: "field", FileNames: []string{"a.txt"}, Paths: []string{upload(t)}}}
		}},
		{"a form file sent with the test case", filepath.Join("test-1", "field", "a.txt"), func(_ *testing.T, tc *models.TestCase) {
			tc.HTTPReq.Form = []models.FormData{{Key: "field", FileNames: []string{"a.txt"}, FileContents: [][]byte{[]byte("sent")}}}
		}},
		{"a form file in its values", filepath.Join("test-1", "field", "a.txt"), func(_ *testing.T, tc *models.TestCase) {
			tc.HTTPReq.Form = []models.FormData{{Key: "field", FileNames: []string{"a.txt"}, Values: []string{"value"}}}
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "keploy")
			out := filepath.Join(base, "outside.txt")
			if err := os.WriteFile(out, []byte("KEEP ME\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			linkTo(t, out, filepath.Join(root, "set", "assets", c.link))
			tc := simpleCase("test-1")
			c.apply(t, tc)
			db := New(zap.NewNop(), root)
			requireWriteRefused(t, "InsertTestCase", db.InsertTestCase(context.Background(), tc, "set", false))
			requireFileIs(t, "InsertTestCase", out, "KEEP ME\n")
		})
		t.Run(c.name+" under a folder that links out", func(t *testing.T) {
			base := t.TempDir()
			root := filepath.Join(base, "keploy")
			out := filepath.Join(base, "outside")
			if err := os.MkdirAll(out, 0o755); err != nil {
				t.Fatal(err)
			}
			linkTo(t, out, filepath.Join(root, "set", "assets"))
			tc := simpleCase("test-1")
			c.apply(t, tc)
			db := New(zap.NewNop(), root)
			requireWriteRefused(t, "InsertTestCase", db.InsertTestCase(context.Background(), tc, "set", false))
			requireEmptyDir(t, "InsertTestCase", out)
		})
	}
}

// A group of files replaced together stays inside the recording: a target
// that links out is refused before anything is written, and what it leads to
// is neither changed nor copied beside it.
func TestReplacedFilesStayInsideTheRecording(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root := filepath.Join(base, "keploy")
	out := filepath.Join(base, "outside.yaml")
	if err := os.WriteFile(out, []byte("SECRET-OUTSIDE\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	db := New(zap.NewNop(), root)
	if err := db.InsertTestCase(ctx, simpleCase("test-1"), "set", false); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(root, "set", "tests")
	good := filepath.Join(dir, "test-1.yaml")
	raw, err := os.ReadFile(good)
	if err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(dir, "test-2.yaml")
	linkTo(t, out, bad)
	err = db.ReplaceFiles(ctx, []yaml.File{{Path: good, Data: []byte("new")}, {Path: bad, Data: []byte("new")}})
	requireWriteRefused(t, "ReplaceFiles", err)
	// Refused as it was read: a copy of what the link leads to would otherwise
	// have been written beside it for a moment.
	if !strings.Contains(err.Error(), "so no file was changed") {
		t.Fatalf("ReplaceFiles was not refused before it changed a file: %v", err)
	}
	requireFileIs(t, "ReplaceFiles", out, "SECRET-OUTSIDE\n")
	requireFileIs(t, "ReplaceFiles", good, string(raw))
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 2 {
		t.Fatalf("ReplaceFiles left %d files in the set, want the 2 it found", len(entries))
	}
}

// Deleting a test case or a part of a set changes nothing outside the
// recording: a path through a link out is refused, and what it leads to is
// kept.
func TestDeletingStaysInsideTheRecording(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root := filepath.Join(base, "keploy")
	out := filepath.Join(base, "outside")
	if err := os.MkdirAll(filepath.Join(out, "tests"), 0o755); err != nil {
		t.Fatal(err)
	}
	kept := filepath.Join(out, "tests", "test-1.yaml")
	if err := os.WriteFile(kept, []byte("KEEP ME\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	linkTo(t, out, filepath.Join(root, "set"))
	db := New(zap.NewNop(), root)
	requireWriteRefused(t, "DeleteTests", db.DeleteTests(ctx, "set", []string{"test-1"}))
	requireFileIs(t, "DeleteTests", kept, "KEEP ME\n")
	requireWriteRefused(t, "DeleteTestSet", db.DeleteTestSet(ctx, filepath.Join("set", "tests")))
	requireFileIs(t, "DeleteTestSet", kept, "KEEP ME\n")
	if _, err := os.Stat(kept); errors.Is(err, os.ErrNotExist) {
		t.Fatal("the file outside the recording was deleted")
	}
}
