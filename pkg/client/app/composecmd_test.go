package app

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/platform/docker"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// SetupCompose can only splice `-f <generated>.yaml` into a command that is a
// literal docker compose invocation. For a wrapper it must leave the command
// alone and hand the file over through COMPOSE_FILE instead — splicing
// produces `make -f ./docker-compose-tmp.yaml up`, which fails with an
// unrecognised option nowhere near its cause.
func TestIsRewritableComposeCommand(t *testing.T) {
	rewritable := []string{
		"docker compose up",
		"docker-compose up",
		"sudo -E docker compose up",
		"env FOO=bar docker compose -f custom.yml up",
		"/usr/bin/docker compose up",
		"DOCKER COMPOSE UP",
	}
	for _, cmd := range rewritable {
		if !isRewritableComposeCommand(cmd) {
			t.Errorf("%q: want rewritable, got not — keploy would fall back to COMPOSE_FILE "+
				"and leave the user's --abort-* flags in for no reason", cmd)
		}
	}

	wrappers := []string{
		"make up",
		"./start.sh",
		"npm run dev",
		"task compose-up",
	}
	for _, cmd := range wrappers {
		if isRewritableComposeCommand(cmd) {
			t.Errorf("%q: want NOT rewritable, got rewritable — keploy would splice compose "+
				"flags into a command that cannot take them", cmd)
		}
	}
}

// composeLaunchPlan is the decision SetupCompose acts on, and the branch that
// used to survive mutation: flipping it either way left every test green
// while restoring the `make -f ./docker-compose-tmp.yaml up` failure the
// whole feature exists to avoid.
func TestComposeLaunchPlan(t *testing.T) {
	const generated = "docker-compose-tmp.yaml"

	t.Run("literal compose command is rewritten, no env", func(t *testing.T) {
		newCmd, env := composeLaunchPlan("docker compose up", generated, "docker-compose.yml")
		if newCmd == "" {
			t.Fatal("want the command rewritten, got none")
		}
		if !strings.Contains(newCmd, generated) {
			t.Errorf("rewritten command %q does not reference %q", newCmd, generated)
		}
		if env != "" {
			t.Errorf("COMPOSE_FILE should not be used when the command can be rewritten, got %q", env)
		}
	})

	t.Run("wrapper is left alone and gets an absolute env path", func(t *testing.T) {
		newCmd, env := composeLaunchPlan("make up", generated, "docker-compose.yml")
		if newCmd != "" {
			t.Errorf("wrapper must not be rewritten, got %q — splicing compose flags into a "+
				"wrapper produces a command it cannot parse", newCmd)
		}
		if env == "" {
			t.Fatal("wrapper needs COMPOSE_FILE, got none")
		}
		if !filepath.IsAbs(env) {
			t.Errorf("COMPOSE_FILE %q is relative; compose resolves it against its OWN working "+
				"directory, so `make -C deploy up` would look in the wrong place", env)
		}
		if filepath.Base(env) != generated {
			t.Errorf("COMPOSE_FILE %q does not point at %q", env, generated)
		}
	})

	t.Run("exactly one mechanism is chosen", func(t *testing.T) {
		for _, cmd := range []string{"docker compose up", "docker-compose up", "make up", "./start.sh"} {
			newCmd, env := composeLaunchPlan(cmd, generated, "docker-compose.yml")
			if (newCmd == "") == (env == "") {
				t.Errorf("%q: want exactly one of rewrite/env, got newCmd=%q env=%q", cmd, newCmd, env)
			}
		}
	})
}

