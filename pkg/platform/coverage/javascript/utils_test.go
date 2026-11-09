package javascript

import (
	"os"
	"path/filepath"
	"slices"
	"testing"
)

func writeTestFile(t *testing.T, path, body string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatalf("mkdir %s: %v", filepath.Dir(path), err)
	}
	if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
		t.Fatalf("write %s: %v", path, err)
	}
}

func TestGetCoverageFilePathsJavascript(t *testing.T) {
	const validProcessInfo = `{"coverageFilename":"/tmp/keploy-coverage.json","files":["index.js"]}`

	tests := []struct {
		name    string
		setup   func(t *testing.T) string
		want    []string
		wantErr bool
	}{
		{
			name: "missing processinfo directory",
			setup: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), ".nyc_output", "processinfo")
			},
			wantErr: true,
		},
		{
			name:  "empty processinfo directory",
			setup: func(t *testing.T) string { return t.TempDir() },
		},
		{
			name: "processinfo with files and a coverage filename",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeTestFile(t, filepath.Join(dir, "a.json"), validProcessInfo)
				return dir
			},
			want: []string{"/tmp/keploy-coverage.json"},
		},
		{
			name: "index.json is ignored",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeTestFile(t, filepath.Join(dir, "index.json"), validProcessInfo)
				return dir
			},
		},
		{
			name: "non-JSON file is skipped",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeTestFile(t, filepath.Join(dir, ".DS_Store"), "not json")
				return dir
			},
		},
		{
			name: "processinfo without files is skipped",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeTestFile(t, filepath.Join(dir, "a.json"), `{"coverageFilename":"/tmp/cov.json","files":[]}`)
				return dir
			},
		},
		{
			name: "processinfo without a coverage filename is skipped",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeTestFile(t, filepath.Join(dir, "a.json"), `{"coverageFilename":"","files":["index.js"]}`)
				return dir
			},
		},
		{
			name: "nested processinfo is found",
			setup: func(t *testing.T) string {
				dir := t.TempDir()
				writeTestFile(t, filepath.Join(dir, "nested", "a.json"), validProcessInfo)
				return dir
			},
			want: []string{"/tmp/keploy-coverage.json"},
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := getCoverageFilePathsJavascript(tt.setup(t))
			if tt.wantErr {
				if err == nil {
					t.Fatal("getCoverageFilePathsJavascript() = nil error, want an error")
				}
				return
			}
			if err != nil {
				t.Fatalf("getCoverageFilePathsJavascript(): %v", err)
			}
			slices.Sort(got)
			slices.Sort(tt.want)
			if !slices.Equal(got, tt.want) {
				t.Fatalf("getCoverageFilePathsJavascript() = %v, want %v", got, tt.want)
			}
		})
	}
}
