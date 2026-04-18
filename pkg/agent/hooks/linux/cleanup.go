//go:build linux

package linux

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"go.uber.org/zap"
)

// ExtraBPFPinPatterns lets callers extend CleanOrphanedBPFPins with
// additional glob patterns (for example, a downstream build that pins
// extra BPF maps under /sys/fs/bpf). Set before the first call.
var ExtraBPFPinPatterns []string

// keployBPFProgNames lists the BPF program names that keploy attaches to cgroups.
// Used to identify orphaned programs from crashed/killed previous runs.
var keployBPFProgNames = map[string]ebpf.AttachType{
	"k_connect4":     ebpf.AttachCGroupInet4Connect,
	"k_connect6":     ebpf.AttachCGroupInet6Connect,
	"k_bind4":        ebpf.AttachCGroupInet4Bind,
	"k_bind6":        ebpf.AttachCGroupInet6Bind,
	"k_getpeername4": ebpf.AttachCgroupInet4GetPeername,
	"k_getpeername6": ebpf.AttachCgroupInet6GetPeername,
	"k_sockops":      ebpf.AttachCGroupSockOps,
	"k_udp4_sendmsg": ebpf.AttachCGroupUDP4Sendmsg,
	"k_udp6_sendmsg": ebpf.AttachCGroupUDP6Sendmsg,
}

// DetectAndCleanOrphanedCgroupPrograms finds BPF programs attached to the
// root cgroup that match keploy's known program names and detaches them.
//
// IMPORTANT: the kernel does not expose the creator PID of an attached
// cgroup program in a queryable field, so we cannot verify per-program
// that the owning process is dead. Instead we gate the whole cleanup on
// the absence of any other live keploy process in /proc — if any live
// sibling exists, the programs might belong to it and we must not touch
// them. The function is therefore a no-op when a second keploy instance
// is running concurrently (intentional: correctness over aggression).
//
// The primary use case is recovery from a crashed / hard-killed prior
// run, where the orphaned cgroup programs would otherwise prevent the
// next keploy startup from attaching its own programs with the same
// names (EEXIST).
func DetectAndCleanOrphanedCgroupPrograms(logger *zap.Logger, cgroupPath string) int {
	// Safety gate: only run when no other keploy process is alive.
	// Without this a second keploy instance would detach a live
	// sibling's cgroup programs, silently breaking its capture.
	if HasOtherKeployProcesses(os.Getpid()) {
		logger.Debug("Skipping orphaned cgroup program cleanup — another keploy process is alive",
			zap.Int("selfPID", os.Getpid()))
		return 0
	}

	// Open the cgroup directory once for all per-name detaches. The
	// inner loop used to open/close the cgroup per program name,
	// which meant 2 extra syscalls × len(keployBPFProgNames) on every
	// startup. Reuse the FD for every query/detach in this run.
	cgroupFD, err := os.Open(cgroupPath)
	if err != nil {
		logger.Debug("Cannot open cgroup for orphan detection",
			zap.String("path", cgroupPath), zap.Error(err))
		return 0
	}
	defer cgroupFD.Close()

	cleaned := 0
	for progName, attachType := range keployBPFProgNames {
		n := detachOrphanedByName(logger, cgroupFD, cgroupPath, progName, attachType)
		cleaned += n
	}

	if cleaned > 0 {
		logger.Info("Cleaned orphaned BPF cgroup programs from previous crashed run",
			zap.Int("count", cleaned))
	}

	return cleaned
}

// detachOrphanedByName iterates all BPF programs of the given attach type on
// the cgroup, finds ones matching the expected name, and detaches them.
// It does not (and cannot, via BPF_PROG_GET_INFO_BY_FD) verify per-program
// creator PID or liveness; the safety guarantee comes from the caller
// invoking it only after DetectAndCleanOrphanedCgroupPrograms has confirmed
// no other keploy process is alive — a name match at that point is
// definitionally an orphan from a crashed prior run.
//
// Caller owns the cgroup FD (opened once in
// DetectAndCleanOrphanedCgroupPrograms and reused across every program-name
// iteration); cgroupPath is kept around only for log context.
func detachOrphanedByName(logger *zap.Logger, cgroupFD *os.File, cgroupPath string, progName string, attachType ebpf.AttachType) int {

	// Query all programs attached to this cgroup for this attach type.
	// queryAttachedPrograms calls link.QueryPrograms (cilium/ebpf's
	// typed wrapper around the BPF_PROG_QUERY syscall), so unsupported
	// attach types or insufficient privileges are reported as errors
	// from that helper.
	progIDs, err := queryAttachedPrograms(cgroupFD.Fd(), attachType)
	if err != nil {
		// Surface the query failure at Debug. Not all attach types are
		// queryable on every kernel (EINVAL on older kernels, EPERM
		// when the process lacks CAP_SYS_ADMIN/CAP_BPF, etc.), and the
		// silent-skip path pre-this-PR made "orphan cleanup became a
		// no-op" invisible — operators would then hit the downstream
		// attach with "program already attached" and have no starting
		// point for diagnosis. Debug keeps it out of normal logs while
		// --debug runs get a trail.
		logger.Debug("cannot query cgroup-attached BPF programs for orphan cleanup; the helper will become a no-op for this attach type — verify kernel support for BPF_PROG_QUERY on this attach type, ensure CAP_SYS_ADMIN/CAP_BPF are granted, check the cgroup v2 mount, and use bpftool to clear stale attachments manually if the subsequent attach fails with 'program already attached'",
			zap.String("cgroupPath", cgroupPath),
			zap.String("attachType", attachType.String()),
			zap.Error(err))
		return 0
	}

	cleaned := 0
	for _, progID := range progIDs {
		prog, err := ebpf.NewProgramFromID(ebpf.ProgramID(progID))
		if err != nil {
			continue
		}

		info, err := prog.Info()
		if err != nil {
			prog.Close()
			continue
		}

		name := info.Name
		prog.Close()

		if name != progName {
			continue
		}

		// Name-match is the only check we can do — the BPF program info
		// exposed via BPF_PROG_GET_INFO_BY_FD does not include a creator
		// PID. The global no-other-keploy-process gate enforced by the
		// caller (DetectAndCleanOrphanedCgroupPrograms) is what makes
		// this safe: if we are the only live keploy and we find a
		// program with a keploy-reserved name attached to the cgroup,
		// it is by definition an orphan from a crashed prior run.

		logger.Info("Detaching orphaned BPF program from cgroup",
			zap.String("program", name),
			zap.Uint32("progID", progID),
			zap.String("attachType", attachType.String()))

		if err := detachProgramFromCgroup(cgroupFD.Fd(), progID, attachType); err != nil {
			logger.Debug("Failed to detach orphaned BPF program; re-run with elevated privileges or check cgroup permissions and retry, otherwise remove the stale attachment manually with bpftool",
				zap.String("program", name),
				zap.Uint32("progID", progID),
				zap.String("cgroupPath", cgroupPath),
				zap.String("attachType", attachType.String()),
				zap.Error(err))
			continue
		}
		cleaned++
	}

	return cleaned
}

