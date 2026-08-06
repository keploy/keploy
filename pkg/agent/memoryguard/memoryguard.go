package memoryguard

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"runtime/debug"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.uber.org/zap"
)

const (
	defaultCheckInterval = 500 * time.Millisecond
	reclaimCooldown      = 5 * time.Second
	pauseThresholdRatio  = 0.80
	resumeThresholdRatio = 0.70 // Lower than pause to avoid rapid toggle (hysteresis)

	// minGoMemLimitBytes floors GOMEMLIMIT. Below this the Go runtime is told to
	// hold the whole agent heap in a few tens of MiB and GCs nearly continuously
	// (see runtime/debug.SetMemoryLimit). k8s-proxy settled on the same 64 MiB
	// floor on the replay path (pkg/platform/k8s/k8s.go, minGoMemLimitBytes) for
	// this exact reason — keep the two in lockstep.
	minGoMemLimitBytes = 64 << 20 // 64 MiB

	// minPauseHeadroomBytes is the absolute memory margin the guard keeps below
	// the OOM ceiling. The percentage thresholds alone leave a margin
	// proportional to the (limit − overhead) budget, which shrinks to a
	// dangerously small ABSOLUTE number for small budgets; the guard polls every
	// 500ms and debug.FreeOSMemory cannot reclaim live heap, so a fast burst can
	// cross a thin margin between ticks and get the agent OOM-killed inside the
	// app's pod. This clamps the pause point to leave at least this much room
	// regardless of budget size. resume keeps 1.5× to preserve pause<resume
	// hysteresis ordering.
	minPauseHeadroomBytes  = 128 << 20 // 128 MiB
	minResumeHeadroomBytes = 192 << 20 // 1.5 × pause headroom

	// minViableEffLimitBytes is the smallest post-overhead budget a low-latency
	// recording will run within. Below it the band between the pause and resume
	// points is too thin for the guard to ever fall back far enough to resume — a
	// one-way pause. Set to the sum of the two headroom floors so, at the floor,
	// resume sits a full pause-headroom below pause (pause=effLimit−128,
	// resume=effLimit−192). Applies to low-latency (overhead>0) only.
	minViableEffLimitBytes = minPauseHeadroomBytes + minResumeHeadroomBytes // 320 MiB
)

// MinViableLowLatencyLimitBytes returns the smallest container memory limit (in
// bytes) a low-latency recording needs for a given fixed capture-buffer
// overhead: the overhead plus the minimum usable post-overhead budget. Callers
// that validate a memory limit before the agent starts (e.g. k8s-proxy's
// record-start check) can derive their floor from this instead of duplicating
// the guard's constants. A negative overhead is treated as zero.
func MinViableLowLatencyLimitBytes(overheadBytes int64) int64 {
	if overheadBytes < 0 {
		overheadBytes = 0
	}
	return overheadBytes + minViableEffLimitBytes
}

var recordingPaused atomic.Bool

// fixedOverheadBytes is memory the agent allocates up-front and that pausing
// recording cannot reclaim — chiefly the eBPF capture ring buffer in
// low-latency mode, which is charged to the container cgroup as kernel memory.
// The enterprise ringbuf loader sets this to the ACTUAL configured buffer size
// (record.ringbufSizeMB), never a hardcoded constant. It is zero in proxy mode.
// The guard discounts it from both the usage and the limit so the pressure
// signal tracks only the growing capture buffers — the memory a pause can
// actually free — budgeted against the room left above the fixed floor.
var fixedOverheadBytes atomic.Int64

// SetFixedOverheadMB records the fixed, non-reclaimable memory overhead (in MB)
// the guard should discount when evaluating memory pressure, and returns the
// number of bytes actually applied so the caller can log it (this setter is the
// only thing that turns the discount on, so a silent no-op is indistinguishable
// from working correctly). Safe to call before or after Start; the running
// guard picks up the new value on its next tick (the ring buffer is allocated
// after the guard has already started). Callers MUST pass the actual configured
// buffer size (e.g. the enterprise low-latency capture ring buffer's
// record.ringbufSizeMB), not a hardcoded value. A non-positive or implausibly
// large value clears the discount (returns 0) rather than overflowing — the
// proxy-mode default.
func SetFixedOverheadMB(mb int) int64 {
	var b int64
	if mb > 0 && int64(mb) <= math.MaxInt64/(1024*1024) {
		b = int64(mb) * 1024 * 1024
	}
	fixedOverheadBytes.Store(b)
	return b
}

// FixedOverheadBytes returns the currently configured fixed overhead in bytes.
// Exposed for diagnostics and tests.
func FixedOverheadBytes() int64 {
	return fixedOverheadBytes.Load()
}

