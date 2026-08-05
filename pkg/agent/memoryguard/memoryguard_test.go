package memoryguard

import (
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// mib converts a MiB count to bytes for readable test tables.
func mib(n int64) int64 { return n * 1024 * 1024 }

func TestResolveMemoryCurrentPathFromSelfCgroup(t *testing.T) {
	t.Parallel()

	cgroupRoot := t.TempDir()
	cgroupDir := filepath.Join(cgroupRoot, "docker", "abcdef1234567890")
	err := os.MkdirAll(cgroupDir, 0o755)
	if err != nil {
		t.Fatalf("failed to create fake cgroup dir: %v", err)
	}

	expectedPath := filepath.Join(cgroupDir, "memory.current")
	err = os.WriteFile(expectedPath, []byte("123"), 0o644)
	if err != nil {
		t.Fatalf("failed to create fake memory.current: %v", err)
	}

	procSelfCgroup := filepath.Join(t.TempDir(), "cgroup")
	err = os.WriteFile(procSelfCgroup, []byte("0::/docker/abcdef1234567890\n"), 0o644)
	if err != nil {
		t.Fatalf("failed to write fake /proc/self/cgroup: %v", err)
	}

	procMountInfo := filepath.Join(t.TempDir(), "mountinfo")
	err = os.WriteFile(procMountInfo, []byte("36 35 0:32 / "+cgroupRoot+" rw - cgroup2 cgroup rw\n"), 0o644)
	if err != nil {
		t.Fatalf("failed to write fake /proc/self/mountinfo: %v", err)
	}

	procMounts := filepath.Join(t.TempDir(), "mounts")
	err = os.WriteFile(procMounts, []byte("cgroup2 "+cgroupRoot+" cgroup2 rw 0 0\n"), 0o644)
	if err != nil {
		t.Fatalf("failed to write fake /proc/mounts: %v", err)
	}

	actualPath, _, err := resolveMemoryUsagePath(procMounts, procSelfCgroup, procMountInfo)
	if err != nil {
		t.Fatalf("resolveMemoryUsagePath returned error: %v", err)
	}

	if actualPath != expectedPath {
		t.Fatalf("expected %s, got %s", expectedPath, actualPath)
	}
}

func TestResolveMemoryCurrentPathFromRootSelfCgroup(t *testing.T) {
	t.Parallel()

	cgroupRoot := t.TempDir()
	expectedPath := filepath.Join(cgroupRoot, "memory.current")
	err := os.WriteFile(expectedPath, []byte("321"), 0o644)
	if err != nil {
		t.Fatalf("failed to create fake root memory.current: %v", err)
	}

	procSelfCgroup := filepath.Join(t.TempDir(), "cgroup")
	err = os.WriteFile(procSelfCgroup, []byte("0::/\n"), 0o644)
	if err != nil {
		t.Fatalf("failed to write fake /proc/self/cgroup: %v", err)
	}

	procMountInfo := filepath.Join(t.TempDir(), "mountinfo")
	err = os.WriteFile(procMountInfo, []byte("36 35 0:32 / "+cgroupRoot+" rw - cgroup2 cgroup rw\n"), 0o644)
	if err != nil {
		t.Fatalf("failed to write fake /proc/self/mountinfo: %v", err)
	}

	procMounts := filepath.Join(t.TempDir(), "mounts")
	err = os.WriteFile(procMounts, []byte("cgroup2 "+cgroupRoot+" cgroup2 rw 0 0\n"), 0o644)
	if err != nil {
		t.Fatalf("failed to write fake /proc/mounts: %v", err)
	}

	actualPath, _, err := resolveMemoryUsagePath(procMounts, procSelfCgroup, procMountInfo)
	if err != nil {
		t.Fatalf("resolveMemoryUsagePath returned error: %v", err)
	}

	if actualPath != expectedPath {
		t.Fatalf("expected %s, got %s", expectedPath, actualPath)
	}
}

func TestResolveMemoryCurrentPathFallsBackToContainerIdentifierSearch(t *testing.T) {
	t.Parallel()

	containerID := strings.Repeat("a", 64)
	cgroupRoot := t.TempDir()
	cgroupDir := filepath.Join(cgroupRoot, "kubepods.slice", "pod-1", "cri-containerd-"+containerID+".scope")
	err := os.MkdirAll(cgroupDir, 0o755)
	if err != nil {
		t.Fatalf("failed to create fake container cgroup dir: %v", err)
	}

	expectedPath := filepath.Join(cgroupDir, "memory.current")
	err = os.WriteFile(expectedPath, []byte("456"), 0o644)
	if err != nil {
		t.Fatalf("failed to create fake memory.current: %v", err)
	}

	procSelfCgroup := filepath.Join(t.TempDir(), "cgroup")
	err = os.WriteFile(procSelfCgroup, []byte("0::/\n"), 0o644)
	if err != nil {
		t.Fatalf("failed to write fake /proc/self/cgroup: %v", err)
	}

	procMountInfo := filepath.Join(t.TempDir(), "mountinfo")
	mountInfo := "36 35 0:32 / " + cgroupRoot + " rw - cgroup2 cgroup rw /var/lib/docker/containers/" + containerID + "/hostname\n"
	err = os.WriteFile(procMountInfo, []byte(mountInfo), 0o644)
	if err != nil {
		t.Fatalf("failed to write fake /proc/self/mountinfo: %v", err)
	}

	procMounts := filepath.Join(t.TempDir(), "mounts")
	err = os.WriteFile(procMounts, []byte("cgroup2 "+cgroupRoot+" cgroup2 rw 0 0\n"), 0o644)
	if err != nil {
		t.Fatalf("failed to write fake /proc/mounts: %v", err)
	}

	actualPath, _, err := resolveMemoryUsagePath(procMounts, procSelfCgroup, procMountInfo)
	if err != nil {
		t.Fatalf("resolveMemoryUsagePath returned error: %v", err)
	}

	if actualPath != expectedPath {
		t.Fatalf("expected %s, got %s", expectedPath, actualPath)
	}
}

func TestResolveMemoryUsagePathFromSelfCgroupV1(t *testing.T) {
	t.Parallel()

	cgroupRoot := t.TempDir()
	memoryMount := filepath.Join(cgroupRoot, "memory")
	cgroupDir := filepath.Join(memoryMount, "system.slice", "docker-abcdef1234567890.scope")
	err := os.MkdirAll(cgroupDir, 0o755)
	if err != nil {
		t.Fatalf("failed to create fake cgroup v1 dir: %v", err)
	}

	expectedPath := filepath.Join(cgroupDir, "memory.usage_in_bytes")
	err = os.WriteFile(expectedPath, []byte("789"), 0o644)
	if err != nil {
		t.Fatalf("failed to create fake memory.usage_in_bytes: %v", err)
	}

	procSelfCgroup := filepath.Join(t.TempDir(), "cgroup")
	err = os.WriteFile(procSelfCgroup, []byte("8:memory:/system.slice/docker-abcdef1234567890.scope\n"), 0o644)
	if err != nil {
		t.Fatalf("failed to write fake /proc/self/cgroup: %v", err)
	}

	procMountInfo := filepath.Join(t.TempDir(), "mountinfo")
	err = os.WriteFile(procMountInfo, []byte("36 35 0:32 / "+memoryMount+" rw - cgroup cgroup rw,memory\n"), 0o644)
	if err != nil {
		t.Fatalf("failed to write fake /proc/self/mountinfo: %v", err)
	}

	procMounts := filepath.Join(t.TempDir(), "mounts")
	err = os.WriteFile(procMounts, []byte("cgroup "+memoryMount+" cgroup rw,memory 0 0\n"), 0o644)
	if err != nil {
		t.Fatalf("failed to write fake /proc/mounts: %v", err)
	}

	actualPath, layout, err := resolveMemoryUsagePath(procMounts, procSelfCgroup, procMountInfo)
	if err != nil {
		t.Fatalf("resolveMemoryUsagePath returned error: %v", err)
	}

	if layout.version != cgroupV1 {
		t.Fatalf("expected cgroup v1 layout, got v%d", layout.version)
	}
	if actualPath != expectedPath {
		t.Fatalf("expected %s, got %s", expectedPath, actualPath)
	}
}

func TestResetAllPressureClearsRecordingPause(t *testing.T) {
	applyPausedState(true)
	t.Cleanup(resetAllPressure)

	if !IsRecordingPaused() {
		t.Fatal("expected recording to be paused after applying pressure state")
	}

	resetAllPressure()
	if IsRecordingPaused() {
		t.Fatal("expected resetAllPressure to clear the paused state")
	}
}

func TestBuildMountedCgroupPath(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name       string
		mountPoint string
		mountRoot  string
		cgroupPath string
		usageFile  string
		wantPath   string
		wantOK     bool
	}{
		{
			name:       "root mount root cgroup",
			mountPoint: "/sys/fs/cgroup",
			mountRoot:  "/",
			cgroupPath: "/",
			usageFile:  "memory.current",
			wantPath:   "/sys/fs/cgroup/memory.current",
			wantOK:     true,
		},
		{
			name:       "root mount nested cgroup",
			mountPoint: "/sys/fs/cgroup",
			mountRoot:  "/",
			cgroupPath: "/docker/abcdef123456",
			usageFile:  "memory.current",
			wantPath:   "/sys/fs/cgroup/docker/abcdef123456/memory.current",
			wantOK:     true,
		},
		{
			name:       "exact non root match",
			mountPoint: "/sys/fs/cgroup",
			mountRoot:  "/docker/abcdef123456",
			cgroupPath: "/docker/abcdef123456",
			usageFile:  "memory.current",
			wantPath:   "/sys/fs/cgroup/memory.current",
			wantOK:     true,
		},
		{
			name:       "non root nested match",
			mountPoint: "/sys/fs/cgroup",
			mountRoot:  "/kubepods.slice",
			cgroupPath: "/kubepods.slice/pod-1/container-1",
			usageFile:  "memory.current",
			wantPath:   "/sys/fs/cgroup/pod-1/container-1/memory.current",
			wantOK:     true,
		},
		{
			name:       "mismatched cgroup path",
			mountPoint: "/sys/fs/cgroup",
			mountRoot:  "/kubepods.slice",
			cgroupPath: "/docker/abcdef123456",
			usageFile:  "memory.current",
			wantOK:     false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotPath, gotOK := buildMountedCgroupPath(tt.mountPoint, tt.mountRoot, tt.cgroupPath, tt.usageFile)
			if gotOK != tt.wantOK {
				t.Fatalf("expected ok=%v, got %v", tt.wantOK, gotOK)
			}
			if gotPath != tt.wantPath {
				t.Fatalf("expected path %q, got %q", tt.wantPath, gotPath)
			}
		})
	}
}

