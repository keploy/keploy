//go:build linux

package linux

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

const (
	scopedCgroupPrefix        = "keploy-scoped-"
	scopedCgroupRemedyROFS    = "mount a writable cgroup v2 filesystem (e.g. docker --privileged or --cgroupns=host -v /sys/fs/cgroup:/sys/fs/cgroup:rw), or run 'go mod download' before keploy"
	scopedCgroupRemedyThread  = "run the test command in a domain (non-threaded) cgroup, or run 'go mod download' before keploy"
	scopedCgroupRemedyPerm    = "run with CAP_SYS_ADMIN or under a delegated cgroup v2 scope (e.g. systemd-run --user --scope -p Delegate=yes), or run 'go mod download' before keploy"
	scopedCgroupRemedyDefault = "ensure writable cgroup v2 delegation for the test runner, or run 'go mod download' before keploy"
)

var (
	errMalformedProcStat     = errors.New("malformed /proc stat")
	errScopedCgroupNotDomain = errors.New("scoped cgroup is not a domain cgroup")
)

// scopedCgroupRemedyFor returns a cause-specific remedy for a scoped cgroup
// setup error. Every remedy also includes 'go mod download' before keploy.
func scopedCgroupRemedyFor(err error) string {
	if err == nil {
		return ""
	}
	if errors.Is(err, unix.EROFS) {
		return scopedCgroupRemedyROFS
	}
	if errors.Is(err, errScopedCgroupNotDomain) || errors.Is(err, unix.EOPNOTSUPP) {
		return scopedCgroupRemedyThread
	}
	if errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM) {
		return scopedCgroupRemedyPerm
	}
	return scopedCgroupRemedyForReason(err.Error())
}

func scopedCgroupRemedyForReason(reason string) string {
	low := strings.ToLower(reason)
	switch {
	case strings.Contains(low, "read-only file system") || strings.Contains(low, "erofs"):
		return scopedCgroupRemedyROFS
	case strings.Contains(low, "domain invalid") || strings.Contains(low, "threaded") || strings.Contains(low, "not a domain cgroup") || strings.Contains(low, "operation not supported"):
		return scopedCgroupRemedyThread
	case strings.Contains(low, "not delegated") || strings.Contains(low, "permission denied") || strings.Contains(low, "operation not permitted"):
		return scopedCgroupRemedyPerm
	default:
		return scopedCgroupRemedyDefault
	}
}

// Test seams for unit testing without root or real cgroups.
var (
	scopedReadProcCgroup = func(pid uint32) ([]byte, error) {
		if pid == 0 {
			return os.ReadFile("/proc/self/cgroup")
		}
		return os.ReadFile("/proc/" + strconv.FormatUint(uint64(pid), 10) + "/cgroup")
	}
	scopedReadProcStat = func(tgid uint32) ([]byte, error) {
		return os.ReadFile("/proc/" + strconv.FormatUint(uint64(tgid), 10) + "/stat")
	}
	scopedProcessGone = gone
	scopedGetEUID     = os.Geteuid
	scopedRemoveWait  = 500 * time.Millisecond
	scopedRemoveStep  = 25 * time.Millisecond
)

// scopedProcStartTicks reads field 22 (start_boottime in USER_HZ=100 ticks) of
// /proc/<tgid>/stat in the caller's PID namespace.
func scopedProcStartTicks(tgid uint32) (uint64, error) {
	b, err := scopedReadProcStat(tgid)
	if err != nil {
		return 0, err
	}
	i := strings.LastIndexByte(string(b), ')')
	if i < 0 || i+2 >= len(b) {
		return 0, errMalformedProcStat
	}
	fields := strings.Fields(string(b[i+2:]))
	if len(fields) <= 19 {
		return 0, errMalformedProcStat
	}
	v, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil || v == 0 {
		return 0, errMalformedProcStat
	}
	return v, nil
}

// parseCgroupV2Path extracts the relative cgroup v2 path from /proc/<pid>/cgroup
// (the "0::<path>" line). Returns ("", false) when absent.
func parseCgroupV2Path(raw []byte) (string, bool) {
	for _, line := range strings.Split(string(raw), "\n") {
		if rel, ok := strings.CutPrefix(strings.TrimSpace(line), "0::"); ok {
			return rel, true
		}
	}
	return "", false
}

