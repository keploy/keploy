// Package engine names the container engine a docker-mode command runs its
// containers on, and holds what keploy needs to drive that engine itself: the
// CLI for its own container commands, the compose command, the Engine API
// endpoint, and the security options its agent container needs there.
//
// Docker is built in. Any other engine is supported only when a build
// registers it (Register); this build registers none, and a command for an
// engine that is not registered is refused (Unsupported) instead of being run
// as a native process, where keploy would capture nothing from it.
package engine

import (
	"context"
	"fmt"
	"os"
	"strings"
	"sync"

	"go.uber.org/zap"
)

// Engine names.
const (
	Docker = "docker"
	Podman = "podman"
)

// Runtime is what keploy needs to drive one container engine.
type Runtime struct {
	// Name is the engine's name (Docker, Podman).
	Name string
	// CLI is the binary keploy runs its own container commands with: the
	// agent container, and the diagnostics and stop around it.
	CLI string
	// Compose is the command keploy runs its own compose commands with, where
	// the compose library is not linked: {"docker", "compose"}.
	Compose []string
	// Host is the Engine API endpoint, exported as DOCKER_HOST so the Docker
	// SDK client and the compose library both reach it. Empty keeps the
	// environment's own (DOCKER_HOST, else the Docker socket).
	Host string
	// AgentSecurityOpts are the --security-opt values keploy's agent
	// container needs on this engine, for `run` and for the compose service.
	AgentSecurityOpts []string
}

// Preparer readies an engine for one keploy run and says how to drive it. It
// may start what the run needs (an API service); whatever it starts must not
// outlive the keploy process.
type Preparer func(ctx context.Context, logger *zap.Logger) (Runtime, error)

// dockerRuntime is Docker's. The agent loads eBPF programs, which an SELinux
// policy denies to a container by default (container_t may not call bpf(2)),
// so on an SELinux host the agent never starts. label=disable lifts SELinux
// confinement for the agent container alone, and Docker accepts it where
// SELinux is off.
var dockerRuntime = Runtime{
	Name:              Docker,
	CLI:               "docker",
	Compose:           []string{"docker", "compose"},
	AgentSecurityOpts: []string{"label=disable"},
}

var (
	mu        sync.RWMutex
	preparers = map[string]Preparer{}
	active    = dockerRuntime
)

// Register makes an engine available to docker-mode commands. Call it from an
// init function, before the CLI runs.
func Register(name string, p Preparer) {
	mu.Lock()
	defer mu.Unlock()
	preparers[name] = p
}

// Supported reports whether this build can drive the engine.
func Supported(name string) bool {
	if name == Docker {
		return true
	}
	mu.RLock()
	defer mu.RUnlock()
	_, ok := preparers[name]
	return ok
}

// Prepare readies the engine for this run and makes it the active one. For
// an engine other than Docker it exports the engine's API endpoint as
// DOCKER_HOST, which every Engine API client keploy builds reads.
func Prepare(ctx context.Context, logger *zap.Logger, name string) error {
	if name == "" || name == Docker {
		mu.Lock()
		active = dockerRuntime
		mu.Unlock()
		return nil
	}
	mu.RLock()
	p, ok := preparers[name]
	mu.RUnlock()
	if !ok {
		return Unsupported(name)
	}
	rt, err := p(ctx, logger)
	if err != nil {
		return fmt.Errorf("failed to prepare %s: %w", name, err)
	}
	// What keploy runs its own commands with must never be empty.
	if rt.Name == "" {
		rt.Name = name
	}
	if rt.CLI == "" {
		rt.CLI = name
	}
	if len(rt.Compose) == 0 {
		rt.Compose = []string{rt.CLI, "compose"}
	}
	if rt.Host != "" {
		if err := os.Setenv("DOCKER_HOST", rt.Host); err != nil {
			return fmt.Errorf("failed to point the Engine API client at %s: %w", name, err)
		}
	}
	mu.Lock()
	active = rt
	mu.Unlock()
	return nil
}

// Active is the engine this run drives; Docker until Prepare says otherwise.
func Active() Runtime {
	mu.RLock()
	defer mu.RUnlock()
	return active
}

// Unsupported is the error for a command on an engine this build cannot
// drive.
func Unsupported(name string) error {
	if name == Podman {
		return fmt.Errorf("this keploy build cannot record or test applications that run in Podman. " +
			"Keploy from keploy.io (free with an account) can: " +
			"curl --silent -O -L https://keploy.io/install.sh && source install.sh")
	}
	return fmt.Errorf("container engine %q is not supported", name)
}

