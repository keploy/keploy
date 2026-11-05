//go:build unix

package tls

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
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

// ownedByThisUser tells this user's directory from one another user owns.
func TestOwnedByThisUser(t *testing.T) {
	if !ownedByThisUser(t.TempDir()) {
		t.Fatal("this user's own directory is not this user's")
	}
	if os.Geteuid() != 0 && ownedByThisUser("/") {
		t.Fatal("root's / is this user's")
	}
	if ownedByThisUser(filepath.Join(t.TempDir(), "missing")) {
		t.Fatal("a missing path is this user's")
	}
}

// A run's directory and files keep their modes whatever the process's umask:
// the app may run as another user (under sudo) and must read them. keploy's
// main sets umask 0, so only a restrictive umask shows a mode left to it.
func TestSetupCaCertEnv_ModesHoldUnderARestrictiveUmask(t *testing.T) {
	isolateCertEnv(t)
	tmp := useTempDir(t)
	withSystemSearch(t, nil, nil)
	t.Setenv(EnvJavaToolOptions, "")
	old := syscall.Umask(0o077)
	t.Cleanup(func() { syscall.Umask(old) })
	control := filepath.Join(t.TempDir(), "control")
	if err := os.WriteFile(control, nil, 0o644); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Stat(control); err != nil {
		t.Fatal(err)
	} else if st.Mode().Perm() != 0o600 {
		t.Fatalf("control: umask 077 did not apply: a 0644 file was made %v", st.Mode().Perm())
	}
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	if err := SetupCaCertEnv(ctx, zap.NewNop(), testCACertPEM()); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Dir(os.Getenv("NODE_EXTRA_CA_CERTS"))
	run := liveRunOf(t, dir)
	t.Cleanup(func() {
		cancel()
		waitRunGone(t, run)
	})
	if dirs := runDirsIn(t, tmp); len(dirs) != 1 || dirs[0] != dir {
		t.Fatalf("the temp directory holds %q; want this run's one, %q", dirs, dir)
	}
	for p, want := range map[string]os.FileMode{dir: 0o755, os.Getenv(envSSLCertFile): 0o644, os.Getenv("NODE_EXTRA_CA_CERTS"): 0o644} {
		st, err := os.Stat(p)
		if err != nil {
			t.Fatal(err)
		}
		if got := st.Mode().Perm(); got != want {
			t.Errorf("%s: mode %v under umask 077; want %v", p, got, want)
		}
	}
}
