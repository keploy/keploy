//go:build windows

package utils

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"runtime"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procPeekNamedPipe = windows.NewLazySystemDLL("kernel32.dll").NewProc("PeekNamedPipe")

// childConsolePipeSecurityAttributes builds an owner-only protected DACL
// granting Generic All only to SYSTEM and the current process user, so no
// other local account can open the single-instance named pipe between
// CreateNamedPipe and CreateFile.
func childConsolePipeSecurityAttributes() (*windows.SecurityAttributes, *windows.SECURITY_DESCRIPTOR, error) {
	tokenUser, err := windows.GetCurrentProcessToken().GetTokenUser()
	if err != nil {
		return nil, nil, fmt.Errorf("get current process token user: %w", err)
	}
	userSID := tokenUser.User.Sid.String()
	sddl := fmt.Sprintf("D:P(A;;GA;;;SY)(A;;GA;;;%s)", userSID)
	if systemSID, serr := windows.CreateWellKnownSid(windows.WinLocalSystemSid); serr == nil && systemSID.Equals(tokenUser.User.Sid) {
		sddl = "D:P(A;;GA;;;SY)"
	}
	sd, err := windows.SecurityDescriptorFromString(sddl)
	if err != nil {
		return nil, nil, fmt.Errorf("build child console pipe security descriptor: %w", err)
	}
	sa := &windows.SecurityAttributes{
		Length:             uint32(unsafe.Sizeof(windows.SecurityAttributes{})),
		SecurityDescriptor: sd,
	}
	return sa, sd, nil
}

// childConsolePipe creates a single-instance overlapped named pipe so its read
// end joins Go's IOCP netpoller and supports SetReadDeadline (os.Pipe on
// Windows creates non-overlapped anonymous pipes that reject read deadlines).
// The pipe is restricted to the current user and SYSTEM via an owner-only
// protected DACL. The write end is opened synchronously with
// GENERIC_WRITE|FILE_READ_ATTRIBUTES (matching Windows' CreatePipe write
// handle) so a child process can both write and query pipe attributes.
func childConsolePipe() (r, w *os.File, err error) {
	var nonce [16]byte
	if _, err := rand.Read(nonce[:]); err != nil {
		return nil, nil, fmt.Errorf("generate child console pipe name: %w", err)
	}
	name := fmt.Sprintf(`\\.\pipe\keploy-child-console-%d-%s`, os.Getpid(), hex.EncodeToString(nonce[:]))
	wname, err := windows.UTF16PtrFromString(name)
	if err != nil {
		return nil, nil, err
	}
	sa, sd, err := childConsolePipeSecurityAttributes()
	if err != nil {
		return nil, nil, err
	}
	const openMode = windows.PIPE_ACCESS_INBOUND | windows.FILE_FLAG_OVERLAPPED | windows.FILE_FLAG_FIRST_PIPE_INSTANCE
	const pipeMode = windows.PIPE_TYPE_BYTE | windows.PIPE_READMODE_BYTE | windows.PIPE_WAIT | windows.PIPE_REJECT_REMOTE_CLIENTS
	rh, err := windows.CreateNamedPipe(wname, openMode, pipeMode, 1, 0, 64<<10, 0, sa)
	runtime.KeepAlive(sd)
	if err != nil {
		return nil, nil, err
	}
	wh, err := windows.CreateFile(wname, windows.GENERIC_WRITE|windows.FILE_READ_ATTRIBUTES, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		_ = windows.CloseHandle(rh)
		return nil, nil, err
	}
	return os.NewFile(uintptr(rh), name), os.NewFile(uintptr(wh), name), nil
}

// peekNamedPipeAvail reports the number of bytes currently buffered in pipe
// handle h without removing them.
func peekNamedPipeAvail(h uintptr) (uint32, bool) {
	var avail uint32
	r1, _, _ := procPeekNamedPipe.Call(
		h,
		0,
		0,
		0,
		uintptr(unsafe.Pointer(&avail)),
		0,
	)
	return avail, r1 != 0
}

// drainNow writes to dst what src holds when it is called, and no more: a
// process the child left running may write on without end. PeekNamedPipe tells
// how many bytes are buffered right now, and only that many bytes are read
// after clearing the read deadline set by Drain.
func drainNow(src *os.File, buf []byte, dst io.Writer) {
	rc, err := src.SyscallConn()
	if err != nil {
		return
	}
	var left int
	if err := rc.Control(func(fd uintptr) {
		if avail, ok := peekNamedPipeAvail(fd); ok {
			left = int(avail)
		}
	}); err != nil || left <= 0 {
		return
	}
	_ = src.SetReadDeadline(time.Time{})
	for left > 0 {
		n, err := src.Read(buf[:min(left, len(buf))])
		if n > 0 {
			_, _ = dst.Write(buf[:n])
			left -= n
		}
		if err != nil || n <= 0 {
			return
		}
	}
}