// StartUnsupported is the error for a command that resumes a container with
// `start` (the docker-start kind): the container was created without keploy's
// network and PID namespaces, and they cannot be added to it.
func StartUnsupported(name string) error {
	return fmt.Errorf("keploy cannot capture an application that `%[1]s start` resumes: its container was "+
		"created without keploy's network and PID namespaces, and they cannot be added to it. "+
		"Start the application with `%[1]s run` instead (`keploy mock record` and `mock replay` can also "+
		"take the container with --from-container, which runs a copy of it that has them), "+
		"or pass --cmd-type native if the application itself runs on the host", name)
}

// Invocation finds the container engine a command runs its application in.
//
// A command runs its application in a container when one of its commands
// (split at sh's list operators and pipes, outside quotes) starts one in the
// foreground. That is a command whose program is the engine: first, or behind
// wrappers (sudo -u alice, env FOO=bar, timeout 600, a full path) with their
// options and arguments, and followed, past its own global flags, by a
// subcommand that starts containers: run, start, container run|start,
// compose up|run|start|restart (docker-compose and podman-compose by name),
// and Podman's pod start and kube play. Other subcommands start nothing
// (compose down, pull, build, exec; container rm). An argument is not enough:
// `npm run docker start` runs a script, `python app.py --backend docker run` a
// program.
//
// A detached start (run -d, compose up -d or --wait, start without -a) is not
// the application: it returns at once, and keploy refuses it as one. It
// starts a dependency, as in `cd svc && docker compose down && docker compose
// up -d db && go run .`, where the application runs on the host. Detaching is
// read from the options before the image or service, not from the
// container's own arguments.
//
// It names the engine, for refusing one. Whether keploy can rewrite the
// command is a stricter question (the CLI's mentionsDockerBinary).
func Invocation(command string) (name string, ok bool) {
	for _, words := range commands(command) {
		if name, detached, ok := engineIn(words); ok && !detached {
			return name, true
		}
	}
	return "", false
}

// program is the index of a simple command's program word, past wrappers
// with their options and arguments and NAME=value assignments; -1 when there
// are only those.
func program(words []string) int {
	wrapper, needsValue := "", false
	for i, w := range words {
		base := programName(strings.ToLower(w))
		switch {
		case needsValue:
			needsValue = false // the value of the option before: sudo -u docker
		case wrappers[base] != nil && (wrapper == "" || !strings.HasPrefix(w, "-")):
			wrapper = base
		case wrapper != "" && strings.HasPrefix(w, "-"):
			needsValue = optionTakesValue(wrappers[wrapper], w)
		case isAssignment(w), wrapper != "" && isWrapperArgument(strings.ToLower(w)):
			// FOO=bar; timeout's 600; taskset's 0-3.
		default:
			return i
		}
	}
	return -1
}

// engineOf is the engine a simple command runs as its program, whatever it
// asks of it.
func engineOf(words []string) (string, bool) {
	i := program(words)
	if i < 0 {
		return "", false
	}
	switch base := programName(strings.ToLower(words[i])); base {
	case Docker, Podman:
		return base, true
	case "docker-compose", "podman-compose":
		return strings.TrimSuffix(base, "-compose"), true
	}
	return "", false
}

// engineIn finds the engine a simple command runs as its program to start
// containers, and whether it detaches from them. The engine's options are
// read as written: they are case-sensitive (-P is not -p).
func engineIn(words []string) (name string, detached, ok bool) {
	name, isEngine := engineOf(words)
	if !isEngine {
		return "", false, false
	}
	i := program(words)
	args := words[i+1:]
	if strings.HasSuffix(strings.ToLower(words[i]), "-compose") {
		detached, ok = composeStart(args)
		return name, detached, ok
	}
	sub, at, found := subcommand(args)
	if !found {
		return "", false, false
	}
	detached, ok = engineStart(strings.ToLower(sub), args[at+1:])
	return name, detached, ok
}

