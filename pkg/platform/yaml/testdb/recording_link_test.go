package testdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A set's test case is read only inside the recording: one whose file links
// out of it is refused, with why, by every read, and one that links to a
// file inside it is read.
func TestATestCaseIsReadOnlyInsideTheRecording(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	root := filepath.Join(base, "keploy")
	db := New(zap.NewNop(), root)
	for i, body := range []string{"one", "SECRET-OUTSIDE"} {
		tc := &models.TestCase{Version: models.GetVersion(), Kind: models.HTTP, Name: []string{"test-1", "test-2"}[i],
			HTTPReq:  models.HTTPReq{Method: "GET", URL: "http://127.0.0.1:9/x"},
			HTTPResp: models.HTTPResp{StatusCode: 200, Body: body}}
		for _, set := range []string{"set", "other"} {
			if err := db.InsertTestCase(ctx, tc, set, false); err != nil {
				t.Fatal(err)
			}
		}
	}
	file := filepath.Join(root, "set", "tests", "test-2.yaml")
	out := filepath.Join(base, "outside-test-2.yaml")
	if err := os.Rename(file, out); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "..", "outside-test-2.yaml"), file); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
	refused := func(what string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), fmt.Sprintf("%q is read only inside its recording %q", filepath.Join("set", "tests", "test-2.yaml"), root)) {
			t.Fatalf("%s: %v, want %q refused as read only inside its recording", what, err, file)
		}
	}
	tcs, err := db.GetTestCases(ctx, "set")
	refused("GetTestCases", err)
	if len(tcs) != 0 {
		t.Fatalf("GetTestCases read %d test cases", len(tcs))
	}
	_, err = db.GetTestCase(ctx, "set", "test-2")
	refused("GetTestCase", err)
	_, err = db.TestCaseFiles(ctx, "set")
	refused("TestCaseFiles", err)
	err = db.UpdateAssertions(ctx, "test-2", "set", map[models.AssertionType]interface{}{models.StatusCode: 200})
	refused("UpdateAssertions", err)

	// A link inside the recording, to the other set's test case, is read.
	other := filepath.Join(root, "other", "tests", "test-2.yaml")
	if err := os.Remove(file); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "other", "tests", "test-2.yaml"), file); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(other); err != nil {
		t.Fatal(err)
	}
	tcs, err = db.GetTestCases(ctx, "set")
	if err != nil || len(tcs) != 2 {
		t.Fatalf("GetTestCases with a link inside: %d, %v", len(tcs), err)
	}
	tc, err := db.GetTestCase(ctx, "set", "test-2")
	if err != nil || tc.HTTPResp.Body != "SECRET-OUTSIDE" {
		t.Fatalf("GetTestCase with a link inside: %v", err)
	}
	files, err := db.TestCaseFiles(ctx, "set")
	if err != nil || len(files) != 2 {
		t.Fatalf("TestCaseFiles with a link inside: %d, %v", len(files), err)
	}
}
