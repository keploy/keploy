package docker

import (
	"context"
	"go/ast"
	"go/token"
	"runtime"
	"strings"
	"testing"

	"github.com/keploy/shlex"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

// TestShellQuote pins the quoting used for every operator-supplied string
// interpolated into the docker alias, against the two tokenizers that actually
// execute it: `sh -c` and the shlex fallback utils.CommandContext uses on
// images with no /bin/sh. Both implement POSIX single-quote rules, so one
// helper is correct for both — this asserts the round trip rather than the
// literal escaping, because the round trip is what matters.
func TestShellQuote(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("the windows alias runner has no quoting layer; see buildUpstreamTLSFlags")
	}
	for _, raw := range []string{
		"",
		"/etc/corp/ca.pem",
		"/Users/me/My Certs/ca.pem",
		"/it's/ca.pem",
		"/tmp/$(touch /tmp/pwned).pem",
		"/tmp/a;rm -rf /.pem",
		"/tmp/`id`.pem",
		`/tmp/back\slash.pem`,
	} {
		line := "keploy --upstream-tls-ca-cert=" + shellQuote(raw)
		args, err := shlex.Split(line)
		if err != nil {
			t.Fatalf("shlex.Split(%q): %v", line, err)
		}
		if len(args) != 2 {
			t.Fatalf("%q split into %d words (%q); a quoted path must stay ONE argument", raw, len(args), args)
		}
		if got, want := args[1], "--upstream-tls-ca-cert="+raw; got != want {
			t.Fatalf("round trip of %q gave %q, want %q", raw, got, want)
		}
	}
}

// TestBuildUpstreamTLSFlags_UnconditionalAndQuoted covers both halves of the
// docker-side propagation fix.
//
// FAILS BEFORE THE FIX on two counts: --upstream-tls-verify was appended only
// when true (so an explicit false was indistinguishable from "not specified"
// and could not switch a keploy.yml `verify: true` off), and the CA path was
// concatenated unquoted (so a path with a space became two argv words and a
// path with metacharacters injected into a sudo'd docker run).
func TestBuildUpstreamTLSFlags_UnconditionalAndQuoted(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows takes the no-quoting branch; see TestBuildUpstreamTLSFlags_WindowsRejectsUnsplittablePath (which no CI job runs — see its caveat)")
	}

	t.Run("verify=false is still forwarded", func(t *testing.T) {
		got, err := buildUpstreamTLSFlags(models.SetupOptions{UpstreamTLSVerify: false})
		if err != nil {
			t.Fatalf("buildUpstreamTLSFlags: %v", err)
		}
		if !strings.Contains(got, "--upstream-tls-verify=false") {
			t.Fatalf("alias flags %q do not carry an explicit =false; the agent cannot tell an off switch from silence", got)
		}
	})

	t.Run("verify=true is forwarded", func(t *testing.T) {
		got, err := buildUpstreamTLSFlags(models.SetupOptions{UpstreamTLSVerify: true})
		if err != nil {
			t.Fatalf("buildUpstreamTLSFlags: %v", err)
		}
		if !strings.Contains(got, "--upstream-tls-verify=true") {
			t.Fatalf("alias flags %q do not carry =true", got)
		}
	})

	t.Run("a CA path with a space survives as one argument", func(t *testing.T) {
		const path = "/Users/me/My Certs/ca.pem"
		flags, err := buildUpstreamTLSFlags(models.SetupOptions{
			UpstreamTLSVerify: true,
			UpstreamTLSCACert: path,
		})
		if err != nil {
			t.Fatalf("buildUpstreamTLSFlags: %v", err)
		}
		args, err := shlex.Split("keploy" + flags)
		if err != nil {
			t.Fatalf("shlex.Split: %v", err)
		}
		var found bool
		for _, a := range args {
			if a == "--upstream-tls-ca-cert="+path {
				found = true
			}
		}
		if !found {
			t.Fatalf("the CA path did not survive tokenisation as one argument: %q", args)
		}
	})

	t.Run("a CA path with shell metacharacters does not become a command", func(t *testing.T) {
		const path = "/tmp/$(touch /tmp/pwned);id.pem"
		flags, err := buildUpstreamTLSFlags(models.SetupOptions{
			UpstreamTLSVerify: true,
			UpstreamTLSCACert: path,
		})
		if err != nil {
			t.Fatalf("buildUpstreamTLSFlags: %v", err)
		}
		args, err := shlex.Split("keploy" + flags)
		if err != nil {
			t.Fatalf("shlex.Split: %v", err)
		}
		if len(args) != 3 {
			t.Fatalf("expected exactly 3 words (keploy, --upstream-tls-verify=true, --upstream-tls-ca-cert=<path>), got %q", args)
		}
		if args[2] != "--upstream-tls-ca-cert="+path {
			t.Fatalf("metacharacters escaped their quoting: %q", args[2])
		}
	})
}

