//go:build linux || darwin
// +build linux darwin

package utils

import (
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
)

// buildSignalIgnorer compiles a tiny server that binds `port` and IGNORES the
// graceful signals (SIGINT/SIGTERM), simulating an app that under contention does
// not exit on the graceful signal within the wait window — it only dies on SIGKILL.
func buildSignalIgnorer(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	prog := `package main
import ("net";"os";"os/signal";"syscall";"time")
func main(){
	signal.Ignore(syscall.SIGINT, syscall.SIGTERM) // survive the graceful signal
	ln,err:=net.Listen("tcp",":"+os.Args[1]); if err!=nil{os.Exit(3)}
	_=ln
	time.Sleep(10*time.Minute) // until SIGKILL'd
}`
	if err := os.WriteFile(src, []byte(prog), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "ignorer")
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build ignorer: %v\n%s", err, out)
	}
	return bin
}

func ephemeralPortUtil(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	return p
}

// TestInterruptProcessTreeEscalatesToSIGKILL guards the "address already in use"
// app-lifecycle flake (go-docker-timefreeze): if the app ignores the graceful
// signal (or is too contention-slow to exit within the wait), InterruptProcessTree
// must escalate to SIGKILL so the app is actually dead and its port freed before
// keploy starts the next app. The previous implementation gave up after the
// graceful wait and returned with the app still alive.
func TestInterruptProcessTreeEscalatesToSIGKILL(t *testing.T) {
	bin := buildSignalIgnorer(t)
	port := ephemeralPortUtil(t)

	cmd := exec.Command(bin, port)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start: %v", err)
	}
	go func() { _ = cmd.Wait() }() // reap on exit

	// Wait until the ignorer has bound the port.
	deadline := time.Now().Add(8 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("ignorer never bound the port")
		}
		time.Sleep(20 * time.Millisecond)
	}

	// Send the graceful signal it ignores; InterruptProcessTree must escalate.
	if err := InterruptProcessTree(zap.NewNop(), cmd.Process.Pid, syscall.SIGINT); err != nil {
		t.Fatalf("InterruptProcessTree: %v", err)
	}

	// Contract: once it returns, the process tree is dead and the port is free.
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		t.Fatalf("port %s still held after InterruptProcessTree returned (process survived the graceful signal, never SIGKILL'd): %v", port, err)
	}
	_ = ln.Close()
}
