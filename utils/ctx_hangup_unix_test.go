//go:build unix

package utils

import (
	"bytes"
	"errors"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"
)

// hangupHelperDir, set, makes TestHangupHelper the keploy run the tests below
// start and signal, with its markers in that directory.
const hangupHelperDir = "KEPLOY_TEST_HANGUP_HELPER"

// TestHangupHelper is not a test of its own. With hangupHelperDir set it is a
// keploy run: NewCtx -- then DieOnHangup, as `keploy agent` calls it, if
// KEPLOY_TEST_HANGUP_AGENT is set -- and a command that runs until the root
// context ends, and then a shutdown that takes a second, as stopping the
// test command and the agent does. It leaves in its directory:
//
//	ready      once it waits, whether SIGHUP is ignored
//	hooks-ran  once the pre-cancel hooks have run
//	stopped    once its shutdown is done, whether it was interrupted
func TestHangupHelper(t *testing.T) {
	dir := os.Getenv(hangupHelperDir)
	if dir == "" {
		return
	}
	// Written whole, then renamed into place: the test reads a marker as
	// soon as it exists.
	mark := func(name, content string) {
		tmp := filepath.Join(dir, name+".tmp")
		if os.WriteFile(tmp, []byte(content), 0o600) != nil || os.Rename(tmp, filepath.Join(dir, name)) != nil {
			os.Exit(3)
		}
	}
	ctx := NewCtx()
	if os.Getenv("KEPLOY_TEST_HANGUP_AGENT") != "" {
		DieOnHangup()
	}
	RegisterPreCancelHook(func() { mark("hooks-ran", "") })
	mark("ready", strconv.FormatBool(signal.Ignored(syscall.SIGHUP)))
	<-ctx.Done()
	time.Sleep(time.Second)
	mark("stopped", strconv.FormatBool(Interrupted()))
	os.Exit(0)
}

// hangupRun is a TestHangupHelper process.
type hangupRun struct {
	t      *testing.T
	dir    string
	cmd    *exec.Cmd
	out    bytes.Buffer
	exited chan error
}

// startHangupRun starts a TestHangupHelper process through launcher (nohup,
// or nothing) and waits until it is ready to be signalled.
func startHangupRun(t *testing.T, agent bool, launcher ...string) *hangupRun {
	t.Helper()
	r := &hangupRun{t: t, dir: t.TempDir(), exited: make(chan error, 1)}
	argv := append(append([]string{}, launcher...), os.Args[0], "-test.run=^TestHangupHelper$")
	r.cmd = exec.Command(argv[0], argv[1:]...)
	r.cmd.Env = append(os.Environ(), hangupHelperDir+"="+r.dir)
	if agent {
		r.cmd.Env = append(r.cmd.Env, "KEPLOY_TEST_HANGUP_AGENT=1")
	}
	r.cmd.Stdout, r.cmd.Stderr = &r.out, &r.out
	if err := r.cmd.Start(); err != nil {
		t.Fatal(err)
	}
	go func() { r.exited <- r.cmd.Wait() }()
	deadline := time.After(30 * time.Second)
	for !r.has("ready") {
		select {
		case err := <-r.exited:
			t.Fatalf("the run ended before it was ready: %v\n%s", err, r.out.String())
		case <-deadline:
			t.Fatalf("the run was never ready\n%s", r.kill())
		case <-time.After(10 * time.Millisecond):
		}
	}
	return r
}

func (r *hangupRun) has(name string) bool {
	_, err := os.Stat(filepath.Join(r.dir, name))
	return err == nil
}

func (r *hangupRun) marker(name string) string {
	b, err := os.ReadFile(filepath.Join(r.dir, name))
	if err != nil {
		return "<none: " + err.Error() + ">"
	}
	return string(b)
}

func (r *hangupRun) signal(sig syscall.Signal) {
	if err := r.cmd.Process.Signal(sig); err != nil {
		r.t.Fatalf("%v: %v\n%s", sig, err, r.kill())
	}
}

// kill ends the run and returns what it printed.
func (r *hangupRun) kill() string {
	_ = r.cmd.Process.Kill()
	<-r.exited
	return r.out.String()
}

// wait returns how the run ended, failing the test if it runs on for 30s.
func (r *hangupRun) wait() syscall.WaitStatus {
	var err error
	select {
	case err = <-r.exited:
	case <-time.After(30 * time.Second):
		r.t.Fatalf("the run was still running 30s after it was signalled\n%s", r.kill())
	}
	var exit *exec.ExitError
	if err != nil && !errors.As(err, &exit) {
		r.t.Fatalf("the run: %v\n%s", err, r.out.String())
	}
	return r.cmd.ProcessState.Sys().(syscall.WaitStatus)
}

// catchHangups starts every run this test starts with SIGHUP's default
// action, as a terminal's task gets it, even when `go test` itself runs under
// nohup, whose SIG_IGN a run would otherwise inherit and, rightly, keep: Go
// starts a child with the default action for a signal it catches.
func catchHangups(t *testing.T) {
	caught := make(chan os.Signal, 1)
	signal.Notify(caught, syscall.SIGHUP)
	t.Cleanup(func() { signal.Stop(caught) })
}

