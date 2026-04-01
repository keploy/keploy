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
}

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
	recordingPaused.Store(false)
	if mgr := syncMock.Get(); mgr != nil {
		mgr.SetMemoryPressure(false)
	}

	limitBytes, err := LimitBytes(memoryLimitMB)
	if err != nil {
		return err
	}
	if limitBytes == 0 || !isDocker {
		return nil
	}

	cgroupMountPath, err := detectCgroupMountPath("/proc/mounts")
	if err != nil {
		return fmt.Errorf("failed to detect cgroup v2 mount: %w", err)
	}

	memoryCurrentPath, err := resolveMemoryCurrentPath(cgroupMountPath, "/proc/self/cgroup", "/proc/self/mountinfo")
	if err != nil {
		return fmt.Errorf("failed to resolve container memory.current path: %w", err)
	}

	g := &guard{
		logger:            logger,
		memoryCurrentPath: memoryCurrentPath,
		limitBytes:        limitBytes,
		memoryLimitMB:     memoryLimitMB,
	}

	logger.Info("Enabled keploy-agent memory guard",
		zap.Uint64("memory_limit_mb", g.memoryLimitMB),
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

			if recordingPaused.Load() && currentBytes <= resumeThreshold {
				g.resetPressure()
				g.logger.Info("Resumed recording after keploy-agent memory recovered",
					zap.Int64("memory_usage_bytes", currentBytes),
					zap.Int64("resume_threshold_bytes", resumeThreshold),
					zap.Int64("memory_limit_bytes", g.limitBytes))
			}
		}
	}
}

func (g *guard) enterPressure(currentBytes, pauseThreshold int64) {
	if mgr := syncMock.Get(); mgr != nil {
		mgr.SetMemoryPressure(true)
	}

	alreadyPaused := recordingPaused.Swap(true)
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
	recordingPaused.Store(false)
	if mgr := syncMock.Get(); mgr != nil {
		mgr.SetMemoryPressure(false)
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

func detectCgroupMountPath(procMountsPath string) (string, error) {
	f, err := os.Open(procMountsPath)
	if err != nil {
		return "", err
	}
	defer f.Close()

	scanner := bufio.NewScanner(f)
	for scanner.Scan() {
		fields := strings.Fields(scanner.Text())
		if len(fields) >= 3 && fields[2] == "cgroup2" {
			return fields[1], nil
		}
	}

	if err := scanner.Err(); err != nil {
		return "", err
	}

	return "", fmt.Errorf("cgroup2 not mounted")
}

func resolveMemoryCurrentPath(cgroupMountPath, procSelfCgroupPath, procMountInfoPath string) (string, error) {
	cgroupPath, err := readSelfCgroupPath(procSelfCgroupPath)
	if err == nil && cgroupPath != "" && cgroupPath != "/" {
		candidate := filepath.Join(cgroupMountPath, strings.TrimPrefix(cgroupPath, "/"), "memory.current")
		if fileExists(candidate) {
			return candidate, nil
		}
	}

	mountRoot, err := readMountRoot(procMountInfoPath, cgroupMountPath)
	if err == nil && mountRoot != "" && mountRoot != "/" {
		candidate := filepath.Join(cgroupMountPath, "memory.current")
		if fileExists(candidate) {
			return candidate, nil
		}
	}

	identifiers, err := collectContainerIdentifiers(procSelfCgroupPath, procMountInfoPath)
	if err != nil {
		return "", err
	}

	candidate, err := findMemoryCurrentByIdentifier(cgroupMountPath, identifiers)
	if err != nil {
		return "", err
	}

	return candidate, nil
}

func readSelfCgroupPath(procSelfCgroupPath string) (string, error) {
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
		if parts[1] == "" || parts[1] == "memory" {
			if parts[2] == "" {
				return "/", nil
			}
			return parts[2], nil
		}
	}

	return "", fmt.Errorf("container cgroup path not found in %s", procSelfCgroupPath)
}

func readMountRoot(procMountInfoPath, mountPoint string) (string, error) {
	data, err := os.ReadFile(procMountInfoPath)
	if err != nil {
		return "", err
	}

	for _, line := range strings.Split(string(data), "\n") {
		fields := strings.Fields(line)
		if len(fields) < 5 {
			continue
		}
		if fields[4] == mountPoint {
			return fields[3], nil
		}
	}

	return "", fmt.Errorf("mount point %s not found in %s", mountPoint, procMountInfoPath)
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

func findMemoryCurrentByIdentifier(cgroupMountPath string, identifiers []string) (string, error) {
	type match struct {
		path  string
		idLen int
		depth int
	}

	best := match{}
	err := filepath.WalkDir(cgroupMountPath, func(path string, d os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.IsDir() || d.Name() != "memory.current" {
			return nil
		}

		dir := filepath.Dir(path)
		if dir == cgroupMountPath {
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
		return "", fmt.Errorf("failed to find container-specific memory.current under %s", cgroupMountPath)
	}

	return best.path, nil
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
