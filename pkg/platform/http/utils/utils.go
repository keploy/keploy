package utils

import (
	"crypto/rand"
	"encoding/hex"
	"regexp"
	"strings"

	"go.uber.org/zap"
)

func ExtractDockerFlags(cmd string) (cleanedCmd string, ports []string, networks []string) {
	// 1. Extract port mapping arguments
	portRegex := regexp.MustCompile(`\s+(-p|--publish)\s+[^\s]+`)
	portArgs := portRegex.FindAllString(cmd, -1)
	for _, p := range portArgs {
		ports = append(ports, strings.TrimSpace(p))
	}
	// Remove port arguments from the command
	cmd = portRegex.ReplaceAllString(cmd, "")

	// 2. Extract network names
	networkRegex := regexp.MustCompile(`(--network|--net)\s+([^\s]+)`)
	networkMatches := networkRegex.FindAllStringSubmatch(cmd, -1)
	if len(networkMatches) > 0 {
		for _, match := range networkMatches {
			// The network name is in the second capturing group (index 2)
			if len(match) > 2 {
				networks = append(networks, match[2])
			}
		}
		// Remove network arguments from the command
		cmd = networkRegex.ReplaceAllString(cmd, "")
	}

	cleanedCmd = cmd
	return
}

// addHostRegex matches a `docker run` host entry flag, --add-host NAME:IP or
// --add-host=NAME:IP, its entry quoted or not.
var addHostRegex = regexp.MustCompile(`\s+--add-host(?:=|\s+)("[^"]*"|'[^']*'|[^\s]+)`)

// ExtractAddHosts takes the app's host entries (--add-host) out of its
// `docker run` command: under keploy the app joins the agent container's
// network namespace, where Docker refuses entries of the app's own, so the
// agent container carries them (models.SetupOptions.AppExtraHosts).
func ExtractAddHosts(cmd string) (cleanedCmd string, hosts []string) {
	for _, m := range addHostRegex.FindAllStringSubmatch(cmd, -1) {
		hosts = append(hosts, strings.Trim(m[1], `"'`))
	}
	return addHostRegex.ReplaceAllString(cmd, ""), hosts
}

func GenerateRandomContainerName(logger *zap.Logger, prefix string) string {
	randomBytes := make([]byte, 2)
	if _, err := rand.Read(randomBytes); err != nil {
		logger.Fatal("Failed to generate random part for container name", zap.Error(err))
	}

	uuidSuffix := hex.EncodeToString(randomBytes)
	return prefix + uuidSuffix
}
