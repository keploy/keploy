package fileStore

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"

	"gopkg.in/yaml.v3"
)

// telemetryFS provides interface for create-read installationID for self-hosted keploy
type telemetryFS struct{}

func UserHomeDir() string {
	if runtime.GOOS == "windows" {
		home := os.Getenv("HOMEDRIVE") + os.Getenv("HOMEPATH")
		if home == "" {
			home = os.Getenv("USERPROFILE")
		}
		return home + "/keploy-config"
	}
	return os.Getenv("HOME") + "/keploy-config"
}

func NewTeleFS() *telemetryFS {
	return &telemetryFS{}
}

func (fs *telemetryFS) Get() (string, error) {
	var (
		path = UserHomeDir()
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

func (fs *telemetryFS) Set(id string) error {
	path := UserHomeDir()
	createMockFile(path, "installation-id")

	data := []byte{}

	d, err := yaml.Marshal(&id)
	if err != nil {
		return fmt.Errorf("failed to marshal document to yaml. error: %s", err.Error())
	}
	data = append(data, d...)

	err = os.WriteFile(filepath.Join(path, "installation-id.yaml"), data, os.ModePerm)
	if err != nil {
		return fmt.Errorf("failed to write test report in yaml file. error: %s", err.Error())
	}

	return nil
}
