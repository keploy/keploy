//go:build linux || darwin

package utils

import (
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// buildServerLeavingRunner compiles a stand-in app that runs
// `sh -c 'SERVER &'` in a process group of its own and never reaps the sh, as
// a harness that starts its server in the background does. The sh exits at
// once and stays a zombie, leading the group in which its server runs on
// (its parent now init, launchd on macOS). The server, the same binary, takes
// SIGINT and exits a second later ("serve"), or ignores it ("deaf"); with
// "gone" the sh starts nothing. The app writes the sh's pid to DIR/sh.pid,
// and the server its own to DIR/server.pid and, as it exits on its own,
// DIR/server.exited.
func buildServerLeavingRunner(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	prog := `package main
import ("fmt";"os";"os/exec";"os/signal";"path/filepath";"syscall";"time")
func put(dir, name string, pid int) {
	_ = os.WriteFile(filepath.Join(dir, name+".tmp"), []byte(fmt.Sprint(pid)), 0o644)
	_ = os.Rename(filepath.Join(dir, name+".tmp"), filepath.Join(dir, name+".pid"))
}
func main(){
	dir, role := os.Args[1], os.Args[2]
	if len(os.Args) > 3 {
		if role == "deaf" {
			signal.Ignore(syscall.SIGINT)
		} else {
			c := make(chan os.Signal, 1)
			signal.Notify(c, syscall.SIGINT)
			go func() {
				<-c
				time.Sleep(time.Second)
				_ = os.WriteFile(filepath.Join(dir, "server.exited"), nil, 0o644)
				os.Exit(0)
			}()
		}
		put(dir, "server", os.Getpid())
		time.Sleep(10 * time.Minute)
		return
	}
	script := ":"
	if role != "gone" {
		script = fmt.Sprintf("'%s' '%s' %s server &", os.Args[0], dir, role)
	}
	c := exec.Command("/bin/sh", "-c", script)
	c.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := c.Start(); err != nil { os.Exit(3) }
	put(dir, "sh", c.Process.Pid)
	time.Sleep(10 * time.Minute)
}`
	if err := os.WriteFile(src, []byte(prog), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "server-leaving-runner")
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build runner: %v\n%s", err, out)
	}
	return bin
}

func readPidFile(t *testing.T, path string) int {
	t.Helper()
	for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if b, err := os.ReadFile(path); err == nil {
			if pid, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
				return pid
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("%s never appeared", path)
		}
	}
}

// A process group counts as exited only when no process of it runs. Its
// leader exiting is not enough: `sh -c 'server &'` whose sh no one reaps
// leaves a zombie leading the group in which the server runs on, and the
// server holds the app's port. The stop waits for that server as it shuts
// down after its SIGINT, and kills, after the grace, one that ignores
// SIGINT, saying so. A group of zombies only has exited: it costs no grace.
func TestTheStopWaitsForTheServerAZombieLeaderLeft(t *testing.T) {
	bin := buildServerLeavingRunner(t)
	for _, tc := range []struct {
		role         string
		killed       bool
		exitedItself bool
		atLeast      time.Duration
		under        time.Duration
	}{
		{role: "serve", exitedItself: true, atLeast: 900 * time.Millisecond, under: TreeInterruptGrace},
		{role: "deaf", killed: true, atLeast: TreeInterruptGrace, under: 2*TreeInterruptGrace + 5*time.Second},
		{role: "gone", under: TreeInterruptGrace},
	} {
		t.Run(tc.role, func(t *testing.T) {
			dir := t.TempDir()
			cmd := exec.Command(bin, dir, tc.role)
			cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // as keploy starts a native command
			if err := cmd.Start(); err != nil {
				t.Fatalf("start: %v", err)
			}
			go func() { _ = cmd.Wait() }() // reap on exit
			t.Cleanup(func() { _ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) })

			sh := readPidFile(t, filepath.Join(dir, "sh.pid"))
			t.Cleanup(func() { _ = syscall.Kill(-sh, syscall.SIGKILL) })
			server := 0
			if tc.role != "gone" {
				server = readPidFile(t, filepath.Join(dir, "server.pid"))
				t.Cleanup(func() { _ = syscall.Kill(server, syscall.SIGKILL) })
				if pgid, err := syscall.Getpgid(server); err != nil || pgid != sh {
					t.Fatalf("the server %d is in group %d (%v); want the sh %d's", server, pgid, err, sh)
				}
			}
			for deadline := time.Now().Add(10 * time.Second); !isZombie(t, sh); time.Sleep(20 * time.Millisecond) {
				if time.Now().After(deadline) {
					t.Fatalf("the sh %d never became a zombie", sh)
				}
			}

			core, logs := observer.New(zap.DebugLevel)
			began := time.Now()
			killed, err := InterruptProcessTreeKilled(zap.New(core), cmd.Process.Pid, syscall.SIGINT)
			took := time.Since(began)
			if err != nil {
				t.Fatalf("InterruptProcessTree: %v", err)
			}
			for _, e := range logs.FilterLevelExact(zap.ErrorLevel).All() {
				t.Errorf("the stop logged an ERROR: %q %v", e.Message, e.ContextMap())
			}
			if (killed != "") != tc.killed {
				t.Errorf("the stop said it killed %q; want a kill %v", killed, tc.killed)
			}
			if took < tc.atLeast || took >= tc.under {
				t.Errorf("the stop took %s; want at least %s and under %s", took, tc.atLeast, tc.under)
			}
			if server != 0 {
				if running, err := isProcessRunning(server); err != nil || running {
					t.Errorf("the server %d, in the zombie sh %d's group, runs after the stop (%v)", server, sh, err)
				}
			}
			_, err = os.Stat(filepath.Join(dir, "server.exited"))
			if exitedItself := err == nil; exitedItself != tc.exitedItself {
				t.Errorf("the server exited on its own %v; want %v", exitedItself, tc.exitedItself)
			}
			if running, err := groupRunning(sh, nil); err != nil || running {
				t.Errorf("the zombie sh %d's group runs after the stop: %v, %v", sh, running, err)
			}
		})
	}
}
