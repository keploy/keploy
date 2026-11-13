package javascript

import (
	"context"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
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

func TestPreProcess_WithoutNyc(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cmd := "npm start"

	got, err := New(t.Context(), zap.NewNop(), nil, cmd).PreProcess(false)

	require.ErrorIs(t, err, exec.ErrNotFound)
	require.Equal(t, cmd, got)
}

func TestGetCoverage(t *testing.T) {
	tests := []struct {
		name      string
		workDir   func(t *testing.T) string
		want      models.TestCoverage
		wantErr   string
		wantErrIs error
	}{
		{
			// server.js: statement 0 is covered by both processes, 1 only by
			// the parent, 2 only by the child, 3 by neither. util.js runs in
			// the parent only and covers nothing.
			name:    "two processes covering the same file at different statements",
			workDir: func(*testing.T) string { return filepath.Join("testdata", "two-processes") },
			want: models.TestCoverage{
				FileCov: map[string]string{
					"/app/server.js": "75.00%",
					"/app/util.js":   "0.00%",
				},
				TotalCov: "50.00%",
				Loc:      models.Loc{Total: 6, Covered: 3},
			},
		},
		{
			name: "empty processinfo directory",
			workDir: func(t *testing.T) string {
				dir := t.TempDir()
				require.NoError(t, os.MkdirAll(filepath.Join(dir, ".nyc_output", "processinfo"), 0o755))
				return dir
			},
			wantErr: "no coverage files found",
		},
		{
			// nyc never ran, so it wrote no .nyc_output at all.
			name:      "no processinfo directory",
			workDir:   func(t *testing.T) string { return t.TempDir() },
			wantErrIs: fs.ErrNotExist,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Chdir(tt.workDir(t))

			got, err := New(t.Context(), zap.NewNop(), nil, "npm start").GetCoverage()

			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			if tt.wantErrIs != nil {
				require.ErrorIs(t, err, tt.wantErrIs)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestAppendCoverage(t *testing.T) {
	db := &recordingReportDB{}
	report := &models.TestCoverage{TotalCov: "50.00%"}

	err := New(t.Context(), zap.NewNop(), db, "npm start").AppendCoverage(report, "test-run-3")

	require.NoError(t, err)
	require.Equal(t, []updateReportCall{{testRunID: "test-run-3", report: report}}, db.calls)
}
