package app

import (
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"syscall"
)

func findComposeFile() string {
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

func ParseDockerCmd(cmd string) (string, string, error) {
	// Regular expression patterns
	containerNamePattern := `--name\s+([^\s]+)`
	networkNamePattern := `(--network|--net)\s+([^\s]+)`

	// Extract container name
	containerNameRegex := regexp.MustCompile(containerNamePattern)
	containerNameMatches := containerNameRegex.FindStringSubmatch(cmd)
	if len(containerNameMatches) < 2 {
		return "", "", fmt.Errorf("failed to parse container name")
	}
	containerName := containerNameMatches[1]

	// Extract network name
	networkNameRegex := regexp.MustCompile(networkNamePattern)
	networkNameMatches := networkNameRegex.FindStringSubmatch(cmd)
	if len(networkNameMatches) < 3 {
		return containerName, "", fmt.Errorf("failed to parse network name")
	}
	networkName := networkNameMatches[2]

	return containerName, networkName, nil
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

func IsDetachMode(command string) bool {
	args := strings.Fields(command)
	for _, arg := range args {
		if arg == "-d" || arg == "--detach" {
			return true
		}
	}

	return false
}
