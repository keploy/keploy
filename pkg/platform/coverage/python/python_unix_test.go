//go:build unix

package python

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// installStub puts an executable shell script named name first on PATH.
func installStub(t *testing.T, name, script string) {
	t.Helper()
	binDir := t.TempDir()
	require.NoError(t, os.WriteFile(filepath.Join(binDir, name), []byte("#!/bin/sh\n"+script), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestPreProcess_WithCoverageTool(t *testing.T) {
	tests := []struct {
		name       string
		cmd        string
		executable string
		want       string
	}{
		{
			name:       "executable is its own field after an env assignment",
			cmd:        "FOO=1 python3 app.py --port 80",
			executable: "python3",
			want:       "FOO=1 python3 -m coverage run app.py --port 80",
		},
		{
			name:       "executable is not a separate field",
			cmd:        "/usr/bin/python3 app.py",
			executable: "python3",
			want:       "/usr/bin/python3 -m coverage run app.py",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installStub(t, "coverage", "exit 0\n")
			t.Chdir(t.TempDir())

			got, err := New(t.Context(), zap.NewNop(), nil, tt.cmd, tt.executable).PreProcess(false)

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}

func TestGetCoverage_JSONReport(t *testing.T) {
	report, err := filepath.Abs(filepath.Join("testdata", "coverage.json"))
	require.NoError(t, err)
	// The stub answers `python3 -m coverage combine|json ...`; json writes the
	// fixture where coverage.py writes its report, ./coverage.json.
	installStub(t, "python3", `[ "$1 $2" = "-m coverage" ] || exit 2
case "$3" in
combine) exit 0 ;;
json) exec cp '`+report+`' coverage.json ;;
esac
exit 2
`)
	t.Chdir(t.TempDir())
	require.NoError(t, os.WriteFile(".coverage.keploy.host.1234.567890", nil, 0o644))

	got, err := New(t.Context(), zap.NewNop(), nil, "python3 app.py", "python3").GetCoverage()

	require.NoError(t, err)
	require.Equal(t, models.TestCoverage{
		FileCov: map[string]string{
			"app.py":         "75%",
			"models/user.py": "33%",
		},
		TotalCov: "64%",
		Loc:      models.Loc{Total: 11, Covered: 7},
	}, got)
}
