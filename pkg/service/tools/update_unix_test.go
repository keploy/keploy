//go:build !windows

package tools

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"go.uber.org/zap"
)

// The self-update over a real installed binary. Unix only: file modes are
// what these pin, and Windows has no update archive to install.

// installedKeploy puts an installed keploy, with mode, alone on PATH, and
// returns its path.
func installedKeploy(t *testing.T, mode os.FileMode) string {
	t.Helper()
	dir := t.TempDir()
	path := filepath.Join(dir, "keploy")
	if err := os.WriteFile(path, []byte("old keploy"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, mode); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", dir)
	return path
}

// releaseArchive is a release tar.gz holding entries (name to contents).
func releaseArchive(t *testing.T, entries map[string]string) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	for name, body := range entries {
		if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg}); err != nil {
			t.Fatal(err)
		}
		if _, err := tw.Write([]byte(body)); err != nil {
			t.Fatal(err)
		}
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gw.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// serveArchive serves archive and returns its URL and sha256.
func serveArchive(t *testing.T, archive []byte) (string, string) {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) { _, _ = w.Write(archive) }))
	t.Cleanup(srv.Close)
	sum := sha256.Sum256(archive)
	return srv.URL + "/keploy_linux_amd64.tar.gz", hex.EncodeToString(sum[:])
}

// requireOnlyTheBinary fails unless the installed keploy is alone in its
// directory: no staged file left beside it.
func requireOnlyTheBinary(t *testing.T, installed string) {
	t.Helper()
	entries, err := os.ReadDir(filepath.Dir(installed))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "keploy" {
		var names []string
		for _, e := range entries {
			names = append(names, e.Name())
		}
		t.Errorf("the install directory holds %v, want only keploy", names)
	}
}

// An update installs the new binary 0755 -- never world-writable, whatever
// the umask, and over an install an older update left 0777 -- in place of the
// old one, through no fixed path in /tmp.
func TestDownloadAndUpdate_InstallsTheBinaryNotWorldWritable(t *testing.T) {
	installed := installedKeploy(t, 0o777)
	_, tmpKeployBefore := os.Lstat("/tmp/keploy")
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })
	url, sum := serveArchive(t, releaseArchive(t, map[string]string{"keploy": "new keploy", "README.md": "readme"}))

	if err := (&Tools{}).downloadAndUpdate(context.Background(), zap.NewNop(), url, sum); err != nil {
		t.Fatalf("downloadAndUpdate: %v", err)
	}
	if b, _ := os.ReadFile(installed); string(b) != "new keploy" {
		t.Errorf("installed keploy holds %q, want the new binary", b)
	}
	fi, err := os.Stat(installed)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o755 {
		t.Errorf("installed keploy mode %v, want -rwxr-xr-x", fi.Mode().Perm())
	}
	requireOnlyTheBinary(t, installed)
	if _, err := os.Lstat("/tmp/keploy"); tmpKeployBefore != nil && err == nil {
		t.Error("the update created /tmp/keploy")
	}
}

// A download that is not the bytes the release publishes is not installed.
func TestDownloadAndUpdate_RefusesADownloadThatIsNotTheRelease(t *testing.T) {
	installed := installedKeploy(t, 0o755)
	url, _ := serveArchive(t, releaseArchive(t, map[string]string{"keploy": "tampered keploy"}))

	err := (&Tools{}).downloadAndUpdate(context.Background(), zap.NewNop(), url, strings.Repeat("0", 64))
	if err == nil || !strings.Contains(err.Error(), "refusing to install") {
		t.Fatalf("downloadAndUpdate = %v, want a refusal of the mismatched download", err)
	}
	if b, _ := os.ReadFile(installed); string(b) != "old keploy" {
		t.Errorf("installed keploy holds %q after a refused update, want it untouched", b)
	}
	requireOnlyTheBinary(t, installed)
}

// An archive without the binary leaves the install as it was, and nothing
// staged beside it.
func TestDownloadAndUpdate_LeavesNothingBehindOnFailure(t *testing.T) {
	installed := installedKeploy(t, 0o755)
	url, sum := serveArchive(t, releaseArchive(t, map[string]string{"README.md": "readme"}))

	if err := (&Tools{}).downloadAndUpdate(context.Background(), zap.NewNop(), url, sum); err == nil {
		t.Fatal("downloadAndUpdate installed an archive with no keploy in it")
	}
	if b, _ := os.ReadFile(installed); string(b) != "old keploy" {
		t.Errorf("installed keploy holds %q, want it untouched", b)
	}
	requireOnlyTheBinary(t, installed)
}
