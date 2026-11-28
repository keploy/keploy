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
// Used to identify orphaned legacy raw-attached programs from crashed/killed
// previous runs.
var keployBPFProgNames = map[string]ebpf.AttachType{
	"k_connect4":     ebpf.AttachCGroupInet4Connect,
	"k_connect6":     ebpf.AttachCGroupInet6Connect,
	"k_bind4":        ebpf.AttachCGroupInet4Bind,
	"k_bind6":        ebpf.AttachCGroupInet6Bind,
	"k_post_bind4":   ebpf.AttachCGroupInet4PostBind,
	"k_post_bind6":   ebpf.AttachCGroupInet6PostBind,
	"k_getpeername4": ebpf.AttachCgroupInet4GetPeername,
	"k_getpeername6": ebpf.AttachCgroupInet6GetPeername,
	"k_sockops":      ebpf.AttachCGroupSockOps,
	"k_sock_create":  ebpf.AttachCGroupInetSockCreate,
	"k_udp4_sendmsg": ebpf.AttachCGroupUDP4Sendmsg,
	"k_udp6_sendmsg": ebpf.AttachCGroupUDP6Sendmsg,
	"k_udp4_recvmsg": ebpf.AttachCGroupUDP4Recvmsg,
	"k_udp6_recvmsg": ebpf.AttachCGroupUDP6Recvmsg,
	// Attached only under the proven switch (udpwatch.go).
	"k_udp_bind4":        ebpf.AttachCGroupInet4Bind,
	"k_udp_bind6":        ebpf.AttachCGroupInet6Bind,
	"k_udp_ingress":      ebpf.AttachCGroupInetIngress,
	"k_udp_egress":       ebpf.AttachCGroupInetEgress,
	"k_udp_sock_release": ebpf.AttachCgroupInetSockRelease,
	// Attached only under the proven switch (readywatch.go).
	"k_ready_bind4":    ebpf.AttachCGroupInet4Bind,
	"k_ready_bind6":    ebpf.AttachCGroupInet6Bind,
	"k_ready_connect4": ebpf.AttachCGroupInet4Connect,
	"k_ready_connect6": ebpf.AttachCGroupInet6Connect,
	// Attached in every mode (hooks.go).
	"k_app_conn_release": ebpf.AttachCgroupInetSockRelease,
}

// kernelProgName is the name the kernel keeps for a program loaded as name:
// at most BPF_OBJ_NAME_LEN-1 (15) bytes, the rest cut (k_udp_sock_release is
// k_udp_sock_rele there, and k_ready_connect4 and 6 are both k_ready_connect,
// told apart by their attach types).
func kernelProgName(name string) string {
	if len(name) > 15 {
		return name[:15]
	}
	return name
}

// attachedCgroupProg pairs a cgroup-attached BPF program ID with whether
// BPF_PROG_QUERY reported an owning bpf_link for it.
type attachedCgroupProg struct {
	ID      uint32
	HasLink bool
}

// cleanupOps isolates kernel/OS probes so unit tests can exercise
// cross-PID-namespace flock gating and bpf_link filtering deterministically
// without requiring CAP_BPF or root.
type cleanupOps struct {
	hasOtherProc      func(selfPID int) bool
	queryPrograms     func(cgroupFD uintptr, attachType ebpf.AttachType) ([]attachedCgroupProg, bool, error)
	liveCgroupLinkIDs func() (map[uint32]struct{}, error)
	lookupProgName    func(progID uint32) (string, error)
	detachRawProgram  func(cgroupFD uintptr, progID uint32, attachType ebpf.AttachType) error
}

func defaultCleanupOps() cleanupOps {
	return cleanupOps{
		hasOtherProc:      HasOtherKeployProcesses,
		queryPrograms:     queryAttachedPrograms,
		liveCgroupLinkIDs: queryLiveCgroupLinkProgIDs,
		lookupProgName:    lookupBPFProgramName,
		detachRawProgram:  detachProgramFromCgroup,
	}
}

func hasOtherKeployInstances(lockDir, selfID string, hasOtherProc func(int) bool) bool {
	if hasOtherLiveInstanceLocks(lockDir, selfID) {
		return true
	}
	if hasOtherProc != nil && hasOtherProc(os.Getpid()) {
		return true
	}
	return false
}

