// Package user provides functionality for working with keploy user configs like installation id.
package user

import (
	"context"
	"errors"
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
	if db.cfg.InDocker {
		id = os.Getenv("INSTALLATION_ID")
	} else {
		id, err = machineid.ID()
		if err != nil {
			return "", errors.New("failed to get machine id")
		}
	}
	if id == "" {
		db.logger.Error("got empty machine id")
		return "", errors.New("empty machine id")
	}
	return id, nil
}
