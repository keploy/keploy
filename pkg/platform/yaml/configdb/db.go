package configdb

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"go.keploy.io/server/v2/pkg/platform/yaml"
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

func (cdb *ConfigDb) GetInstallationId(ctx context.Context) (string, error) {
	var id string
	id = getInstallationFromFile()
	if id != "" {
		id = primitive.NewObjectID().String()
		err := cdb.setInstallationId(ctx, id)
		if err != nil {
			return "", fmt.Errorf("failed to set installation id in file. error: %s", err.Error())
		}
	}
	return id, nil
}

func getInstallationFromFile() string {
	var (
		path = UserHomeDir()
		id   = ""
	)

	file, err := os.OpenFile(filepath.Join(path, "installation-id.yaml"), os.O_RDONLY, os.ModePerm)
	if err != nil {
		return id
	}
	defer file.Close()
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

func (cdb *ConfigDb) setInstallationId(ctx context.Context, id string) error {
	path := UserHomeDir()
	_, err := yaml.CreateYamlFile(ctx, cdb.logger, path, "installation-id")
	if err != nil {
		return fmt.Errorf("failed to create yaml file. error: %s", err.Error())
	}

	data := []byte{}

	d, err := yamlLib.Marshal(&id)
	if err != nil {
		return fmt.Errorf("failed to marshal document to yaml. error: %s", err.Error())
	}
	data = append(data, d...)

	err = os.WriteFile(filepath.Join(path, "installation-id.yaml"), data, os.ModePerm)
	if err != nil {
		return fmt.Errorf("failed to write installation id in yaml file. error: %s", err.Error())
	}

	return nil
}