// DetectAndCleanOrphanedCgroupPrograms detaches legacy raw-attached
// (non-bpf_link) orphaned Keploy cgroup BPF programs left behind by a crashed
// prior run.
//
// Two invariants prevent cross-container interference:
//  1. Cross-PID-namespace liveness is checked via kernel flock(2) instance
//     locks on host-shared bpffs (in addition to /proc for legacy binaries), so
//     a sibling running in an isolated container PID namespace is never
//     misclassified as dead.
//  2. Programs attached via a kernel bpf_link are NEVER passed to
//     RawDetachProgram: unpinned bpf_links are auto-destroyed by the kernel
//     when their owning process exits (so an active unpinned bpf_link cannot be
//     an orphan), and RawDetachProgram always returns ENOENT on bpf_link
//     attachments. Because the Linux kernel does not report link IDs in
//     cgroup BPF_PROG_QUERY (HaveLinkInfo() is false for cgroups), live cgroup
//     bpf_links are enumerated via link.Iterator.
//
// Callers that already hold an instanceLock should call
// detectAndCleanOrphanedCgroupProgramsForInstance with their own lock ID so
// they do not observe their own lock as a live sibling.
func DetectAndCleanOrphanedCgroupPrograms(logger *zap.Logger, cgroupPath string) int {
	return detectAndCleanOrphanedCgroupProgramsForInstance(logger, cgroupPath, defaultInstanceLockDir(), "")
}

func detectAndCleanOrphanedCgroupProgramsForInstance(logger *zap.Logger, cgroupPath, lockDir, selfID string) int {
	return detectAndCleanOrphanedCgroupProgramsWithOps(logger, cgroupPath, lockDir, selfID, defaultCleanupOps())
}

func detectAndCleanOrphanedCgroupProgramsWithOps(logger *zap.Logger, cgroupPath, lockDir, selfID string, ops cleanupOps) int {
	if logger == nil {
		logger = zap.NewNop()
	}
	if hasOtherKeployInstances(lockDir, selfID, ops.hasOtherProc) {
		logger.Debug("Skipping orphaned cgroup program cleanup — another keploy instance is alive",
			zap.Int("selfPID", os.Getpid()),
			zap.String("selfInstanceID", selfID))
		return 0
	}

	cgroupFD, err := os.Open(cgroupPath)
	if err != nil {
		logger.Debug("Cannot open cgroup for orphan detection",
			zap.String("path", cgroupPath), zap.Error(err))
		return 0
	}
	defer cgroupFD.Close()

	var (
		linkIDsLoaded bool
		liveLinkProgs map[uint32]struct{}
	)
	loadLiveLinks := func() map[uint32]struct{} {
		if linkIDsLoaded {
			return liveLinkProgs
		}
		linkIDsLoaded = true
		if ops.liveCgroupLinkIDs != nil {
			m, lerr := ops.liveCgroupLinkIDs()
			if len(m) > 0 {
				liveLinkProgs = m
			}
			if lerr != nil {
				logger.Debug("Cannot enumerate live cgroup bpf_links during orphan cleanup", zap.Error(lerr))
			}
		}
		return liveLinkProgs
	}

	cleaned := 0
	for progName, attachType := range keployBPFProgNames {
		n := detachOrphanedByNameWithOps(logger, cgroupFD.Fd(), cgroupPath, progName, attachType, ops, loadLiveLinks)
		cleaned += n
	}

	if cleaned > 0 {
		logger.Info("Cleaned orphaned BPF cgroup programs from previous crashed run",
			zap.Int("count", cleaned))
	}

	return cleaned
}

// orphanNamed says whether a program the kernel names name (cut to the 15
// bytes it keeps) is keploy's progName.
func orphanNamed(name, progName string) bool {
	return name == kernelProgName(progName)
}

func detachOrphanedByNameWithOps(
	logger *zap.Logger,
	cgroupFD uintptr,
	cgroupPath string,
	progName string,
	attachType ebpf.AttachType,
	ops cleanupOps,
	loadLiveLinks func() map[uint32]struct{},
) int {
	progs, haveLinkInfo, err := ops.queryPrograms(cgroupFD, attachType)
	if err != nil {
		logger.Debug("cannot query cgroup-attached BPF programs for orphan cleanup; the helper will become a no-op for this attach type — verify kernel support for BPF_PROG_QUERY on this attach type, ensure CAP_SYS_ADMIN/CAP_BPF are granted, check the cgroup v2 mount, and use bpftool to clear stale attachments manually if the subsequent attach fails with 'program already attached'",
			zap.String("cgroupPath", cgroupPath),
			zap.String("attachType", attachType.String()),
			zap.Error(err))
		return 0
	}

	cleaned := 0
	for _, ap := range progs {
		// Programs attached via a kernel bpf_link cannot be detached with
		// RawDetachProgram (kernel returns ENOENT) and unpinned bpf_links are
		// auto-destroyed on process exit.
		if ap.HasLink {
			logger.Debug("Skipping cgroup BPF program owned by active bpf_link",
				zap.Uint32("progID", ap.ID),
				zap.String("attachType", attachType.String()))
			continue
		}

		name, err := ops.lookupProgName(ap.ID)
		if err != nil {
			continue
		}
		if !orphanNamed(name, progName) {
			continue
		}

		// Cgroup BPF_PROG_QUERY does not populate link IDs (HaveLinkInfo() is
		// false for cgroups; link info is populated for tcx-style attachments),
		// so enumerate live cgroup bpf_links via link.Iterator before calling
		// RawDetachProgram.
		if !haveLinkInfo && loadLiveLinks != nil {
			if live := loadLiveLinks(); live != nil {
				if _, ownedByLink := live[ap.ID]; ownedByLink {
					logger.Debug("Skipping cgroup BPF program owned by active bpf_link",
						zap.String("program", name),
						zap.Uint32("progID", ap.ID),
						zap.String("attachType", attachType.String()))
					continue
				}
			}
		}

		logger.Info("Detaching orphaned BPF program from cgroup",
			zap.String("program", name),
			zap.Uint32("progID", ap.ID),
			zap.String("attachType", attachType.String()))

		if err := ops.detachRawProgram(cgroupFD, ap.ID, attachType); err != nil {
			logger.Debug("Failed to detach orphaned BPF program; re-run with elevated privileges or check cgroup permissions and retry, otherwise remove the stale attachment manually with bpftool",
				zap.String("program", name),
				zap.Uint32("progID", ap.ID),
				zap.String("cgroupPath", cgroupPath),
				zap.String("attachType", attachType.String()),
				zap.Error(err))
			continue
		}
		cleaned++
	}

	return cleaned
}

