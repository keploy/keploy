package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"io"
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

// TestExtractBinary_DecompressionBombCapped pins the self-update extraction
// cap (#3867): an archive whose tar stream inflates past the limit must fail
// with pkg.ErrDecompressedTooLarge instead of exhausting disk/RAM. Uses the
// extractBinaryWithLimit seam so the test works with a small archive; the
// production entry point passes maxExtractedArchiveBytes.
func TestExtractBinary_DecompressionBombCapped(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "bomb.tar.gz")
	writeTarGz(t, archive, "keploy", 2*1024*1024) // inflates to ~2 MiB

	err := extractBinaryWithLimit(openArchive(t, archive), "keploy", io.Discard, 1024*1024)
	require.ErrorIs(t, err, pkg.ErrDecompressedTooLarge)
}

// TestExtractBinary_UnderLimitExtracts verifies a legitimate archive under
// the cap still extracts completely.
func TestExtractBinary_UnderLimitExtracts(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "ok.tar.gz")
	const contentSize = 2 * 1024 * 1024
	writeTarGz(t, archive, "keploy", contentSize)

	var out bytes.Buffer
	// Limit leaves headroom for tar framing above the file content.
	require.NoError(t, extractBinaryWithLimit(openArchive(t, archive), "keploy", &out, 4*1024*1024))
	assert.Equal(t, contentSize, out.Len())
}

// TestExtractBinary_WritesNothingElse pins that extraction yields the one
// binary and writes no file anywhere: the update used to extract every entry
// into /tmp under the archive's own names.
func TestExtractBinary_WritesNothingElse(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "two.tar.gz")
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for _, e := range []struct{ name, body string }{{"README.md", "readme"}, {"keploy", "the binary"}} {
		require.NoError(t, tw.WriteHeader(&tar.Header{Name: e.name, Mode: 0o644, Size: int64(len(e.body)), Typeflag: tar.TypeReg}))
		_, err := tw.Write([]byte(e.body))
		require.NoError(t, err)
	}
	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())
	require.NoError(t, os.WriteFile(archive, buf.Bytes(), 0o644))
	before, err := os.ReadDir(dir)
	require.NoError(t, err)

	var out bytes.Buffer
	require.NoError(t, extractBinary(openArchive(t, archive), "keploy", &out))
	assert.Equal(t, "the binary", out.String())
	after, err := os.ReadDir(dir)
	require.NoError(t, err)
	assert.Equal(t, len(before), len(after), "extraction wrote files beside the archive")
}

// TestExtractBinary_RefusesALinkOrAMissingBinary pins that only a regular
// file named keploy is ever taken from the archive.
func TestExtractBinary_RefusesALinkOrAMissingBinary(t *testing.T) {
	dir := t.TempDir()
	link := filepath.Join(dir, "link.tar.gz")
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	require.NoError(t, tw.WriteHeader(&tar.Header{Name: "keploy", Linkname: "/etc/passwd", Typeflag: tar.TypeSymlink}))
	require.NoError(t, tw.Close())
	require.NoError(t, gw.Close())
	require.NoError(t, os.WriteFile(link, buf.Bytes(), 0o644))
	require.Error(t, extractBinary(openArchive(t, link), "keploy", io.Discard))

	other := filepath.Join(dir, "other.tar.gz")
	writeTarGz(t, other, "not-keploy", 16)
	require.Error(t, extractBinary(openArchive(t, other), "keploy", io.Discard))
}

// openArchive opens the archive at path for the duration of the test.
func openArchive(t *testing.T, path string) *os.File {
	t.Helper()
	f, err := os.Open(path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = f.Close() })
	return f
}

// TestCopyCapped pins the self-update download's own bound: a body past the
// cap is refused rather than written out in full before its digest is checked.
func TestCopyCapped(t *testing.T) {
	var out bytes.Buffer
	require.NoError(t, copyCapped(&out, bytes.NewReader(make([]byte, 1024)), 1024))
	assert.Equal(t, 1024, out.Len())
	out.Reset()
	require.Error(t, copyCapped(&out, bytes.NewReader(make([]byte, 1025)), 1024))
	assert.LessOrEqual(t, out.Len(), 1025)
}
