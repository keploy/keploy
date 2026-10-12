//go:build linux

package linux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/link"
	"github.com/cilium/ebpf/rlimit"
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
		{"one directory of a debugfs, with tracing", root + "37 22 0:7 /sub " + withTracing + " rw - debugfs debugfs rw\n", false, true},
		{"debugfs with tracing", root + "36 22 0:7 / " + withTracing + " rw shared:16 - debugfs debugfs rw\n", true, true},
		{"debugfs with tracing, at a path with a space", root + "36 22 0:7 / " + strings.ReplaceAll(withSpace, " ", `\040`) + " rw - debugfs debugfs rw\n", true, true},
		{"debugfs without tracing", root + "36 22 0:7 / " + without + " rw - debugfs debugfs rw\n", false, true},
		{"neither", root, false, true},
		{"a line without the dash", root + "35 22 0:12 / /sys/kernel/tracing rw tracefs\n", false, false},
		{"too few fields before the dash", root + "35 22 / /sys/kernel/tracing - tracefs tracefs rw\n", false, false},
		{"nothing after the dash", root + "35 22 0:12 / /sys/kernel/tracing rw - \n", false, false},
	} {
		if found, ok := tracefsInMountinfo(c.table); found != c.found || ok != c.ok {
			t.Errorf("%s: found %v, ok %v; want %v, %v", c.name, found, ok, c.found, c.ok)
		}
	}
}

// The environment a child of TestTracefsMountedAgreesWithCiliumInEveryLayout
// reads: the layout to build, the answer cilium/ebpf must give in it, the
// parent's mount namespace (which the child must not be in), and, once the
// layout is built, the directory it is in.
const (
	tracefsLayoutEnv      = "KEPLOY_TEST_TRACEFS_LAYOUT"
	tracefsWantMissingEnv = "KEPLOY_TEST_TRACEFS_WANT_MISSING"
	tracefsParentNSEnv    = "KEPLOY_TEST_TRACEFS_PARENT_MNTNS"
	tracefsLayoutDirEnv   = "KEPLOY_TEST_TRACEFS_LAYOUT_DIR"
)

// tracefsMounted must give cilium/ebpf's own answer in each way tracefs can
// be mounted, not only in the host's: none at all; at the canonical path; only
// at another path (a container's bind mount); only one of its directories;
// and only a debugfs, whose tracing directory is tracefs. In each, attaching
// a tracepoint to an event no kernel has fails with "neither debugfs nor
// tracefs are mounted" exactly where cilium/ebpf's lookup finds no tracefs.
//
// Each layout is built by a child of this test binary in a mount namespace of
// its own, so the host's mounts are not touched. cilium/ebpf reads mountinfo
// once per process, in its package init (rlimit's probe), so the child then
// re-executes itself, in that namespace, to check the layout in a process
// that has seen nothing else. Needs root.
func TestTracefsMountedAgreesWithCiliumInEveryLayout(t *testing.T) {
	if layout := os.Getenv(tracefsLayoutEnv); layout != "" {
		if dir := os.Getenv(tracefsLayoutDirEnv); dir != "" {
			checkTracefsLayout(t, layout, dir)
		} else {
			buildTracefsLayout(t, layout)
		}
		return
	}
	if os.Geteuid() != 0 {
		t.Skip("building mount layouts needs root")
	}
	ns, err := os.Readlink("/proc/self/ns/mnt")
	if err != nil {
		t.Skipf("no mount namespace to leave: %v", err)
	}
	for _, c := range []struct {
		layout  string
		missing bool
	}{
		{"none", true},
		{"canonical", false},
		{"elsewhere", false},
		{"subdir", true},
		{"debugfs", false},
	} {
		t.Run(c.layout, func(t *testing.T) {
			cmd := exec.Command(os.Args[0], "-test.run=^TestTracefsMountedAgreesWithCiliumInEveryLayout$", "-test.v", "-test.count=1")
			cmd.Env = append(os.Environ(), tracefsLayoutEnv+"="+c.layout,
				fmt.Sprintf("%s=%v", tracefsWantMissingEnv, c.missing), tracefsParentNSEnv+"="+ns)
			// Go marks / MS_REC|MS_PRIVATE in a child it unshares CLONE_NEWNS
			// for, so nothing the child mounts or unmounts reaches this one.
			cmd.SysProcAttr = &syscall.SysProcAttr{Unshareflags: syscall.CLONE_NEWNS}
			out, err := cmd.CombinedOutput()
			switch {
			case strings.Contains(string(out), "--- SKIP: TestTracefsMountedAgreesWithCiliumInEveryLayout"):
				t.Skipf("the %s layout cannot be built here:\n%s", c.layout, out)
			case err != nil || !strings.Contains(string(out), "--- PASS: TestTracefsMountedAgreesWithCiliumInEveryLayout"):
				t.Fatalf("the %s layout: %v\n%s", c.layout, err, out)
			}
		})
	}
}

