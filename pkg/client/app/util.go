package app

import (
	"fmt"
	"os"
	"path/filepath"
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
func ensureInMemoryComposeFlags(appCmd, serviceName string, preferFailureAbort bool) string {
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

	return ensureComposeExitOnAppFailure(strings.Join(result, " "), serviceName, preferFailureAbort)
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

// isRewritableComposeCommand reports whether appCmd is a literal docker
// compose invocation, i.e. one that modifyDockerComposeCommand can splice
// `-f <generated>.yaml` into.
//
// It is false for a wrapper — `make up`, `./start.sh`, an npm script — that
// runs compose internally. Splicing into those produces nonsense
// (`make -f ./docker-compose-tmp.yaml up --abort-on-container-exit`), so
// those commands are run untouched and pointed at keploy's compose file
// through the COMPOSE_FILE environment variable instead.
func isRewritableComposeCommand(appCmd string) bool {
	lower := strings.ToLower(appCmd)
	return strings.Contains(lower, "docker compose") || strings.Contains(lower, "docker-compose") ||
		strings.Contains(lower, "podman compose") || strings.Contains(lower, "podman-compose")
}

// composeLaunchPlan decides how keploy hands its generated compose file to
// the application command, and is pure so both branches can be tested
// without a docker client.
//
// Exactly one of the two results is non-empty. A literal compose command
// gets the file spliced in (and with it --abort-on-container-exit, which is
// how keploy notices the app container dying). A wrapper cannot be rewritten
// — splicing yields `make -f ./docker-compose-tmp.yaml up
// --abort-on-container-exit` — so it runs untouched and the file travels in
// COMPOSE_FILE instead.
//
// composeFileEnv is absolute: compose resolves a relative COMPOSE_FILE
// against its own working directory and derives the project directory from
// it, so `make -C deploy up` would otherwise look for
// deploy/docker-compose-tmp.yaml and find nothing.
func composeLaunchPlan(appCmd, newComposeFile, appComposePath, appServiceName string) (newCmd, composeFileEnv string) {
	if isRewritableComposeCommand(appCmd) {
		return modifyDockerComposeCommand(appCmd, newComposeFile, appComposePath, appServiceName), ""
	}
	abs, err := filepath.Abs(newComposeFile)
	if err != nil {
		abs = newComposeFile
	}
	return "", abs
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
					return ensureComposeExitOnAppFailure(modifiedCmd, appServiceName, false)
				}
			}
		}
		// If no matching compose path found, return original command
		modifiedCmd = appCmd
		return ensureComposeExitOnAppFailure(modifiedCmd, appServiceName, false)
	}

	// If the pattern doesn't exist, inject the new Compose file right before
	// the `up` subcommand: after compose's global flags, whose values (a -p
	// project named "upload", say) are not the subcommand.
	if upAt, ok := composeUpOffset(appCmd, true); ok {
		modifiedCmd = fmt.Sprintf("%s-f %s %s", appCmd[:upAt], newComposeFile, appCmd[upAt:])
		return ensureComposeExitOnAppFailure(modifiedCmd, appServiceName, false)
	}

	modifiedCmd = fmt.Sprintf("%s -f %s", appCmd, newComposeFile)
	return ensureComposeExitOnAppFailure(modifiedCmd, appServiceName, false)
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
func ensureComposeExitOnAppFailure(appCmd, serviceName string, preferFailureAbort bool) string {
	// Under the proven switch keploy, not compose, ends the run on the app's
	// exit, and stops the app itself with its stack running on
	// (compose_appstop.go): the abort flags, which would take keploy's agent
	// down with the app, are left out, the user's own too.
	if composeStopsTheApp() {
		return withoutComposeAbortFlags(appCmd)
	}
	// If the user already passed one of these flags, don't touch the command.
	if strings.Contains(appCmd, "--abort-on-container-exit") || strings.Contains(appCmd, "--abort-on-container-failure") || strings.Contains(appCmd, "--exit-code-from") {
		return appCmd
	}

	// Arguments we want to inject.
	var args []string
	if preferFailureAbort {
		args = []string{"--abort-on-container-failure"}
	} else {
		args = []string{"--abort-on-container-exit"}
		if serviceName != "" {
			args = append(args, "--exit-code-from", serviceName)
		}
	}

	// Insert the flags immediately after the `up` subcommand, where it
	// stands, its spacing and quoting kept. Not into a wrapper's quoted
	// script (`bash -c "docker compose up"`): a flag spliced inside the
	// quotes would end up in the quoted word rather than on compose's own
	// command line, so compose would never see it.
	if upAt, ok := composeUpOffset(appCmd, false); ok {
		end := upAt + len("up")
		return appCmd[:end] + " " + strings.Join(args, " ") + appCmd[end:]
	}

	// Fallback: no explicit "up" token detected — do not append flags.
	return appCmd
}

// composeGlobalValueFlags are compose's global flags whose value is the next
// word: that word is the flag's (a project, a file, a profile), never the
// subcommand, whatever it spells.
var composeGlobalValueFlags = map[string]bool{
	"-f": true, "--file": true, "-p": true, "--project-name": true, "--profile": true,
	"--env-file": true, "--project-directory": true, "--ansi": true, "--parallel": true, "--progress": true,
}

