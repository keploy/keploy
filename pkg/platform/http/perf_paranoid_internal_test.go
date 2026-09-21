package http

// Unit tests for relaxPerfEventParanoid. This is an internal test (package http)
// because the helper and its knob path are unexported. The real procfs knob
// requires root and mutates the host, so the tests point the helper at a temp
// file — the read/skip/write logic is identical regardless of the path.

import (
	"os"
	"path/filepath"
	"testing"

	"go.uber.org/zap"
)

func TestRelaxPerfEventParanoid_LowersWhenTooRestrictive(t *testing.T) {
	path := filepath.Join(t.TempDir(), "perf_event_paranoid")
	if err := os.WriteFile(path, []byte("4\n"), 0644); err != nil {
		t.Fatalf("seed knob: %v", err)
	}

	relaxPerfEventParanoid(zap.NewNop(), path)

	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read knob: %v", err)
	}
	if string(got) != "2\n" {
		t.Fatalf("expected knob lowered to %q, got %q", "2\n", string(got))
	}
}

func TestRelaxPerfEventParanoid_SkipsWhenAlreadyPermissive(t *testing.T) {
	// Values already <= 2 must be left untouched — the helper must not need
	// write permission (or clobber a more permissive value) when there's
	// nothing to do. Using "1" (more permissive than the target 2) proves the
	// write is skipped rather than silently rewriting to "2".
	for _, cur := range []string{"-1\n", "0\n", "1\n", "2\n"} {
		path := filepath.Join(t.TempDir(), "perf_event_paranoid")
		if err := os.WriteFile(path, []byte(cur), 0644); err != nil {
			t.Fatalf("seed knob: %v", err)
		}

		relaxPerfEventParanoid(zap.NewNop(), path)

		got, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read knob: %v", err)
		}
		if string(got) != cur {
			t.Fatalf("value %q should be left untouched, got %q", cur, string(got))
		}
	}
}

func TestRelaxPerfEventParanoid_BestEffortOnUnwritablePath(t *testing.T) {
	// A read failure followed by a write failure (parent dir does not exist)
	// must not panic and must not propagate — the helper is best-effort so a
	// locked-down /proc/sys or non-root caller can never abort Setup.
	path := filepath.Join(t.TempDir(), "no-such-dir", "perf_event_paranoid")

	// Must simply return without panicking.
	relaxPerfEventParanoid(zap.NewNop(), path)

	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("helper should not have created %q; stat err = %v", path, err)
	}
}
