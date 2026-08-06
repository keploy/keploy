//go:build linux

package linux

import (
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"

	"github.com/cilium/ebpf"
)

// perfEventParanoidPath is the sysctl that governs who may call
// perf_event_open. Overridable so the hint can be unit-tested.
var perfEventParanoidPath = "/proc/sys/kernel/perf_event_paranoid"

// PerfEventPermissionHint explains a permission failure from an eBPF attach,
// or returns "" when err is not a permission error.
//
// Attaching a tracepoint/kprobe/uprobe goes through perf_event_open, which the
// kernel gates on kernel.perf_event_paranoid. Debian/Ubuntu kernels carry an
// out-of-tree LEVEL 3 (mainline only defines 0..2) whose check is
// `perf_paranoid_any() && !capable(CAP_SYS_ADMIN)` — so CAP_PERFMON does NOT
// satisfy it. An agent running with the usual eBPF capability set
// (CAP_BPF + CAP_PERFMON, no CAP_SYS_ADMIN) therefore fails with a bare
// "permission denied" on those distros, however new the kernel is.
//
// That is genuinely hard to diagnose from the outside: the same binary, image
// and kernel work on a node with paranoid <= 2 and fail on one with 3, and the
// syscall error says nothing about the sysctl. Naming the sysctl, its value and
// the capability turns it into a one-line fix.
//
// The paranoid level is NOT namespaced: a container inherits its node's value,
// so reading it from inside the agent reports the node's setting.
//
// Exported so the enterprise attach sites (kretprobes/uprobes, which use the
// same syscall) can report the same diagnosis.
func PerfEventPermissionHint(err error) string {
	if err == nil || !errors.Is(err, os.ErrPermission) {
		return ""
	}
	// A rejected eBPF program is NOT a permission problem, but it looks like
	// one: the kernel answers BPF_PROG_LOAD with EACCES when the verifier
	// rejects the program, and VerifierError.Unwrap() exposes that errno — so
	// errors.Is(_, os.ErrPermission) is true for every verifier failure. Left
	// unchecked this tells an operator staring at a verifier log to grant
	// CAP_SYS_ADMIN and change a node sysctl, neither of which can help.
	var ve *ebpf.VerifierError
	if errors.As(err, &ve) {
		return ""
	}
	// Not every permission error on this path is the perf_event_open gate:
	// attaching a tracepoint first READS the event id out of tracefs, and an
	// EACCES there arrives as an *os.PathError. Blaming the sysctl for that
	// would send the reader down entirely the wrong path, so answer the
	// question actually asked.
	var pathErr *os.PathError
	if errors.As(err, &pathErr) {
		// Only prescribe a debugfs mount when the unreadable path actually IS
		// tracefs. Uprobe attaches open the TARGET BINARY through this same
		// helper, and telling an operator to mount /sys/kernel/debug when the
		// agent simply cannot read /app/bin/server is a wrong answer stated
		// confidently.
		if strings.HasPrefix(pathErr.Path, "/sys/kernel/debug") || strings.HasPrefix(pathErr.Path, "/sys/kernel/tracing") {
			return fmt.Sprintf("cannot read %s — the agent needs tracefs/debugfs access; "+
				"mount /sys/kernel/debug (and /sys/kernel/tracing) into the agent container", pathErr.Path)
		}
		return fmt.Sprintf("cannot read %s — the agent needs read access to that path", pathErr.Path)
	}
	level, readErr := perfEventParanoidLevel()
	if readErr == nil && level > 2 {
		return fmt.Sprintf("kernel.perf_event_paranoid is %d on this node, which denies perf_event_open to any process without CAP_SYS_ADMIN "+
			"(CAP_PERFMON is NOT sufficient at this level) — grant the keploy agent CAP_SYS_ADMIN, or set kernel.perf_event_paranoid<=2 on the node", level)
	}
	// Either the sysctl is unreadable or it is permissive, so paranoid is not
	// the explanation. Fall back to the capability requirement itself.
	return "the keploy agent needs CAP_BPF and CAP_PERFMON (kernel >= 5.8), or CAP_SYS_ADMIN, to attach eBPF probes"
}

// perfEventParanoidLevel reads the node's kernel.perf_event_paranoid value.
func perfEventParanoidLevel() (int, error) {
	raw, err := os.ReadFile(perfEventParanoidPath)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(raw)))
}