// resolveScopedCgroupParent returns the directory inside cGroupPath where the
// run's scoped cgroup should be created: the client's own cgroup v2 directory
// (or its nearest domain ancestor if the client is in a threaded cgroup), never
// escaping above the client's container/unit cgroup so ancestor memory.max and
// pids.max limits remain enforced.
func resolveScopedCgroupParent(cGroupPath string, clientNSPID uint32) (string, error) {
	root := filepath.Clean(cGroupPath)
	if root == "" || root == "." {
		return "", errors.New("cgroup v2 mount path is empty")
	}
	raw, err := scopedReadProcCgroup(clientNSPID)
	if err != nil && clientNSPID != 0 {
		raw, err = scopedReadProcCgroup(0)
	}
	if err != nil {
		return "", fmt.Errorf("read client cgroup v2 path: %w", err)
	}
	rel, ok := parseCgroupV2Path(raw)
	if !ok {
		// Pure cgroup v1 host where mountCgroupV2 mounted the default hierarchy
		// at cGroupPath: every process lives at the root of that mount.
		return root, nil
	}
	rel = strings.TrimPrefix(filepath.Clean("/"+rel), "/")
	dir := root
	if rel != "" && rel != "." {
		candidate := filepath.Join(root, rel)
		if fi, statErr := os.Stat(candidate); statErr == nil && fi.IsDir() {
			dir = candidate
		} else {
			// Inside a private cgroup namespace /proc/<pid>/cgroup may report a
			// relative path that does not exist below cGroupPath; stay at root
			// (the container's cgroup namespace root) only when rel is "/" or
			// unreadable below root.
			if statErr != nil && !os.IsNotExist(statErr) {
				return "", fmt.Errorf("stat client cgroup %s: %w", candidate, statErr)
			}
			// If candidate does not exist below root (e.g. pure v1 host mounted
			// at /sys/fs/cgroup/keploy-cgroup2 while /proc shows a v2 path from
			// another mount), use root.
			dir = root
		}
	}

	// If the client's cgroup is threaded, step up to its nearest domain parent
	// within root (still inside the client's resource domain).
	for dir != root {
		b, err := os.ReadFile(filepath.Join(dir, "cgroup.type"))
		if err != nil || strings.TrimSpace(string(b)) != "threaded" {
			break
		}
		parent := filepath.Dir(dir)
		if parent == dir || !strings.HasPrefix(parent, root) {
			break
		}
		dir = parent
	}
	return dir, nil
}

// sweepStaleScopedCgroups removes empty keploy-scoped-<pid>-* directories left
// in parentDir by crashed (SIGKILLed) prior agents whose PID is no longer live.
// Linux rmdir on a cgroup2 directory is kernel-atomic: any directory still
// holding a live process (or concurrent run) fails with EBUSY and is untouched.
func sweepStaleScopedCgroups(logger *zap.Logger, parentDir string, selfPID int) int {
	entries, err := os.ReadDir(parentDir)
	if err != nil {
		return 0
	}
	removed := 0
	for _, e := range entries {
		if !e.IsDir() || !strings.HasPrefix(e.Name(), scopedCgroupPrefix) {
			continue
		}
		rest := strings.TrimPrefix(e.Name(), scopedCgroupPrefix)
		pidStr, _, _ := strings.Cut(rest, "-")
		ownerPID, err := strconv.ParseUint(pidStr, 10, 32)
		if err != nil || ownerPID == 0 || int(ownerPID) == selfPID {
			continue
		}
		if !scopedProcessGone(uint32(ownerPID)) {
			continue
		}
		path := filepath.Join(parentDir, e.Name())
		if err := os.Remove(path); err == nil {
			removed++
		} else if logger != nil && !os.IsNotExist(err) && !errors.Is(err, unix.EBUSY) {
			logger.Debug("could not sweep stale scoped cgroup", zap.String("dir", path), zap.Error(err))
		}
	}
	return removed
}

func randomScopedNonce() string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return strconv.FormatInt(time.Now().UnixNano(), 16)
	}
	return hex.EncodeToString(b[:])
}