// tracefsMountPoints are the mount points of every tracefs and debugfs mount
// in mountinfo whose mount point is under prefix, deepest first.
func tracefsMountPoints(t *testing.T, prefix string) []string {
	t.Helper()
	table, err := os.ReadFile("/proc/self/mountinfo")
	if err != nil {
		t.Fatal(err)
	}
	var points []string
	for _, line := range strings.Split(string(table), "\n") {
		before, after, ok := strings.Cut(line, " - ")
		first, second := strings.Fields(before), strings.Fields(after)
		if !ok || len(first) < 5 || len(second) == 0 || second[0] != "tracefs" && second[0] != "debugfs" {
			continue
		}
		if p := mountinfoUnescape.Replace(first[4]); strings.HasPrefix(p, prefix) {
			points = append(points, p)
		}
	}
	sort.Slice(points, func(i, j int) bool { return len(points[i]) > len(points[j]) })
	return points
}

// buildTracefsLayout, in the child's own mount namespace, detaches every
// tracefs and debugfs mount, builds layout, and re-executes the test binary
// there to check it.
func buildTracefsLayout(t *testing.T, layout string) {
	// The guard against touching the host's mounts.
	if ns, err := os.Readlink("/proc/self/ns/mnt"); err != nil || ns == os.Getenv(tracefsParentNSEnv) {
		t.Fatalf("not in a mount namespace of its own (%s, %v): refusing to unmount anything", ns, err)
	}
	for _, p := range tracefsMountPoints(t, "/") {
		if err := unix.Unmount(p, unix.MNT_DETACH); err != nil && !errors.Is(err, unix.EINVAL) {
			t.Fatalf("unmount %s: %v", p, err)
		}
	}
	// Never removed recursively (checkTracefsLayout): tracefs is the host's
	// superblock, and removing its instances would remove the host's.
	dir, err := os.MkdirTemp("", "keploy-tracefs-layout-")
	if err != nil {
		t.Fatal(err)
	}
	mount := func(src, target, fstype string, flags uintptr) {
		if err := os.MkdirAll(target, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := unix.Mount(src, target, fstype, flags, ""); err != nil {
			_ = os.Remove(target)
			_ = os.Remove(dir)
			t.Skipf("mount %s at %s: %v", fstype, target, err)
		}
	}
	switch layout {
	case "none":
	case "canonical":
		mount("tracefs", "/sys/kernel/tracing", "tracefs", 0)
	case "elsewhere":
		mount("tracefs", filepath.Join(dir, "tracefs"), "tracefs", 0)
	case "subdir":
		mount("tracefs", filepath.Join(dir, "tracefs"), "tracefs", 0)
		mount(filepath.Join(dir, "tracefs", "events"), filepath.Join(dir, "events"), "", unix.MS_BIND)
		if err := unix.Unmount(filepath.Join(dir, "tracefs"), unix.MNT_DETACH); err != nil {
			t.Fatal(err)
		}
	case "debugfs":
		mount("debugfs", filepath.Join(dir, "debugfs"), "debugfs", 0)
		if st, err := os.Stat(filepath.Join(dir, "debugfs", "tracing")); err != nil || !st.IsDir() {
			t.Skipf("this kernel's debugfs has no tracing directory: %v", err)
		}
	default:
		t.Fatalf("unknown layout %q", layout)
	}
	env := append(os.Environ(), tracefsLayoutDirEnv+"="+dir)
	err = syscall.Exec(os.Args[0], os.Args, env)
	t.Fatalf("re-execute %s to check the %s layout: %v", os.Args[0], layout, err)
}

// checkTracefsLayout, in a fresh image of the test binary in the layout's
// mount namespace, holds cilium/ebpf's lookup to the layout's answer and
// tracefsMounted to cilium/ebpf's, then takes the layout down.
func checkTracefsLayout(t *testing.T, layout, dir string) {
	defer func() {
		for _, p := range tracefsMountPoints(t, dir) {
			_ = unix.Unmount(p, unix.MNT_DETACH)
		}
		for _, d := range []string{"tracefs", "events", "debugfs"} {
			_ = os.Remove(filepath.Join(dir, d))
		}
		_ = os.Remove(dir)
	}()
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatalf("remove the memlock limit: %v", err)
	}
	prog, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Type: ebpf.TracePoint, License: "GPL",
		Instructions: asm.Instructions{asm.Mov.Imm(asm.R0, 0), asm.Return()},
	})
	if err != nil {
		t.Fatalf("load a tracepoint program: %v", err)
	}
	defer prog.Close()
	l, err := link.Tracepoint("syscalls", "keploy_no_such_event", prog, nil)
	if err == nil {
		_ = l.Close()
		t.Fatal("a tracepoint attached to an event that does not exist")
	}
	missing := strings.Contains(err.Error(), "neither debugfs nor tracefs are mounted")
	if want := os.Getenv(tracefsWantMissingEnv) == "true"; missing != want {
		t.Fatalf("the %s layout: cilium/ebpf's lookup says %v; want tracefs missing %v", layout, err, want)
	}
	if mounted := tracefsMounted(); mounted == missing {
		t.Fatalf("the %s layout: tracefsMounted() = %v, and cilium/ebpf's lookup says: %v", layout, mounted, err)
	}
}
