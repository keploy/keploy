// Package user provides functionality for working with keploy user configs like installation id.
package user

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"os"
	"runtime"

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
// the machine (machine-id(5)); "" when the machine has none. In a container
// keploy started (KEPLOY_INDOCKER), it is the INSTALLATION_ID the host keploy
// passed, already hashed there.
func (db *Db) GetInstallationID(_ context.Context) (string, error) {
	var id string
	inDocker := os.Getenv("KEPLOY_INDOCKER")
	if inDocker == "true" {
		id = os.Getenv("INSTALLATION_ID")
	} else {
		raw, err := machineID()
		if err != nil {
			db.logger.Debug("failed to get machine id", zap.Error(err))
			return "", nil
		}
		if raw != "" {
			id = installationID(raw)
		}
	}
	if id == "" {
		db.logger.Debug("got empty machine id")
		return "", nil
	}
	return id, nil
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
