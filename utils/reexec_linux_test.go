//go:build linux

package utils

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"unicode/utf8"

	keployLog "go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

func TestIsCloudReplayCmd(t *testing.T) {
	tests := []struct {
		name string
		args []string
		want bool
	}{
		{
			name: "cloud replay",
			args: []string{"keploy", "cloud", "replay"},
			want: true,
		},
		{
			name: "cloud replay with flags",
			args: []string{"keploy", "cloud", "replay", "--verbose"},
			want: true,
		},
		{
			name: "persistent flag before subcommands",
			args: []string{"keploy", "--debug", "cloud", "replay"},
			want: true,
		},
		{
			name: "persistent flag between cloud and replay",
			args: []string{"keploy", "cloud", "--debug", "replay"},
			want: true,
		},
		{
			name: "flag with value between cloud and replay",
			args: []string{"keploy", "cloud", "--config", "/tmp/cfg", "replay"},
			want: true,
		},
		{
			name: "flag with inline value between cloud and replay",
			args: []string{"keploy", "cloud", "--config=/tmp/cfg", "replay"},
			want: true,
		},
		{
			name: "cloud without replay",
			args: []string{"keploy", "cloud", "record"},
			want: false,
		},
		{
			name: "only cloud",
			args: []string{"keploy", "cloud"},
			want: false,
		},
		{
			name: "replay without cloud",
			args: []string{"keploy", "replay"},
			want: false,
		},
		{
			name: "no subcommand",
			args: []string{"keploy"},
			want: false,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isCloudReplayCmd(tt.args); got != tt.want {
				t.Errorf("isCloudReplayCmd(%v) = %v, want %v", tt.args, got, tt.want)
			}
		})
	}
}

// ShouldReexecWithSudo decides whether keploy needs root, from raw argv,
// before cobra or any config file is involved. An explicit --cmd-type has to
// win here or `--cmd-type docker-run -c "make up"` starts unprivileged and
// dies much later writing /proc/sys/kernel/perf_event_paranoid (#4399).
//
// Pins the wiring, not just the argv helper: dropping the ExtractCmdTypeFromArgs
// call would leave that helper's own test green while the feature stayed broken.
func TestShouldReexecWithSudo_HonoursExplicitCmdType(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("runs as root; ShouldReexecWithSudo short-circuits before the cmd-type check")
	}

	cases := []struct {
		name string
		args []string
		want bool
	}{
		// The feature: a command that does not look like docker, declared as docker.
		{"explicit docker-compose on a wrapper", []string{"keploy", "record", "--cmd-type", "docker-compose", "-c", "make up"}, true},
		{"explicit docker-run on a script", []string{"keploy", "record", "--cmd-type=docker-run", "-c", "./start.sh"}, true},
		// The inverse: a docker-looking command declared native still needs no root.
		{"explicit native on a docker command", []string{"keploy", "record", "--cmd-type", "native", "-c", "docker compose up"}, false},
		// Unchanged behaviour when the flag is absent.
		{"no flag, docker command", []string{"keploy", "record", "-c", "docker compose up"}, true},
		{"no flag, plain command", []string{"keploy", "record", "-c", "python app.py"}, false},
		// An unrecognised value falls through to sniffing; ValidateFlags reports it properly later.
		{"garbage falls back to sniffing", []string{"keploy", "record", "--cmd-type", "nope", "-c", "docker compose up"}, true},
		// The camelCase alias cobra also accepts must not skip this gate.
		{"camelCase alias on a wrapper", []string{"keploy", "record", "--cmdType", "docker-compose", "-c", "make up"}, true},
		{"camelCase alias native on a docker command", []string{"keploy", "record", "--cmdType=native", "-c", "docker compose up"}, false},
	}

	orig := os.Args
	defer func() { os.Args = orig }()

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			os.Args = tc.args
			if got := ShouldReexecWithSudo(); got != tc.want {
				t.Errorf("ShouldReexecWithSudo() = %v, want %v for %v", got, tc.want, tc.args)
			}
		})
	}
}

