package python

import (
	"context"
	"os/exec"
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

func TestPreProcess_WithoutCoverageTool(t *testing.T) {
	t.Setenv("PATH", t.TempDir())
	cmd := "python3 app.py"

	got, err := New(t.Context(), zap.NewNop(), nil, cmd, "python3").PreProcess(false)

	require.ErrorIs(t, err, exec.ErrNotFound)
	require.Equal(t, cmd, got)
}

func TestGetCoverage_NoPerProcessFiles(t *testing.T) {
	t.Chdir(t.TempDir())

	got, err := New(t.Context(), zap.NewNop(), nil, "python3 app.py", "python3").GetCoverage()

	require.NoError(t, err)
	require.Equal(t, models.TestCoverage{FileCov: map[string]string{}}, got)
}

func TestAppendCoverage(t *testing.T) {
	db := &recordingReportDB{}
	report := &models.TestCoverage{TotalCov: "64%"}

	err := New(t.Context(), zap.NewNop(), db, "python3 app.py", "python3").AppendCoverage(report, "test-run-3")

	require.NoError(t, err)
	require.Equal(t, []updateReportCall{{testRunID: "test-run-3", report: report}}, db.calls)
}