func lookupBPFProgramName(progID uint32) (string, error) {
	prog, err := ebpf.NewProgramFromID(ebpf.ProgramID(progID))
	if err != nil {
		return "", err
	}
	defer func() { _ = prog.Close() }()

	info, err := prog.Info()
	if err != nil {
		return "", err
	}
	return info.Name, nil
}

// queryAttachedPrograms queries the cgroup for attached BPF programs of the
// given attach type, preserving per-program bpf_link ownership information
// whenever the kernel supplies it.
func queryAttachedPrograms(cgroupFD uintptr, attachType ebpf.AttachType) ([]attachedCgroupProg, bool, error) {
	qr, err := link.QueryPrograms(link.QueryOptions{
		Target: int(cgroupFD),
		Attach: attachType,
	})
	if err != nil {
		return nil, false, err
	}

	haveLinks := qr.HaveLinkInfo()
	result := make([]attachedCgroupProg, 0, len(qr.Programs))
	for i := range qr.Programs {
		p := &qr.Programs[i]
		_, hasLink := p.LinkID()
		result = append(result, attachedCgroupProg{
			ID:      uint32(p.ID),
			HasLink: hasLink,
		})
	}
	return result, haveLinks, nil
}

// queryLiveCgroupLinkProgIDs enumerates all active kernel cgroup bpf_links via
// cilium/ebpf's link.Iterator and returns the set of program IDs attached
// through them.
func queryLiveCgroupLinkProgIDs() (map[uint32]struct{}, error) {
	progIDs := make(map[uint32]struct{})
	var it link.Iterator
	defer it.Close()
	for it.Next() {
		info, err := it.Link.Info()
		if err != nil || info == nil {
			continue
		}
		if info.Cgroup() != nil {
			progIDs[uint32(info.Program)] = struct{}{}
		}
	}
	if err := it.Err(); err != nil {
		return progIDs, err
	}
	return progIDs, nil
}

// detachProgramFromCgroup detaches a specific legacy raw-attached program from
// a cgroup using BPF_PROG_DETACH.
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

// CleanOrphanedBPFPins removes stale legacy top-level pinned BPF objects from
// /sys/fs/bpf/ only when no other live Keploy instance exists (checked via
// flock on shared bpffs across PID namespaces as well as /proc). Call before
// acquiring an instance lock.
func CleanOrphanedBPFPins(logger *zap.Logger) {
	cleanOrphanedBPFPins(logger, defaultInstanceLockDir(), "", HasOtherKeployProcesses)
}

func cleanOrphanedBPFPins(logger *zap.Logger, lockDir, selfID string, hasOtherProc func(int) bool) {
	if logger == nil {
		logger = zap.NewNop()
	}
	if hasOtherKeployInstances(lockDir, selfID, hasOtherProc) {
		logger.Debug("Skipping legacy BPF pin cleanup because another keploy instance is still running")
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
				// pin (the liveness guard above should prevent that,
				// but belt-and-suspenders).
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
		if isKeployProcess(e.Name()) {
			return true
		}
	}
	return false
}

func isKeployProcess(pidStr string) bool {
	exePath, err := os.Readlink(filepath.Join("/proc", pidStr, "exe"))
	if err != nil {
		return false
	}
	exePath = strings.TrimSuffix(exePath, " (deleted)")
	exeBase := strings.ToLower(filepath.Base(exePath))
	return strings.Contains(exeBase, "keploy")
}
