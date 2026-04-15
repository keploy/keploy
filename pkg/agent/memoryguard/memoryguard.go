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
)

var recordingPaused atomic.Bool

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
	goMemLimit := int64(float64(limitBytes) * 0.9)
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


			pauseThreshold := thresholdBytes(g.limitBytes, pauseThresholdRatio)
			resumeThreshold := thresholdBytes(g.limitBytes, resumeThresholdRatio)

			if pauseThreshold == 0 {
				pauseThreshold = g.limitBytes
			}
			if resumeThreshold == 0 {
				resumeThreshold = g.limitBytes
			}

			if currentBytes >= pauseThreshold {
				g.enterPressure(currentBytes, pauseThreshold)
				continue
			}

			if g.underPressure && currentBytes <= resumeThreshold {
				g.resetPressure()
				g.logger.Info("Cleared keploy-agent memory pressure after memory recovered",
					zap.Int64("memory_usage_bytes", currentBytes),
					zap.Int64("resume_threshold_bytes", resumeThreshold),
					zap.Int64("memory_limit_bytes", g.limitBytes))
			}
		}
	}
}

func (g *guard) enterPressure(currentBytes, pauseThreshold int64) {
	alreadyPaused := g.underPressure
	g.underPressure = true
	applyPausedState(true)

	now := time.Now()
	if !alreadyPaused {
		g.logger.Info("Pausing keploy-agent recording due to memory pressure. "+
			"Consider increasing --memory-limit, enabling sampling, or reducing request concurrency",
			zap.Int64("memory_usage_bytes", currentBytes),
			zap.Int64("pause_threshold_bytes", pauseThreshold),
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

func applyPausedState(paused bool) {
	recordingPaused.Store(paused)
	if mgr := syncMock.Get(); mgr != nil {
		mgr.SetMemoryPressure(paused)
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
	currentBytes, err := readMemoryCurrent(memCurrentPath)
	if err != nil {
		return 0, err
	}

	statPath := filepath.Join(filepath.Dir(memCurrentPath), "memory.stat")
	data, err := os.ReadFile(statPath)
	if err != nil {
		// Graceful degradation: memory.stat may not exist on cgroup v1 paths
		// or in some constrained environments; fall back to raw usage.
		return currentBytes, nil
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

	workingSet := currentBytes - inactiveFile
	if workingSet < 0 {
		workingSet = 0
	}
	return workingSet, nil
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

	// Secondary path: scan identifiers extracted from the environment, hostname,
	// /proc/self/cgroup, and /proc/self/mountinfo, then walk the cgroup tree
	// looking for a scope directory whose name contains one of those identifiers.
	identifiers, err := collectContainerIdentifiers(procSelfCgroupPath, procMountInfoPath)
	if err != nil {
		return "", cgroupLayout{}, err
	}

	var resolutionErrs []string
	for _, layout := range layouts {
		candidate, err := findMemoryUsagePathByIdentifier(layout, identifiers)
		if err == nil {
			return candidate, layout, nil
		}
		resolutionErrs = append(resolutionErrs, err.Error())
	}

	// Tertiary / macOS-kind fallback: on macOS with Docker Desktop the cgroup
	// namespace is not propagated into kind node containers, so
	// /proc/self/cgroup reports "/" and the container ID does not appear in
	// mountinfo.  Walk every cgroup.procs file under the mount point and find
	// the scope that actually owns this process (matched by its root-namespace
	// PID read from NSpid in /proc/self/status).
	// This path is never reached on a normal Linux setup, so it has zero impact
	// on the existing Linux flow.
	for _, layout := range layouts {
		candidate, err := findCgroupByOwnPID(layout)
		if err == nil {
			return candidate, layout, nil
		}
		resolutionErrs = append(resolutionErrs, fmt.Sprintf("pid-walk: %v", err))
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
// On macOS with Docker Desktop + kind the cgroup namespace is not propagated,
// so /proc/self/cgroup reports "/" (the root of the entire VM's cgroup tree).
// When cgroupPath is "/" (root), this is valid in containers with proper
// cgroup namespace isolation where the container sits at the root of its
// own cgroup namespace. buildMountedCgroupPath handles the "/" case and
// fileExists validates the resolved path, so we do not reject "/" here.
// If the resolved path doesn't exist (e.g. macOS Docker Desktop + kind
// where "/" maps to VM-wide memory), fileExists will fail and the caller
// falls through to identifier-based or PID-based resolution.
func resolveFromSelfCgroup(layout cgroupLayout, procSelfCgroupPath string) (string, error) {
	cgroupPath, err := readSelfCgroupPath(procSelfCgroupPath, layout)
	if err != nil {
		return "", err
	}

	// An empty cgroup path is invalid — fall through to identifier-based
	// resolution.
	if cgroupPath == "" {
		return "", fmt.Errorf("cgroup self-path is empty; skipping to container-specific resolution")
	}

	candidate, ok := buildMountedCgroupPath(layout.mountPoint, layout.mountRoot, cgroupPath, layout.usageFile)
	if !ok {
		return "", fmt.Errorf("unable to map cgroup path %q to mount %q", cgroupPath, layout.mountPoint)
	}
	if !fileExists(candidate) {
		return "", fmt.Errorf("cgroup memory file %q not found", candidate)
	}
	return candidate, nil
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
