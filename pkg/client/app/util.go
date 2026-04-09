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

// composeSubcommands lists the docker compose subcommands that mark the end of
// global options. "-f -" must be injected before the first subcommand token.
var composeSubcommands = map[string]bool{
	"up": true, "down": true, "run": true, "exec": true, "ps": true,
	"logs": true, "build": true, "config": true, "create": true,
	"events": true, "images": true, "kill": true, "pause": true,
	"port": true, "pull": true, "push": true, "restart": true,
	"rm": true, "start": true, "stop": true, "top": true, "unpause": true,
}

// ensureInMemoryComposeFlags rewrites the docker compose command to use stdin
// ("-f -") for in-memory content and injects the exit-code-from flags.
// It tokenizes the arguments, strips all -f/--file flags (including dangling
// ones without a value), and injects a single "-f -" before the first compose
// subcommand to avoid producing multiple stdin readers.
func ensureInMemoryComposeFlags(appCmd, serviceName string) string {
	parts := strings.Fields(appCmd)

	// Strip every existing -f/--file flag and its value, including dangling
	// -f/--file tokens that appear without a following value.
	cleaned := make([]string, 0, len(parts))
	for i := 0; i < len(parts); i++ {
		if parts[i] == "-f" || parts[i] == "--file" {
			// Skip the value if one follows.
			if i+1 < len(parts) {
				i++
			}
			continue
		}
		if strings.HasPrefix(parts[i], "-f=") || strings.HasPrefix(parts[i], "--file=") {
			continue
		}
		cleaned = append(cleaned, parts[i])
	}

	// Inject a single "-f -" before the first compose subcommand so the flag
	// stays in the global-options position. If no subcommand is found, append
	// at the end as a fallback.
	injected := false
	result := make([]string, 0, len(cleaned)+2)
	for _, p := range cleaned {
		if !injected && composeSubcommands[p] {
			result = append(result, "-f", "-")
			injected = true
		}
		result = append(result, p)
	}
	if !injected {
		result = append(result, "-f", "-")
	}

	return ensureComposeExitOnAppFailure(strings.Join(result, " "), serviceName)
}

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

func modifyDockerComposeCommand(appCmd, newComposeFile, appComposePath, appServiceName string) string {
	// Ensure newComposeFile starts with ./
	if !strings.HasPrefix(newComposeFile, "./") {
		newComposeFile = "./" + newComposeFile
	}

	var modifiedCmd string
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
					modifiedCmd = strings.Replace(appCmd, fullMatch, fmt.Sprintf("-f %s", newComposeFile), 1)
					return ensureComposeExitOnAppFailure(modifiedCmd, appServiceName)
				}
			}
		}
		// If no matching compose path found, return original command
		modifiedCmd = appCmd
		return ensureComposeExitOnAppFailure(modifiedCmd, appServiceName)
	}

	// If the pattern doesn't exist, inject the new Compose file right after "docker-compose" or "docker compose"
	upIdx := strings.Index(appCmd, " up")
	if upIdx != -1 {
		modifiedCmd = fmt.Sprintf("%s -f %s%s", appCmd[:upIdx], newComposeFile, appCmd[upIdx:])
		return ensureComposeExitOnAppFailure(modifiedCmd, appServiceName)
	}

	modifiedCmd = fmt.Sprintf("%s -f %s", appCmd, newComposeFile)
	return ensureComposeExitOnAppFailure(modifiedCmd, appServiceName)
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