type guard struct {
	logger            *zap.Logger
	memoryCurrentPath string
	limitBytes        int64
	memoryLimitMB     uint64
	lastReclaim       time.Time
	underPressure     bool
	readFailCount     int
	prevMemLimit      int64
}

type cgroupLayout struct {
	version    int
	mountPoint string
	mountRoot  string
	controller string
	usageFile  string
}

const (
	cgroupV1 = 1
	cgroupV2 = 2
)

// LimitBytes validates the configured memory limit in MB and converts it to bytes.
func LimitBytes(limitMB uint64) (int64, error) {
	if limitMB == 0 {
		return 0, nil
	}
	if limitMB > math.MaxInt64/(1024*1024) {
		return 0, fmt.Errorf("memory limit %dMB is too large", limitMB)
	}
	return int64(limitMB) * 1024 * 1024, nil
}

// IsRecordingPaused reports whether the agent is currently dropping captured
// tests and mocks due to memory pressure.
func IsRecordingPaused() bool {
	return recordingPaused.Load()
}

// Start enables the memory guard when a Docker agent is running with a limit.
func Start(ctx context.Context, logger *zap.Logger, isDocker bool, memoryLimitMB uint64) error {
	resetAllPressure()

	limitBytes, err := LimitBytes(memoryLimitMB)
	if err != nil {
		return err
	}
	if limitBytes == 0 || !isDocker {
		return nil
	}

	memoryCurrentPath, layout, err := resolveMemoryUsagePath("/proc/mounts", "/proc/self/cgroup", "/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("failed to resolve container memory usage path: %w", err)
	}

	// Inform the Go runtime about the memory constraint so the GC becomes
	// more aggressive well before the cgroup hard-limit is hit.  We use
	// 90% of the container limit because the remaining 10% is needed for
	// non-Go memory (kernel buffers, page cache, OS overhead).  Without
	// this headroom the GC won't kick in until the cgroup is already at
	// the OOM boundary, causing connection drops and I/O disruptions.
	// Floored (see minGoMemLimitBytes) so a small limit can't drive the GC
	// nearly continuous; run() realigns this once the overhead is known.
	goMemLimit := int64(float64(limitBytes) * 0.9)
	if goMemLimit < minGoMemLimitBytes {
		goMemLimit = minGoMemLimitBytes
	}
	prevMemLimit := debug.SetMemoryLimit(goMemLimit)

	g := &guard{
		logger:            logger,
		memoryCurrentPath: memoryCurrentPath,
		limitBytes:        limitBytes,
		memoryLimitMB:     memoryLimitMB,
		prevMemLimit:      prevMemLimit,
	}

	logger.Info("Enabled keploy-agent memory guard",
		zap.Uint64("memory_limit_mb", g.memoryLimitMB))
	logger.Debug("Memory guard cgroup details",
		zap.Int("cgroup_version", layout.version),
		zap.String("memory_current_path", g.memoryCurrentPath))

	go g.run(ctx)
	return nil
}

