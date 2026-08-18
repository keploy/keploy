package app

import (
	"path/filepath"
	"strings"
	"testing"
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