// TestAgentTokenCommand: `sudo docker compose up` is a spelling keploy
// recognises as compose, and sudo's env_reset (or doas's) drops the
// control-plane token the generated agent service asks compose for. A root
// keploy needs no sudo, so it drops a plain one and runs compose with the
// token itself. Any other keploy tells sudo to keep that one variable.
func TestAgentTokenCommand(t *testing.T) {
	const keep = "sudo --preserve-env=KEPLOY_AGENT_TOKEN "
	for _, tc := range []struct {
		in          string
		root        bool
		want        string
		wantViaSudo bool
	}{
		{in: "sudo docker compose up", want: keep + "docker compose up", wantViaSudo: true},
		{in: "sudo -E docker compose up", want: keep + "-E docker compose up", wantViaSudo: true},
		{in: "  sudo docker-compose up", want: "  " + keep + "docker-compose up", wantViaSudo: true},
		{in: "sudo\tdocker compose up", want: keep + "docker compose up", wantViaSudo: true},
		{in: "doas docker compose up", want: "doas docker compose up"},
		{in: "docker compose up", want: "docker compose up"},
		{in: "sudoku up", want: "sudoku up"},
		{in: "sudo", want: "sudo"},

		{in: "sudo docker compose up", root: true, want: "docker compose up"},
		{in: "sudo -E docker compose up", root: true, want: "docker compose up"},
		{in: "sudo --preserve-env -E  docker compose up", root: true, want: "docker compose up"},
		{in: "  sudo\tdocker-compose up", root: true, want: "  docker-compose up"},
		{in: "doas docker compose up", root: true, want: "docker compose up"},
		{in: "docker compose up", root: true, want: "docker compose up"},
		{in: "sudoku up", root: true, want: "sudoku up"},
		// Options that do nothing for root either: no password to ask for,
		// on a terminal (-n) or on stdin (-S), and the end of the options.
		{in: "sudo -n docker compose up", root: true, want: "docker compose up"},
		{in: "sudo -S docker compose up", root: true, want: "docker compose up"},
		{in: "sudo --non-interactive --stdin docker compose up", root: true, want: "docker compose up"},
		{in: "sudo -- docker compose up", root: true, want: "docker compose up"},
		{in: "sudo -n -- docker compose up", root: true, want: "docker compose up"},
		{in: "sudo -EnS docker compose up", root: true, want: "docker compose up"},
		{in: "doas -n docker compose up", root: true, want: "docker compose up"},
		{in: "doas -- docker compose up", root: true, want: "docker compose up"},
		// Not a plain elevation to root: kept, and told to keep the token.
		{in: "sudo -u app docker compose up", root: true, want: keep + "-u app docker compose up", wantViaSudo: true},
		{in: "sudo -nu app docker compose up", root: true, want: keep + "-nu app docker compose up", wantViaSudo: true},
		{in: "sudo -H docker compose up", root: true, want: keep + "-H docker compose up", wantViaSudo: true},
		{in: "sudo - docker compose up", root: true, want: keep + "- docker compose up", wantViaSudo: true},
		{in: "sudo --preserve-env=HOME docker compose up", root: true, want: keep + "--preserve-env=HOME docker compose up", wantViaSudo: true},
		{in: "sudo -E", root: true, want: keep + "-E", wantViaSudo: true},
		{in: "sudo -n --", root: true, want: keep + "-n --", wantViaSudo: true},
		{in: "doas -u app docker compose up", root: true, want: "doas -u app docker compose up"},
		{in: "doas -E docker compose up", root: true, want: "doas -E docker compose up"},
		// Not root: every sudo is needed, whatever its options.
		{in: "sudo -n -- docker compose up", want: keep + "-n -- docker compose up", wantViaSudo: true},
		{in: "doas -n docker compose up", want: "doas -n docker compose up"},
	} {
		got, viaSudo := agentTokenCommand(tc.in, tc.root, token.Env)
		if got != tc.want || viaSudo != tc.wantViaSudo {
			t.Errorf("agentTokenCommand(%q, root=%v) = %q, %v; want %q, %v", tc.in, tc.root, got, viaSudo, tc.want, tc.wantViaSudo)
		}
	}
}

// TestWithAgentToken_OnlyTheComposeCommandIsGivenTheAgentToken: under compose
// the app command is what starts the agent, so it must carry the token its
// compose file names — through a leading sudo too. Under every other kind the
// app command is the application under test, which is handed nothing.
func TestWithAgentToken_OnlyTheComposeCommandIsGivenTheAgentToken(t *testing.T) {
	setEffectiveUID(t, 1000)
	const cmd = "sudo docker compose -f docker-compose-tmp.yaml up"
	gotCmd, gotEnv := (&App{kind: utils.DockerCompose}).withAgentToken(cmd)
	if want := []string{token.Env + "=" + token.Session()}; !slices.Equal(gotEnv, want) {
		t.Errorf("compose command env = %v, want %v", gotEnv, want)
	}
	if want := "sudo --preserve-env=" + token.Env + " docker compose -f docker-compose-tmp.yaml up"; gotCmd != want {
		t.Errorf("compose command = %q, want %q: sudo would drop the token before compose could fill it in", gotCmd, want)
	}
	for _, kind := range []utils.CmdType{utils.Native, utils.DockerRun, utils.DockerStart, utils.FromContainer} {
		gotCmd, gotEnv := (&App{kind: kind}).withAgentToken(cmd)
		if gotEnv != nil {
			t.Errorf("%s: the application under test was handed %v", kind, gotEnv)
		}
		if gotCmd != cmd {
			t.Errorf("%s: the application's command was rewritten to %q", kind, gotCmd)
		}
	}
}

