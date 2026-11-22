package tools

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

type runReports struct{ read string }

func (*runReports) GetAllTestRunIDs(context.Context) ([]string, error) {
	return []string{"test-run-1", "test-run-2"}, nil
}
func (r *runReports) GetReport(_ context.Context, run, _ string) (*models.TestReport, error) {
	r.read = run
	return &models.TestReport{}, nil
}

func TestNormalizeUsesSelectedTestRun(t *testing.T) {
	for _, c := range []struct{ name, selected, want string }{
		{"explicit run", "test-run-1", "test-run-1"},
		{"latest when omitted", "", "test-run-2"},
	} {
		t.Run(c.name, func(t *testing.T) {
			root := t.TempDir()
			require.NoError(t, os.MkdirAll(filepath.Join(root, "test-set-0", "tests"), 0755))
			reports := &runReports{}
			cfg := config.New()
			cfg.Path = root
			cfg.Normalize.TestRun = c.selected
			cfg.Normalize.SelectedTests = []config.SelectedTests{{TestSet: "test-set-0"}}
			svc := &Tools{logger: zap.NewNop(), config: cfg, reportDB: reports, testDB: &normaliseTestDB{}}
			require.NoError(t, svc.Normalize(context.Background()))
			require.Equal(t, c.want, reports.read)
		})
	}
}