// engineStart reports whether an engine subcommand, with its args, starts
// containers (ok), and whether it returns at once once it has (detached).
// Anything else (ps, pull, rm, compose down, ...) starts nothing.
func engineStart(sub string, args []string) (detached, ok bool) {
	switch sub {
	case "run":
		return runDetached(args), true
	case "start":
		for _, a := range args {
			if a == "--attach" || a == "--interactive" ||
				(len(a) > 1 && a[0] == '-' && a[1] != '-' && strings.ContainsAny(a[1:], "ai")) {
				return false, true
			}
		}
		return true, true
	case "container":
		if len(args) == 0 {
			return false, false
		}
		if next := strings.ToLower(args[0]); next == "run" || next == "start" {
			return engineStart(next, args[1:])
		}
		return false, false
	case "compose":
		return composeStart(args)
	case "pod":
		// What they start is the application: keploy cannot drive either, so
		// they count, attached.
		return false, len(args) > 0 && strings.EqualFold(args[0], "start")
	case "kube":
		return false, len(args) > 0 && strings.EqualFold(args[0], "play")
	}
	return false, false
}

// runDetached reports whether run's options, up to the image, detach it:
// -d, --detach, or a cluster of short flags with d in it (-itd). The image's
// own arguments after it are the container's, and are not read.
func runDetached(args []string) bool {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") || a == "-" || a == "--" {
			return false // the image
		}
		detaches, takesNext := readOption(a, runBoolFlags, nil, runValueLetters)
		if detaches {
			return true
		}
		if takesNext {
			i++
		}
	}
	return false
}

// readOption reads one option word: whether it detaches (-d, --detach, or d
// among the booleans of a short cluster), and whether it takes the next word
// as its value. A long option takes one when listed in valued, or when
// bools is given and it is not in it; a short cluster's letters before the
// first in valueLetters take none, and that one takes the rest of the word
// (-p8080:80) or, when it ends it, the next word.
func readOption(a string, bools, valued map[string]bool, valueLetters string) (detaches, takesNext bool) {
	if a == "--detach" || a == "--detach=true" {
		return true, false
	}
	if strings.HasPrefix(a, "--") {
		if strings.Contains(a, "=") {
			return false, false
		}
		if bools != nil {
			return false, !bools[a]
		}
		return false, valued[a]
	}
	letters := a[1:]
	at := strings.IndexAny(letters, valueLetters)
	booleans := letters
	if at >= 0 {
		booleans = letters[:at]
	}
	return strings.ContainsRune(booleans, 'd'), at >= 0 && at == len(letters)-1
}

// runValueLetters are the short options of docker and podman run that take a
// value; the others (-d -i -t -P) take none.
const runValueLetters = "acehlmpuvw"

// runBoolFlags are the long options of docker and podman run that take no
// value.
var runBoolFlags = map[string]bool{
	"--rm": true, "--interactive": true, "--tty": true, "--privileged": true, "--init": true,
	"--read-only": true, "--publish-all": true, "--no-healthcheck": true, "--oom-kill-disable": true,
	"--disable-content-trust": true, "--quiet": true, "--help": true, "--replace": true, "--rmi": true,
	"--no-hosts": true, "--env-host": true, "--read-only-tmpfs": true, "--http-proxy": true,
	"--sig-proxy": true, "--use-api-socket": true, "--tls-verify": true, "--no-hostname": true,
	"--passwd": true, "--rootfs": true, "--unsetenv-all": true,
}

// composeStart reports whether compose arguments (after `compose`, or after
// docker-compose/podman-compose) start the project's containers, and whether
// detached: up with -d, --detach or --wait anywhere among its arguments (up
// takes options after its services too), run with -d before its service
// (after it comes the container's command), start and restart. down, build,
// pull, ps, logs, exec, stop and the rest start nothing.
func composeStart(args []string) (detached, ok bool) {
	i := 0
	for i < len(args) && strings.HasPrefix(args[i], "-") {
		if !strings.Contains(args[i], "=") && composeGlobalFlagsWithValue[args[i]] {
			i++
		}
		i++
	}
	if i >= len(args) {
		return false, false
	}
	sub, opts := strings.ToLower(args[i]), args[i+1:]
	switch sub {
	case "start", "restart":
		return true, true
	case "up", "run":
		for j := 0; j < len(opts); j++ {
			o := opts[j]
			if !strings.HasPrefix(o, "-") || o == "-" || o == "--" {
				if sub == "run" || o == "--" {
					return false, true // run's service: its command follows
				}
				continue // up's services, and its options after them
			}
			if sub == "up" && (o == "--wait" || strings.HasPrefix(o, "--wait=")) {
				return true, true
			}
			detaches, takesNext := readOption(o, nil, composeFlagsWithValue, composeValueLetters)
			if detaches {
				return true, true
			}
			if takesNext {
				j++
			}
		}
		return false, true
	}
	return false, false
}

