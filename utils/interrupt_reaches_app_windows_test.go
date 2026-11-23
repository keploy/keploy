//go:build windows

package utils

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"golang.org/x/sys/windows"
)

// The rows below start an app as keploy starts a native one on Windows
// (ExecuteCommand: cmd /C in a process group of its own) and stop it as
// keploy's stop does (InterruptProcessTreeKilled with SIGINT), from a
// stand-in keploy: this test binary in the role "keploy", started with a
// console of its own (CREATE_NO_WINDOW) or with none (DETACHED_PROCESS). The
// app is this test binary in the role "app": a Go program that says when its
// os.Interrupt arrived. A stop that reaches the app ends it on that interrupt,
// with no kill; one that does not reach it has to kill it, and the app's
// graceful exit (its flushes, its own shutdown) never runs.

const (
	stopRowRoleEnv = "KEPLOY_STOP_ROW_ROLE"
	stopRowDirEnv  = "KEPLOY_STOP_ROW_DIR"
)

// stopRowOutcome is what one stop did: what keploy's stop said it had to do
// ("" when it did not kill), its error, and what the app wrote.
type stopRowOutcome struct {
	Killed  string `json:"killed"`
	StopErr string `json:"stop_err"`
	App     string `json:"app"`
}

// standInOutcome is what a stand-in keploy saw: its console before it
// started anything and after its stops, each stop's outcome, and what its
// send to a process that does not exist returned.
type standInOutcome struct {
	ConsoleBefore string           `json:"console_before"`
	ConsoleAfter  string           `json:"console_after"`
	Stops         []stopRowOutcome `json:"stops"`
	SendToNone    string           `json:"send_to_none"`
}

// TestWindowsStopRowHelper is the rows' other processes: it does nothing
// unless KEPLOY_STOP_ROW_ROLE names a role.
func TestWindowsStopRowHelper(t *testing.T) {
	dir := os.Getenv(stopRowDirEnv)
	switch role := os.Getenv(stopRowRoleEnv); role {
	case "":
		return
	case "app":
		os.Exit(stopRowApp(dir))
	case "keploy", "keploy-two":
		out := standIn(dir, role == "keploy-two")
		b, _ := json.Marshal(out)
		if err := os.WriteFile(filepath.Join(dir, "stop.json"), b, 0o600); err != nil {
			os.Exit(4)
		}
		os.Exit(0)
	default:
		t.Fatalf("unknown %s %q", stopRowRoleEnv, role)
	}
}

// stopRowApp says which process group it is in (its parent's, the cmd /C
// keploy started it with), waits for its os.Interrupt and says it got it, or
// that none came.
func stopRowApp(dir string) int {
	c := make(chan os.Signal, 1)
	signal.Notify(c, os.Interrupt)
	if err := os.WriteFile(filepath.Join(dir, "ready"), []byte(strconv.Itoa(os.Getppid())), 0o600); err != nil {
		return 5
	}
	select {
	case <-c:
		_ = os.WriteFile(filepath.Join(dir, "app"), []byte("interrupted"), 0o600)
		return 0
	case <-time.After(3 * time.Minute):
		_ = os.WriteFile(filepath.Join(dir, "app"), []byte("no interrupt in 3m"), 0o600)
		return 3
	}
}

// consoleState says whether this process is on a console.
func consoleState() string {
	f, err := os.OpenFile("CONOUT$", os.O_WRONLY, 0)
	if err != nil {
		return "none (" + err.Error() + ")"
	}
	_ = f.Close()
	return "attached"
}

// stopRowExe is this test binary's path as cmd /C takes it unquoted (a path
// with a space is read in its short form).
func stopRowExe() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", err
	}
	if !strings.ContainsAny(exe, " \t") {
		return exe, nil
	}
	p, err := windows.UTF16PtrFromString(exe)
	if err != nil {
		return "", err
	}
	buf := make([]uint16, windows.MAX_LONG_PATH)
	n, err := windows.GetShortPathName(p, &buf[0], uint32(len(buf)))
	if err != nil || n == 0 {
		return "", fmt.Errorf("the test binary's path %q has a space and no short form: %v", exe, err)
	}
	short := windows.UTF16ToString(buf[:n])
	if strings.ContainsAny(short, " \t") {
		return "", fmt.Errorf("the test binary's path %q has a space in its short form too", exe)
	}
	return short, nil
}

// rowApp is one app a row started as keploy starts a native app.
type rowApp struct {
	dir    string
	group  int
	cancel context.CancelFunc
	said   chan stopRowOutcome
	ended  chan CmdError
}

