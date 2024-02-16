package hooks

import (
	"context"
	"encoding/binary"
	"errors"
	"net"

	"github.com/docker/docker/client"
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

// GetContainerInfo returns the containerName and networkName from containerId
func GetContainerInfo(containerID string, networkName string) (string, string, error) {

    dockerClient, err := client.NewClientWithOpts(client.FromEnv)
    if err != nil {
        return "", "", err
    }

    containerInfo, err := dockerClient.ContainerInspect(context.Background(), containerID)
    if err != nil {
        return "", "", err
    }

    containerName := containerInfo.Name[1:]

    // Extract network as given by the user
	networkInfo := ""
    for networkInfo, _ = range containerInfo.NetworkSettings.Networks {
        if networkInfo == networkName {
			return containerName, networkName, nil
		}
    }	
	
    return containerName, networkInfo, nil
}
