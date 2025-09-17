//go:build linux

package app

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"syscall"

	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

func findComposeFile(cmd string) string {

	cmdArgs := strings.Fields(cmd)

	for i := 0; i < len(cmdArgs); i++ {
		if cmdArgs[i] == "-f" && i+1 < len(cmdArgs) {
			return cmdArgs[i+1]
		}
	}

	filenames := []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"}

	for _, filename := range filenames {
		if _, err := os.Stat(filename); !os.IsNotExist(err) {
			return filename
		}
	}

	return ""
}

func modifyDockerComposeCommand(appCmd, newComposeFile string) string {
	// Ensure newComposeFile starts with ./
	if !strings.HasPrefix(newComposeFile, "./") {
		newComposeFile = "./" + newComposeFile
	}

	// Define a regular expression pattern to match "-f <file>"
	pattern := `(-f\s+("[^"]+"|'[^']+'|\S+))`
	re := regexp.MustCompile(pattern)

	// Check if the "-f <file>" pattern exists in the appCmd
	if re.MatchString(appCmd) {
		// Replace it with the new Compose file
		return re.ReplaceAllString(appCmd, fmt.Sprintf("-f %s", newComposeFile))
	}

	// If the pattern doesn't exist, inject the new Compose file right after "docker-compose" or "docker compose"
	upIdx := strings.Index(appCmd, " up")
	if upIdx != -1 {
		return fmt.Sprintf("%s -f %s%s", appCmd[:upIdx], newComposeFile, appCmd[upIdx:])
	}

	return fmt.Sprintf("%s -f %s", appCmd, newComposeFile)
}

func getInode(pid int) (uint64, error) {
	path := filepath.Join("/proc", strconv.Itoa(pid), "ns", "pid")

	f, err := os.Stat(path)
	if err != nil {
		return 0, err
	}
	// Dev := (f.Sys().(*syscall.Stat_t)).Dev
	i := (f.Sys().(*syscall.Stat_t)).Ino
	if i == 0 {
		return 0, fmt.Errorf("failed to get the inode of the process")
	}
	return i, nil
}

func isDetachMode(logger *zap.Logger, command string, kind utils.CmdType) bool {
	args := strings.Fields(command)

	if kind == utils.DockerStart {
		flags := []string{"-a", "--attach", "-i", "--interactive"}

		for _, arg := range args {
			if slices.Contains(flags, arg) {
				return false
			}
		}
		utils.LogError(logger, fmt.Errorf("docker start require --attach/-a or --interactive/-i flag"), "failed to start command")
		return true
	}

	for _, arg := range args {
		if arg == "-d" || arg == "--detach" {
			utils.LogError(logger, fmt.Errorf("detach mode is not allowed in Keploy command"), "failed to start command")
			return true
		}
	}

	return false
}
