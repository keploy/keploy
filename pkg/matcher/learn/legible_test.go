package learn

import (
	"os"
	"path/filepath"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// Until the learned files are written, none is: no value leaves redaction,
// no leaf is read, and neither the .gitignore nor a learned file is touched.
func TestTheLearnedFilesContractFailsClosed(t *testing.T) {
	if got := RedactLearnedValue("body.note", "ORD-1 paid 100", nil); got != redacted {
		t.Errorf("RedactLearnedValue = %q, want %q", got, redacted)
	}
	dir := t.TempDir()
	set := filepath.Join(dir, "test-set-0")
	src := filepath.Join(dir, "staging")
	for _, d := range []string{set, src} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(src, models.LearnedLocalFile), []byte("[]\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if leaves, err := Legible(set); err == nil || leaves != nil {
		t.Errorf("Legible = %v, %v: want an error and no leaf", leaves, err)
	}
	if err := EnsureLearnedIgnored(dir); err == nil {
		t.Error("EnsureLearnedIgnored succeeded: want an error, so no learned file is written")
	}
	if err := MergeLocal(set, src); err == nil {
		t.Error("MergeLocal succeeded: want an error")
	}
	for _, p := range []string{filepath.Join(dir, ".gitignore"), filepath.Join(set, models.LearnedLocalFile), filepath.Join(set, models.LearnedFile)} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Errorf("%s was written (%v)", p, err)
		}
	}
}