// TestWithAgentTokenKeepsTheMockTokenThroughSudo: in mock mode the app
// command also needs the mock agent's token. A user's sudo is told to keep it.
// When keploy is root it drops a plain sudo instead (agentTokenCommand), and
// the command inherits both tokens as they are. Each half sets the uid keploy
// sees, so the row holds whichever user go test runs as.
func TestWithAgentTokenKeepsTheMockTokenThroughSudo(t *testing.T) {
	mock := models.SetupOptions{MockMode: true}
	t.Run("not root", func(t *testing.T) {
		setEffectiveUID(t, 1000)
		gotCmd, _ := (&App{kind: utils.DockerCompose, opts: mock}).withAgentToken("sudo docker compose up")
		if want := "sudo --preserve-env=" + token.Env + "," + token.MockAgentTokenEnv + " docker compose up"; gotCmd != want {
			t.Errorf("compose command = %q, want %q", gotCmd, want)
		}
		gotCmd, gotEnv := (&App{kind: utils.DockerRun, opts: mock}).withAgentToken("sudo docker run --rm e2e")
		if want := "sudo --preserve-env=" + token.MockAgentTokenEnv + " docker run --rm e2e"; gotCmd != want || gotEnv != nil {
			t.Errorf("docker run command = %q, env %v, want %q", gotCmd, gotEnv, want)
		}
		if gotCmd, _ := (&App{kind: utils.DockerRun}).withAgentToken("sudo docker run --rm e2e"); gotCmd != "sudo docker run --rm e2e" {
			t.Errorf("outside mock mode docker run was rewritten to %q", gotCmd)
		}
		if got, _ := agentTokenCommand("sudo docker run x", false); got != "sudo docker run x" {
			t.Errorf("nothing to keep, yet rewritten to %q", got)
		}
	})
	t.Run("root", func(t *testing.T) {
		setEffectiveUID(t, 0)
		gotCmd, _ := (&App{kind: utils.DockerCompose, opts: mock}).withAgentToken("sudo docker compose up")
		if want := "docker compose up"; gotCmd != want {
			t.Errorf("compose command = %q, want %q: root's sudo elevates nothing and resets the tokens", gotCmd, want)
		}
		gotCmd, gotEnv := (&App{kind: utils.DockerRun, opts: mock}).withAgentToken("sudo docker run --rm e2e")
		if want := "docker run --rm e2e"; gotCmd != want || gotEnv != nil {
			t.Errorf("docker run command = %q, env %v, want %q", gotCmd, gotEnv, want)
		}
	})
}

