package cli

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"go.keploy.io/server/v2/pkg/platform/yaml"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

func ExtractInstallationId(isNewConfigPath bool) (string, error) {
	var (
		path = utils.FetchHomeDirectory(isNewConfigPath)
	)
	file, err := os.OpenFile(filepath.Join(path, "installation-id.yaml"), os.O_RDONLY, os.ModePerm)
	if err != nil {
		return "", err
	}
	defer file.Close()
	decoder := yamlLib.NewDecoder(file)
	var installationId string
	err = decoder.Decode(&installationId)

	if errors.Is(err, io.EOF) {
		return "", fmt.Errorf("failed to decode the installation-id yaml. error: %v", err.Error())
	}
	if err != nil {
		return "", fmt.Errorf("failed to decode the installation-id yaml. error: %v", err.Error())
	}
	return installationId, nil
}

func GenerateTelemetryConfigFile(logger *zap.Logger, id string) error {
	path := utils.FetchHomeDirectory(true)
	yaml.CreateYamlFile(path, "installation-id", logger)

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
