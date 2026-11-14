//go:build linux

package http

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"go.uber.org/zap"
)

// standInSudoEnv says what the stand-in sudo (putSudoOnPath) does:
// sudoAllows runs its command; anything else, unset included, refuses it, as
// sudo -n does where it would ask for a password -- most developer machines.
const standInSudoEnv = "KEPLOY_TEST_STAND_IN_SUDO"

const sudoAllows = "allows"

// sudoRefusal is what sudo -n says, on stderr, of a command it would have to
// ask a password for.
const sudoRefusal = "sudo: a password is required"

// putSudoOnPath puts a stand-in for sudo in dir, which the caller has first
// on PATH, so that no row depends on the host's own sudo: CI's runners let
// their user run anything through sudo, a developer's machine asks for a
// password. It notes each run in calls ("sudo" and its arguments), then
// refuses, or, as standInSudoEnv says, runs its command as sudo -n does once
// allowed, with KEPLOY_TEST_AS_ROOT set, as a command run as root.
func putSudoOnPath(t *testing.T, dir, calls string) {
	t.Helper()
	script := `#!/bin/sh
echo "sudo $*" >> "` + calls + `"
if [ "$` + standInSudoEnv + `" != ` + sudoAllows + ` ]; then
  echo "` + sudoRefusal + `" >&2
  exit 1
fi
[ "$1" = -n ] && shift
KEPLOY_TEST_AS_ROOT=1 exec "$@"
`
	if err := os.WriteFile(filepath.Join(dir, "sudo"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
}

// underEitherSudo runs row under a stand-in sudo that asks for a password,
// and under one that lets keploy's user run the engine (putSudoOnPath).
func underEitherSudo(t *testing.T, row func(t *testing.T)) {
	for _, sudo := range []string{"refuses", sudoAllows} {
		t.Run("under a sudo that "+sudo, func(t *testing.T) {
			t.Setenv(standInSudoEnv, sudo)
			row(t)
		})
	}
}

// The engine's CLI is retried through sudo only when the engine's daemon
// refused keploy's user (its socket's permission denied), and a run returns
// the engine's own answer: one of the daemon (a container it does not have,
// the daemon not running) is not retried, under a sudo that asks for a
// password or one that lets the user run the engine. A refusal is retried:
// under a sudo that lets the user run the engine, the command's answer as
// root is returned, and once one has succeeded every later command of the
// stop goes through sudo alone; under one that asks for a password, the
// engine's refusal is returned, with sudo's joined to it, and nothing is
// decided. So whichever output a command is read with (stdout, combined).
func TestTheEngineIsRetriedThroughSudoOnlyWhenItsDaemonRefusedKeploysUser(t *testing.T) {
	const (
		gone       = "Error: No such object: keploy-v3-sudo"
		docker29   = "permission denied while trying to connect to the docker API at unix:///var/run/docker.sock"
		olderDock  = `Got permission denied while trying to connect to the Docker daemon socket at unix:///var/run/docker.sock: Get "http://%2Fvar%2Frun%2Fdocker.sock/v1.24/containers/json": dial unix /var/run/docker.sock: connect: permission denied`
		dialDenied = `Error: unable to connect to Podman socket: Get "http://d/v5.8.7/libpod/_ping": dial unix /run/podman/podman.sock: connect: permission denied`
		down       = "Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?"
	)
	for _, tc := range []struct {
		name string
		// user is what the engine's CLI says, failing, run as keploy's
		// user; root, run as root ("": it answers "running").
		user, root, sudo string
		wantCalls        string // the commands run, in order
		wantMode         int32
		wantErr          bool
		wantGone         bool // engineSaysGone of what the run returned
	}{
		{"the daemon's answer, sudo asking for a password", gone, "", "refuses", "user", engineUndecided, true, true},
		{"the daemon's answer, sudo allowing", gone, "", sudoAllows, "user", engineUndecided, true, true},
		{"the daemon not running, sudo allowing", down, "", sudoAllows, "user", engineUndecided, true, false},
		{"the socket's permission denied, docker 29's words, sudo allowing", docker29, "", sudoAllows, "user sudo root", engineSudo, false, false},
		{"the socket's permission denied, older docker's words, sudo allowing", olderDock, "", sudoAllows, "user sudo root", engineSudo, false, false},
		{"the socket's permission denied, the dial's words, sudo allowing", dialDenied, "", sudoAllows, "user sudo root", engineSudo, false, false},
		{"the socket's permission denied, sudo asking for a password", docker29, "", "refuses", "user sudo", engineUndecided, true, false},
		{"the socket's permission denied, sudo allowing, the daemon's answer as root", docker29, gone, sudoAllows, "user sudo root", engineUndecided, true, true},
	} {
		for _, via := range []string{"stdout", "combined"} {
			t.Run(tc.name+", read from "+via, func(t *testing.T) {
				dir := t.TempDir()
				calls := filepath.Join(dir, "calls")
				userSaid, rootSaid := filepath.Join(dir, "user"), filepath.Join(dir, "root")
				if err := os.WriteFile(userSaid, []byte(tc.user+"\n"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(rootSaid, []byte(tc.root), 0o644); err != nil {
					t.Fatal(err)
				}
				script := `#!/bin/sh
if [ -n "$KEPLOY_TEST_AS_ROOT" ]; then
  echo root >> "` + calls + `"
  [ -s "` + rootSaid + `" ] && { cat "` + rootSaid + `" >&2; echo >&2; exit 1; }
  echo running
  exit 0
fi
echo user >> "` + calls + `"
cat "` + userSaid + `" >&2
exit 1
`
				if err := os.WriteFile(filepath.Join(dir, "docker"), []byte(script), 0o755); err != nil {
					t.Fatal(err)
				}
				putSudoOnPath(t, dir, calls)
				t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
				t.Setenv(standInSudoEnv, tc.sudo)
				cli := newEngineCLI(zap.NewNop())
				run := cli.stdout
				if via == "combined" {
					run = cli.combined
				}
				ran := func() string {
					b, _ := os.ReadFile(calls)
					var got []string
					for _, l := range strings.Split(string(b), "\n") {
						if f := strings.Fields(l); len(f) > 0 {
							got = append(got, f[0])
						}
					}
					return strings.Join(got, " ")
				}

				out, err := run(context.Background(), "inspect", "-f", "{{.State.Status}}", "keploy-v3-sudo")
				if got := ran(); got != tc.wantCalls {
					t.Errorf("the commands run were %q; want %q", got, tc.wantCalls)
				}
				if got := cli.mode.Load(); got != tc.wantMode {
					t.Errorf("the mode is %d; want %d", got, tc.wantMode)
				}
				if (err != nil) != tc.wantErr {
					t.Fatalf("the run returned %q, %v; want an error: %v", out, err, tc.wantErr)
				}
				if got := engineSaysGone(out, err); got != tc.wantGone {
					t.Errorf("the run returned %q, %v, which says the container is gone: %v; want %v", out, err, got, tc.wantGone)
				}
				if err == nil {
					if strings.TrimSpace(string(out)) != "running" {
						t.Errorf("the run returned %q; want the command's answer as root", out)
					}
					// The mode is kept: the next command goes through sudo
					// alone.
					if _, err := run(context.Background(), "inspect", "-f", "{{.State.Status}}", "keploy-v3-sudo"); err != nil {
						t.Fatalf("the next command failed: %v", err)
					}
					if got, want := ran(), tc.wantCalls+" sudo root"; got != want {
						t.Errorf("the commands run were %q; want %q", got, want)
					}
					return
				}
				// What is returned is the engine's own failure, as keploy's
				// user, with what sudo said, if it was asked, joined to it.
				var exit *exec.ExitError
				if !errors.As(err, &exit) {
					t.Fatalf("the run returned %v; want the engine's *exec.ExitError", err)
				}
				own := string(out)
				if via == "stdout" {
					own = string(exit.Stderr)
				}
				if !strings.Contains(own, tc.user) || strings.Contains(string(out), sudoRefusal) {
					t.Errorf("the run returned %q, with %q on stderr; want the engine's own %q as keploy's user", out, exit.Stderr, tc.user)
				}
				said := engineSaid(out, err)
				switch {
				case tc.wantCalls == "user sudo" && !strings.Contains(said, sudoRefusal):
					t.Errorf("the run said %q; want sudo's refusal joined to it", said)
				case tc.root != "" && !strings.Contains(said, tc.root):
					t.Errorf("the run said %q; want the command's answer as root joined to it", said)
				}
			})
		}
	}
}

// engineDeniesUser takes the engine's daemon refusing keploy's user from its
// socket's permission denied, wherever the CLI says it (on stderr, an
// *exec.ExitError's, or in its combined output), and nothing else for it.
func TestEngineDeniesUserReadsTheSocketsPermissionDenied(t *testing.T) {
	for _, c := range []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{"docker 29, on stderr", "", &exec.ExitError{Stderr: []byte("permission denied while trying to connect to the docker API at unix:///var/run/docker.sock\n")}, true},
		{"docker 29, combined", "permission denied while trying to connect to the docker API at unix:///var/run/docker.sock\n", errors.New("exit status 1"), true},
		{"the dial's words", "", &exec.ExitError{Stderr: []byte("dial unix /run/podman/podman.sock: connect: permission denied\n")}, true},
		{"no such object", "", &exec.ExitError{Stderr: []byte("Error: No such object: keploy-v3-x\n")}, false},
		{"a conflict", "Error response from daemon: Conflict. The container name \"/keploy-v3\" is already in use\n", errors.New("exit status 1"), false},
		{"daemon down", "", &exec.ExitError{Stderr: []byte("Cannot connect to the Docker daemon at unix:///var/run/docker.sock. Is the docker daemon running?\n")}, false},
		{"a file in the container not readable", "", &exec.ExitError{Stderr: []byte("Error response from daemon: open /tmp/x: permission denied\n")}, false},
	} {
		if got := engineDeniesUser([]byte(c.out), c.err); got != c.want {
			t.Errorf("%s: engineDeniesUser = %v, want %v", c.name, got, c.want)
		}
	}
}
