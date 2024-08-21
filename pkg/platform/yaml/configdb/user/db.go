// Package user provides functionality for working with keploy user configs like installation id.
package user

import (
	"context"
	"os"
	"runtime"

	"github.com/denisbrodbeck/machineid"
	"go.keploy.io/server/v2/config"
	"go.uber.org/zap"
)

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

func New(logger *zap.Logger, cfg *config.Config) *Db {
	return &Db{
		logger: logger,
		cfg:    cfg,
	}
}

func (db *Db) GetInstallationID(_ context.Context) (string, error) {
	var id string
	var err error
	inDocker := os.Getenv("KEPLOY_INDOCKER")
	if inDocker == "true" {
		id = os.Getenv("INSTALLATION_ID")
	} else {
		id, err = machineid.ID()
		if err != nil {
			db.logger.Debug("failed to get machine id", zap.Error(err))
			return "", nil
		}
	}
	if id == "" {
		db.logger.Debug("got empty machine id")
		return "", nil
	}
	return id, nil
}