// A Podman command in a build that cannot drive Podman is refused by
// ValidateFlags. Re-executing under sudo first would ask for a password only
// to refuse the command after it, so it must not happen, whether the kind was
// sniffed or given with --cmd-type. (A build that registers Podman re-executes
// as it does for Docker; that is tested where Podman is registered.)
func TestShouldReexecWithSudo_NotForAnEngineThisBuildCannotDrive(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("runs as root; ShouldReexecWithSudo short-circuits before the engine check")
	}
	orig := os.Args
	defer func() { os.Args = orig }()

	for _, args := range [][]string{
		{"keploy", "record", "-c", "podman run --name app img"},
		{"keploy", "test", "-c", "sudo podman start -a app"},
		{"keploy", "record", "-c", "podman compose up"},
		{"keploy", "record", "--cmd-type", "docker-run", "-c", "podman run --name app img"},
		{"keploy", "record", "--cmd-type", "docker-run", "-c", "sudo -E podman run --name app img"},
	} {
		os.Args = args
		if ShouldReexecWithSudo() {
			t.Errorf("ShouldReexecWithSudo() = true for %v, want false: Podman is not supported in this build", args)
		}
	}

	// Docker still re-executes.
	os.Args = []string{"keploy", "record", "-c", "docker run --name app img"}
	if !ShouldReexecWithSudo() {
		t.Error("ShouldReexecWithSudo() = false for a docker run, want true")
	}
}

// docker-start is refused before anything starts (it cannot work), so it must
// not ask for a sudo password first, detected or given.
func TestShouldReexecWithSudo_NotForDockerStart(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("runs as root; ShouldReexecWithSudo short-circuits")
	}
	orig := os.Args
	defer func() { os.Args = orig }()
	for _, args := range [][]string{
		{"keploy", "record", "-c", "docker start -a app"},
		{"keploy", "record", "--cmd-type", "docker-start", "-c", "docker start -a app"},
	} {
		os.Args = args
		if ShouldReexecWithSudo() {
			t.Errorf("ShouldReexecWithSudo() = true for %v: a refused command would ask for sudo first", args)
		}
	}
	os.Args = []string{"keploy", "record", "-c", "docker run --name app img"}
	if !ShouldReexecWithSudo() {
		t.Error("ShouldReexecWithSudo() = false for docker run")
	}
}

// The command ShouldReexecWithSudo judges is the one cobra runs: the last -c,
// and a value joined to the shorthand. Judging the first -c ran a docker
// command unprivileged, which failed writing
// /proc/sys/kernel/perf_event_paranoid, and asked for sudo for a native one.
//
// Pins the wiring, not just the argv helper.
func TestShouldReexecWithSudo_JudgesTheCommandCobraRuns(t *testing.T) {
	if os.Geteuid() == 0 {
		t.Skip("runs as root; ShouldReexecWithSudo short-circuits")
	}
	cases := []struct {
		name string
		args []string
		want bool
	}{
		{"a docker command after a native one", []string{"keploy", "record", "-c", "python app.py", "-c", "docker compose up"}, true},
		{"a native command after a docker one", []string{"keploy", "record", "-c", "docker compose up", "-c", "python app.py"}, false},
		{"a docker command joined to the shorthand", []string{"keploy", "record", "-cdocker compose up"}, true},
	}
	orig := os.Args
	defer func() { os.Args = orig }()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			os.Args = tc.args
			if got := ShouldReexecWithSudo(); got != tc.want {
				t.Errorf("ShouldReexecWithSudo() = %v, want %v for %q", got, tc.want, tc.args)
			}
		})
	}
}

