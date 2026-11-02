package mapdb

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"reflect"
	"sort"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.uber.org/zap"
)

// DropTests deletes only the named tests' entries and keeps every other entry
// and the startup section; a file that holds none of them is left as it is,
// byte for byte, and a set with no file stays without one.
func TestDropTestsDeletesOnlyTheirEntries(t *testing.T) {
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		t.Run(string(format), func(t *testing.T) {
			dir := t.TempDir()
			db := NewWithFormat(zap.NewNop(), dir, "", format)
			ctx := context.Background()
			if err := db.UpsertBatch(ctx, "set", map[string][]models.MockEntry{
				"t1": {{Name: "mock-0"}}, "t2": {{Name: "mock-1"}}, "t3": {{Name: "mock-2"}},
			}); err != nil {
				t.Fatal(err)
			}
			if err := db.UpsertCases(ctx, "set", nil, []models.MockEntry{{Name: "mock-9"}}, nil); err != nil {
				t.Fatal(err)
			}
			file := filepath.Join(dir, "set", "mappings."+format.FileExtension())
			before, err := os.ReadFile(file)
			if err != nil {
				t.Fatal(err)
			}
			for _, ids := range [][]string{nil, {"t9"}} {
				if err := db.DropTests(ctx, "set", ids); err != nil {
					t.Fatal(err)
				}
				if after, _ := os.ReadFile(file); !bytes.Equal(before, after) {
					t.Fatalf("DropTests(%v) rewrote a file that holds none of them", ids)
				}
			}
			if err := db.DropTests(ctx, "set", []string{"t2", "t9"}); err != nil {
				t.Fatal(err)
			}
			got, _, err := db.Get(ctx, "set")
			if err != nil {
				t.Fatal(err)
			}
			var ids []string
			for id := range got {
				ids = append(ids, id)
			}
			sort.Strings(ids)
			if !reflect.DeepEqual(ids, []string{"t1", "t3"}) || got["t1"][0].Name != "mock-0" || got["t3"][0].Name != "mock-2" {
				t.Fatalf("entries after DropTests = %v, want t1 (mock-0) and t3 (mock-2)", got)
			}
			if startup, err := db.GetStartup(ctx, "set"); err != nil || len(startup) != 1 || startup[0].Name != "mock-9" {
				t.Fatalf("startup after DropTests = %v, %v; want mock-9 kept", startup, err)
			}
			if err := db.DropTests(ctx, "missing", []string{"t1"}); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(filepath.Join(dir, "missing")); !os.IsNotExist(err) {
				t.Fatalf("DropTests created a mapping for a set that had none: %v", err)
			}
		})
	}
}
