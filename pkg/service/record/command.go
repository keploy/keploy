package record

import (
	"regexp"
	"strings"

	"go.keploy.io/server/v3/utils"
)

func inferContainerName(command string, cmdType utils.CmdType) string {
	command = strings.TrimSpace(command)
	if command == "" {
		return ""
	}

	switch cmdType {
	case utils.DockerStart:
		re := regexp.MustCompile(`\bstart\s+(?:-[^\s]+\s+)*([^\s]+)`)
		matches := re.FindStringSubmatch(command)
		if len(matches) >= 2 {
			return matches[1]
		}
	case utils.DockerRun:
		re := regexp.MustCompile(`--name\s+([^\s]+)`)
		matches := re.FindStringSubmatch(command)
		if len(matches) >= 2 {
			return matches[1]
		}
	}

	return ""
}