func (g *guard) run(ctx context.Context) {
	ticker := time.NewTicker(defaultCheckInterval)
	defer ticker.Stop()
	defer g.resetPressure()
	defer debug.SetMemoryLimit(g.prevMemLimit)

	// lastOverhead tracks the fixed-overhead value we last applied so GOMEMLIMIT
	// is only recomputed when it actually changes. It starts at -1 (an
	// impossible byte count) so the first tick always aligns GOMEMLIMIT with the
	// effective budget even when the overhead is still zero.
	lastOverhead := int64(-1)
	// One-time log latches so a persistent misconfiguration (budget too small to
	// record) is reported once, not on every 500ms tick.
	goMemFlooredLogged := false
	nonViableLogged := false

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			// readWorkingSetBytes subtracts inactive_file from memory.current so
			// that page-cache does not count toward the pressure threshold.  This
			// matches what kubectl top / Kubernetes eviction logic uses and avoids
			// false-positive pauses caused by reclaimable cache pages.
			currentBytes, err := readWorkingSetBytes(g.memoryCurrentPath)
			if err != nil {
				g.readFailCount++
				if g.readFailCount == 1 {
					g.logger.Debug("failed to read keploy-agent memory usage; "+
						"ensure /sys/fs/cgroup is mounted in the container or set --memory-limit=0 to disable",
						zap.String("path", g.memoryCurrentPath),
						zap.Error(err))
				}
				// After ~10s of consecutive failures, disable the guard entirely
				if g.readFailCount >= 20 {
					g.logger.Info("Disabling memory guard after persistent read failures",
						zap.Int("consecutive_failures", g.readFailCount))
					g.resetPressure()
					return
				}
				continue
			}
			g.readFailCount = 0

			// Discount the fixed, non-reclaimable overhead (the low-latency eBPF
			// ring buffer) from BOTH usage and limit, then pause when the
			// reclaimable remainder fills the ratio of the room left above the
			// floor — but never closer than an absolute headroom to the ceiling.
			// See evaluateMemory. In proxy mode overhead is 0, so this reduces to
			// the previous working-set-vs-limit behaviour.
			dec := evaluateMemory(currentBytes, g.limitBytes, fixedOverheadBytes.Load())

			// Realign GOMEMLIMIT with the Go-heap budget (the room left after the
			// non-Go fixed overhead), recomputing only when the overhead changes so
			// we don't thrash debug.SetMemoryLimit every tick. Floored (see
			// evaluateMemory) so a small budget can't drive near-continuous GC.
			if dec.overhead != lastOverhead {
				debug.SetMemoryLimit(dec.goMemTarget)
				// Re-log if a later overhead change re-floors GOMEMLIMIT; reset the
				// latch when it is no longer floored so the two states stay in sync.
				if dec.goMemFloored {
					if !goMemFlooredLogged {
						g.logger.Warn("keploy-agent GOMEMLIMIT floored — the heap budget left "+
							"after the capture ring buffer is very small; raise --memory-limit "+
							"or lower record.ringbufSizeMB",
							zap.Int64("go_mem_limit_bytes", dec.goMemTarget),
							zap.Int64("effective_limit_bytes", dec.effLimit),
							zap.Int64("fixed_overhead_bytes", dec.overhead))
						goMemFlooredLogged = true
					}
				} else {
					goMemFlooredLogged = false
				}
				lastOverhead = dec.overhead
			}

			// Non-viable budget (low-latency only): the fixed ring buffer leaves
			// too little room above it for a usable pause/resume band, so there is
			// no way to record without courting an OOM-kill. Hold paused and say so
			// loudly ONCE — silently falling back to raw guarding would reintroduce
			// the startup-pause bug this discount exists to fix. Proxy mode
			// (overhead 0) is never non-viable, so this and its ring-buffer message
			// only fire when a ring buffer is actually present. The real fix is
			// upstream (reject the record-start); this is the in-agent backstop.
			if !dec.viable {
				if !nonViableLogged {
					g.logger.Error("capture ring buffer leaves too little memory below the "+
						"limit for a usable recording budget; recording cannot proceed — "+
						"raise --memory-limit or lower record.ringbufSizeMB",
						zap.Int64("fixed_overhead_bytes", dec.overhead),
						zap.Int64("effective_limit_bytes", dec.effLimit),
						zap.Int64("memory_limit_bytes", g.limitBytes),
						zap.Int64("min_viable_budget_bytes", int64(minViableEffLimitBytes)))
					nonViableLogged = true
				}
				g.enterPressure(dec.effCurrent, 0, dec.overhead, dec.effLimit)
				continue
			}
			nonViableLogged = false

			if dec.effCurrent >= dec.pauseThreshold {
				g.enterPressure(dec.effCurrent, dec.pauseThreshold, dec.overhead, dec.effLimit)
				continue
			}

			if g.underPressure && dec.effCurrent <= dec.resumeThreshold {
				g.resetPressure()
				g.logger.Info("Cleared keploy-agent memory pressure after memory recovered",
					zap.Int64("memory_usage_bytes", dec.effCurrent),
					zap.Int64("resume_threshold_bytes", dec.resumeThreshold),
					zap.Int64("fixed_overhead_bytes", dec.overhead),
					zap.Int64("effective_limit_bytes", dec.effLimit))
			}
		}
	}
}

func (g *guard) enterPressure(currentBytes, pauseThreshold, overhead, effLimit int64) {
	alreadyPaused := g.underPressure
	g.underPressure = true
	applyPausedState(true)

	now := time.Now()
	if !alreadyPaused {
		g.logger.Info("Pausing keploy-agent recording due to memory pressure. "+
			"Consider increasing --memory-limit, enabling sampling, or reducing request concurrency",
			zap.Int64("memory_usage_bytes", currentBytes),
			zap.Int64("pause_threshold_bytes", pauseThreshold),
			zap.Int64("fixed_overhead_bytes", overhead),
			zap.Int64("effective_limit_bytes", effLimit),
			zap.Int64("memory_limit_bytes", g.limitBytes),
			zap.Uint64("memory_limit_mb", g.memoryLimitMB))
	}
	if !alreadyPaused || now.Sub(g.lastReclaim) >= reclaimCooldown {
		g.lastReclaim = now
		debug.FreeOSMemory()
	}
}

func (g *guard) resetPressure() {
	g.underPressure = false
	applyPausedState(false)
}

func resetAllPressure() {
	applyPausedState(false)
}

