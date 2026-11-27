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

	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

const (
	scopedCgroupPrefix = "keploy-scoped-"
	scopedCgroupRemedy = "run with sudo (CAP_SYS_ADMIN) or under a delegated systemd user scope"
)

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

var errMalformedProcStat = errors.New("malformed /proc stat")

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

func (h *Hooks) setupScopedCgroup(cGroupPath string, clientNSPID uint32) (string, uint64, error) {
	parentDir, err := resolveScopedCgroupParent(cGroupPath, clientNSPID)
	if err != nil {
		return "", 0, err
	}
	return createScopedCgroup(h.logger, parentDir, os.Getpid())
}

func (h *Hooks) closeScopedCgroup() {
	h.scopedMu.Lock()
	dir := h.scopedCgroupDir
	h.scopedCgroupDir = ""
	h.scopedCgroupID = 0
	h.scopedMu.Unlock()
	removeScopedCgroup(h.logger, dir)
}
