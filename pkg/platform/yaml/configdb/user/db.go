// Package user provides functionality for working with keploy user configs like installation id.
package user

import (
	"context"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"

	"github.com/denisbrodbeck/machineid"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"gopkg.in/yaml.v2"
	yamlLib "gopkg.in/yaml.v3"
)

type KeployConfig struct {
	UpdatePrompt   string `yaml:"updatePrompt" json:"updatePrompt"`
	InstallationID string `yaml:"installationID,omitempty" json:"installationID,omitempty"`
}

type Db struct {
	logger *zap.Logger
	cfg    *config.Config
}

func HomeDir() string {
	configFolder := "/.keploy"
	if runtime.GOOS == "windows" {
		home := os.Getenv("HOMEDRIVE") + os.Getenv("HOMEPATH")
		if home == "" {
			home = os.Getenv("USERPROFILE")
		}
		return home + configFolder
	}
	return os.Getenv("HOME") + configFolder
}

func (db *Db) ReadKeployConfig() (*KeployConfig, error) {
	path := HomeDir() + "/keploy.yaml"
	content, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	// Decode the yaml file
	var data KeployConfig
	err = yaml.Unmarshal(content, &data)
	if err != nil {
		utils.LogError(db.logger, err, "failed to unmarshal keploy.yaml")
		return nil, err
	}
	return &data, nil
}

func (db *Db) WriteKeployConfig(data *KeployConfig) error {
	// Ensure the config directory exists before opening keploy.yaml.
	dir := HomeDir()
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	path := filepath.Join(dir, "keploy.yaml")
	file, err := os.OpenFile(path, os.O_RDWR|os.O_CREATE, 0644)
	if err != nil {
		return err
	}
	defer func() {
		if err := file.Close(); err != nil {
			db.logger.Error("failed to close file", zap.Error(err))
		}
	}()
	updatedData, err := yamlLib.Marshal(data)
	if err != nil {
		return err
	}
	// Truncate the file before writing to it.
	err = file.Truncate(0)
	if err != nil {
		return err
	}
	_, err = file.Write(updatedData)
	if err != nil {
		return err
	}
	return nil
}

func New(logger *zap.Logger, cfg *config.Config) *Db {
	return &Db{
		logger: logger,
		cfg:    cfg,
	}
}

// machineID reads the OS machine id (/etc/machine-id, IOPlatformUUID,
// MachineGuid). A variable so tests can give one.
var machineID = machineid.ID

// installationAppID keys the installation id to keploy (installationID).
const installationAppID = "keploy"

// processFallbackIDMu guards processFallbackID when neither machineid nor a
// writable ~/.keploy/keploy.yaml is available (for example, a read-only
// container filesystem).
var (
	processFallbackIDMu sync.Mutex
	processFallbackID   string
)

// GetInstallationID returns this machine's installation id: a keyed hash of
// the OS machine id, never the id itself, which systemd says must not leave
// the machine (machine-id(5)). In a container keploy started (KEPLOY_INDOCKER),
// it uses the INSTALLATION_ID the host keploy passed when present. When neither
// a host-passed ID nor an OS machine id is available (containers, CI runners
// without /etc/machine-id, or restricted environments), it loads or persists a
// randomly generated 64-hex fallback ID in ~/.keploy/keploy.yaml (or caches
// one in memory if the home directory is read-only) so OSS events still carry
// a stable, unique installation id instead of falling back to "unattributed".
func (db *Db) GetInstallationID(_ context.Context) (string, error) {
	if os.Getenv("KEPLOY_INDOCKER") == "true" {
		if id := strings.TrimSpace(os.Getenv("INSTALLATION_ID")); id != "" {
			return id, nil
		}
		return db.fallbackInstallationID(), nil
	}
	raw, err := machineID()
	if err == nil && strings.TrimSpace(raw) != "" {
		return installationID(raw), nil
	}
	if err != nil && db.logger != nil {
		db.logger.Debug("failed to get machine id; using persisted fallback installation id", zap.Error(err))
	}
	return db.fallbackInstallationID(), nil
}

// fallbackInstallationID returns a persisted random 64-hex installation ID from
// ~/.keploy/keploy.yaml, creating and saving one on first call. If reading and
// writing keploy.yaml both fail (for example in a read-only container), it
// falls back to a process-scoped random 64-hex ID.
func (db *Db) fallbackInstallationID() string {
	if cfg, err := db.ReadKeployConfig(); err == nil && cfg != nil {
		if id := strings.TrimSpace(cfg.InstallationID); id != "" {
			return id
		}
	}
	id := randomInstallationID()
	cfg, err := db.ReadKeployConfig()
	if err != nil || cfg == nil {
		cfg = &KeployConfig{}
	}
	cfg.InstallationID = id
	if err := db.WriteKeployConfig(cfg); err == nil {
		return id
	} else if db.logger != nil {
		db.logger.Debug("failed to persist fallback installation id; using process-scoped fallback", zap.Error(err))
	}
	processFallbackIDMu.Lock()
	defer processFallbackIDMu.Unlock()
	if processFallbackID == "" {
		processFallbackID = id
	}
	return processFallbackID
}

// randomInstallationID returns a fresh 64-char hex string from crypto/rand.
func randomInstallationID() string {
	var buf [32]byte
	if _, err := rand.Read(buf[:]); err != nil {
		return installationID("keploy-fallback")
	}
	return hex.EncodeToString(buf[:])
}

// installationID is the id keploy sends for the machine whose OS id is raw:
// the hex HMAC-SHA256 of installationAppID keyed by raw, which is
// machineid.ProtectedID("keploy"). It names the machine alike on every run
// and cannot be turned back into raw; whoever holds a raw id an older keploy
// sent can compute it, so the two can be joined.
func installationID(raw string) string {
	mac := hmac.New(sha256.New, []byte(raw))
	mac.Write([]byte(installationAppID))
	return hex.EncodeToString(mac.Sum(nil))
}