// pressureHooks are additional sinks for the memory-pressure signal,
// registered via RegisterPressureHook. The decision to pause is global
// (pod-level cgroup memory), but the buffered mocks that consume that memory
// can live in many sync-mock managers — e.g. the multi-app agent runs one
// manager per app and the package-global Get() manager is then unused. A
// composer registers a hook that fans the pressure out to all its live
// managers so the relief actually reaches the buffers.
var (
	pressureHookMu  sync.RWMutex
	pressureHooks   = map[uint64]func(paused bool){}
	pressureHookSeq uint64
)

// RegisterPressureHook adds fn to the set invoked by applyPausedState
// alongside the package-global manager and returns an unregister func that
// removes it again. A multi-app composer that registers one hook per app/
// session MUST call the returned func when that session ends, otherwise the
// hook — and everything its closure captures (the session's SyncMockManager
// and its buffers) — is pinned for the life of the process and re-invoked on
// every pressure transition. The returned func is idempotent and safe to call
// from any goroutine; calling it more than once is a no-op. Safe for
// concurrent use.
func RegisterPressureHook(fn func(paused bool)) (unregister func()) {
	if fn == nil {
		return func() {}
	}
	pressureHookMu.Lock()
	pressureHookSeq++
	id := pressureHookSeq
	pressureHooks[id] = fn
	pressureHookMu.Unlock()
	return func() {
		pressureHookMu.Lock()
		delete(pressureHooks, id)
		pressureHookMu.Unlock()
	}
}

func applyPausedState(paused bool) {
	recordingPaused.Store(paused)
	// Global manager: the buffering manager in single-app mode.
	if mgr := syncMock.Get(); mgr != nil {
		mgr.SetMemoryPressure(paused)
	}
	// Fan out to registered managers (multi-app: one per app). The global
	// trigger stays global; only the action reaches every live buffer.
	pressureHookMu.RLock()
	hooks := make([]func(paused bool), 0, len(pressureHooks))
	for _, fn := range pressureHooks {
		hooks = append(hooks, fn)
	}
	pressureHookMu.RUnlock()
	for _, fn := range hooks {
		fn(paused)
	}
}

// readMemoryCurrent reads the raw cgroup memory.current value (total bytes
// including page cache).  Callers that need the working-set should use
// readWorkingSetBytes instead.
func readMemoryCurrent(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	value := strings.TrimSpace(string(data))
	if value == "" {
		return 0, fmt.Errorf("empty cgroup memory usage file: %s", path)
	}
	currentBytes, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse cgroup memory usage file %s: %w", path, err)
	}
	return currentBytes, nil
}

// readWorkingSetBytes returns the container's working-set memory:
//
//	working_set = memory.current − inactive_file
//
// This matches what kubectl top and the Kubernetes eviction manager report,
// and avoids false-positive pressure events caused by reclaimable page cache.
// If memory.stat is unavailable the raw memory.current value is returned so
// the guard still functions (conservatively).
func readWorkingSetBytes(memCurrentPath string) (int64, error) {
	currentBytes, inactiveFile, err := readMemoryStats(memCurrentPath)
	if err != nil {
		return 0, err
	}
	workingSet := currentBytes - inactiveFile
	if workingSet < 0 {
		workingSet = 0
	}
	return workingSet, nil
}

// readMemoryStats returns (memory.current, inactive_file). Exposed for
// local-testing diagnostics so callers can log both components of the
// working-set calculation.
func readMemoryStats(memCurrentPath string) (int64, int64, error) {
	currentBytes, err := readMemoryCurrent(memCurrentPath)
	if err != nil {
		return 0, 0, err
	}

	statPath := filepath.Join(filepath.Dir(memCurrentPath), "memory.stat")
	data, err := os.ReadFile(statPath)
	if err != nil {
		return currentBytes, 0, nil
	}

	var inactiveFile int64
	for _, line := range strings.Split(string(data), "\n") {
		if strings.HasPrefix(line, "inactive_file ") {
			fields := strings.Fields(line)
			if len(fields) == 2 {
				inactiveFile, _ = strconv.ParseInt(fields[1], 10, 64)
			}
			break
		}
	}
	return currentBytes, inactiveFile, nil
}

