//go:build windows

package utils

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"time"
	"unsafe"

	"golang.org/x/sys/windows"
)

var procPeekNamedPipe = windows.NewLazySystemDLL("kernel32.dll").NewProc("PeekNamedPipe")

// childConsolePipe creates a single-instance overlapped named pipe so its read
// end joins Go's IOCP netpoller and supports SetReadDeadline (os.Pipe on
// Windows creates non-overlapped anonymous pipes that reject read deadlines).
// The write end is opened synchronously for the child process to inherit and
// write to like a standard pipe.
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
	const openMode = windows.PIPE_ACCESS_INBOUND | windows.FILE_FLAG_OVERLAPPED | windows.FILE_FLAG_FIRST_PIPE_INSTANCE
	const pipeMode = windows.PIPE_TYPE_BYTE | windows.PIPE_READMODE_BYTE | windows.PIPE_WAIT | windows.PIPE_REJECT_REMOTE_CLIENTS
	rh, err := windows.CreateNamedPipe(wname, openMode, pipeMode, 1, 0, 64<<10, 0, nil)
	if err != nil {
		return nil, nil, err
	}
	wh, err := windows.CreateFile(wname, windows.GENERIC_WRITE, 0, nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
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
