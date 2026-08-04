package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"os"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg"
)

// writeTarGz writes a tar.gz at path containing a single regular file named
// name with contentSize zero bytes.
func writeTarGz(t *testing.T, path, name string, contentSize int64) {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	require.NoError(t, tw.WriteHeader(&tar.Header{
		Name:     name,
		Mode:     0o755,
		Size:     contentSize,
		Typeflag: tar.TypeReg,
	}))
	_, err := tw.Write(make([]byte, contentSize))
	require.NoError(t, err)
	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())
	require.NoError(t, os.WriteFile(path, buf.Bytes(), 0o644))
}

// TestExtractTarGz_DecompressionBombCapped pins the self-update extraction
// cap (#3867): an archive whose tar stream inflates past the limit must fail
// with pkg.ErrDecompressedTooLarge instead of exhausting disk/RAM. Uses the
// extractTarGzWithLimit seam so the test works with a small archive; the
// production entry point passes maxExtractedArchiveBytes.
func TestExtractTarGz_DecompressionBombCapped(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "bomb.tar.gz")
	writeTarGz(t, archive, "keploy", 2*1024*1024) // inflates to ~2 MiB

	outDir := filepath.Join(dir, "out")
	require.NoError(t, os.MkdirAll(outDir, 0o755))
	err := extractTarGzWithLimit(archive, outDir, 1024*1024)
	require.ErrorIs(t, err, pkg.ErrDecompressedTooLarge)
}

// TestExtractTarGz_UnderLimitExtracts verifies a legitimate archive under
// the cap still extracts completely.
func TestExtractTarGz_UnderLimitExtracts(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "ok.tar.gz")
	const contentSize = 2 * 1024 * 1024
	writeTarGz(t, archive, "keploy", contentSize)

	outDir := filepath.Join(dir, "out")
	require.NoError(t, os.MkdirAll(outDir, 0o755))
	// Limit leaves headroom for tar framing above the file content.
	require.NoError(t, extractTarGzWithLimit(archive, outDir, 4*1024*1024))

	info, err := os.Stat(filepath.Join(outDir, "keploy"))
	require.NoError(t, err)
	assert.Equal(t, int64(contentSize), info.Size())
}
