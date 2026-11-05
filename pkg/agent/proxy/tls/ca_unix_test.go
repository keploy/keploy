//go:build unix

package tls

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// setOwnProcessGroup makes cmd start in a process group of its own, so that
// killProcessGroup reaches every process it starts.
func setOwnProcessGroup(cmd *exec.Cmd) {
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
}

// killProcessGroup kills the process group cmd leads, if it started.
func killProcessGroup(cmd *exec.Cmd) error {
	if cmd.Process == nil {
		return nil
	}
	return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
}

// The loader reads every file in SSL_CERT_DIR and the default certificate
// directories. Anything but a regular file is refused before it is read: a
// FIFO (an open or a read of one can block forever) and a device such as
// /dev/null or /dev/zero (one reads nothing, the other never ends).
func TestReadRegularFile_RefusesFIFOsAndDevices(t *testing.T) {
	fifo := filepath.Join(t.TempDir(), "fifo.pem")
	if err := syscall.Mkfifo(fifo, 0o644); err != nil {
		t.Fatal(err)
	}
	// An open that blocked on the FIFO is released when the test ends.
	t.Cleanup(func() {
		if w, err := os.OpenFile(fifo, os.O_WRONLY|syscall.O_NONBLOCK, 0); err == nil {
			_ = w.Close()
		}
	})
	for _, path := range []string{fifo, "/dev/null"} {
		type result struct {
			n   int
			err error
		}
		done := make(chan result, 1)
		go func() {
			data, err := readRegularFile(path)
			done <- result{len(data), err}
		}()
		select {
		case r := <-done:
			if r.err == nil || !strings.Contains(r.err.Error(), "not a regular file") {
				t.Errorf("%s: got %d bytes, err %v; want it refused as not a regular file", path, r.n, r.err)
			}
		case <-time.After(10 * time.Second):
			t.Errorf("%s: the read blocked", path)
		}
	}
}
