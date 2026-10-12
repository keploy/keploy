//go:build linux

package linux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/utils"
)

// TestGetSelfInodeNumber verifies that the helper reads /proc/self/ns/pid and
// returns non-zero values for both inode and dev. The dev field is the part
// that breaks across kernel versions when nsfs lands on a different
// unnamed_dev_ida slot (see keploy/enterprise#1940), so an explicit
// non-zero assertion guards against regressions where the field is dropped
// or zeroed before reaching the BPF program.
func TestGetSelfInodeNumber(t *testing.T) {
	if _, err := os.Stat("/proc/self/ns/pid"); err != nil {
		t.Skipf("/proc/self/ns/pid not available in this environment: %v", err)
	}

	ino, dev, err := GetSelfInodeNumber()
	if err != nil {
		t.Fatalf("GetSelfInodeNumber returned error: %v", err)
	}
	if ino == 0 {
		t.Errorf("expected non-zero inode for /proc/self/ns/pid, got 0")
	}
	if dev == 0 {
		t.Errorf("expected non-zero dev for /proc/self/ns/pid, got 0")
	}
}

// A load the kernel refused for want of privileges has to reach the agent's
// exit status as exactly that (utils.ExitCodeFor), whatever step refused it:
// an unprivileged container fails at the memlock rlimit with EPERM, a
// perf_event_paranoid host at the tracepoint with EACCES. A program the
// verifier rejected was not refused for want of privileges, though the
// verifier says so with the same EACCES: the load was allowed, it is keploy's
// program the kernel would not take, and no privilege changes that.
func TestPrivilegeFailure(t *testing.T) {
	for _, tc := range []struct {
		name string
		err  error
		want int
	}{
		{"memlock rlimit refused", fmt.Errorf("failed to set memlock rlimit: %w", unix.EPERM), utils.ExitPrivilegeRequired},
		{"perf event refused", fmt.Errorf("opening tracepoint perf event: %w", unix.EACCES), utils.ExitPrivilegeRequired},
		{"not a permission error", errors.New("neither debugfs nor tracefs are mounted"), utils.ExitKeployError},
		// cilium/ebpf's shapes for a program load, as LoadAndAssign wraps them.
		{"load refused before the verifier ran", fmt.Errorf("field KSockops: program k_sockops: %w",
			&ebpf.VerifierError{Cause: unix.EPERM}), utils.ExitPrivilegeRequired},
		{"program rejected by the verifier", fmt.Errorf("field KSockops: program k_sockops: %w",
			&ebpf.VerifierError{Cause: unix.EACCES, Log: []string{"0: (61) r0 = *(u32 *)(r1 +4000)", "invalid bpf_context access off=4000 size=4"}}), utils.ExitKeployError},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := privilegeFailure(tc.err)
			if code := utils.ExitCodeFor(got); code != tc.want {
				t.Fatalf("privilegeFailure(%q) = %q, exits %d; want %d", tc.err, got, code, tc.want)
			}
			if !errors.Is(got, tc.err) {
				t.Fatalf("the cause was replaced, not wrapped: %q", got)
			}
		})
	}
	// Tagged once: the message a user reads must not stutter.
	tagged := privilegeFailure(fmt.Errorf("failed to set memlock rlimit: %w", unix.EPERM))
	if again := privilegeFailure(tagged); again.Error() != tagged.Error() {
		t.Fatalf("tagged twice: %q", again)
	}
}

// The same, against the real kernel rather than the error shapes above, and
// whichever way this machine is set up: a program the verifier rejects is a
// privilege failure only where the kernel would not load a valid program
// either -- an unprivileged container, a host that disallows unprivileged
// eBPF -- and never where it would.
func TestPrivilegeFailureOfARealLoad(t *testing.T) {
	load := func(insns asm.Instructions) error {
		prog, err := ebpf.NewProgramWithOptions(&ebpf.ProgramSpec{
			Type:         ebpf.SocketFilter,
			Instructions: insns,
			License:      "GPL",
		}, ebpf.ProgramOptions{LogLevel: ebpf.LogLevelInstruction | ebpf.LogLevelBranch})
		if err == nil {
			_ = prog.Close()
		}
		return err
	}
	allowed := load(asm.Instructions{asm.Mov.Imm(asm.R0, 0), asm.Return()})
	// Reads far past the end of the socket buffer's context.
	rejected := load(asm.Instructions{asm.LoadMem(asm.R0, asm.R1, 4000, asm.Word), asm.Return()})
	if rejected == nil {
		t.Fatal("the kernel loaded a program that reads outside its context")
	}
	want := utils.ExitKeployError
	switch {
	case allowed == nil:
	case errors.Is(allowed, os.ErrPermission):
		want = utils.ExitPrivilegeRequired
	default:
		t.Skipf("this kernel loads no eBPF here: %v", allowed)
	}
	if got := utils.ExitCodeFor(privilegeFailure(rejected)); got != want {
		t.Fatalf("a valid program loads with err %v; the rejected one failed with %v and exits %d, want %d", allowed, rejected, got, want)
	}
}

