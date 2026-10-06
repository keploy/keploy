package tls

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"

	"go.uber.org/zap"
)

// EnvCAKeyDir names an agent-only directory where the MITM CA private key is
// persisted so it survives an agent-container restart within the same pod. The
// Kubernetes webhook / pod builder sets it to a sidecar-only emptyDir (e.g.
// /var/lib/keploy-ca) that the application container never mounts. It must never
// point at /tmp/keploy-tls (the app-readable shared volume): if it is unset in a
// cluster, the agent keeps the key in memory instead of writing it somewhere the
// app could read.
const EnvCAKeyDir = "KEPLOY_CA_KEY_DIR"

// caKeyFileName / caCertFileName are the persisted CA material inside a key dir.
const (
	caKeyFileName  = "ca.key"
	caCertFileName = "ca.crt"
)

// caStore abstracts where this run's CA signing key lives.
type caStore interface {
	// loadOrGenerate returns the CA for this run, reusing a persisted one when
	// the backend keeps the key on disk, otherwise minting a fresh one.
	loadOrGenerate(logger *zap.Logger) (*caState, error)
	// persistsKey reports whether the key is kept across runs. It drives whether
	// native teardown removes the system-store install: an ephemeral run removes
	// what it installed, while a persisted per-user Windows CA keeps its install
	// so the user is not re-prompted on the next run.
	persistsKey() bool
}

// selectCAStore picks the storage backend for this process.
//
//   - Kubernetes sidecar with an agent-only key dir mounted: persist the key
//     there so a sidecar restart within the pod reuses the same CA and the
//     running app keeps trusting its leaves. If we are in a cluster but no
//     agent-only dir was mounted, fall back to in-memory (never write the key to
//     an app-readable volume) and say so loudly.
//   - native Windows: persist per-user so the ROOT-store trust prompt fires at
//     most once rather than every run.
//   - everything else (native Linux, native macOS, docker run / compose): keep
//     the key in memory only — pure per-run.
func selectCAStore(logger *zap.Logger, isDocker bool) caStore {
	if isDocker && os.Getenv("KUBERNETES_SERVICE_HOST") != "" {
		dir := os.Getenv(EnvCAKeyDir)
		switch {
		case dir == "":
			logger.Warn("running as a Kubernetes agent but "+EnvCAKeyDir+" is not set; "+
				"keeping the MITM CA key in memory only. A sidecar restart will mint a new CA and "+
				"break TLS for apps already running in the pod. Upgrade the k8s-proxy chart so it mounts "+
				"an agent-only key volume and sets "+EnvCAKeyDir+".",
				zap.String("next_step", "bump keployAgentImage and the chart to a version that mounts keploy-ca-key"))
		case isAppReadableKeyDir(dir):
			logger.Error("refusing to persist the MITM CA private key under "+dir+
				": that path is the app-readable shared volume. Keeping the key in memory instead.",
				zap.String(EnvCAKeyDir, dir))
		default:
			return &fileStore{dir: dir, label: "kubernetes per-pod"}
		}
		return &ephemeralStore{}
	}

	if runtime.GOOS == "windows" {
		if dir := windowsCAKeyDir(logger); dir != "" {
			return &fileStore{dir: dir, label: "windows per-user"}
		}
	}

	return &ephemeralStore{}
}

// isAppReadableKeyDir guards against ever persisting the key where the
// application container can read it. /tmp/keploy-tls is the shared volume the
// app mounts read-only, so a key dir at or under it is forbidden.
func isAppReadableKeyDir(dir string) bool {
	clean := filepath.Clean(dir)
	return clean == "/tmp/keploy-tls" || isSubPath("/tmp/keploy-tls", clean)
}

func isSubPath(parent, child string) bool {
	rel, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator))
}

// windowsCAKeyDir returns a per-user directory for the persisted CA on Windows,
// or "" if none can be resolved (in which case the caller falls back to an
// ephemeral in-memory CA).
func windowsCAKeyDir(logger *zap.Logger) string {
	base := os.Getenv("LOCALAPPDATA")
	if base == "" {
		var err error
		base, err = os.UserConfigDir()
		if err != nil {
			logger.Debug("could not resolve a per-user config dir for the CA key; using an ephemeral CA", zap.Error(err))
			return ""
		}
	}
	return filepath.Join(base, "Keploy", "ca")
}

// ephemeralStore keeps the CA private key in memory only. Nothing is written to
// disk, so there is no key at rest and nothing to clean up.
type ephemeralStore struct{}