func TestEffectiveUsage(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		current      int64
		limit        int64
		overhead     int64
		wantCurrent  int64
		wantLimit    int64
		wantOverhead int64
	}{
		{
			name:         "proxy mode: zero overhead is a no-op",
			current:      mib(400),
			limit:        mib(1000),
			overhead:     0,
			wantCurrent:  mib(400),
			wantLimit:    mib(1000),
			wantOverhead: 0,
		},
		{
			name:         "low-latency: ringbuf discounted from usage and limit",
			current:      mib(500), // 244 real + 256 ringbuf
			limit:        mib(1000),
			overhead:     mib(256),
			wantCurrent:  mib(244),
			wantLimit:    mib(744),
			wantOverhead: mib(256),
		},
		{
			name:         "overhead at/above limit falls back to raw guarding",
			current:      mib(500),
			limit:        mib(200),
			overhead:     mib(256),
			wantCurrent:  mib(500),
			wantLimit:    mib(200),
			wantOverhead: 0,
		},
		{
			name:         "negative overhead treated as zero",
			current:      mib(300),
			limit:        mib(1000),
			overhead:     -1,
			wantCurrent:  mib(300),
			wantLimit:    mib(1000),
			wantOverhead: 0,
		},
		{
			name:         "effective current is floored at zero",
			current:      mib(100),
			limit:        mib(1000),
			overhead:     mib(256),
			wantCurrent:  0,
			wantLimit:    mib(744),
			wantOverhead: mib(256),
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()

			gotCurrent, gotLimit, gotOverhead := effectiveUsage(tt.current, tt.limit, tt.overhead)
			if gotCurrent != tt.wantCurrent {
				t.Errorf("effCurrent: want %d, got %d", tt.wantCurrent, gotCurrent)
			}
			if gotLimit != tt.wantLimit {
				t.Errorf("effLimit: want %d, got %d", tt.wantLimit, gotLimit)
			}
			if gotOverhead != tt.wantOverhead {
				t.Errorf("effOverhead: want %d, got %d", tt.wantOverhead, gotOverhead)
			}
		})
	}
}