// A tracepoint that cannot attach because tracefs is not mounted is the
// environment's to fix, not a privilege to grant -- a --privileged container
// started without /sys/kernel/debug has every capability and still fails
// there. With tracefs mounted, the same failure is left as it was.
func TestTracepointFailure(t *testing.T) {
	cause := errors.New("neither debugfs nor tracefs are mounted")
	if got := tracepointFailure(cause, false); utils.ExitCodeFor(got) != utils.ExitEnvironmentUnsupported || !errors.Is(got, cause) {
		t.Fatalf("tracefs missing: %q exits %d", got, utils.ExitCodeFor(got))
	}
	if got := tracepointFailure(cause, true); got != cause {
		t.Fatalf("tracefs mounted, yet the failure was retagged: %q", got)
	}
}

// tracefsMounted must agree with the mount table about the places the
// tracepoint API is looked up: a tracefs mounted anywhere, or a debugfs with a
// tracing directory (cilium/ebpf's internal/tracefs, from v0.22.0). Whichever
// way this machine is set up, the two answers have to match.
func TestTracefsMountedAgreesWithTheMountTable(t *testing.T) {
	mounts, err := os.ReadFile("/proc/self/mounts")
	if err != nil {
		t.Skipf("no mount table: %v", err)
	}
	want := false
	for _, line := range strings.Split(string(mounts), "\n") {
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		switch f[2] {
		case "tracefs":
			want = true
		case "debugfs":
			if st, err := os.Stat(filepath.Join(f[1], "tracing")); err == nil && st.IsDir() {
				want = true
			}
		}
	}
	if got := tracefsMounted(); got != want {
		t.Fatalf("tracefsMounted() = %v, the mount table says %v", got, want)
	}
}

// What cilium/ebpf takes for tracefs in a mountinfo table: a tracefs mounted
// whole at any path (a container's bind mount at /host/... included), not a
// mount of one of its directories; a debugfs only where it has a tracing
// directory; and nothing from a table its parser refuses.
func TestTracefsInMountinfo(t *testing.T) {
	withTracing := t.TempDir()
	if err := os.Mkdir(filepath.Join(withTracing, "tracing"), 0o755); err != nil {
		t.Fatal(err)
	}
	withSpace := filepath.Join(t.TempDir(), "a dir")
	if err := os.MkdirAll(filepath.Join(withSpace, "tracing"), 0o755); err != nil {
		t.Fatal(err)
	}
	without := t.TempDir()
	const root = "22 1 259:2 / / rw,relatime shared:1 - ext4 /dev/root rw\n"
	for _, c := range []struct {
		name, table string
		found, ok   bool
	}{
		{"the canonical tracefs", root + "35 22 0:12 / /sys/kernel/tracing rw,nosuid shared:15 - tracefs tracefs rw\n", true, true},
		{"tracefs bind-mounted elsewhere", root + "40 22 0:12 / /host/sys/kernel/tracing rw - tracefs tracefs rw\n", true, true},
		{"one directory of a tracefs", root + "41 22 0:12 /instances/foo /mnt/t rw - tracefs tracefs rw\n", false, true},
		{"debugfs with tracing", root + "36 22 0:7 / " + withTracing + " rw shared:16 - debugfs debugfs rw\n", true, true},
		{"debugfs with tracing, at a path with a space", root + "36 22 0:7 / " + strings.ReplaceAll(withSpace, " ", `\040`) + " rw - debugfs debugfs rw\n", true, true},
		{"debugfs without tracing", root + "36 22 0:7 / " + without + " rw - debugfs debugfs rw\n", false, true},
		{"neither", root, false, true},
		{"a line the parser refuses", root + "35 22 0:12 / /sys/kernel/tracing rw tracefs\n", false, false},
	} {
		if found, ok := tracefsInMountinfo(c.table); found != c.found || ok != c.ok {
			t.Errorf("%s: found %v, ok %v; want %v, %v", c.name, found, ok, c.found, c.ok)
		}
	}
}