// queryAttachedPrograms queries the cgroup for attached BPF programs of the
// given attach type. Returns a list of program IDs.
func queryAttachedPrograms(cgroupFD uintptr, attachType ebpf.AttachType) ([]uint32, error) {
	qr, err := link.QueryPrograms(link.QueryOptions{
		Target: int(cgroupFD),
		Attach: attachType,
	})
	if err != nil {
		return nil, err
	}

	result := make([]uint32, 0, len(qr.Programs))
	for _, p := range qr.Programs {
		result = append(result, uint32(p.ID))
	}
	return result, nil
}

// detachProgramFromCgroup detaches a specific program from a cgroup using
// BPF_PROG_DETACH. This is the legacy API that works for all program types.
func detachProgramFromCgroup(cgroupFD uintptr, progID uint32, attachType ebpf.AttachType) error {
	prog, err := ebpf.NewProgramFromID(ebpf.ProgramID(progID))
	if err != nil {
		return fmt.Errorf("open program %d: %w", progID, err)
	}
	defer prog.Close()

	return link.RawDetachProgram(link.RawDetachProgramOptions{
		Target:  int(cgroupFD),
		Program: prog,
		Attach:  attachType,
	})
}

// CleanOrphanedBPFPins removes stale pinned BPF objects from /sys/fs/bpf/.
// Only removes keploy-specific pins. Gated internally on
// HasOtherKeployProcesses so a live keploy sibling cannot have its pins
// yanked out from under it — earlier revisions delegated this check to
// the caller, which was fragile because every new call site had to
// remember to re-enforce it.
func CleanOrphanedBPFPins(logger *zap.Logger) {
	if HasOtherKeployProcesses(os.Getpid()) {
		logger.Debug("Skipping BPF pin cleanup because another keploy process is still running")
		return
	}
	patterns := []string{
		"/sys/fs/bpf/keploy_sock*",
		"/sys/fs/bpf/keploy_capture_rb",
		"/sys/fs/bpf/keploy_redirect_proxy_map",
		"/sys/fs/bpf/keploy_target_namespace_pids",
		"/sys/fs/bpf/keploy_ssl_*",
		"/sys/fs/bpf/keploy_gotls_*",
	}
	patterns = append(patterns, ExtraBPFPinPatterns...)

	for _, pattern := range patterns {
		matches, err := filepath.Glob(pattern)
		if err != nil {
			continue
		}
		for _, m := range matches {
			info, statErr := os.Stat(m)
			if statErr != nil {
				continue
			}
			var removeErr error
			if info.IsDir() {
				removeErr = os.RemoveAll(m)
			} else {
				removeErr = os.Remove(m)
			}
			if removeErr != nil {
				// Surface the failure so the operator can diagnose the
				// subsequent "program already attached" / "pin exists"
				// error that will otherwise hit on the next attach.
				// Common root causes: bpffs mounted read-only, wrong
				// keploy user, another live keploy still holding the
				// pin (the HasOtherKeployProcesses guard above should
				// prevent that but belt-and-suspenders).
				logger.Error("failed to remove stale BPF pin during orphan cleanup; verify /sys/fs/bpf is writable, this process has CAP_SYS_ADMIN/CAP_BPF, and no other keploy instance has the pin in use, then retry startup",
					zap.String("path", m),
					zap.Error(removeErr))
				continue
			}
			logger.Debug("Removed stale BPF pin", zap.String("path", m))
		}
	}
}

// HasOtherKeployProcesses reports whether any keploy process other than us
// is currently running. Exported for use by the enterprise cleanup code.
func HasOtherKeployProcesses(selfPID int) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return true // assume yes if we can't check
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		pid := 0
		fmt.Sscanf(e.Name(), "%d", &pid)
		if pid <= 1 || pid == selfPID {
			continue
		}
		exePath, err := os.Readlink(filepath.Join("/proc", e.Name(), "exe"))
		if err != nil {
			continue
		}
		exePath = strings.TrimSuffix(exePath, " (deleted)")
		exeBase := strings.ToLower(filepath.Base(exePath))
		if strings.Contains(exeBase, "keploy") {
			return true
		}
	}
	return false
}
