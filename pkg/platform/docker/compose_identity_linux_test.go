//go:build linux

package docker

import (
	"os"
	"strings"
	"syscall"
	"testing"
)

// An unset variable is not reported by keploy's read of the files: compose
// says so itself when it runs them, and compose's library writes it straight
// to the process's standard error, outside keploy's log.
func TestComposeIdentity_UnsetVariableIsNotReportedTwice(t *testing.T) {
	dir := composeProjectDir(t, map[string]string{"compose.yml": "services:\n  app:\n    image: x\n    container_name: ${APP_CONTAINER}\n"})
	t.Chdir(dir)

	out, err := os.CreateTemp(t.TempDir(), "stderr")
	if err != nil {
		t.Fatal(err)
	}
	saved, err := syscall.Dup(2)
	if err != nil {
		t.Fatal(err)
	}
	if err := syscall.Dup3(int(out.Fd()), 2, 0); err != nil {
		t.Fatal(err)
	}
	_, findErr := findIn(t, dir, []string{"compose.yml"}, ComposeScope{}, "proj-app-1")
	if err := syscall.Dup3(saved, 2, 0); err != nil {
		t.Fatal(err)
	}
	_ = syscall.Close(saved)
	if findErr != nil {
		t.Fatalf("FindContainerInComposeFiles: %v", findErr)
	}
	written, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(written), "APP_CONTAINER") {
		t.Fatalf("keploy's read of the files reported the unset variable: %q", written)
	}
}
