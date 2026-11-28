package testdb

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func TestRecordedTestWindows_PersistsImmutableScheduleAcrossDeletes(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db := New(zap.NewNop(), dir) // default NamingDescriptive
	t0 := time.Date(2026, 11, 28, 12, 0, 0, 0, time.UTC)

	// Pre-existing set with no recorded_windows file returns (nil, nil).
	if got, err := db.RecordedTestWindows(ctx, "missing-set"); err != nil || len(got) != 0 {
		t.Fatalf("missing set RecordedTestWindows = (%v, %v), want (nil, nil)", got, err)
	}

	makeCase := func(method, rawURL string, start, end time.Time) *models.TestCase {
		return &models.TestCase{
			Version: models.GetVersion(),
			Kind:    models.HTTP,
			HTTPReq: models.HTTPReq{
				Method:    models.Method(method),
				URL:       rawURL,
				Timestamp: start,
			},
			HTTPResp: models.HTTPResp{
				StatusCode: 200,
				Body:       `{"ok":true}`,
				Timestamp:  end,
			},
		}
	}

	c1 := makeCase("GET", "http://localhost:8080/v1/clusters", t0.Add(10*time.Second), t0.Add(11*time.Second))
	c2 := makeCase("POST", "http://localhost:8080/v1/apps", t0.Add(20*time.Second), t0.Add(21*time.Second))
	c3 := makeCase("GET", "http://localhost:8080/v1/users", t0.Add(30*time.Second), t0.Add(31*time.Second))
	for _, tc := range []*models.TestCase{c1, c2, c3} {
		if err := db.InsertTestCase(ctx, tc, "ts-slug", false); err != nil {
			t.Fatalf("InsertTestCase: %v", err)
		}
	}
	if c1.Name == "" || c2.Name == "" || c3.Name == "" {
		t.Fatalf("expected descriptive slug names assigned, got %q %q %q", c1.Name, c2.Name, c3.Name)
	}

	// Delete the middle test case: the immutable schedule must STILL include all 3 windows.
	if err := db.DeleteTests(ctx, "ts-slug", []string{c2.Name}); err != nil {
		t.Fatalf("DeleteTests: %v", err)
	}
	held, err := db.GetTestCases(ctx, "ts-slug")
	if err != nil || len(held) != 2 {
		t.Fatalf("GetTestCases after delete = (%d, %v), want (2, nil)", len(held), err)
	}
	all, err := db.RecordedTestWindows(ctx, "ts-slug")
	if err != nil {
		t.Fatalf("RecordedTestWindows: %v", err)
	}
	if len(all) != 3 {
		t.Fatalf("RecordedTestWindows len = %d, want 3 (deleted test %q must remain in immutable schedule): %+v", len(all), c2.Name, all)
	}
	foundDeleted := false
	for _, w := range all {
		if w.TestCase == c2.Name && w.Start.Equal(t0.Add(20*time.Second)) && w.End.Equal(t0.Add(21*time.Second)) {
			foundDeleted = true
		}
	}
	if !foundDeleted {
		t.Fatalf("deleted test %q missing from RecordedTestWindows: %+v", c2.Name, all)
	}

	// Corrupt schedule file MUST return an error (fail closed), never (nil, nil).
	corruptPath := filepath.Join(dir, "ts-slug", recordedWindowsFileName)
	if err := os.WriteFile(corruptPath, []byte("{not valid json\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := db.RecordedTestWindows(ctx, "ts-slug"); err == nil {
		t.Fatal("expected error reading malformed recorded_windows.jsonl, got nil")
	}

	// config.yaml recorded_windows is also honored when present.
	cfgDir := filepath.Join(dir, "ts-cfg")
	if err := os.MkdirAll(cfgDir, 0o755); err != nil {
		t.Fatal(err)
	}
	cfgYAML := "recorded_windows:\n  - test_case: get-orders-1\n    start: 2026-11-28T12:00:10Z\n    end: 2026-11-28T12:00:11Z\n"
	if err := os.WriteFile(filepath.Join(cfgDir, "config.yaml"), []byte(cfgYAML), 0o644); err != nil {
		t.Fatal(err)
	}
	cfgWins, err := db.RecordedTestWindows(ctx, "ts-cfg")
	if err != nil || len(cfgWins) != 1 || cfgWins[0].TestCase != "get-orders-1" {
		t.Fatalf("config.yaml RecordedTestWindows = (%+v, %v), want [get-orders-1]", cfgWins, err)
	}
}
