package javascript

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

func TestGetCoverage_MissingProcessInfoDirectory(t *testing.T) {
	t.Chdir(t.TempDir())

	j := New(t.Context(), zap.NewNop(), nil, "node index.js")
	if _, err := j.GetCoverage(); err == nil {
		t.Fatal("GetCoverage() = nil error, want an error when .nyc_output/processinfo is missing")
	}
}

func TestGetCoverage_ReadsProcessInfoCoverage(t *testing.T) {
	dir := t.TempDir()

	coveragePath := filepath.Join(dir, "coverage.json")
	coverage := `{
		"/app/index.js": {
			"path": "/app/index.js",
			"statementMap": {
				"0": {"start": {"line": 1, "column": 0}, "end": {"line": 1, "column": 10}}
			},
			"s": {"0": 1}
		}
	}`
	if err := os.WriteFile(coveragePath, []byte(coverage), 0o644); err != nil {
		t.Fatalf("write coverage: %v", err)
	}

	processInfoDir := filepath.Join(dir, ".nyc_output", "processinfo")
	if err := os.MkdirAll(processInfoDir, 0o755); err != nil {
		t.Fatalf("mkdir processinfo: %v", err)
	}
	processInfo := fmt.Sprintf(`{"coverageFilename":%q,"files":["/app/index.js"]}`, coveragePath)
	if err := os.WriteFile(filepath.Join(processInfoDir, "1.json"), []byte(processInfo), 0o644); err != nil {
		t.Fatalf("write processinfo: %v", err)
	}

	t.Chdir(dir)

	j := New(t.Context(), zap.NewNop(), nil, "node index.js")
	got, err := j.GetCoverage()
	if err != nil {
		t.Fatalf("GetCoverage(): %v", err)
	}
	if got.Loc.Total != 1 || got.Loc.Covered != 1 {
		t.Fatalf("Loc = %+v, want {Total:1 Covered:1}", got.Loc)
	}
	if got.TotalCov != "100.00%" {
		t.Fatalf("TotalCov = %q, want %q", got.TotalCov, "100.00%")
	}
	if got.FileCov["/app/index.js"] != "100.00%" {
		t.Fatalf("FileCov[/app/index.js] = %q, want %q", got.FileCov["/app/index.js"], "100.00%")
	}
}
