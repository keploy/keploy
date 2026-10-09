package postmanimport

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

func TestImportRejectsPathTraversalFolderName(t *testing.T) {
	root := t.TempDir()
	project := filepath.Join(root, "project")
	if err := os.MkdirAll(project, 0o755); err != nil {
		t.Fatal(err)
	}
	collectionPath := filepath.Join(project, "collection.json")
	collection := map[string]any{
		"info": map[string]string{"schema": postmanSchemaVersion},
		"item": []any{
			map[string]any{
				"name": "../../escaped",
				"item": []any{
					map[string]any{
						"name":     "health",
						"request":  map[string]any{"method": "GET", "url": "https://example.com"},
						"response": []any{map[string]any{"status": "OK", "code": 200, "body": "{}"}},
					},
				},
			},
		},
	}
	data, err := json.Marshal(collection)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(collectionPath, data, 0o600); err != nil {
		t.Fatal(err)
	}

	originalDir, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(project); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(originalDir) })

	importer := NewPostmanImporter(context.Background(), zap.NewNop())
	err = importer.Import(collectionPath, "")
	if err == nil {
		t.Fatal("Import succeeded for a path-traversal folder name")
	}
	outside := filepath.Join(root, "escaped", "tests", "test-1.yaml")
	if _, statErr := os.Stat(outside); !os.IsNotExist(statErr) {
		t.Fatalf("path-traversal output exists at %q: %v", outside, statErr)
	}
}
