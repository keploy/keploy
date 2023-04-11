package fs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"
)

// telemetry provides interface for create-read installationID for self-hosted keploy
type telemetry struct{}

func UserHomeDir(isNewConfigPath bool) string {

	var configFolder = "/keploy-config"
	if isNewConfigPath {
		configFolder = "/.keploy-config"
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

func NewTeleFS() *telemetry {
	return &telemetry{}
}

func (fs *telemetry) Get(isNewConfigPath bool) (string, error) {
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

func (fs *telemetry) Set(id string) error {
	path := UserHomeDir(true)
	createMockFile(path, "installation-id")

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
