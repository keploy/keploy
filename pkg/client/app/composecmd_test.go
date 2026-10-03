package app

import (
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/token"
	"go.keploy.io/server/v3/utils"
)

// SetupCompose can only splice `-f <generated>.yaml` into a command that is a
// literal docker compose invocation. For a wrapper it must leave the command
// alone and hand the file over through COMPOSE_FILE instead — splicing
// produces `make -f ./docker-compose-tmp.yaml up --abort-on-container-exit`,
// which fails with an unrecognised option nowhere near its cause.
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
				"and lose --abort-on-container-exit for no reason", cmd)
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
// while restoring the `make -f ./docker-compose-tmp.yaml up
// --abort-on-container-exit` failure the whole feature exists to avoid.
func TestComposeLaunchPlan(t *testing.T) {
	const generated = "docker-compose-tmp.yaml"

	t.Run("literal compose command is rewritten, no env", func(t *testing.T) {
		newCmd, env := composeLaunchPlan("docker compose up", generated, "docker-compose.yml", "app")
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
		newCmd, env := composeLaunchPlan("make up", generated, "docker-compose.yml", "app")
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
			newCmd, env := composeLaunchPlan(cmd, generated, "docker-compose.yml", "app")
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
		got, viaSudo := agentTokenCommand(tc.in, tc.root)
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
