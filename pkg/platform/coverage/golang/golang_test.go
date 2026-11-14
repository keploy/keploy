package golang

import (
	"context"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

type updateReportCall struct {
	testRunID string
	report    any
}

type recordingReportDB struct {
	calls []updateReportCall
}

func (r *recordingReportDB) UpdateReport(_ context.Context, testRunID string, report any) error {
	r.calls = append(r.calls, updateReportCall{testRunID: testRunID, report: report})
	return nil
}

func TestCheckForCoverFlag(t *testing.T) {
	tests := []struct {
		name string
		cmd  string
		want bool
	}{
		{name: "go run with -cover", cmd: "go run -cover ./cmd", want: true},
		{name: "go run without -cover", cmd: "go run ./cmd", want: false},
		{name: "env assignment before go is skipped", cmd: "GOFLAGS=x go run -cover .", want: true},
		{name: "empty command", cmd: "", want: false},
		{name: "binary that does not exist", cmd: filepath.Join(t.TempDir(), "missing-app"), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, checkForCoverFlag(zap.NewNop(), tt.cmd))
		})
	}
}

// buildLinuxBinary compiles a one-line program as a Linux ELF binary, so the
// symbol check reads a real Go binary on any host.
func buildLinuxBinary(t *testing.T, buildFlags ...string) string {
	t.Helper()
	dir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(dir, "main.go"), []byte("package main\n\nfunc main() { println(\"ok\") }\n"), 0o644))
	bin := filepath.Join(dir, "app")
	args := append([]string{"build"}, buildFlags...)
	args = append(args, "-o", bin, "main.go")
	cmd := exec.Command("go", args...)
	cmd.Dir = dir
	cmd.Env = append(os.Environ(), "GOOS=linux", "CGO_ENABLED=0")
	out, err := cmd.CombinedOutput()
	require.NoError(t, err, string(out))
	return bin
}

func TestCheckForCoverFlag_Binary(t *testing.T) {
	tests := []struct {
		name       string
		buildFlags []string
		want       bool
	}{
		{name: "built without -cover", want: false},
		{name: "built with -cover", buildFlags: []string{"-cover"}, want: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bin := buildLinuxBinary(t, tt.buildFlags...)
			require.Equal(t, tt.want, checkForCoverFlag(zap.NewNop(), bin))
		})
	}
}

func TestPreProcess_Native(t *testing.T) {
	tests := []struct {
		name    string
		cmd     string
		wantErr string
	}{
		{name: "cover-built command sets GOCOVERDIR", cmd: "go run -cover ./cmd"},
		{name: "command that is not coverable", cmd: "go run ./cmd", wantErr: "binary not coverable"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			base := t.TempDir()
			t.Chdir(base)
			require.NoError(t, os.Mkdir("reports", 0o755))
			t.Setenv("GOCOVERDIR", "")

			g := New(t.Context(), zap.NewNop(), nil, tt.cmd, "reports", string(utils.Native))
			got, err := g.PreProcess(false)

			require.Equal(t, tt.cmd, got)
			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				require.Empty(t, os.Getenv("GOCOVERDIR"))
				return
			}
			require.NoError(t, err)
			require.Equal(t, filepath.Join(base, "reports"), os.Getenv("GOCOVERDIR"))
		})
	}
}

func TestGetCoverage_EmptyCoverDir(t *testing.T) {
	t.Setenv("GOCOVERDIR", t.TempDir())

	got, err := New(t.Context(), zap.NewNop(), nil, "go run -cover .", "", string(utils.Native)).GetCoverage()

	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, models.TestCoverage{FileCov: map[string]string{}}, got)
}

func TestAppendCoverage(t *testing.T) {
	db := &recordingReportDB{}
	report := &models.TestCoverage{TotalCov: "50.00%"}

	err := New(t.Context(), zap.NewNop(), db, "go run -cover .", "", string(utils.Native)).AppendCoverage(report, "test-run-3")

	require.NoError(t, err)
	require.Equal(t, []updateReportCall{{testRunID: "test-run-3", report: report}}, db.calls)
}
