package fs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"go.uber.org/zap"

	"gopkg.in/yaml.v3"
)

// Telemetry provides interface for create-read installationID for self-hosted keploy
type Telemetry struct {
	logger *zap.Logger
}

func UserHomeDir(isNewConfigPath bool) string {

	var configFolder = "/.keploy-config"
	if isNewConfigPath {
		configFolder = "/.keploy"
	}

	if runtime.GOOS == "windows" {
		home := os.Getenv("HOMEDRIVE") + os.Getenv("HOMEPATH")
		if home == "" {
			home = os.Getenv("USERPROFILE")
		}
		return home + configFolder
	}
	return os.Getenv("HOME") + configFolder
}

func NewTeleFS(logger *zap.Logger) *Telemetry {
	return &Telemetry{
		logger: logger,
	}
}

func (fs *Telemetry) Get(isNewConfigPath bool) (string, error) {
	var (
		path = UserHomeDir(isNewConfigPath)
		id   = ""
	)

	file, err := os.OpenFile(filepath.Join(path, "installation-id.yaml"), os.O_RDONLY, os.ModePerm)
	if err != nil {
		return "", err
	}
	defer file.Close()
	decoder := yaml.NewDecoder(file)
	err = decoder.Decode(&id)
	if errors.Is(err, io.EOF) {
		return id, fmt.Errorf("failed to decode the installation-id yaml. error: %v", err.Error())
	}
	if err != nil {
		return id, fmt.Errorf("failed to decode the installation-id yaml. error: %v", err.Error())
	}

	return id, nil
}

func (fs *Telemetry) Set(id string) error {
	path := UserHomeDir(true)
	createYamlFile(path, "installation-id", fs.logger)

	data := []byte{}

	d, err := yaml.Marshal(&id)
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
