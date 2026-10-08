//go:build linux

package linux

import (
	"errors"
	"fmt"
	"os"
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
// tracepoint API is looked up. Whichever way this machine is set up, the two
// answers have to match.
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
		switch {
		case f[1] == "/sys/kernel/tracing" && f[2] == "tracefs",
			f[1] == "/sys/kernel/debug/tracing" && f[2] == "tracefs":
			want = true
		case f[1] == "/sys/kernel/debug" && f[2] == "debugfs":
			// debugfs exposes the tracing directory itself, as
			// /sys/kernel/debug/tracing, when tracefs is not mounted there.
			if _, err := os.Stat("/sys/kernel/debug/tracing"); err == nil {
				want = true
			}
		}
	}
	if got := tracefsMounted(); got != want {
		t.Fatalf("tracefsMounted() = %v, the mount table says %v", got, want)
	}
}