// A hang-up stops keploy as SIGTERM does -- the run is interrupted, the
// pre-cancel hooks run, the root context ends and the shutdown finishes -- and
// every SIGHUP after the first changes nothing. The VS Code extension's Stop
// ends a run with a hang-up, and SIGHUP killed keploy where it stood: the test
// command it had started ran on, and none of the cleanup keploy defers ran.
// One hang-up can deliver two SIGHUPs, the second while keploy is shutting
// down, and that one must not kill it either. 0ms apart the two may be merged
// into one on the way; 30ms and 100ms apart they arrive one by one.
func TestAHangUpStopsKeployAsSIGTERMDoes(t *testing.T) {
	catchHangups(t)
	type step struct {
		sig   syscall.Signal
		after time.Duration // since the step before
	}
	for _, tc := range []struct {
		name  string
		steps []step
		// what the handler says of the second signal, if anything
		says string
	}{
		{name: "SIGTERM", steps: []step{{sig: syscall.SIGTERM}}},
		{name: "SIGHUP", steps: []step{{sig: syscall.SIGHUP}}},
		{name: "two SIGHUPs 0ms apart", steps: []step{{sig: syscall.SIGHUP}, {sig: syscall.SIGHUP}}},
		{name: "two SIGHUPs 30ms apart", steps: []step{{sig: syscall.SIGHUP}, {sig: syscall.SIGHUP, after: 30 * time.Millisecond}}},
		{name: "two SIGHUPs 100ms apart", steps: []step{{sig: syscall.SIGHUP}, {sig: syscall.SIGHUP, after: 100 * time.Millisecond}}},
		// A hang-up while an interrupt is already being handled is no second
		// stop, and must not be reported as one.
		{
			name:  "SIGINT, then SIGHUP 100ms later",
			steps: []step{{sig: syscall.SIGINT}, {sig: syscall.SIGHUP, after: 100 * time.Millisecond}},
			says:  "Signal received: hangup, ignored: Keploy is already handling interrupt\n",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			r := startHangupRun(t, false)
			for _, s := range tc.steps {
				time.Sleep(s.after)
				r.signal(s.sig)
			}
			st := r.wait()
			out := r.out.String()
			if st.Signaled() {
				t.Fatalf("the run died of %v where it stood, want it stopped as SIGTERM stops it\n%s", st.Signal(), out)
			}
			if st.ExitStatus() != 0 {
				t.Fatalf("the run exited %d, want 0\n%s", st.ExitStatus(), out)
			}
			if got := r.marker("stopped"); got != "true" {
				t.Errorf("stopped = %q, want the shutdown finished and the run marked interrupted\n%s", got, out)
			}
			if !r.has("hooks-ran") {
				t.Errorf("the pre-cancel hooks never ran\n%s", out)
			}
			first := "Signal received: " + tc.steps[0].sig.String() + ", canceling context...\n"
			if !strings.HasPrefix(out, first) {
				t.Errorf("the run's output does not start %q\n%s", first, out)
			}
			if n := strings.Count(out, "canceling context"); n != 1 {
				t.Errorf("the run said %d times that it was stopping, want once\n%s", n, out)
			}
			if tc.says != "" && !strings.Contains(out, tc.says) {
				t.Errorf("the run did not say %q\n%s", tc.says, out)
			}
		})
	}
}

// A run started with SIGHUP ignored -- `nohup keploy record ...` -- asked to
// outlive its terminal, and does: NewCtx does not start listening for it, so
// the process stays deaf to it and SIGTERM is still what stops it. The same
// for `keploy agent` under nohup, whose DieOnHangup must give back the SIG_IGN
// it started with, not death.
func TestARunStartedUnderNohupIgnoresHangUps(t *testing.T) {
	catchHangups(t)
	nohup, err := exec.LookPath("nohup")
	if err != nil {
		t.Fatalf("nohup: %v", err)
	}
	for _, agent := range []bool{false, true} {
		t.Run("agent="+strconv.FormatBool(agent), func(t *testing.T) {
			t.Parallel()
			r := startHangupRun(t, agent, nohup)
			if got := r.marker("ready"); got != "true" {
				t.Fatalf("SIGHUP ignored = %q once the run started under nohup, want true\n%s", got, r.kill())
			}
			r.signal(syscall.SIGHUP)
			time.Sleep(30 * time.Millisecond)
			r.signal(syscall.SIGHUP)
			time.Sleep(300 * time.Millisecond)
			r.signal(syscall.SIGTERM)
			st := r.wait()
			out := r.out.String()
			if st.Signaled() || st.ExitStatus() != 0 {
				t.Fatalf("the run ended with %v, want exit 0 once SIGTERM stopped it\n%s", r.cmd.ProcessState, out)
			}
			if first := "Signal received: terminated, canceling context...\n"; !strings.HasPrefix(out, first) {
				t.Errorf("the run's output does not start %q: a run under nohup heard a hang-up\n%s", first, out)
			}
		})
	}
}

// `keploy agent` opts out: DieOnHangup gives SIGHUP back its default action,
// so a hang-up kills the agent, as its owners count on (see DieOnHangup).
func TestDieOnHangupLetsAHangUpKill(t *testing.T) {
	catchHangups(t)
	r := startHangupRun(t, true)
	if got := r.marker("ready"); got != "false" {
		t.Fatalf("SIGHUP ignored = %q in a run not started under nohup, want false\n%s", got, r.kill())
	}
	r.signal(syscall.SIGHUP)
	st := r.wait()
	if !st.Signaled() || st.Signal() != syscall.SIGHUP {
		t.Fatalf("the run ended with %v, want it dead of SIGHUP\n%s", r.cmd.ProcessState, r.out.String())
	}
}