// TestBuildUpstreamTLSFlags_WindowsRejectsUnsplittablePath documents the one
// platform where no escaping can help: util_windows.go hands the alias to
// cmd.exe as strings.Split(alias, " "). Refusing with a message beats emitting
// a docker run that silently loses half the path.
//
// CAVEAT: this test currently runs NOWHERE. It skips off windows, and
// .github/workflows/go_windows.yml only runs `go build -v ./...`, never
// `go test`. Adding a windows test job would make it real cover; until then
// the windows branch of buildUpstreamTLSFlags is asserted by reading, not by
// running, and this test only pays off for someone running it locally on
// windows. Do not cite it as CI coverage.
func TestBuildUpstreamTLSFlags_WindowsRejectsUnsplittablePath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("windows-only branch; see the caveat above — no CI job runs this")
	}
	if _, err := buildUpstreamTLSFlags(models.SetupOptions{
		UpstreamTLSCACert: `C:\My Certs\ca.pem`,
	}); err == nil {
		t.Fatal("a space-bearing CA path was accepted on windows, where the alias is split on spaces")
	}
	if _, err := buildUpstreamTLSFlags(models.SetupOptions{
		UpstreamTLSCACert: `C:\certs\ca.pem`,
	}); err != nil {
		t.Fatalf("a plain windows path was rejected: %v", err)
	}
}

// ---------------------------------------------------------------------------
// Orchestrator → agent propagation, DOCKER legs.
//
// The host's keploy.yml is not visible inside the agent container, so argv is
// the ONLY channel by which record.upstreamTls reaches the agent. The native
// leg (pkg/platform/http/agent.go) is covered elsewhere; these cover the two
// containerised ones — `docker run` (getAlias) and docker-compose
// (GenerateKeployAgentService).
//
// Both directions matter equally. An UNCONFIGURED run must still forward
// `--upstream-tls-verify=false` with an EMPTY CA path, because
// proxy.resolveUpstreamTLSConfig distinguishes "the orchestrator said false"
// from "the orchestrator said nothing" — dropping the flag when the feature is
// off is what let a keploy.yml `verify: true` win over an explicit
// `--upstream-tls-verify=false` on native runs but not under docker.
// ---------------------------------------------------------------------------

// aliasArgv tokenises a generated docker alias the way the alias is actually
// executed on this platform: PrepareDockerCommand → utils.CommandContext →
// `sh -c`, or the shlex fallback on images with no /bin/sh. Asserting on argv
// rather than on a substring is the point — a CA path that lost its quoting
// still SUBSTRING-matches while having become two arguments.
func aliasArgv(t *testing.T, alias string) []string {
	t.Helper()
	argv, err := shlex.Split(alias)
	if err != nil {
		t.Fatalf("shlex.Split(%q): %v", alias, err)
	}
	return argv
}

// argvFlagValues returns every value carried by `--flag=value` occurrences in
// argv, in order. Returning all of them (rather than the first) is what lets a
// caller assert the flag is forwarded EXACTLY once — two occurrences would
// leave the agent's answer up to pflag's last-wins ordering.
func argvFlagValues(argv []string, flag string) []string {
	prefix := flag + "="
	var out []string
	for _, a := range argv {
		if strings.HasPrefix(a, prefix) {
			out = append(out, strings.TrimPrefix(a, prefix))
		}
	}
	return out
}

// requireSoleFlagValue asserts flag appears exactly once in argv, in
// `--flag=value` form, carrying want.
func requireSoleFlagValue(t *testing.T, argv []string, flag, want string) {
	t.Helper()
	got := argvFlagValues(argv, flag)
	if len(got) != 1 {
		t.Fatalf("%s appears %d times in the generated argv, want exactly 1: %q", flag, len(got), argv)
	}
	if got[0] != want {
		t.Fatalf("%s=%q, want %q (argv: %q)", flag, got[0], want, argv)
	}
	// The bare flag must never appear as its own word: `--flag value`
	// would be a second argv element that a space-bearing path splits.
	for _, a := range argv {
		if a == flag {
			t.Fatalf("%s was emitted as a bare word rather than --flag=value: %q", flag, argv)
		}
	}
}

