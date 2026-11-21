package app

import (
	"fmt"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/pkg/platform/engine"
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
// ("-f -") for in-memory content, the abort flags left out
// (withoutComposeAbortFlags).
// It tokenizes the arguments, strips all -f/--file flags (including dangling
// ones without a value), and injects a single "-f -" before the first compose
// subcommand to avoid producing multiple stdin readers.
func ensureInMemoryComposeFlags(appCmd string) string {
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

	return withoutComposeAbortFlags(strings.Join(result, " "))
}

// isRewritableComposeCommand reports whether appCmd is a literal docker
// compose invocation, i.e. one that modifyDockerComposeCommand can splice
// `-f <generated>.yaml` into.
//
// It is false for a wrapper — `make up`, `./start.sh`, an npm script — that
// runs compose internally. Splicing into those produces nonsense
// (`make -f ./docker-compose-tmp.yaml up`), so
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
// gets the file spliced in, its abort flags left out (keploy watches the
// app's container itself: compose_appstop.go). A wrapper cannot be rewritten
// — splicing yields `make -f ./docker-compose-tmp.yaml up` — so it runs
// untouched and the file travels in COMPOSE_FILE instead.
//
// composeFileEnv is absolute: compose resolves a relative COMPOSE_FILE
// against its own working directory and derives the project directory from
// it, so `make -C deploy up` would otherwise look for
// deploy/docker-compose-tmp.yaml and find nothing.
func composeLaunchPlan(appCmd, newComposeFile, appComposePath string) (newCmd, composeFileEnv string) {
	if isRewritableComposeCommand(appCmd) {
		return modifyDockerComposeCommand(appCmd, newComposeFile, appComposePath), ""
	}
	abs, err := filepath.Abs(newComposeFile)
	if err != nil {
		abs = newComposeFile
	}
	return "", abs
}

func modifyDockerComposeCommand(appCmd, newComposeFile, appComposePath string) string {
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
					return withoutComposeAbortFlags(modifiedCmd)
				}
			}
		}
		// If no matching compose path found, return original command
		modifiedCmd = appCmd
		return withoutComposeAbortFlags(modifiedCmd)
	}

	// If the pattern doesn't exist, inject the new Compose file right before
	// the `up` subcommand: after compose's global flags, whose values (a -p
	// project named "upload", say) are not the subcommand.
	if upAt, ok := composeUpOffset(appCmd, true); ok {
		modifiedCmd = fmt.Sprintf("%s-f %s %s", appCmd[:upAt], newComposeFile, appCmd[upAt:])
		return withoutComposeAbortFlags(modifiedCmd)
	}

	modifiedCmd = fmt.Sprintf("%s -f %s", appCmd, newComposeFile)
	return withoutComposeAbortFlags(modifiedCmd)
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

// composeUpSemantics reads the `up` flags that change what the run MEANS out of
// the resolved command string.
//
// It parses the same string the shell-out would have executed rather than
// re-deriving the flags from the compose document, so the in-process path and
// the shell-out can never disagree: only the final string records the flags
// either runs with.
func composeUpSemantics(cmd string) docker.ComposeUpOptions {
	opts := docker.ComposeUpOptions{OnExit: docker.ComposeOnExitIgnore}
	parts := strings.Fields(cmd)
	for i := 0; i < len(parts); i++ {
		switch {
		case parts[i] == "--abort-on-container-exit":
			opts.OnExit = docker.ComposeOnExitStop
		case parts[i] == "--abort-on-container-failure":
			opts.OnExit = docker.ComposeOnExitFail
		case parts[i] == "--exit-code-from" && i+1 < len(parts):
			opts.ExitCodeFrom = parts[i+1]
			i++
		case strings.HasPrefix(parts[i], "--exit-code-from="):
			opts.ExitCodeFrom = strings.TrimPrefix(parts[i], "--exit-code-from=")
		}
	}
	// --exit-code-from IMPLIES --abort-on-container-exit. The CLI does this
	// itself (compose v5.5.1 cmd/compose/up.go), and without it a command
	// carrying only --exit-code-from would leave Up blocking after the app
	// exited, where the shell-out returns. --abort-on-container-failure is the
	// documented exception and keeps precedence.
	//
	// keploy strips every --abort-* and --exit-code-from from the commands it
	// runs (withoutComposeAbortFlags), so nothing reaches this today — which
	// is exactly why it is worth stating: the claim above is that the two
	// paths cannot disagree, and that has to hold for inputs keploy does not
	// currently produce, not only for the ones it does.
	if opts.ExitCodeFrom != "" && opts.OnExit == docker.ComposeOnExitIgnore {
		opts.OnExit = docker.ComposeOnExitStop
	}
	return opts
}

