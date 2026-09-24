//go:build linux

package linux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
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
// in the log keploy asks for, while a load refused for want of privileges is
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

// tracefsMounted reports whether tracefs can be reached where the tracepoint
// API is looked up: the paths, and filesystem types, cilium/ebpf's own lookup
// accepts (internal/tracefs getTracefsPath). That lookup reports a missing
// mount only as a plain string, so this is how that cause is told apart from
// every other reason an attach can fail.
func tracefsMounted() bool {
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
