package postmanimport

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

func TestImportRejectsPathTraversalFolderName(t *testing.T) {
	invalidFolders := []string{
		"../../escaped",
		".",
		"a/b",
		"/tmp/escaped",
		"foo/../bar",
	}

	for _, folderName := range invalidFolders {
		t.Run(folderName, func(t *testing.T) {
			root := t.TempDir()
			project := filepath.Join(root, "project")
			if err := os.MkdirAll(project, 0o755); err != nil {
				t.Fatal(err)
			}
			collectionPath := filepath.Join(project, "collection.json")
			collection := map[string]any{
				"info": map[string]string{"schema": postmanSchemaVersion},
				"item": []any{
					// Include a valid folder before the invalid one to verify
					// pre-validation prevents partial writes.
					map[string]any{
						"name": "valid-folder",
						"item": []any{
							map[string]any{
								"name":     "ok",
								"request":  map[string]any{"method": "GET", "url": "https://example.com/ok"},
								"response": []any{map[string]any{"status": "OK", "code": 200, "body": "{}"}},
							},
						},
					},
					map[string]any{
						"name": folderName,
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

			t.Chdir(project)

			importer := NewPostmanImporter(context.Background(), zap.NewNop())
			err = importer.Import(collectionPath, "")
			if err == nil {
				t.Fatalf("Import succeeded for invalid folder name %q", folderName)
			}
			if !strings.Contains(err.Error(), "invalid test set name") {
				t.Fatalf("Import error = %q, want message containing %q", err.Error(), "invalid test set name")
			}

			outside := filepath.Join(root, "escaped", "tests", "test-1.yaml")
			if _, statErr := os.Stat(outside); !os.IsNotExist(statErr) {
				t.Fatalf("path-traversal output exists at %q: %v", outside, statErr)
			}
			keployDir := filepath.Join(project, "keploy")
			if _, statErr := os.Stat(keployDir); !os.IsNotExist(statErr) {
				t.Fatalf("keploy dir %q should not be created on invalid folder name: %v", keployDir, statErr)
			}
		})
	}
}
