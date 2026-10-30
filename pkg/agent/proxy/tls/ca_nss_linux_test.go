package tls

import (
	"bufio"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
)

// TestNSSDatabaseIsTheAppsUsersHome builds only on Linux: nssDBDir reads the
// app's HOME from /proc/<pid>/environ, which only Linux has.
func TestNSSDatabaseIsTheAppsUsersHome(t *testing.T) {
	home := t.TempDir()
	// The app: a shell that says it runs, then waits on its stdin. Its
	// environ is read once it has said so: a process the exec is still
	// setting up shows an empty one.
	cmd := exec.Command("/bin/sh", "-c", "echo running; read x")
	cmd.Env = []string{"HOME=" + home}
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := cmd.StdinPipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		t.Fatal(err)
	}
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = stdin.Close()
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
		_ = cmd.Wait()
	})
	if line, err := bufio.NewReader(stdout).ReadString('\n'); err != nil || line != "running\n" {
		t.Fatalf("the app did not start: %q %v", line, err)
	}
	if got, want := nssDBDir(cmd.Process.Pid), filepath.Join(home, ".pki", "nssdb"); got != want {
		t.Fatalf("app's database %q, want %q", got, want)
	}
	agentHome := t.TempDir()
	t.Setenv("HOME", agentHome)
	t.Setenv("SUDO_USER", "")
	if got, want := nssDBDir(0), filepath.Join(agentHome, ".pki", "nssdb"); got != want {
		t.Fatalf("with no app PID: %q, want %q", got, want)
	}
}