// startTheRowApp starts the app in dir as keploy starts a native app and
// waits until it listens for its interrupt. Its stop is keploy's.
func startTheRowApp(dir string) (*rowApp, error) {
	exe, err := stopRowExe()
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(context.Background())
	a := &rowApp{dir: dir, cancel: cancel, said: make(chan stopRowOutcome, 1), ended: make(chan CmdError, 1)}
	stop := func(cmd *exec.Cmd) func() error {
		return func() error {
			killed, err := InterruptProcessTreeKilled(zap.NewNop(), cmd.Process.Pid, syscall.SIGINT)
			o := stopRowOutcome{Killed: killed}
			if err != nil {
				o.StopErr = err.Error()
			}
			a.said <- o
			return err
		}
	}
	go func() {
		a.ended <- ExecuteCommand(ctx, zap.NewNop(), exe+" -test.run=TestWindowsStopRowHelper$", Native, stop, 30*time.Second, nil,
			[]string{stopRowRoleEnv + "=app", stopRowDirEnv + "=" + dir})
	}()
	ready := time.Now().Add(time.Minute)
	for {
		if b, err := os.ReadFile(filepath.Join(dir, "ready")); err == nil && len(b) > 0 {
			if a.group, err = strconv.Atoi(string(b)); err != nil {
				cancel()
				return nil, fmt.Errorf("the app said its group is %q", b)
			}
			return a, nil
		}
		select {
		case res := <-a.ended:
			cancel()
			return nil, fmt.Errorf("the app ended before it was stopped: %v", res.Err)
		case <-time.After(100 * time.Millisecond):
		}
		if time.Now().After(ready) {
			// The stop (keploy's) ends what did start.
			cancel()
			return nil, errors.New("the app was not listening for its interrupt within a minute")
		}
	}
}

// wrote reads what the app wrote: "interrupted" once its interrupt came.
func (a *rowApp) wrote() string {
	b, err := os.ReadFile(filepath.Join(a.dir, "app"))
	if errors.Is(err, os.ErrNotExist) {
		return "(nothing: no interrupt has reached it)"
	}
	if err != nil {
		return err.Error()
	}
	return string(b)
}

// stop stops the app as keploy's stop does and says what it did.
func (a *rowApp) stop() stopRowOutcome {
	a.cancel()
	var out stopRowOutcome
	select {
	case out = <-a.said:
	case <-time.After(2 * time.Minute):
		out.StopErr = "keploy's stop had not returned 2m after it began"
		return out
	}
	select {
	case <-a.ended:
	case <-time.After(time.Minute):
		out.StopErr += "; the app's command had not ended a minute after the stop"
	}
	out.App = a.wrote()
	return out
}

// stopOne starts one app in dir and stops it.
func stopOne(dir string) stopRowOutcome {
	a, err := startTheRowApp(dir)
	if err != nil {
		return stopRowOutcome{StopErr: err.Error()}
	}
	return a.stop()
}

// stopTwoAtOnce starts two apps and sends each its CTRL_BREAK as keploy's
// stop does (SendSignal), the second sent while the first send is on the
// first app's console. What each app wrote is read before the apps are
// stopped (keploy's stop), so only those two sends can have reached them.
func stopTwoAtOnce(dir string) []stopRowOutcome {
	var apps []*rowApp
	defer func() {
		for _, a := range apps {
			a.cancel()
			select {
			case <-a.ended:
			case <-time.After(2 * time.Minute):
			}
		}
	}()
	for _, n := range []string{"a", "b"} {
		d := filepath.Join(dir, n)
		if err := os.MkdirAll(d, 0o700); err != nil {
			return []stopRowOutcome{{StopErr: err.Error()}}
		}
		a, err := startTheRowApp(d)
		if err != nil {
			return []stopRowOutcome{{StopErr: err.Error()}}
		}
		apps = append(apps, a)
	}
	var errB error
	sentB := make(chan struct{})
	var once sync.Once
	whileOnTheAppsConsole = func() {
		once.Do(func() {
			go func() {
				errB = SendSignal(zap.NewNop(), -apps[1].group, syscall.SIGINT)
				close(sentB)
			}()
			// The second send starts while this one is on the first app's
			// console.
			time.Sleep(time.Second)
		})
	}
	errA := SendSignal(zap.NewNop(), -apps[0].group, syscall.SIGINT)
	var secondErr error
	select {
	case <-sentB:
		secondErr = errB
	case <-time.After(time.Minute):
		secondErr = errors.New("the second send had not returned in a minute, or never began (the first went through no app's console)")
	}
	whileOnTheAppsConsole = nil
	outs := make([]stopRowOutcome, 2)
	for i, err := range []error{errA, secondErr} {
		if err != nil {
			outs[i].StopErr = err.Error()
		}
	}
	for i, a := range apps {
		deadline := time.Now().Add(30 * time.Second)
		for a.wrote() != "interrupted" && time.Now().Before(deadline) {
			time.Sleep(100 * time.Millisecond)
		}
		outs[i].App = a.wrote()
	}
	return outs
}

