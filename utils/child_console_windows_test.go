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
	"unsafe"

	"golang.org/x/sys/windows"
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

// TestChildConsoleWindowsPipeHasOwnerOnlyDACLAndReadAttributes pins that
// childConsolePipe creates its named pipe with an owner-only protected DACL
// (current user + SYSTEM only, zero Everyone/Anonymous/Authenticated Users/
// Users ACEs) and opens the write end with FILE_READ_ATTRIBUTES so children
// can query pipe attributes just like Windows' CreatePipe write handle.
func TestChildConsoleWindowsPipeHasOwnerOnlyDACLAndReadAttributes(t *testing.T) {
	r, w, err := childConsolePipe()
	if err != nil {
		t.Fatalf("childConsolePipe: %v", err)
	}
	defer func() { _ = r.Close() }()
	defer func() { _ = w.Close() }()

	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		t.Fatalf("GetTokenUser: %v", err)
	}
	currentUserSID := tokenUser.User.Sid
	systemSID, err := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	if err != nil {
		t.Fatalf("CreateWellKnownSid(SYSTEM): %v", err)
	}
	forbiddenSIDs := map[string]windows.WELL_KNOWN_SID_TYPE{
		"Everyone":            windows.WinWorldSid,
		"Anonymous":           windows.WinAnonymousSid,
		"Authenticated Users": windows.WinAuthenticatedUserSid,
		"Builtin Users":       windows.WinBuiltinUsersSid,
	}
	forbidden := make(map[string]*windows.SID, len(forbiddenSIDs))
	for name, kind := range forbiddenSIDs {
		sid, serr := windows.CreateWellKnownSid(kind)
		if serr != nil {
			t.Fatalf("CreateWellKnownSid(%s): %v", name, serr)
		}
		forbidden[name] = sid
	}

	rConn, err := r.SyscallConn()
	if err != nil {
		t.Fatalf("read handle SyscallConn: %v", err)
	}
	var sd *windows.SECURITY_DESCRIPTOR
	var secErr error
	if err := rConn.Control(func(fd uintptr) {
		sd, secErr = windows.GetSecurityInfo(
			windows.Handle(fd),
			windows.SE_KERNEL_OBJECT,
			windows.DACL_SECURITY_INFORMATION,
		)
	}); err != nil {
		t.Fatalf("read handle Control: %v", err)
	}
	if secErr != nil {
		t.Fatalf("GetSecurityInfo: %v", secErr)
	}

	control, _, err := sd.Control()
	if err != nil {
		t.Fatalf("sd.Control: %v", err)
	}
	if control&windows.SE_DACL_PROTECTED == 0 {
		t.Fatalf("pipe DACL is not protected (control=0x%x, sddl=%q)", control, sd.String())
	}

	dacl, defaulted, err := sd.DACL()
	if err != nil {
		t.Fatalf("sd.DACL: %v", err)
	}
	if defaulted || dacl == nil {
		t.Fatalf("pipe DACL defaulted=%v dacl=%v; want explicit owner-only DACL", defaulted, dacl)
	}

	var sawCurrentUser, sawSystem bool
	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if err := windows.GetAce(dacl, uint32(i), &ace); err != nil {
			t.Fatalf("GetAce(%d): %v", i, err)
		}
		if ace.Header.AceType != windows.ACCESS_ALLOWED_ACE_TYPE {
			t.Fatalf("ACE %d has unexpected type %d (sddl=%q)", i, ace.Header.AceType, sd.String())
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		for name, fsid := range forbidden {
			if fsid.Equals(sid) {
				t.Fatalf("pipe DACL grants access to forbidden SID %s (%s); sddl=%q", name, sid.String(), sd.String())
			}
		}
		switch {
		case currentUserSID.Equals(sid):
			sawCurrentUser = true
			if systemSID.Equals(sid) {
				sawSystem = true
			}
		case systemSID.Equals(sid):
			sawSystem = true
		default:
			t.Fatalf("pipe DACL contains unexpected SID %s (want only current user %s and SYSTEM %s); sddl=%q",
				sid.String(), currentUserSID.String(), systemSID.String(), sd.String())
		}
	}
	if !sawCurrentUser || !sawSystem {
		t.Fatalf("pipe DACL missing required ACEs (sawCurrentUser=%v sawSystem=%v, sddl=%q)", sawCurrentUser, sawSystem, sd.String())
	}

	// Verify the write handle has FILE_READ_ATTRIBUTES (matching Windows'
	// CreatePipe write end) so children can query handle/pipe attributes.
	wConn, err := w.SyscallConn()
	if err != nil {
		t.Fatalf("write handle SyscallConn: %v", err)
	}
	var fileInfoErr, pipeInfoErr, pipeStateErr error
	var maxInstances, curInstances uint32
	if err := wConn.Control(func(fd uintptr) {
		h := windows.Handle(fd)
		var info windows.ByHandleFileInformation
		fileInfoErr = windows.GetFileInformationByHandle(h, &info)
		var flags, outSize, inSize uint32
		pipeInfoErr = windows.GetNamedPipeInfo(h, &flags, &outSize, &inSize, &maxInstances)
		var state uint32
		pipeStateErr = windows.GetNamedPipeHandleState(h, &state, &curInstances, nil, nil, nil, 0)
	}); err != nil {
		t.Fatalf("write handle Control: %v", err)
	}
	if fileInfoErr != nil {
		t.Fatalf("GetFileInformationByHandle on write end failed (missing FILE_READ_ATTRIBUTES?): %v", fileInfoErr)
	}
	if pipeInfoErr != nil {
		t.Fatalf("GetNamedPipeInfo on write end failed (missing FILE_READ_ATTRIBUTES?): %v", pipeInfoErr)
	}
	if maxInstances != 1 {
		t.Fatalf("GetNamedPipeInfo maxInstances=%d; want 1", maxInstances)
	}
	if pipeStateErr != nil {
		t.Fatalf("GetNamedPipeHandleState on write end failed (missing FILE_READ_ATTRIBUTES?): %v", pipeStateErr)
	}
	if curInstances != 1 {
		t.Fatalf("GetNamedPipeHandleState curInstances=%d; want 1", curInstances)
	}
}
