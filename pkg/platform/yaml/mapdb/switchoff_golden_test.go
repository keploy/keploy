package mapdb

import (
	"bytes"
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Without the proven switch, mappings.yaml is written byte for byte as the
// goldens hold it: a mock entry without a Reason, and a worker
// scope a move gives none, write what keploy wrote before entries had one.
//
// To regenerate: KEPLOY_GOLDEN_OUT=<dir> go test -run
// TestMappingsAreUnchangedWithoutTheSwitch ./pkg/platform/yaml/mapdb/, then
// copy <dir> to testdata/switchoff_golden. To check a change that must leave
// these files alone, generate them on a checkout from before the change, with
// this file added to it, and run this test after the change.
const goldenSwitchOffDir = "testdata/switchoff_golden"

func checkSwitchOffGolden(t *testing.T, name string, got []byte) {
	t.Helper()
	if out := os.Getenv("KEPLOY_GOLDEN_OUT"); out != "" {
		if err := os.MkdirAll(out, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(out, name), got, 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	want, err := os.ReadFile(filepath.Join(goldenSwitchOffDir, name))
	if err != nil {
		t.Fatalf("read golden %s: %v", name, err)
	}
	if bytes.Equal(got, want) {
		return
	}
	gl, wl := strings.Split(string(got), "\n"), strings.Split(string(want), "\n")
	for i := 0; i < len(gl) || i < len(wl); i++ {
		var g, w string
		if i < len(gl) {
			g = gl[i]
		}
		if i < len(wl) {
			w = wl[i]
		}
		if g != w {
			t.Fatalf("%s differs from the golden without the proven switch, first at line %d:\n got: %q\nwant: %q", name, i+1, g, w)
		}
	}
}

// goldenMapping is a recording's mapping: two tests, a start-up mock and a
// worker scope with a placed entry.
func goldenMapping() *models.Mapping {
	at := time.Date(2026, 10, 22, 9, 0, 0, 123456000, time.UTC)
	entry := func(name, kind string, ms int) models.MockEntry {
		req := at.Add(time.Duration(ms) * time.Millisecond)
		return models.MockEntry{Name: name, Kind: kind, Timestamp: req.Unix(),
			ReqTimestampMock: models.FormatMockTimestamp(req), ResTimestampMock: models.FormatMockTimestamp(req.Add(time.Millisecond))}
	}
	worker := entry("mock-2", "SQS", 10)
	placed := entry("mock-5", "SQS", 2050)
	placed.Place = "post-url-1"
	return &models.Mapping{
		Version:   string(models.V1Beta1),
		Kind:      models.MappingKind,
		TestSetID: "test-set-0",
		TestCases: []models.MappedTestCase{
			{ID: "post-url-0", Mocks: []models.MockEntry{entry("mock-3", "Mongo", 1010)}},
			{ID: "post-url-1", Mocks: []models.MockEntry{entry("mock-6", "Mongo", 2010), entry("mock-7", "Mongo", 2020)}},
		},
		Startup: []models.MockEntry{entry("mock-0", "Mongo", 1)},
		Worker:  []models.MockEntry{worker, placed},
	}
}

func TestMappingsAreUnchangedWithoutTheSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "")
	for _, format := range []yaml.Format{yaml.FormatYAML, yaml.FormatJSON} {
		ext := format.FileExtension()
		t.Run(ext, func(t *testing.T) {
			got, err := EncodeMappingF(goldenMapping(), zap.NewNop(), format)
			if err != nil {
				t.Fatal(err)
			}
			checkSwitchOffGolden(t, "mappings."+ext, got)

			// A move without a Reason (record's judging as it is today) writes
			// the file it wrote before moves could carry one.
			dir := t.TempDir()
			db := NewWithFormat(zap.NewNop(), dir, "mappings", format)
			if err := db.Insert(context.Background(), goldenMapping()); err != nil {
				t.Fatal(err)
			}
			moved, err := db.MoveToWorker(context.Background(), "test-set-0", []models.MockEntry{{Name: "mock-6"}, {Name: "mock-5"}})
			if err != nil || len(moved) != 2 {
				t.Fatalf("MoveToWorker = %v, %v; want both moved", moved, err)
			}
			written, err := os.ReadFile(filepath.Join(dir, "test-set-0", "mappings."+ext))
			if err != nil {
				t.Fatal(err)
			}
			checkSwitchOffGolden(t, "moved."+ext, written)
		})
	}
}