// composeProjectScope returns the -p/--project-name and --project-directory
// values of the compose command's own options (engine.ComposeOptions: before
// its subcommand, where compose takes them), so the library resolves the
// SAME project the shell-out would have. Both are empty when the command sets
// neither, which is the normal case: compose then derives the name from the
// document or the working directory. A value in quotes (`-p "proj"`) is
// taken without them, as the shell hands it to compose. Another program's
// -p, or a container's (docker compose run app ./tool -p <password>), is no
// project.
func composeProjectScope(cmd string) (projectName, projectDir string) {
	parts := engine.ComposeOptions(cmd)
	for i := 0; i < len(parts); i++ {
		switch {
		case (parts[i] == "-p" || parts[i] == "--project-name") && i+1 < len(parts):
			projectName = parts[i+1]
			i++
		case strings.HasPrefix(parts[i], "-p") && !strings.HasPrefix(parts[i], "--") && len(parts[i]) > 2:
			// -p=X, and -pX: compose's -p with its value attached.
			projectName = strings.TrimPrefix(parts[i][2:], "=")
		case strings.HasPrefix(parts[i], "--project-name="):
			projectName = strings.TrimPrefix(parts[i], "--project-name=")
		case (parts[i] == "--project-directory" || parts[i] == "--workdir") && i+1 < len(parts):
			// --workdir: --project-directory's old, hidden name.
			projectDir = parts[i+1]
			i++
		case strings.HasPrefix(parts[i], "--project-directory="):
			projectDir = strings.TrimPrefix(parts[i], "--project-directory=")
		case strings.HasPrefix(parts[i], "--workdir="):
			projectDir = strings.TrimPrefix(parts[i], "--workdir=")
		}
	}
	return projectName, projectDir
}

// composeEnvFiles returns the --env-file values of the compose command's
// own options (engine.ComposeOptions: before its subcommand, the only place
// compose takes them), in order (compose takes the flag more than once), so
// the library and the shell-out's tail resolve the document against the env
// files compose reads; nil when it gives none, when compose reads
// COMPOSE_ENV_FILES or the project's .env. Another program's --env-file
// (node --env-file=.env.test, after compose run's service or after the
// compose command) is none: read as compose's, the library loaded that
// program's file as the stack's environment instead of the project's .env,
// and a line it could not parse was quoted in the run's error.
func composeEnvFiles(cmd string) []string {
	var files []string
	parts := engine.ComposeOptions(cmd)
	for i := 0; i < len(parts); i++ {
		switch {
		case parts[i] == "--env-file" && i+1 < len(parts):
			files = append(files, parts[i+1])
			i++
		case strings.HasPrefix(parts[i], "--env-file="):
			files = append(files, strings.TrimPrefix(parts[i], "--env-file="))
		}
	}
	return files
}

// composeCompatibility reports whether the compose command's own options
// (engine.ComposeOptions: before its subcommand, where compose takes them)
// run it in compatibility mode: --compatibility, or --compatibility=V with V
// true as compose's flag parser reads a bool (strconv.ParseBool), the last
// one given winning. A --compatibility of another program, or after compose's
// subcommand, is not compose's.
func composeCompatibility(cmd string) bool {
	on := false
	for _, part := range engine.ComposeOptions(cmd) {
		switch {
		case part == "--compatibility":
			on = true
		case strings.HasPrefix(part, "--compatibility="):
			on, _ = strconv.ParseBool(strings.TrimPrefix(part, "--compatibility="))
		}
	}
	return on
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