// standIn is a stand-in keploy: it stops one app, or two at once, and sends
// a CTRL_BREAK to a process that does not exist.
func standIn(dir string, two bool) standInOutcome {
	out := standInOutcome{ConsoleBefore: consoleState()}
	if two {
		out.Stops = stopTwoAtOnce(dir)
	} else {
		out.Stops = []stopRowOutcome{stopOne(dir)}
	}
	out.ConsoleAfter = consoleState()
	// A pid no process has: Windows' pids are multiples of 4.
	if err := SendSignal(zap.NewNop(), -0x7ffffffd, syscall.SIGINT); err != nil {
		out.SendToNone = err.Error()
	}
	return out
}

func checkTheStopReachedTheApp(t *testing.T, out stopRowOutcome) {
	t.Helper()
	if out.StopErr != "" || out.Killed != "" || out.App != "interrupted" {
		t.Fatalf("keploy's stop did not end the app on its interrupt: the stop said killed=%q err=%q; the app said %q",
			out.Killed, out.StopErr, out.App)
	}
}

// runStandIn runs the stand-in keploy in role, started with flags, and reads
// what it saw. Its console must be what console says, before and after.
func runStandIn(t *testing.T, role string, flags uint32, console string) standInOutcome {
	t.Helper()
	dir := t.TempDir()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=TestWindowsStopRowHelper$")
	cmd.Env = append(os.Environ(), stopRowRoleEnv+"="+role, stopRowDirEnv+"="+dir)
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: flags | windows.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	var waitErr error
	ended := make(chan struct{})
	go func() { waitErr = cmd.Wait(); close(ended) }()
	t.Cleanup(func() {
		select {
		case <-ended:
			return
		default:
		}
		_ = exec.Command("taskkill", "/PID", strconv.Itoa(cmd.Process.Pid), "/T", "/F").Run()
		select {
		case <-ended:
		case <-time.After(time.Minute):
		}
	})
	select {
	case <-ended:
		if waitErr != nil {
			t.Fatalf("the stand-in keploy ended with %v", waitErr)
		}
	case <-time.After(6 * time.Minute):
		t.Fatal("the stand-in keploy had not ended in 6m")
	}
	b, err := os.ReadFile(filepath.Join(dir, "stop.json"))
	if err != nil {
		t.Fatal(err)
	}
	var out standInOutcome
	if err := json.Unmarshal(b, &out); err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(out.ConsoleBefore, console) {
		t.Fatalf("the stand-in keploy was to start on console %q; it had %s", console, out.ConsoleBefore)
	}
	if !strings.HasPrefix(out.ConsoleAfter, console) {
		t.Fatalf("keploy's stop changed keploy's console: it had %q, and %s after the stop", console, out.ConsoleAfter)
	}
	return out
}

// A keploy on a console (a terminal, a CI step, a scheduled task, a program
// that started it with a hidden one): the app shares it, and keploy's stop
// reaches the app's os.Interrupt; the app ends on it, with no kill.
func TestOnWindowsKeploysStopReachesTheAppOnItsConsole(t *testing.T) {
	out := runStandIn(t, "keploy", windows.CREATE_NO_WINDOW, "attached")
	if len(out.Stops) != 1 {
		t.Fatalf("the stand-in made %d stops; want 1", len(out.Stops))
	}
	checkTheStopReachedTheApp(t, out.Stops[0])
}

// A keploy with no console (started detached, as a service, or by a program
// that has none): the app it starts gets a console keploy is not on. keploy's
// stop still reaches the app's os.Interrupt, with no kill, and leaves keploy
// on no console. A send to a process that does not exist says why it failed
// from the app's console too.
func TestOnWindowsKeploysStopReachesTheAppWhenKeployHasNoConsole(t *testing.T) {
	out := runStandIn(t, "keploy", windows.DETACHED_PROCESS, "none")
	if len(out.Stops) != 1 {
		t.Fatalf("the stand-in made %d stops; want 1", len(out.Stops))
	}
	checkTheStopReachedTheApp(t, out.Stops[0])
	if !strings.Contains(out.SendToNone, "AttachConsole") {
		t.Fatalf("a CTRL_BREAK to no process returned %q; want its failure from the app's console (AttachConsole) said", out.SendToNone)
	}
}

// Two sends at once from a keploy with no console: the second waits while
// the first is on its app's console, so each CTRL_BREAK is sent from its own
// app's console and both apps end on their interrupts.
func TestOnWindowsKeploysStopReachesTwoAppsSentAtOnce(t *testing.T) {
	out := runStandIn(t, "keploy-two", windows.DETACHED_PROCESS, "none")
	if len(out.Stops) != 2 {
		t.Fatalf("the stand-in made %d stops (%+v); want 2", len(out.Stops), out.Stops)
	}
	for _, s := range out.Stops {
		checkTheStopReachedTheApp(t, s)
	}
}
