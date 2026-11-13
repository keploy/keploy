//go:build unix

package golang

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// installStubGo puts a `go` first on PATH that answers
// `go tool covdata textfmt -i=DIR -o=FILE` by copying a testdata profile to
// FILE, and fails on any other arguments.
func installStubGo(t *testing.T, profile string) {
	t.Helper()
	profilePath, err := filepath.Abs(filepath.Join("testdata", profile))
	require.NoError(t, err)
	binDir := t.TempDir()
	script := fmt.Sprintf("#!/bin/sh\n[ \"$1 $2 $3\" = \"tool covdata textfmt\" ] || exit 2\nexec cp '%s' \"${5#-o=}\"\n", profilePath)
	require.NoError(t, os.WriteFile(filepath.Join(binDir, "go"), []byte(script), 0o755))
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
}

func TestGetCoverage_TextProfile(t *testing.T) {
	tests := []struct {
		name    string
		profile string
		want    models.TestCoverage
		wantErr string
	}{
		{
			name:    "one file fully covered and one half covered",
			profile: "two-files.txt",
			want: models.TestCoverage{
				FileCov: map[string]string{
					"example.com/app/full.go": "100.00%",
					"example.com/app/half.go": "50.00%",
				},
				TotalCov: "71.43%",
				Loc:      models.Loc{Total: 7, Covered: 5},
			},
		},
		{
			name:    "profile line with two fields",
			profile: "two-fields.txt",
			wantErr: "go coverage file is malformed at 2",
		},
		{
			name:    "profile with only the mode line",
			profile: "mode-only.txt",
			want:    models.TestCoverage{FileCov: map[string]string{}, TotalCov: "0.00%"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			installStubGo(t, tt.profile)
			coverDir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(coverDir, "covmeta.0"), nil, 0o644))
			t.Setenv("GOCOVERDIR", coverDir)

			got, err := New(t.Context(), zap.NewNop(), nil, "go run -cover .", "", string(utils.Native)).GetCoverage()

			if tt.wantErr != "" {
				require.EqualError(t, err, tt.wantErr)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}
