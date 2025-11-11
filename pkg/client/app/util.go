package app

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"

	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func findComposeFile(cmd string) []string {

	cmdArgs := strings.Fields(cmd)
	composePaths := []string{}
	haveMultipleComposeFiles := false

	for i := 0; i < len(cmdArgs); i++ {
		if cmdArgs[i] == "-f" && i+1 < len(cmdArgs) {
			composePaths = append(composePaths, cmdArgs[i+1])
			haveMultipleComposeFiles = true
		}
	}

	if haveMultipleComposeFiles {
		return composePaths
	}

	filenames := []string{"docker-compose.yml", "docker-compose.yaml", "compose.yml", "compose.yaml"}

	for _, filename := range filenames {
		if _, err := os.Stat(filename); !os.IsNotExist(err) {
			return []string{filename}
		}
	}

	return []string{}
}

func modifyDockerComposeCommand(appCmd, newComposeFile, appComposePath string) string {
	// Ensure newComposeFile starts with ./
	if !strings.HasPrefix(newComposeFile, "./") {
		newComposeFile = "./" + newComposeFile
	}

	// Define a regular expression pattern to match "-f <file>"
	pattern := `(-f\s+("[^"]+"|'[^']+'|\S+))`
	re := regexp.MustCompile(pattern)

	// Find all matches and replace only the one that matches appComposePath
	matches := re.FindAllStringSubmatch(appCmd, -1)
	if len(matches) > 0 {
		for _, match := range matches {
			fullMatch := match[0]
			filePart := match[1]

			// Extract the actual file path from the match (remove -f and whitespace)
			filePattern := `-f\s+("[^"]+"|'[^']+'|\S+)`
			fileRe := regexp.MustCompile(filePattern)
			fileMatch := fileRe.FindStringSubmatch(filePart)

			if len(fileMatch) > 1 {
				quotedFile := fileMatch[1]
				// Remove quotes if present
				actualFile := strings.Trim(quotedFile, `"'`)

				// Check if this file matches the appComposePath
				if actualFile == appComposePath {
					return strings.Replace(appCmd, fullMatch, fmt.Sprintf("-f %s", newComposeFile), 1)
				}
			}
		}
		// If no matching compose path found, return original command
		return appCmd
	}

	// If the pattern doesn't exist, inject the new Compose file right after "docker-compose" or "docker compose"
	upIdx := strings.Index(appCmd, " up")
	if upIdx != -1 {
		return fmt.Sprintf("%s -f %s%s", appCmd[:upIdx], newComposeFile, appCmd[upIdx:])
	}

	return fmt.Sprintf("%s -f %s", appCmd, newComposeFile)
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

// ensureComposeExitOnAppFailure ensures that the docker-compose command will exit when the application
// container stops by injecting --abort-on-container-exit and --exit-code-from flags if not already present.
// It inserts these flags immediately after the "up" subcommand if found, otherwise appends them to the end.
//
// Parameters:
//   - appCmd: the docker-compose command to modify
//   - serviceName: the name of the service whose exit code should be monitored (empty string skips --exit-code-from)
//
// Returns: the modified command with the necessary flags added
func ensureComposeExitOnAppFailure(appCmd, serviceName string) string {
	// If the user already passed one of these flags, don't touch the command.
	if strings.Contains(appCmd, "--abort-on-container-exit") || strings.Contains(appCmd, "--exit-code-from") {
		return appCmd
	}

	// Arguments we want to inject.
	args := []string{"--abort-on-container-exit"}
	if serviceName != "" {
		args = append(args, "--exit-code-from", serviceName)
	}

	parts := strings.Fields(appCmd)
	for i, p := range parts {
		if p == "up" {
			// Insert flags immediately after "up"
			newParts := make([]string, 0, len(parts)+len(args))
			newParts = append(newParts, parts[:i+1]...)
			newParts = append(newParts, args...)
			newParts = append(newParts, parts[i+1:]...)
			return strings.Join(newParts, " ")
		}
	}

	// Fallback: no explicit "up" token detected — do not append flags.
	return appCmd
}
