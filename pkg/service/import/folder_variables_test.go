package postmanimport

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

// A folder's "variable" entries are Postman's variable objects, which carry
// more than strings ("disabled": true, a "description" object). They were
// decoded as map[string]string, so one such entry made the whole collection
// fail to import with "cannot unmarshal bool into Go struct field".
func TestImportAcceptsAFolderVariableThatIsNotAllStrings(t *testing.T) {
	dir := t.TempDir()
	t.Chdir(dir)
	collection := `{"info":{"name":"c"},"item":[{
		"name":"Users",
		"variable":[
			{"key":"id","value":"1","disabled":true},
			{"key":"name","value":"x","description":{"content":"the user","type":"text/plain"}}
		],
		"item":[{"name":"list","request":{"method":"GET","url":"http://example.com/users"},
			"response":[{"name":"ok","code":200,"status":"OK","body":"[]","header":[]}]}]}]}`
	path := filepath.Join(dir, "collection.json")
	if err := os.WriteFile(path, []byte(collection), 0o644); err != nil {
		t.Fatal(err)
	}

	pi := NewPostmanImporter(context.Background(), zap.NewNop())
	if err := pi.Import(path, ""); err != nil {
		t.Fatalf("Import: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "keploy", "Users", "tests", "test-1.yaml")); err != nil {
		t.Fatalf("the folder's test was not written: %v", err)
	}
}
