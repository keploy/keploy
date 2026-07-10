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

// buildPortHolder compiles a tiny server that binds `port` and, on SIGTERM, waits
// briefly before releasing it — simulating a graceful shutdown / contention-slowed
// port release (mirrors a `go run`-style app whose :8080 lingers after SIGTERM).
func buildPortHolder(t *testing.T) string {
	t.Helper()
	dir := t.TempDir()
	src := filepath.Join(dir, "main.go")
	prog := `package main
import ("net";"os";"os/signal";"syscall";"time")
func main(){
	ln,err:=net.Listen("tcp",":"+os.Args[1]); if err!=nil{os.Exit(3)}
	ch:=make(chan os.Signal,1); signal.Notify(ch,syscall.SIGTERM)
	<-ch
	time.Sleep(1500*time.Millisecond) // graceful-ish delay before releasing the port
	_=ln.Close(); os.Exit(0)
}`
	if err := os.WriteFile(src, []byte(prog), 0o644); err != nil {
		t.Fatal(err)
	}
	bin := filepath.Join(dir, "holder")
	if out, err := exec.Command("go", "build", "-o", bin, src).CombinedOutput(); err != nil {
		t.Fatalf("build holder: %v\n%s", err, out)
	}
	return bin
}

func ephemeralPort(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", ":0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = ln.Close() }()
	_, p, _ := net.SplitHostPort(ln.Addr().String())
	return p
}

// TestStopCommandReleasesPortBeforeReturning guards the "address already in use"
// app-lifecycle flake (go-docker-timefreeze): when keploy stops an app and then
// starts the next, StopCommand must not return until the process group has actually
// exited and freed its port. The buggy pgid path SIGTERMs and returns immediately,
// so the next bind races a still-held port.
func TestStopCommandReleasesPortBeforeReturning(t *testing.T) {
	bin := buildPortHolder(t)
	port := ephemeralPort(t)

	cmd := exec.Command(bin, port)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true} // mirror NewAgentCommand
	if err := StartCommand(cmd); err != nil {
		t.Fatalf("start: %v", err)
	}
	// Reap on exit in the background, mirroring keploy's app-watch goroutine
	// (so an exited process doesn't linger as a zombie).
	go func() { _ = cmd.Wait() }()

	// Wait until the holder has bound the port.
	deadline := time.Now().Add(8 * time.Second)
	for {
		c, err := net.DialTimeout("tcp", "127.0.0.1:"+port, 100*time.Millisecond)
		if err == nil {
			_ = c.Close()
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("holder never bound the port")
		}
		time.Sleep(20 * time.Millisecond)
	}

	if err := StopCommand(cmd, zap.NewNop()); err != nil {
		t.Fatalf("StopCommand: %v", err)
	}

	// Contract: once StopCommand returns, the app is stopped and the port is free.
	ln, err := net.Listen("tcp", ":"+port)
	if err != nil {
		t.Fatalf("port %s still held after StopCommand returned (app not fully stopped): %v", port, err)
	}
	_ = ln.Close()
}
