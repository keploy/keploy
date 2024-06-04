package docker

import (
	"fmt"
	"regexp"

	"go.keploy.io/server/v2/utils"
)

func ParseDockerCmd(cmd string, kind utils.CmdType, idc Client) (string, string, error) {

	// Regular expression patterns
	var containerNamePattern string
	switch kind {
	case utils.DockerStart:
		containerNamePattern = `start\s+(?:-[^\s]+\s+)*([^\s]*)`
	default:
		containerNamePattern = `--name\s+([^\s]+)`
	}

	networkNamePattern := `(--network|--net)\s+([^\s]+)`

	// Extract container name
	containerNameRegex := regexp.MustCompile(containerNamePattern)
	containerNameMatches := containerNameRegex.FindStringSubmatch(cmd)
	if len(containerNameMatches) < 2 {
		return "", "", fmt.Errorf("failed to parse container name")
	}
	containerName := containerNameMatches[1]

	if kind == utils.DockerStart {
		networks, err := idc.ExtractNetworksForContainer(containerName)
		if err != nil {
			return containerName, "", err
		}
		for i := range networks {
			return containerName, i, nil
		}
		return containerName, "", fmt.Errorf("failed to parse network name")
	}

	// Extract network name
	networkNameRegex := regexp.MustCompile(networkNamePattern)
	networkNameMatches := networkNameRegex.FindStringSubmatch(cmd)
	if len(networkNameMatches) < 3 {
		return containerName, "", fmt.Errorf("failed to parse network name")
	}
	networkName := networkNameMatches[2]

	return containerName, networkName, nil
}
