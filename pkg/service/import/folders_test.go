package postmanimport

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// Two Postman folders may share a name. Their requests were both numbered from
// test-1 in the one test set of that name, so the second folder overwrote the
// first's tests and the import reported success.
func TestImportKeepsTheTestsOfFoldersThatShareAName(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)

	folder := func(name, req string) string {
		return `{"name":"` + name + `","item":[{"name":"` + req + `","request":{"method":"GET","url":"http://example.com/` + req + `"},` +
			`"response":[{"name":"ok","code":200,"status":"OK","body":"` + req + `","header":[]}]}]}`
	}
	collection := `{"info":{"name":"c"},"item":[` +
		folder("Users", "first") + `,` + folder("Orders", "other") + `,` + folder("Users", "second") + `]}`
	path := filepath.Join(dir, "collection.json")
	if err := os.WriteFile(path, []byte(collection), 0o644); err != nil {
		t.Fatal(err)
	}

	pi := NewPostmanImporter(context.Background(), zap.NewNop())
	if err := pi.Import(path, ""); err != nil {
		t.Fatalf("Import: %v", err)
	}

	read := func(set, test string) string {
		b, err := os.ReadFile(filepath.Join(dir, "keploy", set, "tests", test+".yaml"))
		if err != nil {
			t.Fatalf("%s/%s: %v", set, test, err)
		}
		return string(b)
	}
	if got := read("Users", "test-1"); !strings.Contains(got, "name: first") {
		t.Errorf("Users/test-1 is not the first folder's request:\n%s", got)
	}
	if got := read("Users", "test-2"); !strings.Contains(got, "name: second") {
		t.Errorf("Users/test-2 is not the second folder's request:\n%s", got)
	}
	// A folder of another name still starts at test-1.
	if got := read("Orders", "test-1"); !strings.Contains(got, "name: other") {
		t.Errorf("Orders/test-1 is not the Orders request:\n%s", got)
	}
}
