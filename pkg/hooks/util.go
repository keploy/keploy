package hooks

import (
	"encoding/binary"
	"errors"
	"net"
	"os"

	yaml "gopkg.in/yaml.v3"
)

const mockTable string = "mock"
const configMockTable string = "configMock"
const mockTableIndex string = "id"
const configMockTableIndex string = "id"
const mockTableIndexField string = "Id"
const configMockTableIndexField string = "Id"

// ConvertIPToUint32 converts a string representation of an IPv4 address to a 32-bit integer.
func ConvertIPToUint32(ipStr string) (uint32, error) {
	ipAddr := net.ParseIP(ipStr)
	if ipAddr != nil {
		ipAddr = ipAddr.To4()
		if ipAddr != nil {
			return binary.BigEndian.Uint32(ipAddr), nil
		} else {
			return 0, errors.New("not a valid IPv4 address")
		}
	} else {
		return 0, errors.New("failed to parse IP address")
	}
}

// getContainerFromComposeFile parses docker-compose file to get all container names
func getContainerFromComposeFile(filePath string) ([]string, error) {

	yamlFile, err := os.ReadFile(filePath)
	if err != nil {
		return nil, err
	}

	var composeConfig struct {
		Services map[string]struct {
			ContainerName string `yaml:"container_name"`
		} `yaml:"services"`
	}

	if err := yaml.Unmarshal(yamlFile, &composeConfig); err != nil {
		return nil, err
	}

	var containerNames []string
	for _, serviceConfig := range composeConfig.Services {
		containerNames = append(containerNames, serviceConfig.ContainerName)
	}

	return containerNames, nil
}
