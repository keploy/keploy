package memoryguard

import (
	"bufio"
	"context"
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
	defaultCheckInterval = time.Second
	reclaimCooldown      = 5 * time.Second
	pauseThresholdRatio  = 0.90
	resumeThresholdRatio = 0.80
)

var recordingPaused atomic.Bool

type guard struct {
	logger            *zap.Logger
	memoryCurrentPath string
	limitBytes        int64
	memoryLimitMB     uint64
	lastReclaim       time.Time
	underPressure     bool
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

	g := &guard{
		logger:            logger,
		memoryCurrentPath: memoryCurrentPath,
		limitBytes:        limitBytes,
		memoryLimitMB:     memoryLimitMB,
	}

	logger.Info("Enabled keploy-agent memory guard",
		zap.Uint64("memory_limit_mb", g.memoryLimitMB),
		zap.Int("cgroup_version", layout.version),
		zap.String("memory_current_path", g.memoryCurrentPath))

	go g.run(ctx)
	return nil
}

func (g *guard) run(ctx context.Context) {
	ticker := time.NewTicker(defaultCheckInterval)
	defer ticker.Stop()
	defer g.resetPressure()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			currentBytes, err := readMemoryCurrent(g.memoryCurrentPath)
			if err != nil {
				g.logger.Warn("failed to read keploy-agent memory usage", zap.String("path", g.memoryCurrentPath), zap.Error(err))
				continue
			}

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
		g.logger.Warn("Pausing keploy-agent recording due to memory pressure",
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

func readMemoryCurrent(path string) (int64, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}

	value := strings.TrimSpace(string(data))
	if value == "" {
		return 0, fmt.Errorf("empty memory.current")
	}

	currentBytes, err := strconv.ParseInt(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("parse memory.current: %w", err)
	}

	return currentBytes, nil
}

func resolveMemoryUsagePath(procMountsPath, procSelfCgroupPath, procMountInfoPath string) (string, cgroupLayout, error) {
	layouts, err := detectCgroupLayouts(procMountsPath, procMountInfoPath)
	if err != nil {
		return "", cgroupLayout{}, err
	}

	for _, layout := range layouts {
		candidate, err := resolveFromSelfCgroup(layout, procSelfCgroupPath)
		if err == nil {
			return candidate, layout, nil
		}
	}

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
		return "", false
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
	type match struct {
		path  string
		idLen int
		depth int
	}

	best := match{}
	err := filepath.WalkDir(layout.mountPoint, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
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
