//go:build windows

package tls

import (
	"bytes"
	"crypto/sha256"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/windows"
)

// TestLegacyTempSweepWindowsSkipsALinkAndADir: on Windows the sweep's open
// takes a reparse point itself (FILE_FLAG_OPEN_REPARSE_POINT). A ca.crt*
// symlink to the very CA outside the temp dir, and a ca.crt* directory, are
// skipped with a debug line and kept, the link's target untouched, while a
// real leaked copy is still read and removed.
func TestLegacyTempSweepWindowsSkipsALinkAndADir(t *testing.T) {
	tmp := t.TempDir()
	outside := t.TempDir()
	// os.TempDir on Windows reads TMP, then TEMP, never TMPDIR.
	t.Setenv("TMP", tmp)
	t.Setenv("TEMP", tmp)
	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	fp := sha256.Sum256(ca.der)

	target := filepath.Join(outside, "target.pem")
	if err := os.WriteFile(target, ca.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(tmp, "ca.crt")
	if err := os.Symlink(target, link); err != nil {
		if errors.Is(err, windows.ERROR_PRIVILEGE_NOT_HELD) {
			t.Skipf("creating a symlink needs admin or developer mode here: %v", err)
		}
		t.Fatal(err)
	}
	dir := filepath.Join(tmp, "ca.crt.d")
	if err := os.Mkdir(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	leaked := filepath.Join(tmp, "ca.crt.leaked")
	if err := os.WriteFile(leaked, ca.certPEM, 0o644); err != nil {
		t.Fatal(err)
	}

	core, logs := observer.New(zap.DebugLevel)
	removeLegacyTempFiles(zap.New(core), fp, false)

	for _, p := range []string{link, dir} {
		if _, err := os.Lstat(p); err != nil {
			t.Errorf("%s must be left in place: %v", p, err)
		}
		named := false
		for _, e := range logs.All() {
			if v, ok := e.ContextMap()["path"].(string); ok && v == p && strings.Contains(e.Message, "skipped") {
				named = true
			}
		}
		if !named {
			t.Errorf("no debug line names the skipped entry %s", p)
		}
	}
	if fi, err := os.Lstat(link); err == nil && fi.Mode()&os.ModeSymlink == 0 {
		t.Errorf("%s is no longer a symlink", link)
	}
	if data, err := os.ReadFile(target); err != nil || !bytes.Equal(data, ca.certPEM) {
		t.Errorf("the link's target must be left untouched (err: %v)", err)
	}
	if _, err := os.Lstat(leaked); !os.IsNotExist(err) {
		t.Errorf("a real leaked copy of the CA must still be removed; lstat: %v", err)
	}

	// The open half, for an entry swapped in after the Lstat: the link
	// opened itself is not regular, and a regular file is read whole.
	if data, err := readOpenedRegular(link, maxLegacyTempCertBytes); !errors.Is(err, errTempEntryNotRegular) {
		t.Errorf("the open must take the link itself and refuse it, got %d bytes, err %v", len(data), err)
	}
	if data, err := readOpenedRegular(target, maxLegacyTempCertBytes); err != nil || !bytes.Equal(data, ca.certPEM) {
		t.Errorf("a regular file must be read whole, got %d bytes, err %v", len(data), err)
	}
}