// createScopedCgroup creates a unique cgroup v2 directory for this agent run
// inside parentDir, preflights write access to cgroup.procs (and parent
// cgroup.procs when non-root, per cgroups(7) delegation rules), and returns
// (dir, inodeID, nil). On any failure, removes the directory and returns an
// actionable error.
func createScopedCgroup(logger *zap.Logger, parentDir string, agentPID int) (string, uint64, error) {
	sweepStaleScopedCgroups(logger, parentDir, agentPID)

	// Unprivileged cgroup v2 migration requires write permission on the common
	// ancestor's cgroup.procs (parentDir). Preflight it before creating the dir
	// so non-delegated sessions fail cleanly at load time.
	if scopedGetEUID() != 0 {
		parentProcs := filepath.Join(parentDir, "cgroup.procs")
		f, err := os.OpenFile(parentProcs, os.O_WRONLY, 0)
		if err != nil {
			return "", 0, fmt.Errorf("cgroup %s is not delegated for unprivileged migration (%w)", parentDir, err)
		}
		_ = f.Close()
	}

	name := fmt.Sprintf("%s%d-%s", scopedCgroupPrefix, agentPID, randomScopedNonce())
	dir := filepath.Join(parentDir, name)
	if err := os.Mkdir(dir, 0o755); err != nil {
		return "", 0, fmt.Errorf("create scoped cgroup %s: %w", dir, err)
	}

	// On real cgroup v2, a child created under a "domain threaded" parent starts
	// as "domain invalid" and rejects every cgroup.procs migration with
	// EOPNOTSUPP. Verify cgroup.type is "domain" at setup time so we fall back
	// cleanly with scoped_ready=false rather than failing every test launch.
	if typeRaw, typeErr := os.ReadFile(filepath.Join(dir, "cgroup.type")); typeErr == nil {
		typ := strings.TrimSpace(string(typeRaw))
		if typ != "domain" {
			_ = os.Remove(dir)
			return "", 0, fmt.Errorf("%w: %s has cgroup.type=%q (parent cgroup is threaded; cannot migrate processes)", errScopedCgroupNotDomain, dir, typ)
		}
	}

	procs := filepath.Join(dir, "cgroup.procs")
	f, err := os.OpenFile(procs, os.O_WRONLY, 0)
	if err != nil {
		_ = os.Remove(dir)
		return "", 0, fmt.Errorf("open %s for writing: %w", procs, err)
	}
	_ = f.Close()

	fi, err := os.Stat(dir)
	if err != nil {
		_ = os.Remove(dir)
		return "", 0, fmt.Errorf("stat scoped cgroup %s: %w", dir, err)
	}
	st, ok := fi.Sys().(*syscall.Stat_t)
	if !ok || st.Ino == 0 {
		_ = os.Remove(dir)
		return "", 0, fmt.Errorf("read inode of scoped cgroup %s", dir)
	}
	return dir, st.Ino, nil
}

// removeScopedCgroup removes dir, polling briefly on EBUSY while exiting
// children finish tearing down. If a detached daemon outlives the run, leaves
// dir intact for the next run's startup sweep without blocking or killing it.
func removeScopedCgroup(logger *zap.Logger, dir string) {
	if dir == "" {
		return
	}
	deadline := time.Now().Add(scopedRemoveWait)
	for {
		err := os.Remove(dir)
		if err == nil || os.IsNotExist(err) {
			return
		}
		if !errors.Is(err, unix.EBUSY) {
			if logger != nil {
				logger.Debug("could not remove scoped cgroup on shutdown", zap.String("dir", dir), zap.Error(err))
			}
			return
		}
		if time.Now().After(deadline) {
			if logger != nil {
				logger.Debug("scoped cgroup still populated at shutdown (surviving child process); leaving for next run's startup sweep",
					zap.String("dir", dir))
			}
			return
		}
		time.Sleep(scopedRemoveStep)
	}
}

func cgroupPathDepth(rel string) uint32 {
	rel = strings.Trim(filepath.Clean("/"+rel), "/")
	if rel == "" || rel == "." {
		return 0
	}
	return uint32(strings.Count(rel, "/") + 1)
}

// computeScopedCgroupLevel computes the host-kernel cgrp->level of scopedDir
// from the agent's own kernel cgrp->level (probed via BPF register_agent), the
// agent's relative /proc/self/cgroup v2 path, and scopedDir's path inside
// cGroupPath. Works in both root and private cgroup namespaces:
//
//	host_root_level = selfLevel - depth(selfRel)
//	scoped_level    = host_root_level + depth(scopedRel)
//
// Returns 0 when the level cannot be derived safely (BPF then falls back to
// its top-down ancestor_ids[] scan).
func computeScopedCgroupLevel(cGroupPath, scopedDir string, selfLevel uint32, selfProcCgroup []byte) uint32 {
	relScoped, err := filepath.Rel(filepath.Clean(cGroupPath), filepath.Clean(scopedDir))
	if err != nil || relScoped == "." || strings.HasPrefix(relScoped, "..") {
		return 0
	}
	dScoped := cgroupPathDepth(relScoped)
	selfRel, ok := parseCgroupV2Path(selfProcCgroup)
	if !ok {
		// Pure cgroup v1 host where cGroupPath is a fresh cgroup2 mount root.
		return dScoped
	}
	dSelf := cgroupPathDepth(selfRel)
	if selfLevel < dSelf {
		return 0
	}
	return selfLevel - dSelf + dScoped
}