func resolveMemoryUsagePath(procMountsPath, procSelfCgroupPath, procMountInfoPath string) (string, cgroupLayout, error) {
	layouts, err := detectCgroupLayouts(procMountsPath, procMountInfoPath)
	if err != nil {
		return "", cgroupLayout{}, err
	}

	// Primary path: derive the container's cgroup from /proc/self/cgroup.
	// This works on Linux (including Linux kind clusters) where cgroup namespace
	// isolation is in effect and /proc/self/cgroup reports the container-relative
	// path rather than "/".
	for _, layout := range layouts {
		candidate, err := resolveFromSelfCgroup(layout, procSelfCgroupPath)
		if err == nil {
			return candidate, layout, nil
		}
	}

	// Secondary path: walk every cgroup.procs file under the mount point and
	// find the scope that actually owns this process (matched by its
	// root-namespace PID read from NSpid in /proc/self/status). This is
	// unambiguous — exactly one cgroup contains our PID — so we prefer it
	// over identifier matching, which can fuzzy-match sibling container
	// scopes when multiple pod cgroups are visible (kind, nested Docker).
	var resolutionErrs []string
	for _, layout := range layouts {
		candidate, err := findCgroupByOwnPID(layout)
		if err == nil {
			return candidate, layout, nil
		}
		resolutionErrs = append(resolutionErrs, fmt.Sprintf("pid-walk: %v", err))
	}

	// Tertiary path: scan identifiers extracted from the environment, hostname,
	// /proc/self/cgroup, and /proc/self/mountinfo, then walk the cgroup tree
	// looking for a scope directory whose name contains one of those identifiers.
	// Used only when PID-based resolution fails (e.g. environments where
	// /proc/self/status NSpid is hidden or cgroup.procs is not readable).
	identifiers, err := collectContainerIdentifiers(procSelfCgroupPath, procMountInfoPath)
	if err != nil {
		return "", cgroupLayout{}, err
	}
	for _, layout := range layouts {
		candidate, err := findMemoryUsagePathByIdentifier(layout, identifiers)
		if err == nil {
			return candidate, layout, nil
		}
		resolutionErrs = append(resolutionErrs, err.Error())
	}

	return "", cgroupLayout{}, fmt.Errorf("no container-specific cgroup memory file found (%s)", strings.Join(resolutionErrs, "; "))
}

func detectCgroupLayouts(procMountsPath, procMountInfoPath string) ([]cgroupLayout, error) {
	mountRoots, err := readMountRoots(procMountInfoPath)
	if err != nil {
		return nil, err
	}

	f, err := os.Open(procMountsPath)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var layouts []cgroupLayout
	seen := make(map[string]struct{})
	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) < 4 {
			continue
		}
		mountPoint := fields[1]
		fsType := fields[2]
		options := strings.Split(fields[3], ",")

		switch {
		case fsType == "cgroup" && hasController(options, "memory"):
			key := "v1:" + mountPoint
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			layouts = append(layouts, cgroupLayout{
				version:    cgroupV1,
				mountPoint: mountPoint,
				mountRoot:  mountRoots[mountPoint],
				controller: "memory",
				usageFile:  "memory.usage_in_bytes",
			})
		case fsType == "cgroup2":
			key := "v2:" + mountPoint
			if _, ok := seen[key]; ok {
				continue
			}
			seen[key] = struct{}{}
			layouts = append(layouts, cgroupLayout{
				version:    cgroupV2,
				mountPoint: mountPoint,
				mountRoot:  mountRoots[mountPoint],
				usageFile:  "memory.current",
			})
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(layouts) == 0 {
		return nil, fmt.Errorf("no cgroup v1 memory or cgroup v2 mounts found")
	}

	sort.SliceStable(layouts, func(i, j int) bool {
		if layouts[i].version == layouts[j].version {
			return layouts[i].mountPoint < layouts[j].mountPoint
		}
		return layouts[i].version < layouts[j].version
	})
	return layouts, nil
}

func readSelfCgroupPath(procSelfCgroupPath string, layout cgroupLayout) (string, error) {
	data, err := os.ReadFile(procSelfCgroupPath)
	if err != nil {
		return "", err
	}
	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		parts := strings.SplitN(line, ":", 3)
		if len(parts) != 3 {
			continue
		}
		switch layout.version {
		case cgroupV2:
			if parts[1] == "" {
				if parts[2] == "" {
					return "/", nil
				}
				return parts[2], nil
			}
		case cgroupV1:
			if hasController(strings.Split(parts[1], ","), layout.controller) {
				if parts[2] == "" {
					return "/", nil
				}
				return parts[2], nil
			}
		}
	}
	return "", fmt.Errorf("container cgroup path not found in %s for cgroup v%d", procSelfCgroupPath, layout.version)
}

func readMountRoots(procMountInfoPath string) (map[string]string, error) {
	data, err := os.ReadFile(procMountInfoPath)
	if err != nil {
		return nil, err
	}
	roots := make(map[string]string)
	for _, line := range strings.Split(string(data), "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		parts := strings.SplitN(line, " - ", 2)
		if len(parts) != 2 {
			continue
		}
		fields := strings.Fields(parts[0])
		if len(fields) < 5 {
			continue
		}
		roots[fields[4]] = fields[3]
	}
	return roots, nil
}

