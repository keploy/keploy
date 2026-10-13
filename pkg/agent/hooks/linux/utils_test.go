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

// A refused perf_event_open names its cause. The sidecar the k8s-proxy chart
// injects holds CAP_BPF and CAP_PERFMON and no CAP_SYS_ADMIN, loads every
// program, and on a Debian host still at its boot-time perf_event_paranoid of 3
// is refused the tracepoint's perf event with EACCES: keploy/enterprise
// pipeline 10867, where the log said only "permission denied" and the search
// went to the pod's capabilities. Each case says what to change and only what
// can apply, the cause stays wrapped, and the agent still exits
// ExitPrivilegeRequired.
func TestExplainPerfEventRefusal(t *testing.T) {
	const (
		lower       = "set kernel.perf_event_paranoid=2 on the host"
		persist     = "/etc/sysctl.d/"
		grant       = "or grant keploy CAP_SYS_ADMIN"
		policy      = "a security policy refused perf_event_open"
		restriction = "a Debian kernel refuses perf_event_open to any process without CAP_SYS_ADMIN, CAP_PERFMON included, while kernel.perf_event_paranoid is above 2"
	)
	eacces := fmt.Errorf("opening tracepoint perf event: %w", unix.EACCES)
	eperm := fmt.Errorf("opening tracepoint perf event: %w", unix.EPERM)
	sidecar := perfEventEnv{eacces: true, capsKnown: true, kernel: "6.12.111+deb13-cloud-amd64", debian: true}
	fedora := perfEventEnv{eacces: true, capsKnown: true, kernel: "7.2.7-200.fc44.x86_64"}
	at := func(env perfEventEnv, level int) perfEventEnv {
		env.paranoid, env.paranoidKnown = level, true
		return env
	}
	withEPERM := func(env perfEventEnv) perfEventEnv {
		env.eacces = false
		return env
	}
	for _, tc := range []struct {
		name    string
		err     error
		env     perfEventEnv
		want    []string
		notWant []string
	}{
		{
			name: "CAP_PERFMON on a Debian host at 3 (pipeline 10867)", err: eacces, env: at(sidecar, 3),
			want:    []string{"kernel.perf_event_paranoid is 3 (kernel 6.12.111+deb13-cloud-amd64, Debian's) and keploy holds CAP_PERFMON but not CAP_SYS_ADMIN: " + restriction + "; to fix it, " + lower, persist, grant},
			notWant: []string{policy, "if this kernel"},
		},
		{
			name: "CAP_PERFMON on a Debian host at 4", err: eacces, env: at(sidecar, 4),
			want: []string{"kernel.perf_event_paranoid is 4", restriction, lower, grant},
		},
		{
			// A hardened mainline host at 3 admits CAP_PERFMON, and its
			// SELinux refuses with EACCES too: both are said, Debian's as an if.
			name: "CAP_PERFMON at 3 on a kernel that is not Debian's", err: eacces, env: at(fedora, 3),
			want:    []string{"kernel.perf_event_paranoid is 3 (kernel 7.2.7-200.fc44.x86_64) and keploy holds CAP_PERFMON but not CAP_SYS_ADMIN: if this kernel restricts perf events as Debian's does (" + restriction + "), to fix it, " + lower, grant, "; if not, it lets CAP_PERFMON through, so " + policy},
			notWant: []string{"Debian's)"},
		},
		{
			name: "CAP_PERFMON, the sysctl unreadable", err: eacces, env: sidecar,
			want:    []string{"kernel.perf_event_paranoid could not be read (kernel 6.12.111+deb13-cloud-amd64, Debian's) and keploy holds CAP_PERFMON but not CAP_SYS_ADMIN: if it is above 2 on a kernel that restricts perf events as Debian's does (" + restriction + "), to fix it, " + lower, "; if not, " + policy},
			notWant: []string{"above 2, above 2"},
		},
		{
			name: "CAP_PERFMON at 2: not the sysctl", err: eacces, env: at(sidecar, 2),
			want:    []string{"kernel.perf_event_paranoid is 2 (kernel 6.12.111+deb13-cloud-amd64, Debian's), which lets CAP_PERFMON through, and keploy holds CAP_PERFMON but not CAP_SYS_ADMIN, so " + policy},
			notWant: []string{lower, restriction},
		},
		{
			// An Ubuntu node at its boot-time 4 admits CAP_PERFMON; a seccomp
			// profile or a BPF LSM answering EPERM is not the sysctl.
			name: "CAP_PERFMON at 4, refused with EPERM", err: eperm, env: withEPERM(at(fedora, 4)),
			want:    []string{"the kernel answered EPERM, not the EACCES of kernel.perf_event_paranoid's restriction (kernel.perf_event_paranoid is 4", "so " + policy},
			notWant: []string{lower, restriction},
		},
		{
			name: "CAP_SYS_ADMIN at 4: not the sysctl", err: eacces, env: at(perfEventEnv{eacces: true, capsKnown: true, sysAdmin: true, debian: true}, 4),
			want:    []string{"keploy holds CAP_SYS_ADMIN, which no kernel.perf_event_paranoid level refuses (kernel.perf_event_paranoid is 4), so " + policy},
			notWant: []string{lower},
		},
		{
			name: "capabilities unreadable, at 3, on Debian", err: eacces, env: at(perfEventEnv{eacces: true, debian: true}, 3),
			want: []string{"kernel.perf_event_paranoid is 3 and keploy could not read its capabilities: " + restriction, lower},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := explainPerfEventRefusal(tc.err, tc.env)
			for _, w := range tc.want {
				if !strings.Contains(got.Error(), w) {
					t.Errorf("lacks %q: %q", w, got)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(got.Error(), w) {
					t.Errorf("has %q, which does not apply here: %q", w, got)
				}
			}
			if !errors.Is(got, tc.err) || !strings.HasSuffix(got.Error(), ": "+tc.err.Error()) {
				t.Errorf("the cause was replaced or moved, not wrapped: %q", got)
			}
			if code := utils.ExitCodeFor(privilegeFailure(got)); code != utils.ExitPrivilegeRequired {
				t.Errorf("exits %d, want %d (ExitPrivilegeRequired)", code, utils.ExitPrivilegeRequired)
			}
		})
	}
}

