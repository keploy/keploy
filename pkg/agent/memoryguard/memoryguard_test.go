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

func TestEvaluateMemory(t *testing.T) {
	t.Parallel()

	tests := []struct {
		name         string
		current      int64
		limit        int64
		overhead     int64
		wantCurrent  int64
		wantLimit    int64
		wantOverhead int64
		wantViable   bool
	}{
		{
			name: "proxy mode: zero overhead", current: mib(400), limit: mib(1000), overhead: 0,
			wantCurrent: mib(400), wantLimit: mib(1000), wantOverhead: 0, wantViable: true,
		},
		{
			name:    "low-latency: ringbuf discounted from usage and limit",
			current: mib(500), limit: mib(1000), overhead: mib(256),
			wantCurrent: mib(244), wantLimit: mib(744), wantOverhead: mib(256), wantViable: true,
		},
		{
			name: "effective current floored at zero", current: mib(100), limit: mib(1000), overhead: mib(256),
			wantCurrent: 0, wantLimit: mib(744), wantOverhead: mib(256), wantViable: true,
		},
		{
			name: "negative overhead treated as zero", current: mib(300), limit: mib(1000), overhead: -1,
			wantCurrent: mib(300), wantLimit: mib(1000), wantOverhead: 0, wantViable: true,
		},
		{
			// overhead >= limit is NON-viable (not silently zeroed — that would
			// reintroduce the startup-pause bug), so the guard holds paused.
			name: "overhead above limit is non-viable", current: mib(500), limit: mib(200), overhead: mib(256),
			wantCurrent: 0, wantLimit: mib(200) - mib(256), wantOverhead: mib(256), wantViable: false,
		},
		{
			name: "budget below resume headroom is non-viable", current: mib(300), limit: mib(400), overhead: mib(256),
			wantCurrent: 0, wantLimit: mib(144), wantOverhead: mib(256), wantViable: false,
		},
	}

	for _, tt := range tests {
		tt := tt
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			d := evaluateMemory(tt.current, tt.limit, tt.overhead)
			if d.effCurrent != tt.wantCurrent {
				t.Errorf("effCurrent: want %d, got %d", tt.wantCurrent, d.effCurrent)
			}
			if d.effLimit != tt.wantLimit {
				t.Errorf("effLimit: want %d, got %d", tt.wantLimit, d.effLimit)
			}
			if d.overhead != tt.wantOverhead {
				t.Errorf("overhead: want %d, got %d", tt.wantOverhead, d.overhead)
			}
			if d.viable != tt.wantViable {
				t.Errorf("viable: want %v, got %v", tt.wantViable, d.viable)
			}
			if d.viable {
				// Invariants: hysteresis ordering and both absolute headroom floors.
				if d.resumeThreshold >= d.pauseThreshold {
					t.Errorf("resume (%d) must be below pause (%d)", d.resumeThreshold, d.pauseThreshold)
				}
				if hr := d.effLimit - d.pauseThreshold; hr < int64(minPauseHeadroomBytes) {
					t.Errorf("pause headroom %d below floor %d", hr, minPauseHeadroomBytes)
				}
				if hr := d.effLimit - d.resumeThreshold; hr < int64(minResumeHeadroomBytes) {
					t.Errorf("resume headroom %d below floor %d", hr, minResumeHeadroomBytes)
				}
			} else if d.pauseThreshold != 0 || d.resumeThreshold != 0 {
				t.Errorf("non-viable must leave thresholds zero, got pause=%d resume=%d", d.pauseThreshold, d.resumeThreshold)
			}
		})
	}
}

// TestEvaluateMemoryRatioDominatesLargeBudget: for a large budget the percentage
// thresholds win (0.2/0.3 of the budget exceed the absolute floors), so proxy
// mode is provably unchanged.
func TestEvaluateMemoryRatioDominatesLargeBudget(t *testing.T) {
	t.Parallel()
	d := evaluateMemory(mib(500), mib(1000), 0)
	if d.pauseThreshold != mib(800) { // 0.8 * 1000
		t.Errorf("pause: want %d, got %d", mib(800), d.pauseThreshold)
	}
	if d.resumeThreshold != mib(700) { // 0.7 * 1000
		t.Errorf("resume: want %d, got %d", mib(700), d.resumeThreshold)
	}
}

// TestEvaluateMemoryAbsoluteHeadroomFloor is the A4 fix: for a small budget the
// percentage margin would be a dangerously small absolute number, so the
// absolute headroom floor binds instead (>=128MiB pause / >=192MiB resume).
func TestEvaluateMemoryAbsoluteHeadroomFloor(t *testing.T) {
	t.Parallel()
	// effLimit = 500MiB: 0.2*500=100MiB < 128MiB pause floor, 0.3*500=150MiB < 192MiB resume floor.
	d := evaluateMemory(mib(300), mib(500), 0)
	if !d.viable {
		t.Fatal("expected viable")
	}
	if hr := d.effLimit - d.pauseThreshold; hr != int64(minPauseHeadroomBytes) {
		t.Errorf("pause headroom: want floor %d, got %d", minPauseHeadroomBytes, hr)
	}
	if hr := d.effLimit - d.resumeThreshold; hr != int64(minResumeHeadroomBytes) {
		t.Errorf("resume headroom: want floor %d, got %d", minResumeHeadroomBytes, hr)
	}
}

// TestEvaluateMemoryGoMemFloor: a tiny budget floors GOMEMLIMIT rather than
// telling the runtime to hold the heap in a few MiB.
func TestEvaluateMemoryGoMemFloor(t *testing.T) {
	t.Parallel()
	d := evaluateMemory(0, mib(60), 0) // 0.9*60 = 54MiB < 64MiB floor
	if !d.goMemFloored || d.goMemTarget != int64(minGoMemLimitBytes) {
		t.Errorf("want floored GOMEMLIMIT %d, got target=%d floored=%v", minGoMemLimitBytes, d.goMemTarget, d.goMemFloored)
	}
	big := evaluateMemory(0, mib(1000), 0) // 0.9*1000 well above floor
	if big.goMemFloored {
		t.Error("large budget should not floor GOMEMLIMIT")
	}
}

func TestSetFixedOverheadMB(t *testing.T) {
	t.Cleanup(func() { fixedOverheadBytes.Store(0) })

	if got := SetFixedOverheadMB(256); got != mib(256) || FixedOverheadBytes() != mib(256) {
		t.Fatalf("SetFixedOverheadMB(256): returned %d, stored %d", got, FixedOverheadBytes())
	}
	if got := SetFixedOverheadMB(0); got != 0 || FixedOverheadBytes() != 0 {
		t.Fatalf("SetFixedOverheadMB(0): returned %d, stored %d", got, FixedOverheadBytes())
	}
	// Negative clears the discount (no huge-positive conversion).
	SetFixedOverheadMB(64)
	if got := SetFixedOverheadMB(-5); got != 0 || FixedOverheadBytes() != 0 {
		t.Fatalf("SetFixedOverheadMB(-5): returned %d, stored %d", got, FixedOverheadBytes())
	}
	// Implausibly large clears rather than overflowing.
	SetFixedOverheadMB(64)
	if got := SetFixedOverheadMB(math.MaxInt); got != 0 || FixedOverheadBytes() != 0 {
		t.Fatalf("SetFixedOverheadMB(MaxInt): returned %d, stored %d", got, FixedOverheadBytes())
	}
}