// composeGlobalFlagsWithValue are compose's global options (docker compose,
// docker-compose, podman-compose) whose value can be the next argument.
var composeGlobalFlagsWithValue = map[string]bool{
	"-f": true, "--file": true, "-p": true, "--project-name": true, "--profile": true, "--env-file": true,
	"--project-directory": true, "--ansi": true, "--progress": true, "--parallel": true,
	"--in-pod": true, "--pod-args": true, "--podman-path": true, "--podman-args": true,
	"--podman-pull-args": true, "--podman-push-args": true, "--podman-build-args": true,
	"--podman-inspect-args": true, "--podman-run-args": true, "--podman-start-args": true,
	"--podman-stop-args": true, "--podman-rm-args": true, "--podman-volume-args": true,
}

// composeFlagsWithValue are compose up's and run's long options whose value
// can be the next argument; composeValueLetters their short ones (up's -t,
// run's -e -u -w -v -p -l). The other short ones (-d -V -T -i) take none.
var composeFlagsWithValue = map[string]bool{
	"--scale": true, "--timeout": true, "--exit-code-from": true, "--attach": true,
	"--no-attach": true, "--pull": true, "--wait-timeout": true, "--env": true, "--user": true,
	"--workdir": true, "--name": true, "--entrypoint": true, "--volume": true, "--publish": true,
	"--label": true, "--cap-add": true, "--cap-drop": true,
}

const composeValueLetters = "teuwvpl"

// wrappers run the command that follows their own options and arguments; each
// maps to its options that take the next word as their value.
var wrappers = map[string][]string{
	"sudo":     {"-u", "-g", "-h", "-p", "-C", "-D", "-r", "-t", "-U", "-T", "--user", "--group", "--host", "--prompt", "--close-from", "--chdir", "--role", "--type", "--other-user", "--command-timeout"},
	"doas":     {"-u", "-C"},
	"env":      {"-u", "-C", "-S", "--unset", "--chdir", "--split-string"},
	"timeout":  {"-s", "-k", "--signal", "--kill-after"},
	"nice":     {"-n", "--adjustment"},
	"ionice":   {"-c", "-n", "-p", "--class", "--classdata", "--pid"},
	"stdbuf":   {"-i", "-o", "-e", "--input", "--output", "--error"},
	"nohup":    {},
	"time":     {"-o", "-f", "--output", "--format"},
	"setsid":   {},
	"exec":     {"-a"},
	"command":  {},
	"taskset":  {"--cpu-list"},
	"chrt":     {},
	"unbuffer": {},
}

// optionTakesValue reports whether a wrapper's option word takes the next
// word as its value: one of valued as given (case matters: sudo's -H takes
// none, its -h one), or a cluster of short options ending in one (-Eu).
func optionTakesValue(valued []string, opt string) bool {
	if strings.Contains(opt, "=") {
		return false
	}
	for _, v := range valued {
		if opt == v {
			return true
		}
	}
	if len(opt) > 2 && opt[0] == '-' && opt[1] != '-' {
		last := "-" + opt[len(opt)-1:]
		for _, v := range valued {
			if last == v {
				return true
			}
		}
	}
	return false
}