func collectContainerIdentifiers(procSelfCgroupPath, procMountInfoPath string) ([]string, error) {
	candidates := make(map[string]struct{})

	for _, value := range []string{os.Getenv("HOSTNAME")} {
		for _, id := range extractHexIdentifiers(value) {
			candidates[id] = struct{}{}
		}
	}
	if hostname, err := os.Hostname(); err == nil {
		for _, id := range extractHexIdentifiers(hostname) {
			candidates[id] = struct{}{}
		}
	}
	for _, path := range []string{procSelfCgroupPath, procMountInfoPath} {
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		for _, id := range extractHexIdentifiers(string(data)) {
			candidates[id] = struct{}{}
		}
	}

	if len(candidates) == 0 {
		return nil, fmt.Errorf("failed to derive container identifier for cgroup lookup")
	}

	identifiers := make([]string, 0, len(candidates))
	for id := range candidates {
		identifiers = append(identifiers, id)
	}
	sort.Slice(identifiers, func(i, j int) bool {
		if len(identifiers[i]) == len(identifiers[j]) {
			return identifiers[i] < identifiers[j]
		}
		return len(identifiers[i]) > len(identifiers[j])
	})
	return identifiers, nil
}

func extractHexIdentifiers(value string) []string {
	re := regexp.MustCompile(`(?i)[a-f0-9]{12,64}`)
	matches := re.FindAllString(strings.ToLower(value), -1)
	if len(matches) == 0 {
		return nil
	}
	seen := make(map[string]struct{}, len(matches))
	result := make([]string, 0, len(matches))
	for _, match := range matches {
		if _, ok := seen[match]; ok {
			continue
		}
		seen[match] = struct{}{}
		result = append(result, match)
	}
	return result
}

// resolveFromSelfCgroup derives the container's cgroup memory file path by
// reading /proc/self/cgroup.  On Linux with proper cgroup namespace isolation
// this reports the container-relative path (e.g.
// /kubepods/burstable/pod.../cri-containerd-<id>) and is the fastest,
// most reliable resolution method.
//
// When the resolved path is the root ("/"), the mounted candidate may point
// at an ancestor cgroup rather than the container's own: macOS Docker Desktop
// exposes the VM's root cgroup, and kind nodes expose the node-level tree.
// For the root case we therefore require proof that the candidate cgroup
// actually owns this process (its cgroup.procs contains our PID); otherwise
// the caller falls through to PID-based or identifier-based resolution.
// If cgroup.procs cannot be read (e.g. unit tests with a fake tree), the
// candidate is accepted optimistically to preserve previous behavior.
func resolveFromSelfCgroup(layout cgroupLayout, procSelfCgroupPath string) (string, error) {
	cgroupPath, err := readSelfCgroupPath(procSelfCgroupPath, layout)
	if err != nil {
		return "", err
	}

	candidate, ok := buildMountedCgroupPath(layout.mountPoint, layout.mountRoot, cgroupPath, layout.usageFile)
	if !ok {
		return "", fmt.Errorf("unable to map cgroup path %q to mount %q", cgroupPath, layout.mountPoint)
	}
	if !fileExists(candidate) {
		return "", fmt.Errorf("cgroup memory file %q not found", candidate)
	}

	if cgroupPath == "/" || cgroupPath == "" {
		if owns, checked := cgroupOwnsSelf(filepath.Dir(candidate)); checked && !owns {
			return "", fmt.Errorf("cgroup %q is not container-specific (self not listed in cgroup.procs)", filepath.Dir(candidate))
		}
	}

	return candidate, nil
}

// cgroupOwnsSelf reports whether the cgroup at cgroupDir owns this process.
// The second return value indicates whether we could actually read
// cgroup.procs — callers use it to distinguish "verified not ours" from
// "unable to verify" so missing-file environments (e.g. unit tests) don't
// incorrectly fail resolution.
func cgroupOwnsSelf(cgroupDir string) (owns, checked bool) {
	data, err := os.ReadFile(filepath.Join(cgroupDir, "cgroup.procs"))
	if err != nil {
		return false, false
	}
	pid, err := getRootNSPID()
	if err != nil {
		return false, false
	}
	pidStr := strconv.Itoa(pid)
	for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
		if strings.TrimSpace(line) == pidStr {
			return true, true
		}
	}
	return false, true
}

func buildMountedCgroupPath(mountPoint, mountRoot, cgroupPath, usageFile string) (string, bool) {
	cleanMountRoot := filepath.Clean(mountRoot)
	if cleanMountRoot == "." || cleanMountRoot == "" {
		cleanMountRoot = "/"
	}
	cleanCgroupPath := filepath.Clean(cgroupPath)
	if cleanCgroupPath == "." || cleanCgroupPath == "" {
		cleanCgroupPath = "/"
	}

	var relativePath string
	switch {
	case cleanMountRoot == "/" && cleanCgroupPath == "/":
		relativePath = ""
	case cleanMountRoot == "/":
		relativePath = strings.TrimPrefix(cleanCgroupPath, "/")
	case cleanCgroupPath == cleanMountRoot:
		relativePath = ""
	case strings.HasPrefix(cleanCgroupPath, cleanMountRoot+"/"):
		relativePath = strings.TrimPrefix(cleanCgroupPath, cleanMountRoot+"/")
	default:
		return "", false
	}

	if relativePath == "" {
		return filepath.Join(mountPoint, usageFile), true
	}
	return filepath.Join(mountPoint, relativePath, usageFile), true
}

