package testdb

import (
	"os"
	"path/filepath"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestMultipartDuplicateFilenameAssetPaths(t *testing.T) {
	t.Run("temp files", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "keploy")
		db := NewWithNaming(zap.NewNop(), root, NamingDescriptive)
		tempFiles := make([]string, 2)
		for i, content := range []string{"first temp content", "second temp content"} {
			path := filepath.Join(t.TempDir(), "captured-"+string(rune('1'+i))+".bin")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			tempFiles[i] = path
		}
		tc := &models.TestCase{
			Name: "multipart-temp",
			Kind: models.HTTP,
			HTTPReq: models.HTTPReq{
				Method: "POST",
				Form: []models.FormData{{
					Key:       "upload",
					FileNames: []string{"upload.txt", "upload.txt"},
					Paths:     tempFiles,
					Captured:  tempFiles,
				}},
			},
		}
		if err := db.saveAssets("set", tc, "test-1"); err != nil {
			t.Fatalf("saveAssets: %v", err)
		}
		assertMultipartAssetFiles(t, root,
			[]string{"first temp content", "second temp content"},
			[]string{
				filepath.Join("set", "assets", "test-1", "upload", "0", "upload.txt"),
				filepath.Join("set", "assets", "test-1", "upload", "1", "upload.txt"),
			})
	})

	t.Run("legacy inline values", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "keploy")
		db := NewWithNaming(zap.NewNop(), root, NamingDescriptive)
		tc := &models.TestCase{
			Name: "multipart-inline",
			Kind: models.HTTP,
			HTTPReq: models.HTTPReq{
				Method: "POST",
				Form: []models.FormData{{
					Key:       "upload",
					FileNames: []string{"upload.txt", "upload.txt"},
					Values:    []string{"first inline content", "second inline content"},
				}},
			},
		}
		if err := db.saveAssets("set", tc, "test-1"); err != nil {
			t.Fatalf("saveAssets: %v", err)
		}
		assertMultipartAssetFiles(t, root,
			[]string{"first inline content", "second inline content"},
			[]string{
				filepath.Join("set", "assets", "test-1", "upload", "0", "upload.txt"),
				filepath.Join("set", "assets", "test-1", "upload", "1", "upload.txt"),
			})
	})

	t.Run("single file", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "keploy")
		db := NewWithNaming(zap.NewNop(), root, NamingDescriptive)
		tempFile := filepath.Join(t.TempDir(), "captured.bin")
		if err := os.WriteFile(tempFile, []byte("only content"), 0o600); err != nil {
			t.Fatal(err)
		}
		tc := &models.TestCase{
			Name: "multipart-single",
			Kind: models.HTTP,
			HTTPReq: models.HTTPReq{
				Method: "POST",
				Form: []models.FormData{{
					Key:       "upload",
					FileNames: []string{"upload.txt"},
					Paths:     []string{tempFile},
					Captured:  []string{tempFile},
				}},
			},
		}
		if err := db.saveAssets("set", tc, "test-1"); err != nil {
			t.Fatalf("saveAssets: %v", err)
		}
		assertMultipartAssetFiles(t, root,
			[]string{"only content"},
			[]string{filepath.Join("set", "assets", "test-1", "upload", "upload.txt")})
	})

	t.Run("separate fields with same key and filename", func(t *testing.T) {
		root := filepath.Join(t.TempDir(), "keploy")
		db := NewWithNaming(zap.NewNop(), root, NamingDescriptive)
		tempFiles := make([]string, 2)
		for i, content := range []string{"first field", "second field"} {
			path := filepath.Join(t.TempDir(), "captured-field-"+string(rune('1'+i))+".bin")
			if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
				t.Fatal(err)
			}
			tempFiles[i] = path
		}
		tc := &models.TestCase{
			Name: "multipart-fields",
			Kind: models.HTTP,
			HTTPReq: models.HTTPReq{
				Method: "POST",
				Form: []models.FormData{
					{Key: "upload", FileNames: []string{"upload.txt"}, Paths: []string{tempFiles[0]}, Captured: []string{tempFiles[0]}},
					{Key: "upload", FileNames: []string{"upload.txt"}, Paths: []string{tempFiles[1]}, Captured: []string{tempFiles[1]}},
				},
			},
		}
		if err := db.saveAssets("set", tc, "test-1"); err != nil {
			t.Fatalf("saveAssets: %v", err)
		}
		assertMultipartAssetFiles(t, root,
			[]string{"first field", "second field"},
			[]string{
				filepath.Join("set", "assets", "test-1", "upload", "upload.txt"),
				filepath.Join("set", "assets", "test-1", "upload", "0", "upload.txt"),
			})
	})
}

func assertMultipartAssetFiles(t *testing.T, root string, wantContents, wantRelPaths []string) {
	t.Helper()
	if len(wantContents) != len(wantRelPaths) {
		t.Fatalf("content/path count mismatch: %d vs %d", len(wantContents), len(wantRelPaths))
	}
	for i, wantRelPath := range wantRelPaths {
		path := filepath.Join(root, wantRelPath)
		content, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read asset %q: %v", path, err)
		}
		if string(content) != wantContents[i] {
			t.Fatalf("asset %q holds %q, want %q", path, content, wantContents[i])
		}
	}
}