// TestEffectiveUsageDrivesPauseBoundary documents the end-to-end intent: pause
// when the reclaimable usage fills the configured ratio of the room left above
// the fixed floor. With a 1 GiB limit and a 256 MiB ring buffer, the pause line
// is 0.80 * (1024-256) = 614.4 MiB of reclaimable usage.
func TestEffectiveUsageDrivesPauseBoundary(t *testing.T) {
	t.Parallel()

	limit := mib(1024)
	overhead := mib(256)

	// Just below the boundary: 600 MiB reclaimable + 256 ringbuf = 856 total.
	effCurrent, effLimit, _ := effectiveUsage(mib(600)+overhead, limit, overhead)
	if pause := thresholdBytes(effLimit, pauseThresholdRatio); effCurrent >= pause {
		t.Fatalf("did not expect pause: effCurrent=%d pause=%d", effCurrent, pause)
	}

	// Just above the boundary: 700 MiB reclaimable + 256 ringbuf = 956 total.
	effCurrent, effLimit, _ = effectiveUsage(mib(700)+overhead, limit, overhead)
	if pause := thresholdBytes(effLimit, pauseThresholdRatio); effCurrent < pause {
		t.Fatalf("expected pause: effCurrent=%d pause=%d", effCurrent, pause)
	}
}

func TestSetFixedOverheadMB(t *testing.T) {
	t.Cleanup(func() { fixedOverheadBytes.Store(0) })

	SetFixedOverheadMB(256)
	if got, want := FixedOverheadBytes(), mib(256); got != want {
		t.Fatalf("after SetFixedOverheadMB(256): want %d, got %d", want, got)
	}

	SetFixedOverheadMB(0)
	if got := FixedOverheadBytes(); got != 0 {
		t.Fatalf("after SetFixedOverheadMB(0): want 0, got %d", got)
	}

	// An implausibly large value must be ignored (no overflow), leaving the
	// previous value untouched.
	SetFixedOverheadMB(64)
	SetFixedOverheadMB(math.MaxUint64)
	if got, want := FixedOverheadBytes(), mib(64); got != want {
		t.Fatalf("overflow input should be ignored: want %d, got %d", want, got)
	}
}
