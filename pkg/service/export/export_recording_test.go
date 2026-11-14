package export

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.uber.org/zap"
)

// What is exported is shared, so a test case is read only inside the
// recording: one whose file links out of it is left out of the collection,
// and one that links to a file inside it is exported.
func TestExportReadsATestCaseOnlyInsideTheRecording(t *testing.T) {
	ctx := context.Background()
	base := t.TempDir()
	t.Chdir(base)
	root := filepath.Join(base, "keploy")
	db := testdb.New(zap.NewNop(), root)
	insert := func(name, set, url string) {
		t.Helper()
		tc := &models.TestCase{Version: models.GetVersion(), Kind: models.HTTP, Name: name,
			HTTPReq:  models.HTTPReq{Method: "GET", URL: url},
			HTTPResp: models.HTTPResp{StatusCode: 200, Body: "ok"}}
		if err := db.InsertTestCase(ctx, tc, set, false); err != nil {
			t.Fatal(err)
		}
	}
	insert("test-1", "set", "http://127.0.0.1:9/plain")
	insert("test-2", "set", "http://127.0.0.1:9/SECRET-OUTSIDE")
	insert("test-3", "other", "http://127.0.0.1:9/linked-inside")
	file := filepath.Join(root, "set", "tests", "test-2.yaml")
	if err := os.Rename(file, filepath.Join(base, "outside.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "..", "outside.yaml"), file); err != nil {
		t.Skipf("cannot make a symbolic link here: %v", err)
	}
	if err := os.Symlink(filepath.Join("..", "..", "other", "tests", "test-3.yaml"), filepath.Join(root, "set", "tests", "test-4.yaml")); err != nil {
		t.Fatal(err)
	}
	if err := Export(ctx, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(filepath.Join(base, "output.json"))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(out), "SECRET-OUTSIDE") {
		t.Fatal("the collection holds a test case read from outside the recording")
	}
	for _, want := range []string{"plain", "linked-inside"} {
		if !strings.Contains(string(out), want) {
			t.Fatalf("the collection has no %q:\n%.600s", want, out)
		}
	}
}
