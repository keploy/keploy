package tls

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"go.uber.org/zap"
)

func TestEphemeralStore_DoesNotPersist(t *testing.T) {
	s := ephemeralStore{}
	if s.persistsKey() {
		t.Fatal("ephemeral store must not report persistence")
	}
	ca, err := s.loadOrGenerate(zap.NewNop())
	if err != nil || ca == nil {
		t.Fatalf("ephemeral loadOrGenerate: ca=%v err=%v", ca, err)
	}
}

func TestFileStore_PersistsAndReuses(t *testing.T) {
	dir := t.TempDir()
	s := &fileStore{dir: dir, label: "test"}
	if !s.persistsKey() {
		t.Fatal("file store must report persistence")
	}

	first, err := s.loadOrGenerate(zap.NewNop())
	if err != nil {
		t.Fatalf("first loadOrGenerate: %v", err)
	}
	// The key file exists and is owner-only on unix.
	keyPath := filepath.Join(dir, caKeyFileName)
	fi, err := os.Lstat(keyPath)
	if err != nil {
		t.Fatalf("key not persisted: %v", err)
	}
	if runtime.GOOS != "windows" && fi.Mode().Perm() != 0600 {
		t.Fatalf("persisted key perms = %o, want 0600", fi.Mode().Perm())
	}

	// A second load reuses the same CA — the property a pod restart relies on.
	second, err := s.loadOrGenerate(zap.NewNop())
	if err != nil {
		t.Fatalf("second loadOrGenerate: %v", err)
	}
	if first.fp != second.fp || first.runID != second.runID {
		t.Fatal("file store minted a new CA instead of reusing the persisted one")
	}
}

func TestFileStore_RegeneratesOnTamper(t *testing.T) {
	dir := t.TempDir()
	s := &fileStore{dir: dir, label: "test"}
	first, err := s.loadOrGenerate(zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	// Corrupt the persisted key: a reload must not trust it and must mint a new CA.
	if err := os.WriteFile(filepath.Join(dir, caKeyFileName), []byte("garbage"), 0600); err != nil {
		t.Fatal(err)
	}
	second, err := s.loadOrGenerate(zap.NewNop())
	if err != nil {
		t.Fatalf("reload after tamper: %v", err)
	}
	if first.fp == second.fp {
		t.Fatal("file store reused a corrupted key instead of regenerating")
	}
	// The regenerated CA must be RE-PERSISTED (the corrupt file removed and
	// rewritten), otherwise the next pod restart would mint yet another CA and
	// defeat per-pod reuse. A third load must return the same CA as the second.
	third, err := s.loadOrGenerate(zap.NewNop())
	if err != nil {
		t.Fatalf("reload after self-heal: %v", err)
	}
	if third.fp != second.fp {
		t.Fatal("file store did not re-persist after healing a corrupt key; it would mint a new CA every restart")
	}
}

func TestIsAppReadableKeyDir(t *testing.T) {
	cases := map[string]bool{
		"/tmp/keploy-tls":         true,
		"/tmp/keploy-tls/sub":     true,
		"/var/lib/keploy-ca":      false,
		"/var/run/keploy-ca":      false,
		"/tmp/keploy-tls-not-sub": false, // sibling prefix, not a subpath
	}
	for dir, want := range cases {
		if got := isAppReadableKeyDir(dir); got != want {
			t.Errorf("isAppReadableKeyDir(%q) = %v, want %v", dir, got, want)
		}
	}
}

func TestSelectCAStore(t *testing.T) {
	log := zap.NewNop()

	// Native: always ephemeral (not docker).
	if _, ok := selectCAStore(log, false).(*ephemeralStore); !ok {
		t.Error("native should select the ephemeral store")
	}

	// In cluster with an agent-only key dir: file store.
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.0.0.1")
	t.Setenv(EnvCAKeyDir, t.TempDir())
	if _, ok := selectCAStore(log, true).(*fileStore); !ok {
		t.Error("in-cluster with a key dir should select the file store")
	}

	// In cluster but the key dir points at the app-readable volume: refuse,
	// fall back to ephemeral (never write the key where the app can read it).
	t.Setenv(EnvCAKeyDir, "/tmp/keploy-tls")
	if _, ok := selectCAStore(log, true).(*ephemeralStore); !ok {
		t.Error("an app-readable key dir must fall back to the ephemeral store")
	}

	// In cluster with no key dir mounted: fall back to ephemeral.
	os.Unsetenv(EnvCAKeyDir)
	if _, ok := selectCAStore(log, true).(*ephemeralStore); !ok {
		t.Error("in-cluster with no key dir should fall back to the ephemeral store")
	}
}