func findMemoryUsagePathByIdentifier(layout cgroupLayout, identifiers []string) (string, error) {
	const maxWalkDepth = 8 // bound the search to avoid slow walks over large cgroup trees

	type match struct {
		path  string
		idLen int
		depth int
	}

	mountDepth := strings.Count(filepath.Clean(layout.mountPoint), string(os.PathSeparator))
	best := match{}

	err := filepath.WalkDir(layout.mountPoint, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		currentDepth := strings.Count(path, string(os.PathSeparator)) - mountDepth
		if d.IsDir() && currentDepth > maxWalkDepth {
			return filepath.SkipDir
		}
		if d.IsDir() || d.Name() != layout.usageFile {
			return nil
		}
		dir := filepath.Dir(path)
		if dir == layout.mountPoint {
			return nil
		}
		dirLower := strings.ToLower(dir)
		for _, identifier := range identifiers {
			if !strings.Contains(dirLower, identifier) {
				continue
			}
			depth := strings.Count(dirLower, string(os.PathSeparator))
			if len(identifier) > best.idLen || (len(identifier) == best.idLen && depth > best.depth) {
				best = match{
					path:  path,
					idLen: len(identifier),
					depth: depth,
				}
			}
			break
		}
		return nil
	})
	if err != nil {
		return "", err
	}
	if best.path == "" {
		return "", fmt.Errorf("failed to find container-specific %s under %s", layout.usageFile, layout.mountPoint)
	}
	return best.path, nil
}

// getRootNSPID returns this process's PID in the root (initial) PID namespace
// by reading the NSpid field from /proc/self/status.  NSpid lists namespaces
// innermost-first, so the last entry is the outermost / host-level PID — the
// one written into cgroup.procs files by the container runtime.
func getRootNSPID() (int, error) {
	data, err := os.ReadFile("/proc/self/status")
	if err != nil {
		return 0, err
	}
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, "NSpid:") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			break
		}
		// Last field = outermost (root) namespace PID
		return strconv.Atoi(fields[len(fields)-1])
	}
	return 0, fmt.Errorf("NSpid field not found in /proc/self/status")
}

// findCgroupByOwnPID walks the cgroup tree under layout.mountPoint looking for
// the leaf scope whose cgroup.procs file contains this process's root-namespace
// PID.  It is used as a last-resort fallback for environments where
// /proc/self/cgroup reports "/" and no container identifier can be found in
// mountinfo — specifically macOS + Docker Desktop + kind clusters where the
// cgroup namespace is not propagated into kind-node containers.
//
// This function is never reached on a standard Linux setup (the primary
// resolveFromSelfCgroup path succeeds there), so it has zero impact on the
// existing Linux behaviour.
func findCgroupByOwnPID(layout cgroupLayout) (string, error) {
	rootPID, err := getRootNSPID()
	if err != nil {
		return "", fmt.Errorf("could not determine root namespace PID: %w", err)
	}
	pidStr := strconv.Itoa(rootPID)

	const maxWalkDepth = 8
	mountDepth := strings.Count(filepath.Clean(layout.mountPoint), string(os.PathSeparator))

	var foundPath string
	// sentinelErr signals a successful early-exit from WalkDir.
	sentinelErr := errors.New("found")

	walkErr := filepath.WalkDir(layout.mountPoint, func(path string, d os.DirEntry, err error) error {
		if err != nil {
			return nil // skip unreadable entries; don't abort the walk
		}
		currentDepth := strings.Count(path, string(os.PathSeparator)) - mountDepth
		if d.IsDir() && currentDepth > maxWalkDepth {
			return filepath.SkipDir
		}
		if d.IsDir() || d.Name() != "cgroup.procs" {
			return nil
		}

		data, err := os.ReadFile(path)
		if err != nil {
			return nil
		}
		for _, line := range strings.Split(strings.TrimSpace(string(data)), "\n") {
			if strings.TrimSpace(line) != pidStr {
				continue
			}
			candidate := filepath.Join(filepath.Dir(path), layout.usageFile)
			if fileExists(candidate) {
				foundPath = candidate
				return sentinelErr // signal early exit
			}
		}
		return nil
	})

	// WalkDir returns sentinelErr when we found the path — that is success.
	if walkErr != nil && !errors.Is(walkErr, sentinelErr) {
		return "", walkErr
	}
	if foundPath == "" {
		return "", fmt.Errorf("no cgroup scope found containing PID %d (root NS) under %s", rootPID, layout.mountPoint)
	}
	return foundPath, nil
}

