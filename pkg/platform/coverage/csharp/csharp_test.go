//go:build unix

package csharp

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestPreProcess_AddsCoverageCollector(t *testing.T) {
	collector := filepath.Join(t.TempDir(), "dotnet-coverage")
	require.NoError(t, os.WriteFile(collector, []byte("collector"), 0o755))
	tests := []struct {
		name string
		cmd  string
		want string
	}{
		{
			name: "dotnet run",
			cmd:  "dotnet run --project src/App/App.csproj",
			want: "dotnet collect --output target/${TESTSETID}.cobertura --output-format cobertura run --project src/App/App.csproj",
		},
		{
			name: "dotnet with a built dll",
			cmd:  "dotnet bin/Release/net8.0/App.dll",
			want: "dotnet collect --output target/${TESTSETID}.cobertura --output-format cobertura bin/Release/net8.0/App.dll",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// PreProcess runs `dotnet tool list` and `dotnet tool install`; the
			// stub answers both without the .NET SDK.
			binDir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(binDir, "dotnet"), []byte("#!/bin/sh\n[ \"$1\" = \"tool\" ] || exit 2\n"), 0o755))
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))
			t.Setenv("TMPDIR", t.TempDir())

			got, err := New(t.Context(), zap.NewNop(), nil, tt.cmd, collector, "dotnet").PreProcess(false)

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}
