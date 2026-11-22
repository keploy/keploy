//go:build windows

package utils

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

type winSyncBuffer struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *winSyncBuffer) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.Write(p)
}

func (s *winSyncBuffer) String() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.b.String()
}

// TestChildConsoleWindowsGrandchildHelper is the subprocess helper driven by
// TestChildConsoleWindowsDrainsWithoutWaitingForAGrandchildHoldingThePipe.
func TestChildConsoleWindowsGrandchildHelper(t *testing.T) {
	role := os.Getenv("KEPLOY_WIN_PIPE_HELPER_ROLE")
	addr := os.Getenv("KEPLOY_WIN_PIPE_HELPER_ADDR")
	if role == "" || addr == "" {
		return
	}
	switch role {
	case "child":
		grand := exec.Command(os.Args[0], "-test.run=^TestChildConsoleWindowsGrandchildHelper$")
		grand.Env = append(os.Environ(), "KEPLOY_WIN_PIPE_HELPER_ROLE=grandchild")
		grand.Stdout = os.Stdout
		grand.Stderr = os.Stderr
		if err := grand.Start(); err != nil {
			_, _ = fmt.Fprintf(os.Stderr, "start grandchild: %v\n", err)
			os.Exit(2)
		}
		_, _ = fmt.Fprintln(os.Stdout, "Bind for 127.0.0.1:61234 failed: port is already allocated")
		os.Exit(0)
	case "grandchild":
		c, err := net.Dial("tcp", addr)
		if err != nil {
			os.Exit(3)
		}
		// Hold the inherited stdout/stderr pipe open until the test closes c.
		var buf [1]byte
		_, _ = c.Read(buf[:])
		_ = c.Close()
		os.Exit(0)
	}
}

// TestChildConsoleWindowsDrainsWithoutWaitingForAGrandchildHoldingThePipe pins
// that Windows ChildConsole uses an overlapped named pipe whose read end takes
// deadlines, so Drain flushes everything the exited child printed to its tee
// without waiting for a grandchild that inherited the pipe to exit.
func TestChildConsoleWindowsDrainsWithoutWaitingForAGrandchildHoldingThePipe(t *testing.T) {
	// Direct pipe drain check: Drain flushes buffered output (including an
	// unended line) while the write end is still open.
	for _, keep := range []bool{true, false} {
		t.Run("pipe/keep="+strconv.FormatBool(keep), func(t *testing.T) {
			r, w, err := childConsolePipe()
			if err != nil {
				t.Fatalf("childConsolePipe: %v", err)
			}
			defer func() { _ = r.Close() }()
			defer func() { _ = w.Close() }()

			var dst winSyncBuffer
			cp := StartChildOutputCopy(&dst, r, keep)
			if _, err := w.WriteString("first line\nunended tail"); err != nil {
				t.Fatalf("write: %v", err)
			}
			cp.Drain()
			if !cp.canDrain {
				t.Fatal("overlapped named pipe on Windows did not support SetReadDeadline")
			}
			if got, want := dst.String(), "first line\nunended tail"; got != want {
				t.Fatalf("after Drain got %q, want %q", got, want)
			}
			if !keep {
				<-cp.Done()
				return
			}
			select {
			case <-cp.Done():
				t.Fatal("keep=true copy stopped at Drain while writer remained open")
			default:
			}
			if _, err := w.WriteString("\nafter drain\n"); err != nil {
				t.Fatalf("write after drain: %v", err)
			}
			_ = w.Close()
			<-cp.Done()
			if got, want := dst.String(), "first line\nunended tail\nafter drain\n"; got != want {
				t.Fatalf("at EOF got %q, want %q", got, want)
			}
		})
	}

	// End-to-end child + grandchild check with SetChildConsoleTee.
	t.Run("grandchild_holding_pipe_does_not_block_Exited", func(t *testing.T) {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			t.Fatalf("listen: %v", err)
		}
		defer func() { _ = ln.Close() }()

		accepted := make(chan net.Conn, 1)
		go func() {
			c, aerr := ln.Accept()
			if aerr == nil {
				accepted <- c
			}
		}()

		var tee winSyncBuffer
		cmd := exec.Command(os.Args[0], "-test.run=^TestChildConsoleWindowsGrandchildHelper$")
		cmd.Env = append(os.Environ(),
			"KEPLOY_WIN_PIPE_HELPER_ROLE=child",
			"KEPLOY_WIN_PIPE_HELPER_ADDR="+ln.Addr().String(),
		)
		console, err := SetChildConsoleTee(cmd, func() io.Writer { return &tee })
		if err != nil {
			t.Fatalf("SetChildConsoleTee: %v", err)
		}
		err = cmd.Start()
		console.Started()
		if err != nil {
			t.Fatalf("cmd.Start: %v", err)
		}
		if err := cmd.Wait(); err != nil {
			t.Fatalf("cmd.Wait: %v", err)
		}

		// Wait for the grandchild to connect so we know it is alive and holding
		// the inherited pipe open while Exited runs.
		waitCtx, cancelWait := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancelWait()
		var grandConn net.Conn
		select {
		case grandConn = <-accepted:
			defer func() { _ = grandConn.Close() }()
		case <-waitCtx.Done():
			t.Fatal("grandchild never connected to hold gate")
		}

		exited := make(chan struct{})
		go func() {
			console.Exited()
			close(exited)
		}()
		exitedCtx, cancelExited := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelExited()
		select {
		case <-exited:
		case <-exitedCtx.Done():
			t.Fatal("ChildConsole.Exited blocked while a grandchild still held the inherited pipe")
		}

		if got := tee.String(); !strings.Contains(got, "127.0.0.1:61234 failed: port is already allocated") {
			t.Fatalf("tee got %q; want the exited child's output", got)
		}

		// Release the grandchild and confirm every copy finishes cleanly at EOF.
		_ = grandConn.Close()
		doneCtx, cancelDone := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancelDone()
		for _, cp := range console.copies {
			select {
			case <-cp.Done():
			case <-doneCtx.Done():
				t.Fatal("copy goroutine did not finish after grandchild closed the pipe")
			}
		}
	})
}
