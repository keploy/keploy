//go:build linux

package linux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/utils"
)

func GetSelfInodeNumber() (uint64, uint64, error) {
	p := filepath.Join("/proc", "self", "ns", "pid")

	f, err := os.Stat(p)
	if err != nil {
		return 0, 0, fmt.Errorf("failed to stat %s for keploy pid namespace: %w", p, err)
	}
	st := f.Sys().(*syscall.Stat_t)
	return st.Ino, uint64(st.Dev), nil
}

// privilegeFailure tags an eBPF load the kernel refused for want of
// privileges, so the agent's exit status can say so (utils.ExitCodeFor).
// Every step of the load is a privileged kernel operation — raising the
// memlock rlimit, bpf(2), perf_event_open(2), attaching to a cgroup — so a
// permission error from any of them (EPERM, EACCES) means the same thing: run
// elevated, or grant the capabilities.
//
// Except from the verifier, which rejects a program with the same EACCES
// (an invalid context access, a misaligned read). A load is only verified
// once the kernel has allowed it, and the verifier always leaves its reasons
// in the log cilium/ebpf asks for when it retries a failed load
// (hookLoadOptions), while a load refused for want of privileges is
// refused before that log is written; cilium/ebpf tells its MEMLOCK hint
// apart the same way. A rejected program is keploy's to fix, not something
// any privilege can.
func privilegeFailure(err error) error {
	var ve *ebpf.VerifierError
	if errors.As(err, &ve) && len(ve.Log) > 0 {
		return err
	}
	if errors.Is(err, os.ErrPermission) && !errors.Is(err, utils.ErrPrivilegeRequired) {
		return fmt.Errorf("%w: %w", utils.ErrPrivilegeRequired, err)
	}
	return err
}

// tracepointFailure tags a tracepoint that could not be attached because
// tracefs is not mounted — a container started without /sys/kernel/debug or
// /sys/kernel/tracing — as the environment's failure rather than keploy's.
func tracepointFailure(err error, tracefsMounted bool) error {
	if tracefsMounted {
		return err
	}
	return fmt.Errorf("%w: %w", utils.ErrEnvironmentUnsupported, err)
}

// tracefsMounted reports whether tracefs can be reached where cilium/ebpf
// looks for it to attach a tracepoint (internal/tracefs getTracefsPath, from
// v0.22.0): a tracefs mounted whole anywhere in /proc/self/mountinfo, so a
// container's bind mount of it at another path counts, else a whole debugfs
// with a tracing directory, else the canonical paths, by filesystem type. That
// lookup reports a missing mount only as a plain string, so this is how that
// cause is told apart from every other reason an attach can fail. cilium/ebpf
// reads mountinfo once per process, at its first BPF call (its BPF token
// lookup), and works out the tracefs path, with the debugfs and canonical-path
// checks, once, at its first tracefs use; this reads all of it when asked, so
// the two differ only where the mounts changed in between.
// TestTracefsMountedAgreesWithCiliumInEveryLayout holds the two to the same
// answer in each way tracefs can be mounted.
func tracefsMounted() bool {
	if table, err := os.ReadFile("/proc/self/mountinfo"); err == nil {
		if found, ok := tracefsInMountinfo(string(table)); ok && found {
			return true
		}
	}
	for _, m := range []struct {
		path  string
		magic int64
	}{
		{"/sys/kernel/tracing", unix.TRACEFS_MAGIC},
		{"/sys/kernel/debug/tracing", unix.TRACEFS_MAGIC},
		{"/sys/kernel/debug/tracing", unix.DEBUGFS_MAGIC},
	} {
		var st unix.Statfs_t
		if unix.Statfs(m.path, &st) == nil && int64(st.Type) == m.magic {
			return true
		}
	}
	return false
}

// mountinfoUnescape undoes show_mountinfo's escapes of ' ', '\t', '\n' and
// '\\', as cilium/ebpf's internal/mountinfo does.
var mountinfoUnescape = strings.NewReplacer(`\040`, " ", `\011`, "\t", `\012`, "\n", `\134`, `\`)

// tracefsInMountinfo reports whether a mountinfo table (proc(5)) has a place
// cilium/ebpf takes for tracefs: a tracefs mount whose root is "/", or a
// debugfs one whose mount point has a tracing directory. ok is false where
// cilium/ebpf's parser refuses the table (a line without " - ", or with too
// few fields), which then falls back to the canonical paths.
func tracefsInMountinfo(table string) (found, ok bool) {
	var debugfs []string
	for _, line := range strings.Split(table, "\n") {
		if line == "" {
			continue
		}
		before, after, cut := strings.Cut(line, " - ")
		first, second := strings.Fields(before), strings.Fields(after)
		if !cut || len(second) == 0 || len(first) < 6 {
			return false, false
		}
		if mountinfoUnescape.Replace(first[3]) != "/" {
			continue
		}
		switch second[0] {
		case "tracefs":
			found = true
		case "debugfs":
			debugfs = append(debugfs, mountinfoUnescape.Replace(first[4]))
		}
	}
	for _, m := range debugfs {
		if st, err := os.Stat(filepath.Join(m, "tracing")); err == nil && st.IsDir() {
			found = true
		}
	}
	return found, true
}
