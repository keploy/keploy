package export

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.uber.org/zap"
)

// exportedItems records one test case per URL into a test set, exports the
// recording, and returns the items of the set's folder as output.json holds
// them. The test cases are named test-01, test-02, ... so that the order
// their files are read in is the order they were recorded in.
func exportedItems(t *testing.T, urls []string) []interface{} {
	t.Helper()
	ctx := context.Background()
	base := t.TempDir()
	t.Chdir(base)
	db := testdb.New(zap.NewNop(), filepath.Join(base, "keploy"))
	for i, u := range urls {
		tc := &models.TestCase{Version: models.GetVersion(), Kind: models.HTTP, Name: fmt.Sprintf("test-%02d", i+1),
			HTTPReq:  models.HTTPReq{Method: "GET", URL: u},
			HTTPResp: models.HTTPResp{StatusCode: 200, Body: "ok"}}
		if err := db.InsertTestCase(ctx, tc, "set", false); err != nil {
			t.Fatal(err)
		}
	}
	if err := Export(ctx, zap.NewNop()); err != nil {
		t.Fatal(err)
	}
	out, err := os.ReadFile(filepath.Join(base, "output.json"))
	if err != nil {
		t.Fatal(err)
	}
	var coll struct {
		Item []struct {
			Name string        `json:"name"`
			Item []interface{} `json:"item"`
		} `json:"item"`
	}
	if err := json.Unmarshal(out, &coll); err != nil {
		t.Fatalf("output.json is not JSON: %v", err)
	}
	if len(coll.Item) != 1 {
		t.Fatalf("want one folder, got %d", len(coll.Item))
	}
	return coll.Item[0].Item
}

func itemNames(t *testing.T, items []interface{}) []string {
	t.Helper()
	var names []string
	for _, it := range items {
		m, ok := it.(map[string]interface{})
		if !ok {
			names = append(names, fmt.Sprintf("<%T>", it))
			continue
		}
		names = append(names, m["name"].(string))
	}
	return names
}

// A collection is a file people keep in git and diff: exporting the same
// recording twice must give the same file, with a folder's requests in the
// order the test cases were recorded.
func TestExportKeepsARecordingsRequestsInTheirOrder(t *testing.T) {
	var urls []string
	var want []string
	for i := 1; i <= 12; i++ {
		urls = append(urls, fmt.Sprintf("http://127.0.0.1:9/item-%02d", i))
		want = append(want, fmt.Sprintf("item-%02d", i))
	}
	for run := 0; run < 5; run++ {
		got := itemNames(t, exportedItems(t, urls))
		if fmt.Sprint(got) != fmt.Sprint(want) {
			t.Fatalf("run %d: requests are not in recording order:\n got  %v\n want %v", run, got, want)
		}
	}
}

// A test case whose URL cannot be turned into a Postman request is left out
// of the collection; it must not leave a null item in it, which Postman
// cannot import.
func TestExportLeavesNoNullItemForAnUnconvertibleTestCase(t *testing.T) {
	items := exportedItems(t, []string{"http://127.0.0.1:9/ok", "/no-host/path"})
	for _, it := range items {
		if it == nil {
			t.Fatalf("the collection holds a null item: %v", itemNames(t, items))
		}
	}
	if len(items) != 1 {
		t.Fatalf("want only the convertible request, got %v", itemNames(t, items))
	}
}
