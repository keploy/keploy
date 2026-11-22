package mapdb

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

// TestPlainYAMLInsertMatchesFullPath verifies that Insert's streaming plain
// YAML fast path writes the exact same bytes as the full decode+encodeOver
// path across added tests, updated tests (preserving RecordedTestCase fields
// and set sections), custom comment headers, and large test entries, and falls
// back cleanly on hand-edited files.
func TestPlainYAMLInsertMatchesFullPath(t *testing.T) {
	eachSwitch(t, func(t *testing.T) {
		ctx := context.Background()

		seed := generatedMapping()
		seed.Startup = []models.MockEntry{{Name: "boot-1", Kind: "Http"}}
		seed.Worker = []models.MockEntry{{Name: "bg-1", Kind: "Http", Every: "5m"}}
		seed.Source = "keploy-record"
		seed.RecordedWith = []string{"keploy-v3"}
		seed.TestCases[1].Key = "worker-k"
		seed.TestCases[1].Dir = "pkg/a"
		seed.TestCases[1].Cases = []string{"sub-1"}

		// Include a large test entry (> chunkMocks) so both the unchanged and
		// updated large-entry paths are exercised.
		largeMocks := make([]models.MockEntry, chunkMocks+5)
		for i := range largeMocks {
			largeMocks[i] = models.MockEntry{Name: "lm-" + string(rune('a'+i%26)) + "-" + string(rune('0'+i%10)), Kind: "Http"}
		}
		seed.TestCases = append(seed.TestCases, models.MappedTestCase{ID: "test-large", Mocks: largeMocks})

		plainBytes, err := marshalMappingYAML(seed)
		if err != nil {
			t.Fatal(err)
		}
		withHead := append([]byte("# keep this header\n"), plainBytes...)

		incoming := &models.Mapping{
			Version:   string(models.V1Beta1),
			Kind:      models.MappingKind,
			TestSetID: seed.TestSetID,
			TestCases: []models.MappedTestCase{
				{ID: seed.TestCases[1].ID, Mocks: []models.MockEntry{{Name: "updated-mock", Kind: "Http"}}},
				{ID: "test-new-zz", Mocks: []models.MockEntry{{Name: "new-mock", Kind: "Http"}}},
			},
		}

		// Fast path via Insert.
		fastDir := t.TempDir()
		setDir := filepath.Join(fastDir, seed.TestSetID)
		if err := os.MkdirAll(setDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(setDir, "mappings.yaml"), withHead, 0o644); err != nil {
			t.Fatal(err)
		}
		fastDB := New(zap.NewNop(), fastDir, "mappings")
		handled, total, err := fastDB.tryInsertPlainYAML(ctx, incoming, setDir, "mappings", withHead)
		if err != nil || !handled {
			t.Fatalf("tryInsertPlainYAML handled=%v err=%v; want handled=true, nil", handled, err)
		}
		if total != len(seed.TestCases)+1 {
			t.Fatalf("totalTests = %d, want %d", total, len(seed.TestCases)+1)
		}
		gotFast, err := os.ReadFile(filepath.Join(setDir, "mappings.yaml"))
		if err != nil {
			t.Fatal(err)
		}

		// Reference path via prepareInsert + encodeOver.
		refDB := New(zap.NewNop(), t.TempDir(), "mappings")
		refMapping, old, oldDoc, plainHead, err := refDB.prepareInsert(incoming, yaml.FormatYAML, true, withHead)
		if err != nil {
			t.Fatal(err)
		}
		var wantBuf bytes.Buffer
		if oldDoc == plainDoc {
			if err := writePlainYAML(&wantBuf, refMapping, plainHead); err != nil {
				t.Fatal(err)
			}
		} else {
			b, err := refDB.encodeOver(refMapping, yaml.FormatYAML, old, oldDoc, headerAlways, seed.TestSetID)
			if err != nil {
				t.Fatal(err)
			}
			wantBuf.Write(b)
		}
		if !bytes.Equal(gotFast, wantBuf.Bytes()) {
			t.Fatalf("plain fast-path output differs from full decode+encode path (got %d bytes, want %d bytes)", len(gotFast), wantBuf.Len())
		}

		// Hand-edited file must fall back (handled=false) and keep the edit on Insert.
		edited := []byte(strings.Replace(string(withHead), "    - id: test-1\n", "    - id: test-1 # keep me\n", 1))
		editDir := t.TempDir()
		editSetDir := filepath.Join(editDir, seed.TestSetID)
		if err := os.MkdirAll(editSetDir, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(editSetDir, "mappings.yaml"), edited, 0o644); err != nil {
			t.Fatal(err)
		}
		editDB := New(zap.NewNop(), editDir, "mappings")
		if handled, _, err := editDB.tryInsertPlainYAML(ctx, incoming, editSetDir, "mappings", edited); err != nil || handled {
			t.Fatalf("hand-edited file: handled=%v err=%v; want handled=false, nil", handled, err)
		}
		if err := editDB.Insert(ctx, incoming); err != nil {
			t.Fatalf("Insert on hand-edited file: %v", err)
		}
		gotEdited, err := os.ReadFile(filepath.Join(editSetDir, "mappings.yaml"))
		if err != nil {
			t.Fatal(err)
		}
		if !bytes.Contains(gotEdited, []byte("# keep me")) {
			t.Fatalf("Insert on hand-edited file dropped comment # keep me")
		}
	})
}