// The run goes on in the process the sudo exec starts, which keeps a log of
// its own. This process's log, a temp file, is gone by the exec: nothing would
// be left to remove it, one log left in the temp directory per Docker or cloud
// replay run that re-executes itself with sudo.
func TestReexecWithSudo_RemovesThisProcesssLogBeforeTheExec(t *testing.T) {
	bin := t.TempDir()
	sudo := filepath.Join(bin, "sudo")
	if err := os.WriteFile(sudo, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("TMPDIR", t.TempDir())
	_, f, err := keployLog.New()
	if err != nil || f == nil {
		t.Fatalf("log.New = %v, %v; want a log file", f, err)
	}
	t.Cleanup(func() {
		_ = keployLog.CloseLogFile(f)
		keployLog.TestOnlyResetSink()
	})
	prevLog := LogFile
	LogFile = f
	t.Cleanup(func() { LogFile = prevLog })

	execed, leftBehind := "", true
	prev := execProcess
	execProcess = func(path string, _ []string, _ []string) error {
		execed = path
		_, err := os.Lstat(f.Name())
		leftBehind = !os.IsNotExist(err)
		return nil
	}
	t.Cleanup(func() { execProcess = prev })

	if err := ReexecWithSudo(zap.NewNop()); err != nil {
		t.Fatalf("ReexecWithSudo = %v, want nil where its exec succeeds", err)
	}
	if execed != sudo {
		t.Fatalf("ReexecWithSudo exec'd %q, want %s", execed, sudo)
	}
	if leftBehind {
		t.Errorf("this process's log %s was still there at the exec", f.Name())
	}
}

// A re-exec that cannot happen ends the run through its caller, with an
// error, not inside ReexecWithSudo: an os.Exit there skipped every deferred
// call of the run, the removal of its temp log among them (here it would end
// this test binary). Where the exec itself fails, the log's name is gone
// already, and closing the log after it is no error.
func TestReexecWithSudo_AFailureReturnsToItsCaller(t *testing.T) {
	t.Run("no sudo on PATH", func(t *testing.T) {
		t.Setenv("PATH", t.TempDir())
		execed := false
		prev := execProcess
		execProcess = func(string, []string, []string) error { execed = true; return nil }
		t.Cleanup(func() { execProcess = prev })
		if err := ReexecWithSudo(zap.NewNop()); err == nil {
			t.Error("ReexecWithSudo = nil with no sudo on PATH, want an error")
		}
		if execed {
			t.Error("ReexecWithSudo exec'd with no sudo on PATH")
		}
	})
	t.Run("the exec fails", func(t *testing.T) {
		bin := t.TempDir()
		sudo := filepath.Join(bin, "sudo")
		if err := os.WriteFile(sudo, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
			t.Fatal(err)
		}
		t.Setenv("PATH", bin)
		t.Setenv("TMPDIR", t.TempDir())
		_, f, err := keployLog.New()
		if err != nil || f == nil {
			t.Fatalf("log.New = %v, %v; want a log file", f, err)
		}
		t.Cleanup(keployLog.TestOnlyResetSink)
		prevLog := LogFile
		LogFile = f
		t.Cleanup(func() { LogFile = prevLog })
		prev := execProcess
		execProcess = func(string, []string, []string) error { return syscall.EACCES }
		t.Cleanup(func() { execProcess = prev })

		if err := ReexecWithSudo(zap.NewNop()); !errors.Is(err, syscall.EACCES) {
			t.Errorf("ReexecWithSudo = %v, want the exec's own error", err)
		}
		if _, err := os.Lstat(f.Name()); !os.IsNotExist(err) {
			t.Errorf("this process's log %s is still there after a failed exec (lstat: %v)", f.Name(), err)
		}
		if err := keployLog.CloseLogFile(f); err != nil {
			t.Errorf("CloseLogFile after a failed exec = %v, want nil", err)
		}
	})
}

// sudo-rs (Ubuntu 26.04's sudo) ignores -E, so a re-exec through `sudo -E`
// ran keploy, and the app it runs, without the user's environment. The
// command names every variable for sudo to keep instead, which sudo and
// sudo-rs both honour, and leaves out what sudo sets itself, as -E did.
func TestSudoReexecArgs(t *testing.T) {
	t.Run("names every variable but the ones sudo sets", func(t *testing.T) {
		environ := []string{
			"FOO=foo-val", "HOME=/home/u1", "PATH=/home/u1/bin:/usr/bin",
			"USER=u1", "LOGNAME=u1", "USERNAME=u1", "SUDO_USER=stale", "SUDO_UID=4242",
			"STAMP_CREATED_AT=2026-11-04T00:00:00Z", "KEPLOY_API_KEY=s3cr3t-value",
			"A,B=comma", "FOO=second", "=no-name", "MAIL=/var/mail/u1", "EMPTY=",
			"N\xffU=not-utf-8", "OK=\xff",
		}
		argv, unkept := sudoReexecArgs(environ, "/home/u1/bin:/usr/bin", "/opt/keploy", []string{"record", "-c", "docker compose up"})
		want := []string{
			"sudo",
			"--preserve-env=FOO,HOME,STAMP_CREATED_AT,KEPLOY_API_KEY,MAIL,EMPTY,OK",
			"env", "PATH=/home/u1/bin:/usr/bin", "/opt/keploy",
			"record", "-c", "docker compose up",
		}
		if strings.Join(argv, "\x00") != strings.Join(want, "\x00") {
			t.Errorf("sudoReexecArgs =\n  %q\nwant\n  %q", argv, want)
		}
		if strings.Join(unkept, "\x00") != "A,B\x00N\xffU" {
			t.Errorf("unkept = %q, want the name with a comma and the name that is not UTF-8, [A,B N\xffU] (a value that is not UTF-8 is kept: only the name goes in the list)", unkept)
		}
		for _, a := range argv {
			if a == "-E" {
				t.Error("the command still asks for -E, which sudo-rs ignores")
			}
			if strings.Contains(a, "s3cr3t-value") || strings.Contains(a, "foo-val") {
				t.Errorf("a value is on the command line, which any user can read: %q", a)
			}
		}
	})
	t.Run("no list when nothing is left to name", func(t *testing.T) {
		argv, unkept := sudoReexecArgs([]string{"PATH=/usr/bin", "USER=u1", "SUDO_GID=7"}, "/usr/bin", "/opt/keploy", nil)
		want := []string{"sudo", "env", "PATH=/usr/bin", "/opt/keploy"}
		if strings.Join(argv, "\x00") != strings.Join(want, "\x00") || len(unkept) != 0 {
			t.Errorf("sudoReexecArgs = %q, %q; want %q, none unkept", argv, unkept, want)
		}
	})
}

// A sudo that would not keep the list is refused before anything runs:
// sudo-rs before 0.2.7 ignores it, with a warning only, and sudo before 1.8.21
// rejects it as a usage error. Any other sudo gets the command, with the
// values in the environment it execs with.
func TestReexecWithSudo_TheEnvironmentReachesSudoOrTheRunIsRefused(t *testing.T) {
	bin := t.TempDir()
	sudo := filepath.Join(bin, "sudo")
	if err := os.WriteFile(sudo, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	t.Setenv("STAMP_CREATED_AT", "2026-11-04T00:00:00Z")
	prevLog := LogFile
	LogFile = nil
	t.Cleanup(func() { LogFile = prevLog })
	for _, tc := range []struct {
		version string
		refused string // what the refusal names; empty where sudo runs
	}{
		{version: "sudo-rs 0.2.5\n", refused: "sudo-rs 0.2.5 ignores that list"},
		{version: "sudo-rs 0.2.6\n", refused: "sudo-rs 0.2.6"},
		{version: "sudo-rs 0.2.6-1\n", refused: "implements it from 0.2.7 on), so the run would go on without your environment. Upgrade sudo-rs to 0.2.7 or later"},
		{version: "Sudo version 1.8.16\n", refused: "sudo 1.8.16 does not accept that list"},
		{version: "Sudo version 1.8.20p2\n", refused: "(sudo accepts it from 1.8.21 on). Upgrade sudo to 1.8.21 or later"},
		{version: "sudo-rs 0.2.7\n"},
		{version: "sudo-rs 0.2.13-0ubuntu1.2\n"},
		{version: "Sudo version 1.8.21p2\n"},
		{version: "Sudo version 1.9.15p5\nSudoers policy plugin version 1.9.15p5\n"},
		{version: ""},
	} {
		t.Run(strings.TrimSpace(tc.version), func(t *testing.T) {
			prevVersion := sudoVersionOf
			sudoVersionOf = func(context.Context) string { return tc.version }
			t.Cleanup(func() { sudoVersionOf = prevVersion })
			var execed string
			var argv, envv []string
			prev := execProcess
			execProcess = func(path string, a, e []string) error { execed, argv, envv = path, a, e; return nil }
			t.Cleanup(func() { execProcess = prev })

			err := ReexecWithSudo(zap.NewNop())
			if tc.refused != "" {
				if err == nil || !strings.Contains(err.Error(), tc.refused) {
					t.Fatalf("ReexecWithSudo = %v, want a refusal naming %q", err, tc.refused)
				}
				if execed != "" {
					t.Errorf("ReexecWithSudo exec'd %s %q through a sudo it refused", execed, argv)
				}
				return
			}
			if err != nil {
				t.Fatalf("ReexecWithSudo = %v, want the exec", err)
			}
			if execed != sudo || len(argv) < 2 || !strings.HasPrefix(argv[1], "--preserve-env=") ||
				!slices.Contains(strings.Split(strings.TrimPrefix(argv[1], "--preserve-env="), ","), "STAMP_CREATED_AT") {
				t.Errorf("ReexecWithSudo exec'd %s %q, want %s --preserve-env=…,STAMP_CREATED_AT,… env PATH=…", execed, argv, sudo)
			}
			if !slices.Contains(envv, "STAMP_CREATED_AT=2026-11-04T00:00:00Z") {
				t.Error("the environment sudo is exec'd with has no STAMP_CREATED_AT: the list names a variable sudo does not get")
			}
		})
	}
}

// A name with a comma cannot be in the --preserve-env list: the run goes on
// without that variable, and says which it is. A run with no such name says
// nothing.
func TestReexecWithSudo_SaysWhichVariablesTheListCannotCarry(t *testing.T) {
	bin := t.TempDir()
	if err := os.WriteFile(filepath.Join(bin, "sudo"), []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", bin)
	prevLog := LogFile
	LogFile = nil
	t.Cleanup(func() { LogFile = prevLog })
	prevVersion := sudoVersionOf
	sudoVersionOf = func(context.Context) string { return "sudo-rs 0.2.13\n" }
	t.Cleanup(func() { sudoVersionOf = prevVersion })
	prev := execProcess
	execProcess = func(string, []string, []string) error { return nil }
	t.Cleanup(func() { execProcess = prev })
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.Contains(name, ",") || !utf8.ValidString(name) {
			t.Skipf("this test's own environment has %q, a name the list cannot carry", name)
		}
	}
	warnings := func() []observer.LoggedEntry {
		core, logs := observer.New(zap.WarnLevel)
		if err := ReexecWithSudo(zap.New(core)); err != nil {
			t.Fatalf("ReexecWithSudo = %v, want the exec", err)
		}
		return logs.All()
	}

	if w := warnings(); len(w) != 0 {
		t.Errorf("ReexecWithSudo warned %q with no name the list cannot carry", w[0].Message)
	}
	t.Setenv("A,B", "comma")
	w := warnings()
	if len(w) != 1 || !strings.Contains(w[0].Message, "comma") || !slices.Contains(w[0].ContextMap()["variables"].([]interface{}), any("A,B")) {
		t.Errorf("ReexecWithSudo warned %v, want one warning that names A,B", w)
	}
	// sudo-rs refuses a whole command line that is not UTF-8.
	t.Setenv("N\xffU", "not-utf-8")
	w = warnings()
	if len(w) != 1 || !strings.Contains(w[0].Message, "not UTF-8") || len(w[0].ContextMap()["variables"].([]interface{})) != 2 {
		t.Errorf("ReexecWithSudo warned %v, want one warning that names A,B and the name that is not UTF-8", w)
	}
}

func TestSudoPreserveEnvListGap(t *testing.T) {
	for _, tc := range []struct {
		version string
		want    string // the gap's String; empty for none
		ignored bool
	}{
		{version: "sudo-rs 0.2.6\n", want: "sudo-rs 0.2.6", ignored: true},
		{version: "sudo-rs 0.1.9\n", want: "sudo-rs 0.1.9", ignored: true},
		{version: "sudo-rs 0.2.7\n"},
		{version: "sudo-rs 1.0.0\n"},
		{version: "Sudo version 1.8.20p2\n", want: "sudo 1.8.20"},
		{version: "Sudo version 1.7.30\n", want: "sudo 1.7.30"},
		{version: "Sudo version 1.8.21\n"},
		{version: "Sudo version 2.0.0\n"},
		{version: "sudo: unable to resolve host box\nSudo version 1.8.20\n", want: "sudo 1.8.20"},
		{version: "sudo: unknown option --version\n"},
		// A version past what an int holds names none: let through.
		{version: "sudo-rs 0.1.99999999999999999999\n"},
	} {
		gap, ok := SudoPreserveEnvListGap(tc.version)
		if got := map[bool]string{true: gap.String()}[ok]; got != tc.want || (ok && gap.Ignored != tc.ignored) {
			t.Errorf("SudoPreserveEnvListGap(%q) = %v %v (ignored %v), want %q (ignored %v)", tc.version, gap, ok, gap.Ignored, tc.want, tc.ignored)
		}
	}
}