// probeSelfCgroupLevel registers the agent's NS PID/inode briefly and triggers
// a single socket()+close() so sys_enter_socket's register_agent records the
// agent's exact host-kernel cgrp->level in keploy_agent_cgroup_level_map.
func (h *Hooks) probeSelfCgroupLevel(base structs.AgentInfo) uint32 {
	if h.objects.KeployAgentRegistrationMap == nil || h.objects.KeployAgentCgroupLevelMap == nil {
		return 0
	}
	probe := base
	probe.Flags = 0
	probe.ScopedCgroupID = 0
	probe.ScopedCgroupLevel = 0
	key := uint32(0)
	if err := h.objects.KeployAgentRegistrationMap.Update(&key, &probe, 0); err != nil {
		return 0
	}
	if fd, err := unix.Socket(unix.AF_INET, unix.SOCK_DGRAM, 0); err == nil {
		_ = unix.Close(fd)
	}
	var selfLevel uint32
	if err := h.objects.KeployAgentCgroupLevelMap.Lookup(&key, &selfLevel); err != nil {
		return 0
	}
	return selfLevel
}

// applyScopedCgroupSetup runs setup when native scoped mode is requested,
// updating h's scoped status and info's Flags/ScopedCgroupID/ScopedCgroupLevel.
// On setup failure it clears FlagScopedPids and zeroes the cgroup ID/level so
// the run fails closed to full-tree interception with scoped_ready=false.
func (h *Hooks) applyScopedCgroupSetup(
	info *structs.AgentInfo,
	cGroupPath string,
	clientNSPID uint32,
	setup func(cGroupPath string, clientNSPID uint32) (string, uint64, uint32, error),
) {
	if info == nil || info.IsDocker != 0 || (info.Flags&structs.FlagScopedPids) == 0 {
		return
	}
	cgDir, cgID, cgLevel, scErr := setup(cGroupPath, clientNSPID)
	remedy := ""
	h.scopedMu.Lock()
	h.scopedRequested = true
	if scErr != nil {
		remedy = scopedCgroupRemedyFor(scErr)
		h.scopedReady = false
		h.scopedReason = scErr.Error()
		h.scopedRemedy = remedy
		h.scopedCgroupDir = ""
		h.scopedCgroupID = 0
		info.Flags &^= structs.FlagScopedPids
		info.ScopedCgroupID = 0
		info.ScopedCgroupLevel = 0
	} else {
		h.scopedReady = true
		h.scopedReason = ""
		h.scopedRemedy = ""
		h.scopedCgroupDir = cgDir
		h.scopedCgroupID = cgID
		info.ScopedCgroupID = cgID
		info.ScopedCgroupLevel = cgLevel
	}
	h.scopedMu.Unlock()
	if h.logger != nil {
		if scErr != nil {
			h.logger.Warn("go toolchain exemption unavailable (scoped_ready=false): could not create a scoped cgroup, falling back to full-tree interception — Go module downloads may be mocked or blocked",
				zap.Error(scErr),
				zap.String("remedy", remedy))
		} else {
			h.logger.Info("created scoped cgroup for Go toolchain exemption",
				zap.String("scoped_cgroup_dir", cgDir),
				zap.Uint64("scoped_cgroup_id", cgID),
				zap.Uint32("scoped_cgroup_level", cgLevel))
		}
	}
}

func (h *Hooks) setupScopedCgroup(cGroupPath string, clientNSPID uint32, selfLevel uint32) (string, uint64, uint32, error) {
	parentDir, err := resolveScopedCgroupParent(cGroupPath, clientNSPID)
	if err != nil {
		return "", 0, 0, err
	}
	dir, id, err := createScopedCgroup(h.logger, parentDir, os.Getpid())
	if err != nil {
		return "", 0, 0, err
	}
	selfRaw, _ := scopedReadProcCgroup(0)
	level := computeScopedCgroupLevel(cGroupPath, dir, selfLevel, selfRaw)
	return dir, id, level, nil
}

func (h *Hooks) closeScopedCgroup() {
	h.scopedMu.Lock()
	dir := h.scopedCgroupDir
	h.scopedCgroupDir = ""
	h.scopedCgroupID = 0
	h.scopedMu.Unlock()
	removeScopedCgroup(h.logger, dir)
}