// keploy splices its compose file in before the `up` subcommand, and leaves
// compose's abort flags out (compose_appstop.go): the subcommand is the word "up" that is not a global
// flag's value, so a project (-p) or profile whose name starts with, or is,
// "up" keeps its value, and the command still runs `up`.
func TestComposeCommandRewriteFindsTheUpSubcommand(t *testing.T) {
	const tmp = "docker-compose-tmp.yaml"
	for cmd, want := range map[string]string{
		"docker compose up":                    "docker compose -f ./docker-compose-tmp.yaml up",
		"docker compose -p upload up":          "docker compose -p upload -f ./docker-compose-tmp.yaml up",
		"docker compose -p up up --build":      "docker compose -p up -f ./docker-compose-tmp.yaml up --build",
		"docker compose --profile upstream up": "docker compose --profile upstream -f ./docker-compose-tmp.yaml up",
		"docker-compose -p updates up -d":      "docker-compose -p updates -f ./docker-compose-tmp.yaml up -d",
		// A wrapper's quoted script: keploy's file is spliced inside it, where
		// compose runs.
		`bash -c "docker compose up"`:       `bash -c "docker compose -f ./docker-compose-tmp.yaml up"`,
		`sh -c 'docker compose -p shop up'`: `sh -c 'docker compose -p shop -f ./docker-compose-tmp.yaml up'`,
		// A quoted file's name is one word, however many "up"s it holds.
		`docker compose -f "my up dir/c.yaml" up`: `docker compose -f "my up dir/c.yaml" up`,
	} {
		if got := modifyDockerComposeCommand(cmd, tmp, "docker-compose.yml"); got != want {
			t.Errorf("%q:\n got  %q\n want %q", cmd, got, want)
		}
		if strings.HasPrefix(cmd, "docker") {
			name, _ := composeProjectScope(modifyDockerComposeCommand(cmd, tmp, "docker-compose.yml"))
			if wantName, _ := composeProjectScope(cmd); name != wantName {
				t.Errorf("%q: the rewritten command's project is %q, want %q", cmd, name, wantName)
			}
		}
	}
	if got := modifyDockerComposeCommand("docker compose -p up up --abort-on-container-exit --exit-code-from app", tmp, "docker-compose.yml"); got != "docker compose -p up -f ./docker-compose-tmp.yaml up" {
		t.Errorf("-p up up: %q; the user's abort flags are left out, the project's value kept", got)
	}
	if at, ok := composeUpOffset(`docker compose -f "my up.yaml" up`, false); !ok || at != len(`docker compose -f "my up.yaml" `) {
		t.Errorf("a quoted file name is one word: up at %d (%v)", at, ok)
	}
	if _, ok := composeUpOffset(`bash -c "docker compose up"`, false); ok {
		t.Error("a wrapper's quoted script has no `up` of the command's own")
	}
}