// TestGetAlias_LinuxForwardsUpstreamTLS drives the real `docker run` alias
// builder. Only the linux switch arm is reachable here — getAlias branches on
// runtime.GOOS, and the darwin/windows arms additionally shell out to
// `docker context ls` — so the remaining four arms are covered structurally by
// TestGetAlias_EveryPlatformBranchForwardsUpstreamTLSFlags below.
func TestGetAlias_LinuxForwardsUpstreamTLS(t *testing.T) {
	t.Parallel()

	if runtime.GOOS != "linux" {
		t.Skip("getAlias switches on runtime.GOOS; only the linux arm is reachable here")
	}

	baseOpts := models.SetupOptions{
		KeployContainer: "keploy-test",
		AgentPort:       16789,
		ProxyPort:       16790,
		DnsPort:         16791,
		ClientNSPID:     4242,
		Mode:            models.MODE_RECORD,
	}

	t.Run("unconfigured still forwards an explicit off and an empty CA", func(t *testing.T) {
		t.Parallel()

		alias, err := getAlias(context.Background(), zap.NewNop(), baseOpts, false)
		if err != nil {
			t.Fatalf("getAlias: %v", err)
		}
		argv := aliasArgv(t, alias)

		// Explicit =false, not silence: silence is what the agent reads as
		// "fall back to keploy.yml", and the container has no keploy.yml.
		requireSoleFlagValue(t, argv, "--upstream-tls-verify", "false")
		// An empty CA path is the same thing the native leg sends and
		// resolves to "no extra anchors" in the agent — i.e. it changes
		// nothing for an operator who never configured the feature.
		requireSoleFlagValue(t, argv, "--upstream-tls-ca-cert", "")
	})

	t.Run("verify on with a CA path", func(t *testing.T) {
		t.Parallel()

		opts := baseOpts
		opts.UpstreamTLSVerify = true
		opts.UpstreamTLSCACert = "/etc/corp/ca.pem"

		alias, err := getAlias(context.Background(), zap.NewNop(), opts, false)
		if err != nil {
			t.Fatalf("getAlias: %v", err)
		}
		argv := aliasArgv(t, alias)

		requireSoleFlagValue(t, argv, "--upstream-tls-verify", "true")
		requireSoleFlagValue(t, argv, "--upstream-tls-ca-cert", "/etc/corp/ca.pem")
	})

	t.Run("an explicit off is forwarded even when a CA path is configured", func(t *testing.T) {
		t.Parallel()

		opts := baseOpts
		opts.UpstreamTLSVerify = false
		opts.UpstreamTLSCACert = "/etc/corp/ca.pem"

		alias, err := getAlias(context.Background(), zap.NewNop(), opts, false)
		if err != nil {
			t.Fatalf("getAlias: %v", err)
		}
		argv := aliasArgv(t, alias)

		requireSoleFlagValue(t, argv, "--upstream-tls-verify", "false")
		requireSoleFlagValue(t, argv, "--upstream-tls-ca-cert", "/etc/corp/ca.pem")
	})

	t.Run("a CA path with a space survives the whole alias round trip", func(t *testing.T) {
		t.Parallel()

		const caPath = "/Users/me/My Certs/ca.pem"
		opts := baseOpts
		opts.UpstreamTLSVerify = true
		opts.UpstreamTLSCACert = caPath

		alias, err := getAlias(context.Background(), zap.NewNop(), opts, false)
		if err != nil {
			t.Fatalf("getAlias: %v", err)
		}
		// Tokenised out of the FULL alias, not out of buildUpstreamTLSFlags
		// alone: the flags are concatenated into a string that also carries
		// quoted -e envs and free-form ExtraArgs, so this is the assertion
		// that the path is still one argv word by the time docker sees it.
		requireSoleFlagValue(t, aliasArgv(t, alias), "--upstream-tls-ca-cert", caPath)
	})
}

// TestGetAlias_EveryPlatformBranchForwardsUpstreamTLSFlags covers the four
// getAlias arms that cannot run here.
//
// getAlias switches on runtime.GOOS and each arm rebuilds the alias from
// scratch, appending the same flag blocks in the same order. On linux the
// windows/colima, windows/default, darwin/colima and darwin/default arms are
// dead code at runtime (and the darwin/windows arms shell out to
// `docker context ls` before they get anywhere), so no amount of fixture work
// executes them — but the copy-paste between them is exactly the hazard: the
// upstream-TLS flags were added to five places by hand, and dropping one
// silently disables the feature for that platform.
//
// So assert the invariant on the source instead: EVERY path that returns an
// alias must have appended upstreamTLSFlags first. That is checkable exactly,
// it fails if any arm loses the append, and it automatically covers a sixth
// arm if one is ever added.
func TestGetAlias_EveryPlatformBranchForwardsUpstreamTLSFlags(t *testing.T) {
	t.Parallel()

	forEachAliasReturningBranch(t, func(t *testing.T, pos token.Position, list []ast.Stmt) {
		t.Helper()

		for _, stmt := range list {
			if isAliasAppendOf(stmt, "upstreamTLSFlags") {
				return
			}
		}
		t.Errorf("%s: this getAlias platform branch returns the alias without ever appending upstreamTLSFlags — the agent container on that platform would never receive --upstream-tls-verify/--upstream-tls-ca-cert and would silently fall back to its own defaults", pos)
	})
}