// Only the perf_event_open step is diagnosed. link.Tracepoint can be refused
// at its other steps, and a missing tracefs is the environment's
// (tracepointFailure): each comes back exactly as it went in.
func TestDiagnosePerfEventRefusalLeavesOtherErrorsAlone(t *testing.T) {
	for _, err := range []error{
		fmt.Errorf("reading trace event ID of syscalls/sys_enter_socket: %w", unix.EACCES),
		fmt.Errorf("setting perf event bpf program: %w", unix.EACCES),
		fmt.Errorf("cannot create bpf perf link: %v", unix.EPERM),
		fmt.Errorf("opening tracepoint perf event: %w", unix.ENOENT),
		errors.New("neither debugfs nor tracefs are mounted"),
	} {
		if got := diagnosePerfEventRefusal(err); got != err {
			t.Errorf("diagnosePerfEventRefusal(%q) = %q, want it untouched", err, got)
		}
	}
}

// diagnosePerfEventRefusal reads the sysctl, the kernel release, the agent's
// capabilities and the errno itself, and each decides the diagnosis.
func TestDiagnosePerfEventRefusalReadsTheHostAndTheAgent(t *testing.T) {
	origParanoid, origVersion, origCaps := perfEventParanoidPath, procVersionPath, effectiveCapabilities
	t.Cleanup(func() {
		perfEventParanoidPath, procVersionPath, effectiveCapabilities = origParanoid, origVersion, origCaps
	})
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	caps := func(mask uint64, err error) func() (uint64, error) {
		return func() (uint64, error) { return mask, err }
	}
	const (
		sidecar  = uint64(1)<<utils.CapPerfmon | uint64(1)<<utils.CapBPF | uint64(1)<<utils.CapNetAdmin
		sysAdmin = sidecar | uint64(1)<<utils.CapSysAdmin
		lower    = "set kernel.perf_event_paranoid=2 on the host"
		policy   = "a security policy refused perf_event_open"
	)
	eacces := fmt.Errorf("opening tracepoint perf event: %w", unix.EACCES)
	eperm := fmt.Errorf("opening tracepoint perf event: %w", unix.EPERM)
	debian := write("version-debian", "Linux version 6.12.111+deb13-cloud-amd64 (debian-kernel@lists.debian.org) (x86_64-linux-gnu-gcc-14 (Debian 14.2.0-19) 14.2.0, GNU ld (GNU Binutils for Debian) 2.44) #1 SMP PREEMPT_DYNAMIC Debian 6.12.111-1 (2026-09-28)\n")
	proxmox := write("version-proxmox", "Linux version 6.8.12-4-pve (build@proxmox) (gcc (Debian 12.2.0-14) 12.2.0, GNU ld (GNU Binutils for Debian) 2.40) #1 SMP PREEMPT_DYNAMIC PMX 6.8.12-4 (2024-11-06T15:04Z)\n")
	fedora := write("version-fedora", "Linux version 7.2.7-200.fc44.x86_64 (mockbuild@4bc3738664d84c798c5efe0695a77dbf) (gcc (GCC) 16.2.1 20260819 (Red Hat 16.2.1-2), GNU ld version 2.46.1-1.fc44) #1 SMP PREEMPT_DYNAMIC Mon Sep 21 18:25:57 UTC 2026\n")

	for _, tc := range []struct {
		name     string
		paranoid string
		version  string
		mask     func() (uint64, error)
		err      error
		want     []string
		notWant  []string
	}{
		{"the sidecar at 3 (pipeline 10867)", "3\n", debian, caps(sidecar, nil), eacces,
			[]string{"kernel.perf_event_paranoid is 3 (kernel 6.12.111+deb13-cloud-amd64, Debian's) and keploy holds CAP_PERFMON but not CAP_SYS_ADMIN: a Debian kernel refuses", lower}, []string{policy}},
		{"the sidecar at 3 on Fedora", "3\n", fedora, caps(sidecar, nil), eacces,
			[]string{"kernel.perf_event_paranoid is 3 (kernel 7.2.7-200.fc44.x86_64) and keploy holds CAP_PERFMON but not CAP_SYS_ADMIN: if this kernel restricts", lower, policy}, []string{"Debian's)"}},
		// Built with Debian's gcc, so its /proc/version says "Debian", but not
		// Debian's kernel: the restriction is an if.
		{"the sidecar at 4 on a Proxmox kernel", "4\n", proxmox, caps(sidecar, nil), eacces,
			[]string{"kernel.perf_event_paranoid is 4 (kernel 6.8.12-4-pve) and keploy holds CAP_PERFMON but not CAP_SYS_ADMIN: if this kernel restricts", lower, policy}, []string{"Debian's)"}},
		{"CAP_SYS_ADMIN at 3", "3\n", debian, caps(sysAdmin, nil), eacces,
			[]string{"keploy holds CAP_SYS_ADMIN", policy}, []string{lower}},
		{"the sidecar at 3, refused with EPERM", "3\n", debian, caps(sidecar, nil), eperm,
			[]string{"the kernel answered EPERM", policy}, []string{lower}},
		{"the sidecar at 2", "2\n", debian, caps(sidecar, nil), eacces,
			[]string{"kernel.perf_event_paranoid is 2", policy}, []string{lower}},
		{"capabilities unreadable, at 3", "3\n", debian, caps(0, errors.New("no CapEff")), eacces,
			[]string{"keploy could not read its capabilities", lower}, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			perfEventParanoidPath = write("paranoid", tc.paranoid)
			procVersionPath = tc.version
			effectiveCapabilities = tc.mask
			got := diagnosePerfEventRefusal(tc.err)
			for _, w := range tc.want {
				if !strings.Contains(got.Error(), w) {
					t.Errorf("lacks %q: %q", w, got)
				}
			}
			for _, w := range tc.notWant {
				if strings.Contains(got.Error(), w) {
					t.Errorf("has %q, which does not apply here: %q", w, got)
				}
			}
			if !errors.Is(got, tc.err) {
				t.Errorf("lost the cause: %q", got)
			}
		})
	}

	t.Run("nothing readable", func(t *testing.T) {
		perfEventParanoidPath = filepath.Join(dir, "missing")
		procVersionPath = filepath.Join(dir, "missing")
		effectiveCapabilities = caps(sidecar, nil)
		got := diagnosePerfEventRefusal(eacces).Error()
		if !strings.Contains(got, "kernel.perf_event_paranoid could not be read and keploy holds CAP_PERFMON") || strings.Contains(got, "(kernel ") {
			t.Errorf("unreadable files must read as unknown: %q", got)
		}
	})
}
