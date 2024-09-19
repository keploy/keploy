// Package user provides functionality for working with keploy user configs like installation id.
package user

import (
	"context"
	"os"
	"runtime"

	"github.com/denisbrodbeck/machineid"
	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/utils"
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

func (db *Db) GetInstallationID(_ context.Context) (string, error) {
	var id string
	var err error

	id, err = machineid.ID()
	if err != nil {
		db.logger.Debug("failed to get machine id", zap.Error(err))
		return "", nil
	}

	if id == "" {
		db.logger.Debug("got empty machine id")
		return "", nil
	}
	return id, nil
}