// isAliasAppendOf reports whether stmt is `alias += <ident>`.
func isAliasAppendOf(stmt ast.Stmt, ident string) bool {
	assign, ok := stmt.(*ast.AssignStmt)
	if !ok || assign.Tok != token.ADD_ASSIGN || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
		return false
	}
	lhs, ok := assign.Lhs[0].(*ast.Ident)
	if !ok || lhs.Name != "alias" {
		return false
	}
	rhs, ok := assign.Rhs[0].(*ast.Ident)
	return ok && rhs.Name == ident
}

// isReturnAliasNil reports whether stmt is `return alias, nil`.
func isReturnAliasNil(stmt ast.Stmt) bool {
	ret, ok := stmt.(*ast.ReturnStmt)
	if !ok || len(ret.Results) != 2 {
		return false
	}
	first, ok := ret.Results[0].(*ast.Ident)
	if !ok || first.Name != "alias" {
		return false
	}
	second, ok := ret.Results[1].(*ast.Ident)
	return ok && second.Name == "nil"
}

// commandScalars returns the agent service's `command` list as plain strings.
// Compose passes each element as its own argv word, so unlike the docker-run
// alias there is no shell to re-split them — which is precisely what has to be
// asserted for a CA path containing a space.
func commandScalars(t *testing.T, serviceNode *yaml.Node) []string {
	t.Helper()
	cmd := mappingValue(serviceNode, "command")
	if cmd == nil {
		t.Fatal("the generated keploy-agent service has no command block")
	}
	if cmd.Kind != yaml.SequenceNode {
		t.Fatalf("command is a %v, want a sequence", cmd.Kind)
	}
	out := make([]string, 0, len(cmd.Content))
	for _, n := range cmd.Content {
		out = append(out, n.Value)
	}
	return out
}

func newAgentService(t *testing.T, opts models.SetupOptions) *yaml.Node {
	t.Helper()
	serviceNode, err := (&Impl{
		logger: zap.NewNop(),
		conf:   &config.Config{},
	}).GenerateKeployAgentService(opts)
	if err != nil {
		t.Fatalf("GenerateKeployAgentService: %v", err)
	}
	return serviceNode
}

// TestGenerateKeployAgentService_ForwardsUpstreamTLS is the compose leg of the
// same propagation path. A compose run's agent container has no keploy.yml
// either, so the same unconditional `--flag=value` contract applies.
func TestGenerateKeployAgentService_ForwardsUpstreamTLS(t *testing.T) {
	t.Parallel()

	baseOpts := models.SetupOptions{
		KeployContainer: "keploy-agent",
		AgentPort:       16789,
		ProxyPort:       16790,
		DnsPort:         16791,
		Mode:            models.MODE_RECORD,
	}

	t.Run("unconfigured still forwards an explicit off and an empty CA", func(t *testing.T) {
		t.Parallel()

		argv := commandScalars(t, newAgentService(t, baseOpts))
		requireSoleFlagValue(t, argv, "--upstream-tls-verify", "false")
		requireSoleFlagValue(t, argv, "--upstream-tls-ca-cert", "")
	})

	t.Run("verify on with a CA path", func(t *testing.T) {
		t.Parallel()

		opts := baseOpts
		opts.UpstreamTLSVerify = true
		opts.UpstreamTLSCACert = "/etc/corp/ca.pem"

		argv := commandScalars(t, newAgentService(t, opts))
		requireSoleFlagValue(t, argv, "--upstream-tls-verify", "true")
		requireSoleFlagValue(t, argv, "--upstream-tls-ca-cert", "/etc/corp/ca.pem")
	})

	t.Run("an explicit off is forwarded even when a CA path is configured", func(t *testing.T) {
		t.Parallel()

		opts := baseOpts
		opts.UpstreamTLSVerify = false
		opts.UpstreamTLSCACert = "/etc/corp/ca.pem"

		argv := commandScalars(t, newAgentService(t, opts))
		requireSoleFlagValue(t, argv, "--upstream-tls-verify", "false")
		requireSoleFlagValue(t, argv, "--upstream-tls-ca-cert", "/etc/corp/ca.pem")
	})

	t.Run("a CA path with a space stays one command element", func(t *testing.T) {
		t.Parallel()

		const caPath = "/Users/me/My Certs/ca.pem"
		opts := baseOpts
		opts.UpstreamTLSVerify = true
		opts.UpstreamTLSCACert = caPath

		argv := commandScalars(t, newAgentService(t, opts))
		requireSoleFlagValue(t, argv, "--upstream-tls-ca-cert", caPath)
	})
}
