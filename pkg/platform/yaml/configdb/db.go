// Package configdb provides functionality for working with keploy configuration databases.
package configdb

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

type ConfigDb struct {
	logger *zap.Logger
}

func UserHomeDir() string {

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

func NewConfigDb(logger *zap.Logger) *ConfigDb {
	return &ConfigDb{
		logger: logger,
	}
}

func (cdb *ConfigDb) GetInstallationID(ctx context.Context) (string, error) {
	var id string
	id = getInstallationFromFile(cdb.logger)
	if id == "" {
		id = primitive.NewObjectID().String()
		err := cdb.setInstallationID(ctx, id)
		if err != nil {
			return "", fmt.Errorf("failed to set installation id in file. error: %s", err.Error())
		}
	}
	return id, nil
}

func getInstallationFromFile(logger *zap.Logger) string {
	var (
		path = UserHomeDir()
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

func (cdb *ConfigDb) setInstallationID(ctx context.Context, id string) error {
	path := UserHomeDir()
	data := []byte{}

	d, err := yamlLib.Marshal(&id)
	if err != nil {
		return fmt.Errorf("failed to marshal document to yaml. error: %s", err.Error())
	}
	data = append(data, d...)
	// Append in each test case and mock the Keploy version as a comment to facilitate the debugging
	data = append([]byte(utils.GenerateKeployVersionComment()), data...)
	err = yaml.WriteFile(ctx, cdb.logger, path, "installation-id", data, false)
	if err != nil {
		utils.LogError(cdb.logger, err, "failed to write installation id in yaml file")
		return err
	}

	return nil
}
