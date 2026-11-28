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
	UpdatePrompt string `yaml:"updatePrompt" json:"updatePrompt"`
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
	// Open the keploy.yaml file
	path := HomeDir() + "/keploy.yaml"
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

// GetInstallationID returns this machine's installation id: a keyed hash of
// the OS machine id, never the id itself, which systemd says must not leave
// the machine (machine-id(5)). In a container keploy started (KEPLOY_INDOCKER),
// it uses the INSTALLATION_ID the host keploy passed when non-empty. When the
// environment has no readable OS machine id (e.g. minimal containers or CI
// runners without /etc/machine-id), it falls back to a cryptographically random
// 64-hex installation id persisted under HomeDir()/installation_id (or
// process-scoped when HomeDir() is unwritable) so OSS installs are never
// dropped as unattributed.
func (db *Db) GetInstallationID(_ context.Context) (string, error) {
	var id string
	inDocker := os.Getenv("KEPLOY_INDOCKER")
	if inDocker == "true" {
		id = strings.TrimSpace(os.Getenv("INSTALLATION_ID"))
	} else {
		raw, err := machineID()
		if err != nil {
			db.logger.Debug("failed to get machine id, falling back to persisted installation id", zap.Error(err))
		} else if raw != "" {
			id = installationID(raw)
		}
	}
	if id == "" {
		id = persistedFallbackInstallationID(db.logger)
	}
	return id, nil
}

const fallbackInstallationIDFile = "installation_id"

var (
	fallbackIDMu sync.Mutex
	ephemeralID  string
)

// persistedFallbackInstallationID loads or creates a random 64-hex installation
// id in HomeDir()/installation_id so container/CI environments without an OS
// machine-id remain stable and valid across runs. If HomeDir() is unavailable
// or read-only, a process-scoped random 64-hex id is returned.
func persistedFallbackInstallationID(logger *zap.Logger) string {
	fallbackIDMu.Lock()
	defer fallbackIDMu.Unlock()

	dir := HomeDir()
	// HomeDir() returns "/.keploy" when HOME/USERPROFILE is unset; avoid writing
	// to root "/.keploy".
	if dir != "" && dir != "/.keploy" {
		p := filepath.Join(dir, fallbackInstallationIDFile)
		if b, err := os.ReadFile(p); err == nil {
			if existing := strings.TrimSpace(string(b)); isValidFallbackID(existing) {
				return existing
			}
		}
		generated := newRandomHexID()
		if err := os.MkdirAll(dir, 0o755); err == nil {
			tmp := p + "." + generated[:8] + ".tmp"
			if err := os.WriteFile(tmp, []byte(generated+"\n"), 0o600); err == nil {
				if err := os.Rename(tmp, p); err == nil {
					return generated
				}
				_ = os.Remove(tmp)
			}
		} else if logger != nil {
			logger.Debug("failed to persist fallback installation id", zap.Error(err))
		}
		if ephemeralID == "" {
			ephemeralID = generated
		}
		return ephemeralID
	}
	if ephemeralID == "" {
		ephemeralID = newRandomHexID()
	}
	return ephemeralID
}

func isValidFallbackID(s string) bool {
	if len(s) != 64 {
		return false
	}
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !((c >= '0' && c <= '9') || (c >= 'a' && c <= 'f')) {
			return false
		}
	}
	return true
}

func newRandomHexID() string {
	var b [32]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
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