func hasController(controllers []string, target string) bool {
	for _, controller := range controllers {
		if controller == target {
			return true
		}
	}
	return false
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func thresholdBytes(limit int64, ratio float64) int64 {
	if limit <= 0 {
		return 0
	}
	threshold := int64(float64(limit) * ratio)
	if threshold == 0 {
		return limit
	}
	return threshold
}

// memoryDecision is the pure result of evaluating one memory sample against the
// limit and the fixed overhead. run() applies it (pause/resume, GOMEMLIMIT,
// logging); keeping the arithmetic here makes it unit-testable without a cgroup.
type memoryDecision struct {
	effCurrent      int64 // reclaimable usage = working set − overhead, floored at 0
	effLimit        int64 // budget for reclaimable memory = limit − overhead
	overhead        int64 // sanitised overhead actually applied
	pauseThreshold  int64 // pause when effCurrent ≥ this (viable only)
	resumeThreshold int64 // resume when effCurrent ≤ this (viable only)
	goMemTarget     int64 // GOMEMLIMIT to install (floored at minGoMemLimitBytes)
	goMemFloored    bool  // goMemTarget hit its floor
	viable          bool  // false ⇒ budget too small to record; hold paused
}

// evaluateMemory discounts the fixed, non-reclaimable overhead (the low-latency
// eBPF ring buffer, charged to the cgroup as kernel memory) from both the
// working-set usage and the limit — pausing cannot reclaim it, so the guard
// budgets only the reclaimable remainder against the room above the floor.
//
// Two safety rules on top of the percentage thresholds:
//   - GOMEMLIMIT is floored at minGoMemLimitBytes so a tiny budget can't drive
//     the runtime into near-continuous GC.
//   - The pause/resume points never leave less than an absolute headroom below
//     the ceiling (minPause/minResumeHeadroomBytes); the percentage margin alone
//     shrinks to a dangerously small ABSOLUTE number for small budgets, and the
//     500ms poll + FreeOSMemory-can't-reclaim-live-heap means a thin margin gets
//     the agent OOM-killed between ticks. If the floor leaves less than the
//     resume headroom, the budget is non-viable and the guard holds paused.
//
// In proxy mode overhead is 0, so effLimit == limit and the ratios apply as
// before (the absolute floor only binds for small budgets). A negative overhead
// means "unset/bad" and is treated as no discount.
func evaluateMemory(currentBytes, limitBytes, overhead int64) memoryDecision {
	if overhead < 0 {
		overhead = 0
	}
	effLimit := limitBytes - overhead
	effCurrent := currentBytes - overhead
	if effCurrent < 0 {
		effCurrent = 0
	}
	// effCurrent is computed even on the non-viable path so the pause log carries
	// the real usage, not a zero.
	d := memoryDecision{overhead: overhead, effLimit: effLimit, effCurrent: effCurrent}

	// GOMEMLIMIT floored so a tiny budget can't drive near-continuous GC. effLimit
	// may be negative here when overhead > limit; 0.9×negative is still below the
	// floor, so it lands on the floor and is safe.
	goTarget := int64(float64(effLimit) * 0.9)
	if goTarget < minGoMemLimitBytes {
		goTarget = minGoMemLimitBytes
		d.goMemFloored = true
	}
	d.goMemTarget = goTarget

	// Proxy mode (overhead == 0): there is no fixed floor to compensate for, so
	// guard the raw working set against the plain ratios exactly as before — no
	// absolute headroom, no non-viability. Those rules exist only to stop a large
	// ring buffer from eating the low-latency budget; imposing them on proxy mode
	// would wrongly declare any small --memory-limit unrecordable.
	if overhead == 0 {
		d.viable = true
		d.pauseThreshold = thresholdBytes(effLimit, pauseThresholdRatio)
		d.resumeThreshold = thresholdBytes(effLimit, resumeThresholdRatio)
		return d
	}

	// Low-latency: the ring buffer is a fixed floor. Require a usable budget above
	// it — pause and resume must be separated by a real band — else hold paused
	// and report the config as too small (also catches overhead ≥ limit ⇒
	// effLimit ≤ 0).
	if effLimit < minViableEffLimitBytes {
		d.viable = false
		return d
	}
	d.viable = true
	pauseHeadroom := max(effLimit-thresholdBytes(effLimit, pauseThresholdRatio), int64(minPauseHeadroomBytes))
	resumeHeadroom := max(effLimit-thresholdBytes(effLimit, resumeThresholdRatio), int64(minResumeHeadroomBytes))
	d.pauseThreshold = effLimit - pauseHeadroom
	d.resumeThreshold = effLimit - resumeHeadroom
	return d
}
