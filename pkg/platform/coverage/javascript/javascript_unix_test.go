//go:build unix

package javascript

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
)

func TestPreProcess_WithNyc(t *testing.T) {
	tests := []struct {
		name                string
		disableLineCoverage bool
		want                string
	}{
		{name: "line coverage on", disableLineCoverage: false, want: "nyc --clean=$CLEAN npm start"},
		{name: "line coverage off", disableLineCoverage: true, want: "nyc --clean=$CLEAN --reporter=none npm start"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			binDir := t.TempDir()
			require.NoError(t, os.WriteFile(filepath.Join(binDir, "nyc"), []byte("#!/bin/sh\n[ \"$1\" = \"--version\" ] || exit 2\necho 17.1.0\n"), 0o755))
			t.Setenv("PATH", binDir+string(os.PathListSeparator)+os.Getenv("PATH"))

			got, err := New(t.Context(), zap.NewNop(), nil, "npm start").PreProcess(tt.disableLineCoverage)

			require.NoError(t, err)
			require.Equal(t, tt.want, got)
		})
	}
}