func (ephemeralStore) loadOrGenerate(_ *zap.Logger) (*caState, error) { return generateRunCA() }
func (ephemeralStore) persistsKey() bool                              { return false }

// fileStore loads a persisted CA from an agent-only directory, generating and
// persisting a fresh one the first time. The key file is created 0600 with
// O_EXCL|O_NOFOLLOW under a 0700 directory; a reload verifies ownership and mode
// before trusting the key (see writeKeyFileSecurely / verifyKeyFileSecure).
type fileStore struct {
	dir   string
	label string
}

func (fileStore) persistsKey() bool { return true }

func (s *fileStore) loadOrGenerate(logger *zap.Logger) (*caState, error) {
	if err := ensureKeyDir(s.dir); err != nil {
		logger.Warn("could not prepare the CA key directory; using an ephemeral in-memory CA",
			zap.String("dir", s.dir), zap.Error(err))
		return generateRunCA()
	}

	keyPath := filepath.Join(s.dir, caKeyFileName)
	certPath := filepath.Join(s.dir, caCertFileName)

	if ca, ok := s.tryLoad(logger, keyPath, certPath); ok {
		logger.Debug("reusing the persisted "+s.label+" MITM CA", zap.String("run_id", ca.runID))
		return ca, nil
	}

	ca, err := generateRunCA()
	if err != nil {
		return nil, err
	}
	// Remove any existing key first so a torn/corrupt file (one that passed the
	// ownership check but failed to parse) does not make the O_EXCL create below
	// fail — which would otherwise leave the bad file in place and force a fresh
	// CA on every restart, defeating the per-pod reuse this backend exists for.
	if err := os.Remove(keyPath); err != nil && !os.IsNotExist(err) {
		logger.Debug("could not remove a stale CA key before regenerating", zap.String("path", keyPath), zap.Error(err))
	}
	if err := writeKeyFileSecurely(keyPath, ca.keyPEM); err != nil {
		// Do not fall back to persisting the key insecurely. Keep this run's CA
		// in memory; the next restart will mint another and log the same warning.
		logger.Warn("could not persist the MITM CA key securely; using it in memory for this run only",
			zap.String("path", keyPath), zap.Error(err))
		return ca, nil
	}
	if err := os.WriteFile(certPath, ca.certPEM, 0644); err != nil {
		logger.Debug("could not write the persisted CA certificate", zap.String("path", certPath), zap.Error(err))
	}
	logger.Info("generated and persisted the "+s.label+" MITM CA",
		zap.String("run_id", ca.runID), zap.String("dir", s.dir))
	return ca, nil
}

// tryLoad returns the persisted CA when both files are present, secure and
// parse. Any problem returns ok=false so the caller mints a fresh one.
func (s *fileStore) tryLoad(logger *zap.Logger, keyPath, certPath string) (*caState, bool) {
	if err := verifyKeyFileSecure(keyPath); err != nil {
		if !os.IsNotExist(err) {
			logger.Warn("ignoring a persisted CA key that is not owner-only; regenerating",
				zap.String("path", keyPath), zap.Error(err))
		}
		return nil, false
	}
	keyPEM, err := os.ReadFile(keyPath)
	if err != nil {
		return nil, false
	}
	certPEM, err := os.ReadFile(certPath)
	if err != nil {
		return nil, false
	}
	ca, err := caStateFromPEM(certPEM, keyPEM)
	if err != nil {
		logger.Warn("ignoring an unparseable persisted CA; regenerating", zap.String("dir", s.dir), zap.Error(err))
		return nil, false
	}
	return ca, true
}

// ensureKeyDir creates dir 0700 under the process-wide umask 0 (main.go), where
// a bare MkdirAll would leave it world-accessible.
func ensureKeyDir(dir string) error {
	if err := os.MkdirAll(dir, 0700); err != nil {
		return fmt.Errorf("failed to create CA key dir: %w", err)
	}
	if err := os.Chmod(dir, 0700); err != nil {
		return fmt.Errorf("failed to tighten CA key dir perms: %w", err)
	}
	return nil
}

// loadOrGenerateActiveCA selects the backend, establishes the run CA and
// publishes it as the active CA. It returns the CA so the caller can install its
// public half and hand it to the client.
func loadOrGenerateActiveCA(logger *zap.Logger, isDocker bool) (*caState, caStore, error) {
	store := selectCAStore(logger, isDocker)
	ca, err := store.loadOrGenerate(logger)
	if err != nil {
		return nil, nil, fmt.Errorf("failed to establish the MITM CA: %w", err)
	}
	setActiveCA(ca)
	return ca, store, nil
}
