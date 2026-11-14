package java

import (
	"context"
	"os"
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

func TestPreProcess_AddsJacocoAgent(t *testing.T) {
	agent := filepath.Join(t.TempDir(), "org.jacoco.agent-runtime.jar")
	require.NoError(t, os.WriteFile(agent, []byte("agent"), 0o644))
	tests := []struct {
		name string
		cmd  string
		want string
	}{
		{
			name: "java -jar",
			cmd:  "java -jar target/app.jar",
			want: "java -javaagent:" + agent + "=destfile=target/${TESTSETID}.exec -jar target/app.jar",
		},
		{
			name: "env assignment before java",
			cmd:  "JAVA_OPTS=-Xmx1g java -jar target/app.jar --server.port=8080",
			want: "JAVA_OPTS=-Xmx1g java -javaagent:" + agent + "=destfile=target/${TESTSETID}.exec -jar target/app.jar --server.port=8080",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// A jacococli.jar already in the temp dir means PreProcess skips the download.
			tmp := t.TempDir()
			t.Setenv("TMPDIR", tmp)
			require.NoError(t, os.MkdirAll(filepath.Join(tmp, "jacoco"), 0o755))
			require.NoError(t, os.WriteFile(filepath.Join(tmp, "jacoco", "jacococli.jar"), []byte("cli"), 0o644))

			got, err := New(t.Context(), zap.NewNop(), nil, tt.cmd, agent, "java").PreProcess(false)

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

// Without the agent jar the run cannot collect coverage, so PreProcess says
// so (the caller then skips coverage) and leaves the command as it was.
func TestPreProcess_MissingJacocoAgent(t *testing.T) {
	tmp := t.TempDir()
	t.Setenv("TMPDIR", tmp)
	require.NoError(t, os.MkdirAll(filepath.Join(tmp, "jacoco"), 0o755))
	require.NoError(t, os.WriteFile(filepath.Join(tmp, "jacoco", "jacococli.jar"), []byte("cli"), 0o644))
	missing := filepath.Join(t.TempDir(), "no-such-agent.jar")

	got, err := New(t.Context(), zap.NewNop(), nil, "java -jar target/app.jar", missing, "java").PreProcess(false)

	require.ErrorContains(t, err, missing)
	require.Equal(t, "java -jar target/app.jar", got)
}

func TestGetCoverage_JacocoCSV(t *testing.T) {
	t.Chdir("testdata")

	got, err := New(t.Context(), zap.NewNop(), nil, "java -jar target/app.jar", "", "java").GetCoverage()

	require.NoError(t, err)
	// Application has no covered instruction, so it is left out of FileCov but
	// still counts toward the totals.
	require.Equal(t, models.TestCoverage{
		FileCov: map[string]string{
			filepath.Join("com", "example", "app", "OrderService"):  "75.00%",
			filepath.Join("com", "example", "app", "util", "Money"): "100.00%",
		},
		TotalCov: "70.00%",
		Loc:      models.Loc{Total: 60, Covered: 42},
	}, got)
}

func TestAppendCoverage(t *testing.T) {
	db := &recordingReportDB{}
	report := &models.TestCoverage{TotalCov: "70.00%"}

	err := New(t.Context(), zap.NewNop(), db, "java -jar target/app.jar", "", "java").AppendCoverage(report, "test-run-3")

	require.NoError(t, err)
	require.Equal(t, []updateReportCall{{testRunID: "test-run-3", report: report}}, db.calls)
}
