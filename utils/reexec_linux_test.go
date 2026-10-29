//go:build linux

package utils

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	keployLog "go.keploy.io/server/v3/utils/log"
	"go.uber.org/zap"
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