// composeUpField is the index in parts, a compose command's words, of its
// `up` subcommand: the first word "up" that is not the value of a global
// flag, so `docker compose -p up up` and `-p upload up` find the second
// word. It is -1 when there is none.
func composeUpField(parts []string) int {
	value := false
	for i, p := range parts {
		switch {
		case value:
			value = false
		case composeGlobalValueFlags[p]:
			value = true
		case p == "up":
			return i
		}
	}
	return -1
}

// composeUpOffset is the byte offset in cmd of its `up` subcommand
// (composeUpField), so the command can be spliced where it stands, its
// spacing and quoting kept. A quoted word ("my file.yaml") is one word. A
// command with no `up` of its own may run compose inside a quoted word, as
// `bash -c "docker compose up"` does: with intoQuoted, the `up` is then
// found inside that word, so a splice lands in the script the wrapper runs.
func composeUpOffset(cmd string, intoQuoted bool) (int, bool) {
	var starts []int
	var words []string
	for i := 0; i < len(cmd); {
		for i < len(cmd) && (cmd[i] == ' ' || cmd[i] == '\t') {
			i++
		}
		if i >= len(cmd) {
			break
		}
		start := i
		var quote byte
		for i < len(cmd) && (quote != 0 || (cmd[i] != ' ' && cmd[i] != '\t')) {
			switch {
			case quote == 0 && (cmd[i] == '"' || cmd[i] == '\''):
				quote = cmd[i]
			case quote != 0 && cmd[i] == quote:
				quote = 0
			}
			i++
		}
		starts = append(starts, start)
		words = append(words, cmd[start:i])
	}
	if i := composeUpField(words); i >= 0 {
		return starts[i], true
	}
	for i, w := range words {
		if !intoQuoted {
			break
		}
		if len(w) >= 2 && (w[0] == '"' || w[0] == '\'') && w[len(w)-1] == w[0] {
			if at, ok := composeUpOffset(w[1:len(w)-1], true); ok {
				return starts[i] + 1 + at, true
			}
		}
	}
	return 0, false
}

// composeProjectScope returns the -p/--project-name and --project-directory
// values from the command, so the library resolves the SAME project the
// shell-out would have. Both are empty when the command sets neither, which is
// the normal case: compose then derives the name from the document or the
// working directory. A value in quotes (`-p "proj"`) is taken without them,
// as the shell hands it to compose.
func composeProjectScope(cmd string) (projectName, projectDir string) {
	parts := strings.Fields(cmd)
	for i := 0; i < len(parts); i++ {
		switch {
		case (parts[i] == "-p" || parts[i] == "--project-name") && i+1 < len(parts):
			projectName = unquoteWord(parts[i+1])
			i++
		case strings.HasPrefix(parts[i], "-p="):
			projectName = unquoteWord(strings.TrimPrefix(parts[i], "-p="))
		case strings.HasPrefix(parts[i], "--project-name="):
			projectName = unquoteWord(strings.TrimPrefix(parts[i], "--project-name="))
		case parts[i] == "--project-directory" && i+1 < len(parts):
			projectDir = unquoteWord(parts[i+1])
			i++
		case strings.HasPrefix(parts[i], "--project-directory="):
			projectDir = unquoteWord(strings.TrimPrefix(parts[i], "--project-directory="))
		}
	}
	return projectName, projectDir
}

// composeEnvFiles returns the command's --env-file values, in order (compose
// takes the flag more than once), so the library and the shell-out's tail
// resolve the document against the env files compose reads; nil when it
// gives none, when compose reads COMPOSE_ENV_FILES or the project's .env.
func composeEnvFiles(cmd string) []string {
	var files []string
	parts := strings.Fields(cmd)
	for i := 0; i < len(parts); i++ {
		switch {
		case parts[i] == "--env-file" && i+1 < len(parts):
			files = append(files, unquoteWord(parts[i+1]))
			i++
		case strings.HasPrefix(parts[i], "--env-file="):
			files = append(files, unquoteWord(strings.TrimPrefix(parts[i], "--env-file=")))
		}
	}
	return files
}

// unquoteWord is a command word without the quotes around it, as the shell
// passes it on: `"proj"` and `'proj'` are proj.
func unquoteWord(w string) string {
	return strings.Trim(w, `"'`)
}

// composeAbortFlag matches, with the space before it, a compose up flag that
// ends the stack on a container's exit: --abort-on-container-exit,
// --abort-on-container-failure, and --exit-code-from with its value.
var composeAbortFlag = regexp.MustCompile(`\s+(--abort-on-container-(exit|failure)|--exit-code-from(=|\s+)\S+)(\s|$)`)

// withoutComposeAbortFlags is cmd without the compose up flags that end the
// stack on a container's exit, the rest of it as written.
func withoutComposeAbortFlags(cmd string) string {
	for {
		loc := composeAbortFlag.FindStringSubmatchIndex(cmd)
		if loc == nil {
			return cmd
		}
		// Keep the space or end that followed the flag.
		cmd = cmd[:loc[0]] + cmd[loc[8]:]
	}
}
