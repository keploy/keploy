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
	yamlV "gopkg.in/yaml.v2"

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
	err = yaml.WriteFile(ctx, cdb.logger, path, "installation-id", data, false)
	if err != nil {
		utils.LogError(cdb.logger, err, "failed to write installation id in yaml file")
		return err
	}

	return nil
}

func (cdb *ConfigDb) ReadAccessToken(_ context.Context) (string, error) {
	// read from cred.yaml
	filePath := getTokenPath()

	file, err := os.OpenFile(filePath, os.O_RDONLY, os.ModePerm)
	if err != nil {
		return "", fmt.Errorf("keploy access token file not found at %s", filePath)
	}
	defer func() {
		err := file.Close()
		if err != nil {
			utils.LogError(cdb.logger, err, "failed to close file")
		}
	}()
	decoder := yamlV.NewDecoder(file)
	var apiKey string
	err = decoder.Decode(&apiKey)
	if errors.Is(err, io.EOF) || apiKey == "" {
		return "", nil
	}

	return apiKey, nil
}

func getTokenPath() string {
	path := UserHomeDir()
	fileName := "token.yaml"
	filePath := filepath.Join(path, fileName)
	return filePath
}

func (cdb *ConfigDb) WriteAccessToken(ctx context.Context, token string) error {
	filePath := getTokenPath()

	// Check if the file exists; if not, create it
	_, err := os.Stat(filePath)
	if err != nil && os.IsNotExist(err) {
		path := filepath.Dir(filePath)
		_, err := yaml.CreateYamlFile(ctx, cdb.logger, path, "token")
		if err != nil {
			fileName := filepath.Base(filePath)
			return fmt.Errorf("failed to create file %s. error: %s", fileName, err.Error())
		}
	} else if err != nil {
		fileName := filepath.Base(filePath)
		return fmt.Errorf("unable to access the keploy file %s. error: %s", fileName, err.Error())

	}
	d, err := yamlV.Marshal(&token)
	if err != nil {
		return fmt.Errorf("failed to marshal document to yaml. error: %s", err.Error())
	}
	err = os.WriteFile(filePath, d, os.ModePerm)
	if err != nil {
		return fmt.Errorf("failed to write token in the yaml file. Please check the Unix permissions error: %s", err.Error())
	}
	return nil
}
