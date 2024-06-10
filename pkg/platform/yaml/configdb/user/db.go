// Package user provides functionality for working with keploy user configs like installation id.
package user

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"

	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.keploy.io/server/v2/utils"
	"go.mongodb.org/mongo-driver/bson/primitive"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

type Db struct {
	logger *zap.Logger
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

func New(logger *zap.Logger) *Db {
	return &Db{
		logger: logger,
	}
}

func (db *Db) GetInstallationID(ctx context.Context) (string, error) {
	var id string
	id = getInstallationFromFile(db.logger)
	if id == "" {
		id = primitive.NewObjectID().String()
		err := db.setInstallationID(ctx, id)
		if err != nil {
			return "", fmt.Errorf("failed to set installation id in file. error: %s", err.Error())
		}
	}
	return id, nil
}

func getInstallationFromFile(logger *zap.Logger) string {
	var (
		path = HomeDir()
		id   = ""
	)

	file, err := os.OpenFile(filepath.Join(path, "installation-id.yaml"), os.O_RDONLY, fs.ModePerm)
	if err != nil {
		return id
	}
	defer func() {
		if err := file.Close(); err != nil {
			utils.LogError(logger, err, "failed to close file")
		}
	}()
	decoder := yamlLib.NewDecoder(file)
	err = decoder.Decode(&id)
	if errors.Is(err, io.EOF) {
		return id
	}
	if err != nil {
		return id
	}
	return id
}

func (db *Db) setInstallationID(ctx context.Context, id string) error {
	path := HomeDir()
	data := []byte{}

	d, err := yamlLib.Marshal(&id)
	if err != nil {
		return fmt.Errorf("failed to marshal document to yaml. error: %s", err.Error())
	}
	data = append(data, d...)
	err = yaml.WriteFile(ctx, db.logger, path, "installation-id", data, false)
	if err != nil {
		utils.LogError(db.logger, err, "failed to write installation id in yaml file")
		return err
	}

	return nil
}
