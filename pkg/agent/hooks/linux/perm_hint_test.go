//go:build linux

package linux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"

	"github.com/cilium/ebpf"
)

// withParanoid points the hint at a temp file holding the given contents.
func withParanoid(t *testing.T, contents string) {
	t.Helper()
	p := filepath.Join(t.TempDir(), "perf_event_paranoid")
	if err := os.WriteFile(p, []byte(contents), 0o600); err != nil {
		t.Fatalf("write temp sysctl: %v", err)
	}
	old := perfEventParanoidPath
	perfEventParanoidPath = p
	t.Cleanup(func() { perfEventParanoidPath = old })
}

func TestPerfEventPermissionHint_ParanoidThreeNamesSysctlAndCap(t *testing.T) {
	withParanoid(t, "3\n")

	got := PerfEventPermissionHint(syscall.EPERM)

	for _, want := range []string{"kernel.perf_event_paranoid", "3", "CAP_SYS_ADMIN", "CAP_PERFMON is NOT sufficient"} {
		if !strings.Contains(got, want) {
			t.Errorf("hint %q missing %q", got, want)
		}
	}
}

// EACCES is the other permission errno perf_event_open can return; it must be
// diagnosed identically, not fall through as an unrelated error.
func TestPerfEventPermissionHint_EACCESIsAPermissionError(t *testing.T) {
	withParanoid(t, "3\n")

	if got := PerfEventPermissionHint(syscall.EACCES); got == "" {
		t.Error("EACCES produced no hint")
	}
}

// With a permissive sysctl the paranoid level is NOT the explanation, so the
// hint must not blame it — claiming the wrong cause is worse than a generic
// message.
func TestPerfEventPermissionHint_PermissiveParanoidFallsBackToCapabilities(t *testing.T) {
	withParanoid(t, "2\n")

	got := PerfEventPermissionHint(syscall.EPERM)

	if strings.Contains(got, "perf_event_paranoid") {
		t.Errorf("hint blames the sysctl at a permissive level: %q", got)
	}
	if !strings.Contains(got, "CAP_BPF") {
		t.Errorf("hint %q does not state the capability requirement", got)
	}
}

// An unreadable sysctl must degrade to the capability hint rather than
// reporting a bogus level (Atoi of "" yields 0, which must not be treated as a
// successful read).
func TestPerfEventPermissionHint_UnreadableSysctlFallsBack(t *testing.T) {
	old := perfEventParanoidPath
	perfEventParanoidPath = filepath.Join(t.TempDir(), "does-not-exist")
	t.Cleanup(func() { perfEventParanoidPath = old })

	got := PerfEventPermissionHint(syscall.EPERM)

	if strings.Contains(got, "perf_event_paranoid") {
		t.Errorf("hint blames the sysctl it could not read: %q", got)
	}
	if got == "" {
		t.Error("unreadable sysctl produced no hint at all")
	}
}

// Only permission failures get this explanation; anything else must pass
// through untouched so its own error is not masked.
func TestPerfEventPermissionHint_NonPermissionErrorsAreIgnored(t *testing.T) {
	withParanoid(t, "3\n")

	if got := PerfEventPermissionHint(errors.New("no such file or directory")); got != "" {
		t.Errorf("non-permission error produced hint %q", got)
	}
	if got := PerfEventPermissionHint(nil); got != "" {
		t.Errorf("nil error produced hint %q", got)
	}
}

// The exact error shape cilium/ebpf produces: link/perf_event.go wraps the
// raw errno from unix.PerfEventOpen with %w. If errors.Is stopped matching
// through that wrapping the hint would silently never fire, so pin it.
func TestPerfEventPermissionHint_MatchesCiliumEbpfErrorShape(t *testing.T) {
	withParanoid(t, "3\n")

	err := fmt.Errorf("opening tracepoint perf event: %w", syscall.EPERM)

	if got := PerfEventPermissionHint(err); !strings.Contains(got, "CAP_SYS_ADMIN") {
		t.Errorf("library error shape not diagnosed: %q", got)
	}
}

// link.Tracepoint reads the tracefs event id BEFORE it calls perf_event_open,
// and an EACCES there is also os.ErrPermission. Blaming the paranoid sysctl for
// a missing debugfs mount would send the reader down the wrong path entirely.
func TestPerfEventPermissionHint_TracefsErrorIsNotBlamedOnTheSysctl(t *testing.T) {
	withParanoid(t, "3\n")

	err := fmt.Errorf("reading event id: %w", &os.PathError{
		Op:   "open",
		Path: "/sys/kernel/tracing/events/syscalls/sys_enter_socket/id",
		Err:  syscall.EACCES,
	})

	got := PerfEventPermissionHint(err)

	if strings.Contains(got, "perf_event_paranoid") {
		t.Errorf("filesystem permission error blamed on the sysctl: %q", got)
	}
	if !strings.Contains(got, "/sys/kernel/tracing") {
		t.Errorf("hint %q does not name the unreadable path", got)
	}
}

// Uprobe attaches open the TARGET BINARY through this same helper. A permission
// error there must not be answered with "mount debugfs" — that is a confidently
// wrong answer that sends the operator to the wrong system.
func TestPerfEventPermissionHint_NonTracefsPathIsNotADebugfsProblem(t *testing.T) {
	withParanoid(t, "3\n")

	err := fmt.Errorf("open executable: %w", &os.PathError{
		Op: "open", Path: "/app/bin/server", Err: syscall.EACCES,
	})

	got := PerfEventPermissionHint(err)

	if strings.Contains(got, "debugfs") || strings.Contains(got, "/sys/kernel") {
		t.Errorf("non-tracefs path answered with a debugfs prescription: %q", got)
	}
	if !strings.Contains(got, "/app/bin/server") {
		t.Errorf("hint %q does not name the unreadable path", got)
	}
}

// The kernel answers BPF_PROG_LOAD with EACCES when the VERIFIER rejects a
// program, and VerifierError.Unwrap() exposes that errno — so a verifier
// failure satisfies errors.Is(_, os.ErrPermission) and would otherwise be
// answered with "grant CAP_SYS_ADMIN / change kernel.perf_event_paranoid".
// Neither can fix a program the verifier refused; the operator would burn a
// support cycle on permissions while staring at a verifier log.
func TestPerfEventPermissionHint_VerifierRejectionIsNotAPermissionProblem(t *testing.T) {
	withParanoid(t, "3\n")

	ve := &ebpf.VerifierError{
		Cause: syscall.EACCES,
		Log:   []string{"invalid mem access 'inv'"},
	}

	if got := PerfEventPermissionHint(ve); got != "" {
		t.Errorf("verifier rejection diagnosed as a permission problem: %q", got)
	}
	// ...including when it is wrapped, which is how it reaches the caller.
	wrapped := fmt.Errorf("load program: %w", ve)
	if got := PerfEventPermissionHint(wrapped); got != "" {
		t.Errorf("wrapped verifier rejection diagnosed as a permission problem: %q", got)
	}
}