// programName is a command word's program: the base of a path (either
// separator), without Windows' .exe.
func programName(w string) string {
	if j := strings.LastIndexAny(w, `/\`); j >= 0 {
		w = w[j+1:]
	}
	return strings.TrimSuffix(w, ".exe")
}

// isAssignment reports whether w is an environment assignment, NAME=value.
func isAssignment(w string) bool {
	eq := strings.IndexByte(w, '=')
	if eq <= 0 {
		return false
	}
	for i, c := range w[:eq] {
		if c != '_' && (c < 'a' || c > 'z') && (c < 'A' || c > 'Z') && (i == 0 || c < '0' || c > '9') {
			return false
		}
	}
	return true
}

// isWrapperArgument reports whether w is a wrapper's own positional argument:
// a number, duration or CPU list (timeout 600, timeout 10m, taskset -c 0-3,
// chrt 50), or a hex mask (taskset 0x3).
func isWrapperArgument(w string) bool {
	if hex, ok := strings.CutPrefix(w, "0x"); ok {
		return hex != "" && strings.Trim(hex, "0123456789abcdef") == ""
	}
	if n := len(w); n > 1 && strings.ContainsRune("smhd", rune(w[n-1])) {
		w = w[:n-1]
	}
	return w != "" && w[0] >= '0' && w[0] <= '9' && strings.Trim(w, "0123456789.,-") == ""
}

// commands is command's simple commands, as words, split the way sh splits
// them: at ; & && || | and newlines, outside quotes, with the quotes removed.
// Grouping parentheses and braces separate words; an & or | inside a
// redirection (2>&1, &>, >|) does not split, and a backslash before a newline
// continues the line. A backslash escapes only inside double quotes, as sh's
// does there; elsewhere it is kept, so a Windows path stays one.
func commands(command string) [][]string {
	var (
		all     [][]string
		words   []string // the current command's words so far
		word    strings.Builder
		inWord  bool
		quote   rune
		prev    rune
		runes   = []rune(command)
		endWord = func() {
			if inWord {
				words = append(words, word.String())
				word.Reset()
				inWord = false
			}
		}
		endCommand = func() {
			endWord()
			if len(words) > 0 {
				all = append(all, words)
				words = nil
			}
		}
	)
	for i := 0; i < len(runes); i++ {
		c := runes[i]
		next := rune(0)
		if i+1 < len(runes) {
			next = runes[i+1]
		}
		switch {
		case c == '\\' && (next == '\n' || (next == '\r' && i+2 < len(runes) && runes[i+2] == '\n')):
			// A continued line, in quotes or out.
			if next == '\r' {
				i++
			}
			i++
			if quote == 0 {
				endWord()
			}
			continue
		case quote != 0:
			if c == quote {
				quote = 0
			} else if c == '\\' && quote == '"' && strings.ContainsRune("\"\\$`", next) {
				word.WriteRune(next)
				i++
			} else {
				word.WriteRune(c)
			}
		case c == '\'' || c == '"':
			quote, inWord = c, true
		case c == ' ' || c == '\t' || c == '\r' || c == '(' || c == ')' || c == '{' || c == '}':
			endWord()
		case c == '&' && (prev == '>' || prev == '<' || next == '>'):
			word.WriteRune(c) // 2>&1, &>log
			inWord = true
		case c == '|' && prev == '>':
			word.WriteRune(c) // >|
			inWord = true
		case c == '\n' || c == ';' || c == '&' || c == '|':
			endCommand()
		default:
			word.WriteRune(c)
			inWord = true
		}
		prev = c
	}
	endCommand()
	return all
}

// subcommand is the first argument after an engine's global flags, and its
// index in args. A flag that takes its value as the next argument (podman
// --connection m run, docker --context prod run) consumes it.
func subcommand(args []string) (string, int, bool) {
	for i := 0; i < len(args); i++ {
		a := args[i]
		if !strings.HasPrefix(a, "-") {
			return a, i, true
		}
		if !strings.Contains(a, "=") && globalFlagsWithValue[a] {
			i++
		}
	}
	return "", 0, false
}

// globalFlagsWithValue are the docker and podman global flags whose value can
// be the next argument.
var globalFlagsWithValue = map[string]bool{
	// docker
	"--config": true, "--context": true, "-c": true, "--host": true, "-H": true,
	"--log-level": true, "-l": true, "--tlscacert": true, "--tlscert": true, "--tlskey": true,
	// podman (-c and --log-level are shared)
	"--connection": true, "--url": true, "--identity": true, "--root": true, "--runroot": true,
	"--storage-driver": true, "--storage-opt": true, "--cgroup-manager": true, "--runtime": true,
	"--runtime-flag": true, "--module": true, "--ssh": true, "--tmpdir": true,
	"--events-backend": true, "--conmon": true, "--network-cmd-path": true,
	"--network-config-dir": true, "--hooks-dir": true, "--imagestore": true, "--volumepath": true,
	"--registries-conf": true, "--cdi-spec-dir": true, "--out": true, "--db-backend": true,
}

// Detect names the engine a docker-mode command runs on: the first one any of
// its commands runs as its program to start containers, attached or not, else
// the first one any of them runs at all, and Docker when there is none, which
// is what a wrapper (make up, a script) given --cmd-type has always meant.
func Detect(command string) string {
	all := commands(command)
	for _, words := range all {
		if name, _, ok := engineIn(words); ok {
			return name
		}
	}
	for _, words := range all {
		if name, ok := engineOf(words); ok {
			return name
		}
	}
	return Docker
}