// TestSetupComposeWrapperDoesNotLeakComposeFileEnvAcrossTestSets pins the
// per-test-set replay flow (runner.go:376) for wrapper commands such as
// `make up`. SetupCompose must hand COMPOSE_FILE only to the wrapper child's
// environment (withAgentToken -> ExecuteCommand extraEnv) and NEVER mutate
// keploy's own process environment via os.Setenv: otherwise the next test set's
// FindComposeFiles reads keploy's own generated docker-compose-tmp.yaml (which
// already defines keploy-agent), drops user overrides / original COMPOSE_FILE
// published ports from composeExcludedPorts, and fails in
// AddKeployAgentToCompose.
func TestSetupComposeWrapperDoesNotLeakComposeFileEnvAcrossTestSets(t *testing.T) {
	dc, err := docker.New(zap.NewNop(), &config.Config{})
	if err != nil {
		t.Fatalf("docker.New: %v", err)
	}

	const baseYAML = "services:\n  app:\n    image: alpine:3.21\n    container_name: app\n    ports:\n      - \"61560:8080\"\n"
	const overrideYAML = "services:\n  sidecar:\n    image: alpine:3.21\n    ports:\n      - \"61561:9000\"\n"
	const customYAML = "services:\n  app:\n    image: alpine:3.21\n    container_name: app\n    ports:\n      - \"61562:8080\"\n"

	t.Run("default compose + override across two test sets", func(t *testing.T) {
		dir := t.TempDir()
		if err := os.WriteFile(filepath.Join(dir, "docker-compose.yml"), []byte(baseYAML), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, "docker-compose.override.yml"), []byte(overrideYAML), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		t.Setenv("COMPOSE_FILE", "")
		t.Setenv("COMPOSE_PATH_SEPARATOR", "")

		opts1 := models.SetupOptions{CommandType: string(utils.DockerCompose), Container: "app", AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791}
		a1 := NewApp(zap.NewNop(), "make up", dc, opts1)
		if err := a1.SetupCompose(context.Background(), nil); err != nil {
			t.Fatalf("test set 1 SetupCompose: %v", err)
		}

		// Test set 2 SetupCompose in the same process must succeed (fails in
		// AddKeployAgentToCompose if COMPOSE_FILE leaked docker-compose-tmp.yaml).
		opts2 := models.SetupOptions{CommandType: string(utils.DockerCompose), Container: "app", AgentPort: 16792, ProxyPort: 16790, DnsPort: 16791}
		a2 := NewApp(zap.NewNop(), "make up", dc, opts2)
		if err := a2.SetupCompose(context.Background(), nil); err != nil {
			t.Fatalf("test set 2 SetupCompose failed: %v", err)
		}

		// keploy's own process env must remain untouched.
		if got := os.Getenv("COMPOSE_FILE"); got != "" {
			t.Fatalf("SetupCompose leaked COMPOSE_FILE=%q into keploy's own process env", got)
		}

		// The wrapper child's env carries COMPOSE_FILE pointing at the generated file.
		wantGenerated, err := filepath.Abs("docker-compose-tmp.yaml")
		if err != nil {
			t.Fatal(err)
		}
		setEffectiveUID(t, 1000)
		_, env1 := a1.withAgentToken(a1.cmd.WholeUnredacted())
		if !slices.Contains(env1, "COMPOSE_FILE="+wantGenerated) {
			t.Fatalf("wrapper runEnv = %v, want COMPOSE_FILE=%s", env1, wantGenerated)
		}
		gotSudoCmd, envSudo := a1.withAgentToken("sudo make up")
		if wantSudo := "sudo --preserve-env=" + token.Env + ",COMPOSE_FILE make up"; gotSudoCmd != wantSudo {
			t.Fatalf("sudo wrapper cmd = %q, want %q", gotSudoCmd, wantSudo)
		}
		if !slices.Contains(envSudo, "COMPOSE_FILE="+wantGenerated) {
			t.Fatalf("sudo wrapper runEnv = %v, want COMPOSE_FILE=%s", envSudo, wantGenerated)
		}

		// Next test set's FindComposeFiles and port exclusion still see the user's
		// original base + override files, not docker-compose-tmp.yaml.
		gotPaths := docker.FindComposeFiles("make up")
		if wantPaths := []string{"docker-compose.yml", "docker-compose.override.yml"}; !slices.Equal(gotPaths, wantPaths) {
			t.Fatalf("test set 2 FindComposeFiles = %v, want %v", gotPaths, wantPaths)
		}
		gotPorts := docker.ComposePublishedHostPortsFromFiles(zap.NewNop(), gotPaths)
		if !slices.Contains(gotPorts, 61560) || !slices.Contains(gotPorts, 61561) {
			t.Fatalf("test set 2 excluded ports = %v, want 61560 and 61561", gotPorts)
		}
	})

	t.Run("user COMPOSE_FILE preserved across two test sets", func(t *testing.T) {
		dir := t.TempDir()
		customPath := filepath.Join(dir, "custom-compose.yml")
		if err := os.WriteFile(customPath, []byte(customYAML), 0o600); err != nil {
			t.Fatal(err)
		}
		t.Chdir(dir)
		t.Setenv("COMPOSE_FILE", customPath)
		t.Setenv("COMPOSE_PATH_SEPARATOR", "")

		opts1 := models.SetupOptions{CommandType: string(utils.DockerCompose), Container: "app", AgentPort: 16789, ProxyPort: 16790, DnsPort: 16791}
		a1 := NewApp(zap.NewNop(), "make up", dc, opts1)
		if err := a1.SetupCompose(context.Background(), nil); err != nil {
			t.Fatalf("test set 1 SetupCompose: %v", err)
		}

		// Test set 2 SetupCompose in the same process must still read customPath.
		opts2 := models.SetupOptions{CommandType: string(utils.DockerCompose), Container: "app", AgentPort: 16792, ProxyPort: 16790, DnsPort: 16791}
		a2 := NewApp(zap.NewNop(), "make up", dc, opts2)
		if err := a2.SetupCompose(context.Background(), nil); err != nil {
			t.Fatalf("test set 2 SetupCompose failed: %v", err)
		}
		if got := os.Getenv("COMPOSE_FILE"); got != customPath {
			t.Fatalf("keploy process COMPOSE_FILE = %q, want preserved user value %q", got, customPath)
		}
		wantGenerated, err := filepath.Abs("docker-compose-tmp.yaml")
		if err != nil {
			t.Fatal(err)
		}
		_, env1 := a1.withAgentToken(a1.cmd.WholeUnredacted())
		if !slices.Contains(env1, "COMPOSE_FILE="+wantGenerated) {
			t.Fatalf("wrapper runEnv = %v, want COMPOSE_FILE=%s", env1, wantGenerated)
		}
		if gotPaths := docker.FindComposeFiles("make up"); !slices.Equal(gotPaths, []string{customPath}) {
			t.Fatalf("test set 2 FindComposeFiles = %v, want [%s]", gotPaths, customPath)
		}
	})
}
