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

// TestKeepAgentTokenThroughSudo: `sudo docker compose up` is a spelling keploy
// recognises as compose, and sudo's env_reset drops the control-plane token
// the generated agent service asks compose for. Without --preserve-env that
// agent comes up with no token at all.
func TestKeepAgentTokenThroughSudo(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"sudo docker compose up", "sudo --preserve-env=KEPLOY_AGENT_TOKEN docker compose up"},
		{"sudo -E docker compose up", "sudo --preserve-env=KEPLOY_AGENT_TOKEN -E docker compose up"},
		{"  sudo docker-compose up", "  sudo --preserve-env=KEPLOY_AGENT_TOKEN docker-compose up"},
		{"docker compose up", "docker compose up"},
		{"sudoku up", "sudoku up"},
	} {
		if got := keepAgentTokenThroughSudo(tc.in); got != tc.want {
			t.Errorf("keepAgentTokenThroughSudo(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// TestWithAgentToken_OnlyTheComposeCommandIsGivenTheAgentToken: under compose
// the app command is what starts the agent, so it must carry the token its
// compose file names — through a leading sudo too. Under every other kind the
// app command is the application under test, which is handed nothing.
func TestWithAgentToken_OnlyTheComposeCommandIsGivenTheAgentToken(t *testing.T) {
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
