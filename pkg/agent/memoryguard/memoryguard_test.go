package memoryguard

import (
	"context"
	"errors"
	"math"
	"os"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"runtime/metrics"
	"strings"
	"sync"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
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
			// Proxy mode is viable at ANY positive limit — the absolute floor and
			// non-viability rules are low-latency-only and must not touch it.
			name: "proxy mode: small limit is still viable", current: mib(50), limit: mib(128), overhead: 0,
			wantCurrent: mib(50), wantLimit: mib(128), wantOverhead: 0, wantViable: true,
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
			// reintroduce the startup-pause bug). effCurrent is still computed so
			// the pause log carries real usage.
			name: "overhead above limit is non-viable", current: mib(500), limit: mib(200), overhead: mib(256),
			wantCurrent: mib(244), wantLimit: mib(200) - mib(256), wantOverhead: mib(256), wantViable: false,
		},
		{
			// effLimit 144 MiB < the 320 MiB viable floor → non-viable.
			name: "budget below viable floor is non-viable", current: mib(300), limit: mib(400), overhead: mib(256),
			wantCurrent: mib(44), wantLimit: mib(144), wantOverhead: mib(256), wantViable: false,
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
				// The absolute headroom floors apply to low-latency (overhead>0)
				// only; proxy mode uses the plain ratios with no floor.
				if tt.overhead > 0 {
					if hr := d.effLimit - d.pauseThreshold; hr < int64(minPauseHeadroomBytes) {
						t.Errorf("pause headroom %d below floor %d", hr, minPauseHeadroomBytes)
					}
					if hr := d.effLimit - d.resumeThreshold; hr < int64(minResumeHeadroomBytes) {
						t.Errorf("resume headroom %d below floor %d", hr, minResumeHeadroomBytes)
					}
				}
			} else if d.pauseThreshold != 0 || d.resumeThreshold != 0 {
				t.Errorf("non-viable must leave thresholds zero, got pause=%d resume=%d", d.pauseThreshold, d.resumeThreshold)
			}
		})
	}
}

// TestEvaluateMemoryProxyUsesPlainRatios: proxy mode (overhead 0) uses the plain
// pause/resume ratios at ANY limit, with no absolute floor and no non-viability
// — provably the pre-discount behaviour.
func TestEvaluateMemoryProxyUsesPlainRatios(t *testing.T) {
	t.Parallel()
	// Large budget.
	d := evaluateMemory(mib(500), mib(1000), 0)
	if d.pauseThreshold != mib(800) || d.resumeThreshold != mib(700) {
		t.Errorf("large: pause/resume want 800/700, got %d/%d", d.pauseThreshold, d.resumeThreshold)
	}
	// Small budget: still plain ratios (0.8/0.7 × 200), still viable — the
	// low-latency floors must NOT bind in proxy mode.
	s := evaluateMemory(mib(50), mib(200), 0)
	if !s.viable || s.pauseThreshold != mib(160) || s.resumeThreshold != mib(140) {
		t.Errorf("small: viable=%v pause/resume want 160/140, got %d/%d", s.viable, s.pauseThreshold, s.resumeThreshold)
	}
}

// TestEvaluateMemoryAbsoluteHeadroomFloor is the A4 fix: for a small LOW-LATENCY
// budget the percentage margin would be a dangerously small absolute number, so
// the absolute headroom floor binds instead (>=128MiB pause / >=192MiB resume).
func TestEvaluateMemoryAbsoluteHeadroomFloor(t *testing.T) {
	t.Parallel()
	// limit 756 − 256 ringbuf = effLimit 500MiB: 0.2*500=100MiB < 128 pause floor,
	// 0.3*500=150MiB < 192 resume floor, so both floors bind.
	d := evaluateMemory(mib(400), mib(756), mib(256))
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

// TestMinViableLowLatencyLimitBytes: the exported floor k8s-proxy derives its
// record-start check from = overhead + the minimum usable budget.
func TestMinViableLowLatencyLimitBytes(t *testing.T) {
	t.Parallel()
	if got, want := MinViableLowLatencyLimitBytes(mib(256)), mib(256)+int64(minViableEffLimitBytes); got != want {
		t.Errorf("overhead 256Mi: want %d, got %d", want, got)
	}
	if got := MinViableLowLatencyLimitBytes(-1); got != int64(minViableEffLimitBytes) {
		t.Errorf("negative overhead: want %d, got %d", int64(minViableEffLimitBytes), got)
	}
	// A low-latency limit at exactly this floor must be viable; one byte under
	// must not — pinning the validator and the guard to the same boundary.
	floor := MinViableLowLatencyLimitBytes(mib(256))
	if d := evaluateMemory(0, floor, mib(256)); !d.viable {
		t.Errorf("limit at floor should be viable, effLimit=%d", d.effLimit)
	}
	if d := evaluateMemory(0, floor-1, mib(256)); d.viable {
		t.Errorf("limit one byte under floor should be non-viable, effLimit=%d", d.effLimit)
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

// heap is what the Go runtime reports of its heap, in MiB, after some GCs.
func heap(objects, live, free, goal int64) goMemory {
	return goMemory{objects: mib(objects), live: mib(live), free: mib(free), goal: mib(goal), cycles: 7, ok: true}
}

func TestHeldAndNext(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name               string
		ws                 int64
		m                  goMemory
		active             int64
		wantHeld, wantNext int64
	}{
		// A GC of 630 -> 630 -> 163 MB at a recorded app's exec: all but the
		// live 163 MiB is garbage; the next GC is due now.
		{name: "garbage before its collection", ws: mib(911), m: heap(630, 163, 0, 630), wantHeld: mib(444), wantNext: mib(911)},
		// The collection has freed it, and the runtime keeps it as free heap.
		{name: "garbage freed and kept as free heap", ws: mib(911), m: heap(170, 163, 460, 326), wantHeld: mib(444), wantNext: mib(911)},
		// The heap grows into its free memory first, then on to the goal.
		{name: "growth until the next GC is due", ws: mib(600), m: heap(300, 200, 50, 500), wantHeld: mib(450), wantNext: mib(750)},
		{name: "live memory is held", ws: mib(600), m: heap(400, 400, 0, 800), wantHeld: mib(600), wantNext: mib(1000)},
		// Active page cache is held, as the working set holds it, but the
		// kernel reclaims it before the limit: next leaves it out.
		{name: "active page cache", ws: mib(600), active: mib(40), m: heap(300, 200, 50, 500), wantHeld: mib(450), wantNext: mib(710)},
		// Before the first GC nothing has been found live.
		{name: "no GC yet", ws: mib(300), m: goMemory{objects: mib(200), free: mib(20), goal: mib(4), ok: true}, wantHeld: mib(280), wantNext: mib(300)},
		{name: "a live figure over the objects", ws: mib(300), m: heap(100, 120, 0, 240), wantHeld: mib(300), wantNext: mib(440)},
		{name: "never negative", ws: mib(50), m: heap(300, 100, 0, 300), wantHeld: 0, wantNext: mib(50)},
		// Unreadable: the working set alone, as the guard judged it before.
		{name: "unreadable figures", ws: mib(500), m: goMemory{objects: mib(400), live: mib(10), goal: mib(900), cycles: 3}, wantHeld: mib(500), wantNext: mib(500)},
	}
	for _, tt := range tests {
		held, next := heldAndNext(tt.ws, tt.active, tt.m)
		if held != tt.wantHeld || next != tt.wantNext {
			t.Errorf("%s: held %d MiB, next %d MiB; want %d, %d", tt.name, held>>20, next>>20, tt.wantHeld>>20, tt.wantNext>>20)
		}
	}
}

// TestEvaluateMemoryCeiling: the ceiling keeps a quarter of the room the pause
// point leaves under the budget.
func TestEvaluateMemoryCeiling(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name                   string
		limit, overhead, pause int64
		ceiling                int64
	}{
		{name: "1 GiB sidecar, 64 MiB ring buffer", limit: mib(1024), overhead: mib(64), pause: mib(768), ceiling: mib(912)},
		{name: "512 MiB proxy mode", limit: mib(512), overhead: 0, pause: 429496729, ceiling: 510027367},
		{name: "4 GiB proxy mode", limit: mib(4096), overhead: 0, pause: 3435973836, ceiling: 4080218931},
		{name: "the 128 MiB headroom floor binds", limit: mib(512), overhead: mib(64), pause: mib(320), ceiling: mib(416)},
	}
	for _, tt := range tests {
		d := evaluateMemory(0, tt.limit, tt.overhead)
		if d.pauseThreshold != tt.pause || d.ceiling != tt.ceiling {
			t.Errorf("%s: pause point %d, ceiling %d; want %d, %d", tt.name, d.pauseThreshold, d.ceiling, tt.pause, tt.ceiling)
		}
	}
}

// fakeMemory is a working set, the container's active page cache and memory
// limit, and the Go runtime's figures, as a row sets them; the collections
// the guard forces, and the GOMEMLIMITs it installs, with when (since
// simStart).
type fakeMemory struct {
	ws        int64
	active    int64 // active page cache
	limit     int64 // the container's memory limit; 0: none
	noStat    bool  // memory.stat unreadable
	readErr   error // the cgroup unreadable
	m         goMemory
	onReclaim func(*fakeMemory) // what a forced collection does; nil: nothing
	reclaimAt []time.Duration
	installs  []int64
	installAt []time.Duration
}

func (f *fakeMemory) set(ws int64, m goMemory) { f.ws, f.m = ws, m }

func (f *fakeMemory) forced() int { return len(f.reclaimAt) }

// simStart is where the fake clock of simulate starts.
var simStart = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)

// fakeGuard is a guard on a limitMB budget with an overheadMB ring buffer
// that reads f and installs its GOMEMLIMITs in f; the row puts the overhead
// back.
func fakeGuard(t *testing.T, limitMB, overheadMB int64, f *fakeMemory) *guard {
	t.Helper()
	prevOverhead := fixedOverheadBytes.Swap(mib(overheadMB))
	prevLimit := debug.SetMemoryLimit(-1)
	t.Cleanup(func() {
		fixedOverheadBytes.Store(prevOverhead)
		debug.SetMemoryLimit(prevLimit)
		resetAllPressure()
	})
	g := &guard{logger: zap.NewNop(), limitBytes: mib(limitMB), memoryLimitMB: uint64(limitMB), prevMemLimit: prevLimit}
	g.readCgroup = func() (cgroupSample, error) {
		if f.readErr != nil {
			return cgroupSample{}, f.readErr
		}
		return cgroupSample{workingSet: f.ws, activeFile: f.active, statOK: !f.noStat, limit: f.limit}, nil
	}
	g.setMemoryLimit = func(b int64) int64 {
		f.installs = append(f.installs, b)
		f.installAt = append(f.installAt, g.timeNow().Sub(simStart))
		return 0
	}
	g.readGoMemory = func() goMemory { return f.m }
	g.reclaim = func() {
		f.reclaimAt = append(f.reclaimAt, g.timeNow().Sub(simStart))
		if f.onReclaim != nil {
			f.onReclaim(f)
		}
	}
	return g
}

// pauseEdge is a change of the paused state, as a pressure hook saw it.
type pauseEdge struct {
	paused bool
	after  time.Duration // since run() started
}

// simulate runs g's real run() for d on a fake clock that starts at
// simStart: each wait run() asks for moves the clock on by it at once, and
// then at (if not nil) sets the figures for the sample at that time. It
// returns every change of the paused state, at the time it happened (run's
// own reset on stopping is not one), and the waits run() asked for, in
// order. Nothing sleeps and nothing runs concurrently: every time in a row
// is exact.
func simulate(t *testing.T, g *guard, d time.Duration, at func(time.Duration)) (edges []pauseEdge, waits []time.Duration) {
	t.Helper()
	now := simStart
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	paused, stopping := false, false
	g.clock = func() time.Time { return now }
	g.wait = func(w time.Duration) <-chan time.Time {
		waits = append(waits, w)
		if now.Add(w).Sub(simStart) > d {
			stopping = true
			cancel()
			return nil
		}
		now = now.Add(w)
		if at != nil {
			at(now.Sub(simStart))
		}
		c := make(chan time.Time, 1)
		c <- now
		return c
	}
	defer RegisterPressureHook(func(p bool) {
		if p != paused && !stopping {
			edges = append(edges, pauseEdge{paused: p, after: now.Sub(simStart)})
			paused = p
		}
	})()
	g.run(ctx)
	return edges, waits
}

// phase is figures a row sets once, at the first sample at or after from.
type phase struct {
	from time.Duration
	ws   int64
	m    goMemory
}

// phases is simulate's at for figures that change at set times.
func phases(f *fakeMemory, ps ...phase) func(time.Duration) {
	return func(now time.Duration) {
		for len(ps) > 0 && now >= ps[0].from {
			f.set(ps[0].ws, ps[0].m)
			ps = ps[1:]
		}
	}
}

// guardClock is the guard's clock for its logger: a log line carries the
// time the guard sampled at.
type guardClock struct{ g *guard }

func (c guardClock) Now() time.Time                         { return c.g.timeNow() }
func (c guardClock) NewTicker(d time.Duration) *time.Ticker { return time.NewTicker(d) }

// observe makes g log to the returned lines, from level up.
func observe(g *guard, level zapcore.Level) *observer.ObservedLogs {
	core, logs := observer.New(level)
	g.logger = zap.New(core, zap.WithClock(guardClock{g}))
	return logs
}

// twoSamples runs a guard for two samples.
const twoSamples = 2*defaultCheckInterval + 200*time.Millisecond

// TestGarbageOverThePausePointNeitherPausesNorCollects: a working set over
// the pause point whose excess the runtime's last GC did not find live, with
// the next GC due under the ceiling, keeps recording and forces no collection.
func TestGarbageOverThePausePointNeitherPausesNorCollects(t *testing.T) {
	tests := []struct {
		name            string
		limitMB, ringMB int64
		ws              int64
		m               goMemory
	}{
		// CI 9937's sidecar at its exec: 911 MiB against the 832 MiB pause
		// point, during the GC of 630 -> 630 -> 163 MB.
		{name: "1 GiB sidecar, garbage before its collection", limitMB: 1024, ringMB: 64, ws: mib(911), m: heap(630, 163, 0, 630)},
		{name: "1 GiB sidecar, garbage kept as free heap", limitMB: 1024, ringMB: 64, ws: mib(911), m: heap(170, 163, 460, 326)},
		// Held 800 MiB raw is 736 MiB of the budget once the ring buffer is
		// discounted, under the 768 MiB pause point.
		{name: "1 GiB sidecar, held under the pause point once the ring buffer is discounted", limitMB: 1024, ringMB: 64, ws: mib(900), m: heap(400, 300, 0, 400)},
		// The runtime keeps a 512 MiB proxy agent at its 460.8 MiB GOMEMLIMIT.
		{name: "512 MiB proxy mode at its soft limit", limitMB: 512, ws: mib(480), m: heap(330, 150, 0, 330)},
		{name: "4 GiB proxy mode", limitMB: 4096, ws: mib(3800), m: heap(3600, 1700, 100, 3700)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeMemory{ws: tt.ws, m: tt.m}
			g := fakeGuard(t, tt.limitMB, tt.ringMB, f)
			if d := evaluateMemory(tt.ws, mib(tt.limitMB), mib(tt.ringMB)); d.effCurrent < d.pauseThreshold {
				t.Fatalf("setup: working set %d MiB is not over the pause point", tt.ws>>20)
			}
			if edges, _ := simulate(t, g, twoSamples, nil); len(edges) != 0 {
				t.Errorf("recording paused on garbage: %v", edges)
			}
			if n := f.forced(); n != 0 {
				t.Errorf("the guard forced %d collection(s) while recording", n)
			}
		})
	}
}

// TestLiveMemoryOverThePausePointPauses: what the last GC found live, with
// the memory a collection cannot free, at the pause point pauses on that
// sample; the pause collects once, as before, and not again within
// reclaimCooldown.
func TestLiveMemoryOverThePausePointPauses(t *testing.T) {
	tests := []struct {
		name            string
		limitMB, ringMB int64
		ws              int64
		m               goMemory
	}{
		{name: "512 MiB proxy mode", limitMB: 512, ws: mib(480), m: heap(330, 320, 0, 460)},
		{name: "1 GiB sidecar", limitMB: 1024, ringMB: 64, ws: mib(900), m: heap(600, 580, 0, 830)},
		// Live memory with the runtime's free heap released: the working set is held.
		{name: "4 GiB proxy mode", limitMB: 4096, ws: mib(3300), m: heap(3200, 3200, 0, 3600)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeMemory{ws: tt.ws, m: tt.m}
			g := fakeGuard(t, tt.limitMB, tt.ringMB, f)
			edges, _ := simulate(t, g, twoSamples, nil)
			if len(edges) != 1 || !edges[0].paused || edges[0].after != defaultCheckInterval {
				t.Fatalf("pause edges %v; want one pause on the first sample", edges)
			}
			if n := f.forced(); n != 1 {
				t.Errorf("%d collections; want 1, on pausing", n)
			}
		})
	}
}

// TestARuntimeGCDueOnlyAtTheCeilingPausesAtThePausePoint: when the working
// set is over the pause point and the runtime's next GC is not due until the
// working set reaches the ceiling, the guard pauses on the working set as
// before; under the pause point, or with the next GC due under the ceiling,
// it does not.
func TestARuntimeGCDueOnlyAtTheCeilingPausesAtThePausePoint(t *testing.T) {
	tests := []struct {
		name            string
		limitMB, ringMB int64
		ws              int64
		m               goMemory
		pause           bool
	}{
		// 512 MiB proxy mode: pause point 409.6 MiB, ceiling 486.4 MiB.
		{name: "512 MiB: next GC due at 520 MiB", limitMB: 512, ws: mib(420), m: heap(200, 150, 0, 300), pause: true},
		{name: "512 MiB: next GC due at 486 MiB, under the ceiling", limitMB: 512, ws: mib(420), m: heap(200, 150, 0, 266), pause: false},
		{name: "512 MiB: next GC due at 487 MiB, over the ceiling", limitMB: 512, ws: mib(420), m: heap(200, 150, 0, 267), pause: true},
		{name: "512 MiB: the working set at the ceiling", limitMB: 512, ws: mib(490), m: heap(330, 150, 0, 330), pause: true},
		{name: "512 MiB: under the pause point", limitMB: 512, ws: mib(400), m: heap(200, 150, 0, 350), pause: false},
		// 1 GiB sidecar: pause point 832 MiB raw, ceiling 976 MiB raw.
		{name: "1 GiB sidecar: next GC due at 1140 MiB", limitMB: 1024, ringMB: 64, ws: mib(840), m: heap(400, 300, 0, 700), pause: true},
		{name: "1 GiB sidecar: next GC due at 975 MiB", limitMB: 1024, ringMB: 64, ws: mib(840), m: heap(400, 300, 0, 535), pause: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeMemory{ws: tt.ws, m: tt.m}
			g := fakeGuard(t, tt.limitMB, tt.ringMB, f)
			edges, _ := simulate(t, g, twoSamples, nil)
			if !tt.pause {
				if len(edges) != 0 || f.forced() != 0 {
					t.Errorf("pause edges %v, %d collections; want none", edges, f.forced())
				}
				return
			}
			if len(edges) != 1 || !edges[0].paused || edges[0].after != defaultCheckInterval {
				t.Fatalf("pause edges %v; want one pause on the first sample", edges)
			}
			if n := f.forced(); n != 1 {
				t.Errorf("%d collections; want 1, on pausing", n)
			}
		})
	}
}

// TestTheCeilingBoundsALiveRampTheRuntimeHasNotYetMarked: with a container
// limit, and so a derived soft limit, a GC that runs long while live memory
// grows past its goal leaves held memory stale; the working set reaching the
// ceiling pauses recording on the first sample that sees it, under the
// container's limit. These are the runtime model's shapes where, with no
// ceiling rule under a derived limit, the working set went to 1118 MiB on
// the 1 GiB sidecar (128 MiB/s, GCs of 1.5 s) and to 5062 MiB on 4 GiB
// (1 GiB/s).
func TestTheCeilingBoundsALiveRampTheRuntimeHasNotYetMarked(t *testing.T) {
	tests := []struct {
		name                     string
		limitMB, ringMB, nonGoMB int64
		live, goal, objects0     int64 // MiB: what the GC running marked last, its goal, the heap at the start
		rateMBs                  int64 // live growth
		pauseAt                  time.Duration
	}{
		// Soft limit 708 MiB; pause point 832 MiB raw, ceiling 976 MiB raw:
		// the working set is 972.8 MiB at 1100 ms, 979.2 MiB at 1150 ms.
		{name: "1 GiB sidecar, 128 MiB/s", limitMB: 1024, ringMB: 64, nonGoMB: 252, live: 400, goal: 677, objects0: 560, rateMBs: 128,
			pauseAt: 1150 * time.Millisecond},
		// Soft limit 3686.4 MiB (90% of the budget); pause point 3276.8 MiB,
		// ceiling 3891.2 MiB: 3871.6 MiB at 900 ms, 3922.8 MiB at 950 ms.
		{name: "4 GiB proxy mode, 1 GiB/s", limitMB: 4096, nonGoMB: 50, live: 1700, goal: 3575, objects0: 2880, rateMBs: 1024,
			pauseAt: 950 * time.Millisecond},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			const goNonHeap = 20
			figures := func(f *fakeMemory, now time.Duration) {
				objects := mib(tt.objects0) + mib(tt.rateMBs)*now.Milliseconds()/1000
				f.ws = mib(tt.nonGoMB) + objects + mib(goNonHeap)
				f.m = goMemory{objects: objects, live: mib(tt.live), goal: mib(tt.goal), mapped: objects + mib(goNonHeap), cycles: 9, ok: true}
			}
			f := &fakeMemory{limit: mib(tt.limitMB)}
			figures(f, 0)
			g := fakeGuard(t, tt.limitMB, tt.ringMB, f)
			var wsAtPause int64
			defer RegisterPressureHook(func(p bool) {
				if p && wsAtPause == 0 {
					wsAtPause = f.ws
				}
			})()
			edges, _ := simulate(t, g, 2*time.Second, func(now time.Duration) { figures(f, now) })
			if len(edges) == 0 {
				t.Fatalf("no pause in 2 s: the working set reached %d MiB, the container's limit is %d MiB", f.ws>>20, tt.limitMB)
			}
			if !edges[0].paused || edges[0].after != tt.pauseAt {
				t.Errorf("pause edges %v; want the first at %v, the first sample with the working set at the ceiling", edges, tt.pauseAt)
			}
			if wsAtPause >= mib(tt.limitMB) {
				t.Errorf("paused with the working set at %d MiB, at or over the container's %d MiB", wsAtPause>>20, tt.limitMB)
			}
		})
	}
}

// TestResumeJudgesHeldMemory: a pause on live memory lifts once held memory
// is back at the resume point, even with the working set over the pause
// point on free heap the runtime keeps, unless the next GC is due only at the
// ceiling; then the working set must come down too, as before.
func TestResumeJudgesHeldMemory(t *testing.T) {
	tests := []struct {
		name   string
		ws     int64
		m      goMemory
		resume bool
	}{
		// 512 MiB proxy mode: resume point 358.4 MiB.
		{name: "held at the resume point, free heap kept", ws: mib(450), m: heap(160, 150, 140, 300), resume: true},
		{name: "held over the resume point", ws: mib(450), m: heap(240, 230, 60, 300), resume: false},
		{name: "held at the resume point, next GC due over the ceiling", ws: mib(400), m: heap(200, 150, 0, 300), resume: false},
		{name: "held and the working set at the resume point", ws: mib(350), m: heap(160, 150, 0, 400), resume: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Live memory pauses; the pause's collection leaves the case's figures.
			f := &fakeMemory{ws: mib(480), m: heap(330, 320, 0, 460)}
			f.onReclaim = func(f *fakeMemory) { f.ws, f.m = tt.ws, tt.m }
			g := fakeGuard(t, 512, 0, f)
			edges, _ := simulate(t, g, twoSamples, nil)
			if len(edges) == 0 || !edges[0].paused {
				t.Fatalf("setup: live memory did not pause: %v", edges)
			}
			resumed := len(edges) == 2 && !edges[1].paused && edges[1].after == 2*defaultCheckInterval
			if resumed != tt.resume || len(edges) > 2 {
				t.Errorf("pause edges %v; want resumed on the next sample: %v", edges, tt.resume)
			}
			if n := f.forced(); n != 1 {
				t.Errorf("%d collections; want 1, on pausing", n)
			}
		})
	}
}

// TestUnreadableRuntimeFiguresJudgeTheWorkingSet: without the runtime's
// figures the guard judges the working set alone, as before.
func TestUnreadableRuntimeFiguresJudgeTheWorkingSet(t *testing.T) {
	for _, tt := range []struct {
		ws    int64
		pause bool
	}{{ws: mib(420), pause: true}, {ws: mib(400), pause: false}} {
		f := &fakeMemory{ws: tt.ws}
		g := fakeGuard(t, 512, 0, f)
		edges, _ := simulate(t, g, twoSamples, nil)
		if paused := len(edges) == 1 && edges[0].paused; paused != tt.pause || len(edges) > 1 {
			t.Errorf("working set %d MiB: pause edges %v; want paused: %v", tt.ws>>20, edges, tt.pause)
		}
	}
}

// TestAPausedGuardCollectsEveryCooldown: while paused, the guard collects
// again at the first sample reclaimCooldown after its last collection, as
// before.
func TestAPausedGuardCollectsEveryCooldown(t *testing.T) {
	f := &fakeMemory{ws: mib(480), m: heap(330, 320, 0, 460)}
	g := fakeGuard(t, 512, 0, f)
	g.underPressure = true
	g.lastReclaim = simStart.Add(-reclaimCooldown + 700*time.Millisecond)
	simulate(t, g, twoSamples, nil)
	if len(f.reclaimAt) != 1 || f.reclaimAt[0] != 2*defaultCheckInterval {
		t.Errorf("collections at %v; want one, at the sample at %v", f.reclaimAt, 2*defaultCheckInterval)
	}
}

// TestPersistentReadFailuresStopTheGuard: a guard whose cgroup cannot be
// read on 20 samples running (10 s) lifts its pause and stops sampling, as
// before, and GoMemoryBudget is 0 again: no guard runs.
func TestPersistentReadFailuresStopTheGuard(t *testing.T) {
	t.Cleanup(SetGoMemoryBudgetForTest(0))
	f := &fakeMemory{ws: mib(480), m: heap(330, 320, 0, 460)}
	g := fakeGuard(t, 512, 0, f)
	var budget int64
	edges, waits := simulate(t, g, 30*time.Second, func(now time.Duration) {
		if now == time.Second {
			budget = GoMemoryBudget() // the 500 ms sample's, which read the cgroup
		}
		if now >= time.Second {
			f.readErr = errors.New("unreadable")
		}
	})
	if want := int64(float64(mib(512)) * 0.9); budget != want {
		t.Errorf("GoMemoryBudget after the 500 ms sample: %d MiB; want %d MiB", budget>>20, want>>20)
	}
	if got := GoMemoryBudget(); got != 0 {
		t.Errorf("GoMemoryBudget once the guard stopped itself: %d MiB; want 0", got>>20)
	}
	if len(edges) != 2 || !edges[0].paused || edges[0].after != defaultCheckInterval || edges[1].paused || edges[1].after != 10500*time.Millisecond {
		t.Errorf("pause edges %v; want paused at 500 ms, lifted at the 20th failed sample, 10.5 s", edges)
	}
	if len(waits) != 21 {
		t.Errorf("%d waits; want 21: the guard stops sampling after its 20th failed sample", len(waits))
	}
}

// TestReadGoMemoryStatsReadsTheRuntime: the figures come from the runtime,
// what Go has mapped among them: its free heap in, its released heap out.
func TestReadGoMemoryStatsReadsTheRuntime(t *testing.T) {
	keep := make([][]byte, 32)
	for i := range keep {
		keep[i] = make([]byte, 1<<20)
	}
	// 96 MiB dropped, and a GC that returns it to the OS: released heap,
	// which GOMEMLIMIT does not count.
	garbage(96)
	debug.FreeOSMemory()
	// 48 MiB of it taken again and dropped, then a GC: free heap, which it does.
	garbage(48)
	runtime.GC()
	m := readGoMemoryStats()
	mapped, released := goManaged(), heapReleased()
	runtime.KeepAlive(keep)
	if !m.ok || m.cycles == 0 || m.live < mib(32) || m.objects < mib(32) || m.goal <= m.live {
		t.Fatalf("runtime figures %+v: want readable, after a GC, at least the 32 MiB kept live, a goal over it", m)
	}
	if m.free < mib(16) || released < mib(16) {
		t.Fatalf("setup: free heap %d MiB, released %d MiB; want 16 MiB of each", m.free>>20, released>>20)
	}
	if m.mapped > mapped+mib(4) || m.mapped < mapped-mib(4) {
		t.Fatalf("mapped %d MiB (objects %d, free %d, released %d); want what Go has mapped and not released, %d MiB",
			m.mapped>>20, m.objects>>20, m.free>>20, released>>20, mapped>>20)
	}
}

// garbage allocates n MiB in 1 MiB pieces and drops them.
func garbage(n int) {
	b := make([][]byte, n)
	for i := range b {
		b[i] = make([]byte, 1<<20)
	}
	runtime.KeepAlive(b)
}

// goManaged is what the Go runtime has mapped and not released: the part of a
// process's working set its heap and collections change.
func goManaged() int64 {
	s := []metrics.Sample{{Name: "/memory/classes/total:bytes"}, {Name: "/memory/classes/heap/released:bytes"}}
	metrics.Read(s)
	return int64(s[0].Value.Uint64() - s[1].Value.Uint64())
}

func heapReleased() int64 {
	s := []metrics.Sample{{Name: "/memory/classes/heap/released:bytes"}}
	metrics.Read(s)
	return int64(s[0].Value.Uint64())
}

func forcedGCs() uint64 {
	s := []metrics.Sample{{Name: "/gc/cycles/forced:gc-cycles"}}
	metrics.Read(s)
	return s[0].Value.Uint64()
}

// realGuard is a guard on a 512 MiB proxy-mode budget (pause point 409.6
// MiB) over the real runtime: its working set is otherMB of memory Go does
// not manage plus this process's Go memory, in a container with a
// containerMB memory limit (0: none); the runtime's figures, its GOMEMLIMIT,
// the collections and the clock are real. GOMEMLIMIT is in force from the
// start, as Start installs it before run: 90% of the budget, then the limit
// derived from the budget. It reports the highest working set it read.
func realGuard(t *testing.T, otherMB, containerMB int64) (*guard, func() int64) {
	t.Helper()
	prevOverhead := fixedOverheadBytes.Swap(0)
	prevLimit := debug.SetMemoryLimit(-1)
	prevGC := debug.SetGCPercent(100)
	t.Cleanup(func() {
		debug.SetGCPercent(prevGC)
		debug.SetMemoryLimit(prevLimit)
		fixedOverheadBytes.Store(prevOverhead)
		resetAllPressure()
	})
	var mu sync.Mutex
	var maxWS int64
	g := &guard{
		logger: zap.NewNop(), limitBytes: mib(512), memoryLimitMB: 512, prevMemLimit: prevLimit,
		readCgroup: func() (cgroupSample, error) {
			ws := mib(otherMB) + goManaged()
			mu.Lock()
			defer mu.Unlock()
			maxWS = max(maxWS, ws)
			return cgroupSample{workingSet: ws, statOK: true, limit: mib(containerMB)}, nil
		},
	}
	// Start's sample, of this process alone: no earlier row's heap in it,
	// and not among the working sets reported.
	debug.FreeOSMemory()
	g.softLimit = evaluateMemory(0, g.limitBytes, 0).goMemTarget
	debug.SetMemoryLimit(g.softLimit)
	g.sampleSoftLimit()
	maxWS = 0
	return g, func() int64 {
		mu.Lock()
		defer mu.Unlock()
		return maxWS
	}
}

// runGuardForEdges runs g's real run() in real time for d and returns every
// change of the paused state until it stops it (run's own reset on stopping
// is not one). applyPausedState calls the hooks synchronously, so no edge is
// missed, however short.
func runGuardForEdges(t *testing.T, g *guard, d time.Duration) []pauseEdge {
	t.Helper()
	var mu sync.Mutex
	var edges []pauseEdge
	paused, stopping := false, false
	start := time.Now()
	defer RegisterPressureHook(func(p bool) {
		mu.Lock()
		defer mu.Unlock()
		if p != paused && !stopping {
			edges = append(edges, pauseEdge{paused: p, after: time.Since(start)})
			paused = p
		}
	})()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { g.run(ctx); close(done) }()
	time.Sleep(d)
	mu.Lock()
	stopping = true
	mu.Unlock()
	cancel()
	<-done
	mu.Lock()
	defer mu.Unlock()
	return append([]pauseEdge(nil), edges...)
}

// churn allocates 1 MiB pieces for d, holding only the current 10 MiB batch,
// and takes no piece while the heap is at its goal: there the runtime's GC is
// behind, and churn waits for it to end. Go's mark assists would hold an
// allocator there, but they scale with the heap's pointers, and a heap of
// byte slices has next to none. So on a loaded host, where the GC's workers
// wait for a CPU while it marks (210 ms of marking for 1 ms of its CPU, seen),
// an unpaced churn takes the heap past its goal and Go's memory over its
// GOMEMLIMIT, and the GC counts live all it allocates meanwhile
// (/gc/heap/live), as far as the load lets the GC fall behind. Paced, the
// heap stays at its goal, and a GC counts live at most what churn allocates
// between the GC's start and the goal, whatever the load.
func churn(d time.Duration) {
	for end := time.Now().Add(d); time.Now().Before(end); {
		batch := make([][]byte, 10)
		for i := 0; i < len(batch) && time.Now().Before(end); {
			if m := readGoMemoryStats(); m.ok && m.objects >= m.goal {
				time.Sleep(time.Millisecond)
				continue
			}
			batch[i] = make([]byte, 1<<20)
			i++
		}
		runtime.KeepAlive(batch)
		time.Sleep(5 * time.Millisecond)
	}
}

// TestRealGarbageOverThePausePointNeitherPausesNorCollects: on the real
// runtime, GOGC 100 and the GOMEMLIMIT the guard installs, with no container
// limit and 120 MiB not Go's, garbage churned over 180 MiB of live memory
// takes the working set over the pause point and neither pauses recording
// nor makes the guard collect. The limit derived from --memory-limit (350.4
// MiB) has the runtime's next GC due under the ceiling, as a container's
// limit would. 90% of the budget (460.8 MiB) would not: GOGC's goal, twice
// what the last GC marked (190 MiB), has the working set due over the 486.4
// MiB ceiling at the next GC, and the guard pause recording on garbage. The
// churn keeps the heap at its goal (churn), so the row's figures are the
// same on a loaded host.
func TestRealGarbageOverThePausePointNeitherPausesNorCollects(t *testing.T) {
	g, maxWS := realGuard(t, 120, 0)
	debug.FreeOSMemory()
	live := make([][]byte, 180)
	for i := range live {
		live[i] = make([]byte, 1<<20)
	}
	f0 := forcedGCs()
	var edges []pauseEdge
	done := make(chan struct{})
	go func() { edges = runGuardForEdges(t, g, 3*time.Second); close(done) }()
	churn(2700 * time.Millisecond)
	<-done
	forced := forcedGCs() - f0
	runtime.KeepAlive(live)
	if maxWS() < int64(float64(mib(512))*pauseThresholdRatio) {
		t.Fatalf("setup: the working set peaked at %d MiB, never over the 409.6 MiB pause point", maxWS()>>20)
	}
	if len(edges) != 0 {
		t.Errorf("recording paused on garbage: %v (working set up to %d MiB)", edges, maxWS()>>20)
	}
	if forced != 0 {
		t.Errorf("%d forced collection(s) while recording", forced)
	}
}

// TestRealLiveGrowthPausesAtTheFirstGCThatFindsIt: on the real runtime,
// live memory grown over the pause point among churned garbage pauses
// recording at the guard's first sample after a GC has found it live, and
// no collection is forced before that pause. Samples are counted, not
// timed: the sample that pauses is at most the first one the guard begins
// after the row has seen the GC's figures.
func TestRealLiveGrowthPausesAtTheFirstGCThatFindsIt(t *testing.T) {
	g, maxWS := realGuard(t, 120, 0)
	var mu sync.Mutex
	samples := 0
	g.readGoMemory = func() goMemory {
		mu.Lock()
		samples++
		mu.Unlock()
		return readGoMemoryStats()
	}
	debug.FreeOSMemory()
	live := make([][]byte, 0, 320)
	for len(live) < 100 {
		live = append(live, make([]byte, 1<<20))
	}
	pausedAt, crossedAt := 0, 0 // samples begun by then
	var forcedBefore uint64
	f0 := forcedGCs()
	defer RegisterPressureHook(func(p bool) {
		mu.Lock()
		defer mu.Unlock()
		if p && pausedAt == 0 {
			pausedAt, forcedBefore = samples, forcedGCs()-f0
		}
	})()
	var edges []pauseEdge
	done := make(chan struct{})
	go func() { edges = runGuardForEdges(t, g, 4*time.Second); close(done) }()
	time.Sleep(600 * time.Millisecond)
	// 200 MiB of live growth in 400 ms: 120 + 300 MiB live and Go's own memory
	// are over the 409.6 MiB pause point.
	for len(live) < 300 {
		for i := 0; i < 10; i++ {
			live = append(live, make([]byte, 1<<20))
		}
		time.Sleep(20 * time.Millisecond)
	}
	grown := time.Now()
	for time.Since(grown) < 2500*time.Millisecond {
		churn(50 * time.Millisecond)
		if m := readGoMemoryStats(); mib(120)+m.live >= int64(float64(mib(512))*pauseThresholdRatio) {
			mu.Lock()
			if crossedAt == 0 {
				crossedAt = max(samples, 1)
			}
			mu.Unlock()
		}
	}
	<-done
	runtime.KeepAlive(live)
	mu.Lock()
	defer mu.Unlock()
	if pausedAt == 0 || crossedAt == 0 {
		t.Fatalf("live growth never paused (edges %v, a GC found it live: %v, working set up to %d MiB)", edges, crossedAt != 0, maxWS()>>20)
	}
	if forcedBefore != 0 {
		t.Errorf("%d collection(s) forced before the pause", forcedBefore)
	}
	if pausedAt > crossedAt+1 {
		t.Errorf("paused at sample %d; a GC had found the live memory over the pause point by sample %d", pausedAt, crossedAt)
	}
}

// TestTheGuardSamplesQuicklyOnlyWhileWaitingForTheRuntime: recording with
// the working set over the pause point, waiting for the runtime's next GC,
// the guard samples every bandCheckInterval; under the pause point, and
// while paused, every defaultCheckInterval as before.
func TestTheGuardSamplesQuicklyOnlyWhileWaitingForTheRuntime(t *testing.T) {
	tests := []struct {
		name    string
		ws      int64
		m       goMemory
		afterWS int64 // what the pause's collection leaves; 0: unchanged
		after   goMemory
		quick   bool
	}{
		{name: "over the pause point, waiting for the runtime's next GC", ws: mib(480), m: heap(330, 150, 0, 330), quick: true},
		{name: "under the pause point", ws: mib(400), m: heap(250, 150, 0, 300), quick: false},
		{name: "paused on live memory", ws: mib(480), m: heap(330, 320, 0, 460), quick: false},
		// Held between the resume and pause points, the working set over the
		// pause point: still paused, not waiting for the runtime.
		{name: "paused, held over the resume point", ws: mib(480), m: heap(330, 320, 0, 460), afterWS: mib(450), after: heap(240, 230, 60, 300), quick: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeMemory{ws: tt.ws, m: tt.m}
			if tt.afterWS != 0 {
				f.onReclaim = func(f *fakeMemory) { f.ws, f.m = tt.afterWS, tt.after }
			}
			g := fakeGuard(t, 512, 0, f)
			_, waits := simulate(t, g, twoSamples, nil)
			want := defaultCheckInterval
			if tt.quick {
				want = bandCheckInterval
			}
			if len(waits) < 3 || waits[0] != defaultCheckInterval {
				t.Fatalf("waits %v; want the first sample at %v", waits, defaultCheckInterval)
			}
			for _, w := range waits[1:] {
				if w != want {
					t.Fatalf("waits %v; want %v after the first sample", waits, want)
				}
			}
		})
	}
}

// TestAResumeOverThePausePointSamplesQuickly: a guard that resumes with the
// working set still over the pause point (held memory back at the resume
// point, the runtime's next GC due under the ceiling) is then recording and
// waiting for that GC: it samples every bandCheckInterval from the sample
// that resumes, not a defaultCheckInterval later.
func TestAResumeOverThePausePointSamplesQuickly(t *testing.T) {
	// 512 MiB proxy mode: live memory pauses at 500 ms; the pause's
	// collection leaves 300 MiB held and the working set at 450 MiB, over the
	// 409.6 MiB pause point, the next GC due at 450 MiB.
	f := &fakeMemory{ws: mib(480), m: heap(330, 320, 0, 460)}
	f.onReclaim = func(f *fakeMemory) { f.ws, f.m = mib(450), heap(160, 150, 140, 300) }
	g := fakeGuard(t, 512, 0, f)
	edges, waits := simulate(t, g, 1300*time.Millisecond, nil)
	if len(edges) != 2 || !edges[0].paused || edges[0].after != 500*time.Millisecond || edges[1].paused || edges[1].after != time.Second {
		t.Fatalf("pause edges %v; want paused at 500 ms, resumed at 1 s", edges)
	}
	want := []time.Duration{defaultCheckInterval, defaultCheckInterval, bandCheckInterval, bandCheckInterval}
	for i, w := range want {
		if i >= len(waits) || waits[i] != w {
			t.Fatalf("waits %v; want them to begin %v", waits, want)
		}
	}
}

// TestWhileWaitingTheGuardSeesWithinAQuickSample: while the guard waits for
// the runtime's next GC, the working set reaching the ceiling, or a GC that
// finds the memory live, pauses at the next quick sample, not at the next
// regular one.
func TestWhileWaitingTheGuardSeesWithinAQuickSample(t *testing.T) {
	tests := []struct {
		name string
		ws   int64
		m    goMemory
	}{
		{name: "the working set reaches the ceiling", ws: mib(490), m: heap(330, 150, 0, 330)},
		{name: "a GC finds the memory live", ws: mib(480), m: heap(330, 320, 0, 460)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// 512 MiB proxy mode: garbage over the pause point, the next GC due
			// under the ceiling; the change at 820 ms. The next regular sample
			// would be at 1 s; the next quick one is at 850 ms.
			f := &fakeMemory{ws: mib(480), m: heap(330, 150, 0, 330)}
			g := fakeGuard(t, 512, 0, f)
			edges, _ := simulate(t, g, 1300*time.Millisecond, phases(f, phase{820 * time.Millisecond, tt.ws, tt.m}))
			if len(edges) != 1 || !edges[0].paused || edges[0].after != 850*time.Millisecond {
				t.Fatalf("pause edges %v; want one pause, at 850 ms", edges)
			}
		})
	}
}

// TestReadCgroupSample: the working set, the active page cache and the memory
// limit, as cgroup v2 and v1 write them.
func TestReadCgroupSample(t *testing.T) {
	t.Parallel()
	const stat = "anon 100\ninactive_file 30\nactive_file 20\ntotal_inactive_file 999\ntotal_active_file 999\n"
	tests := []struct {
		name    string
		version int
		stat    string // "": no memory.stat
		limit   string // "": no limit file
		want    cgroupSample
	}{
		{name: "v2 with a limit", version: cgroupV2, stat: stat, limit: "1073741824\n", want: cgroupSample{workingSet: 470, activeFile: 20, statOK: true, limit: 1 << 30}},
		{name: "v2 without one", version: cgroupV2, stat: stat, limit: "max\n", want: cgroupSample{workingSet: 470, activeFile: 20, statOK: true}},
		{name: "v1 with a limit", version: cgroupV1, stat: stat, limit: "536870912", want: cgroupSample{workingSet: 470, activeFile: 20, statOK: true, limit: 512 << 20}},
		{name: "v1 without one", version: cgroupV1, stat: stat, limit: "9223372036854771712\n", want: cgroupSample{workingSet: 470, activeFile: 20, statOK: true}},
		{name: "no limit file", version: cgroupV2, stat: stat, want: cgroupSample{workingSet: 470, activeFile: 20, statOK: true}},
		{name: "a limit that is not a byte count", version: cgroupV2, stat: stat, limit: "lots", want: cgroupSample{workingSet: 470, activeFile: 20, statOK: true}},
		{name: "no memory.stat", version: cgroupV2, limit: "1073741824", want: cgroupSample{workingSet: 500, limit: 1 << 30}},
	}
	for _, tt := range tests {
		dir := t.TempDir()
		usage := "memory.current"
		if tt.version == cgroupV1 {
			usage = "memory.usage_in_bytes"
		}
		write := func(name, data string) {
			if err := os.WriteFile(filepath.Join(dir, name), []byte(data), 0o600); err != nil {
				t.Fatal(err)
			}
		}
		write(usage, "500\n")
		if tt.stat != "" {
			write("memory.stat", tt.stat)
		}
		if tt.limit != "" {
			write(filepath.Base(limitPathFor(filepath.Join(dir, usage), tt.version)), tt.limit)
		}
		got, err := readCgroupSample(filepath.Join(dir, usage), limitPathFor(filepath.Join(dir, usage), tt.version))
		if err != nil || got != tt.want {
			t.Errorf("%s: %+v, %v; want %+v", tt.name, got, err, tt.want)
		}
	}
	if v2, v1 := limitPathFor("/c/memory.current", cgroupV2), limitPathFor("/c/memory.usage_in_bytes", cgroupV1); v2 != "/c/memory.max" || v1 != "/c/memory.limit_in_bytes" {
		t.Errorf("limit files %q, %q", v2, v1)
	}
	if _, err := readCgroupSample(filepath.Join(t.TempDir(), "memory.current"), ""); err == nil {
		t.Error("no usage file read without an error")
	}
}

// goFig is the Go runtime's figures, in MiB, after some GCs: heap objects,
// what the last GC marked live, free heap, and all Go has mapped and not
// released (heap and the rest); the goal is twice the live heap.
func goFig(objects, live, free, mapped int64) goMemory {
	return goMemory{objects: mib(objects), live: mib(live), free: mib(free), goal: mib(2 * live), mapped: mib(mapped), cycles: 7, ok: true}
}

// withGoal is m with the heap goal goalMB.
func withGoal(m goMemory, goalMB int64) goMemory {
	m.goal = mib(goalMB)
	return m
}

// TestDeriveSoftLimit: the GOMEMLIMIT is the room the budget (the container's
// limit, or --memory-limit where that is lower or the container has none)
// leaves Go, never over 90% of the budget nor under Go's live memory plus 64
// MiB; the room is short only where it is under that and under 90% of the
// budget.
func TestDeriveSoftLimit(t *testing.T) {
	t.Parallel()
	// A 1 GiB sidecar with a 64 MiB ring buffer: margin 48 + 16 MiB. Go holds
	// 230 MiB no GC can free (400 mapped, 20 free, 150 unmarked).
	sidecar := goFig(350, 200, 20, 400)
	ninety := func(budgetMB int64) int64 { return int64(float64(mib(budgetMB)) * 0.9) }
	tests := []struct {
		name                string
		limitMB, overheadMB int64 // --memory-limit, ring buffer
		cs                  cgroupSample
		m                   goMemory
		want                int64
		derived, short      bool
		wantNonGo, wantHard int64
	}{
		{name: "1 GiB sidecar, 252 MiB not Go's", limitMB: 1024, overheadMB: 64, m: sidecar,
			cs:   cgroupSample{workingSet: mib(652), statOK: true, limit: mib(1024)},
			want: mib(708), derived: true, wantNonGo: mib(252), wantHard: mib(1024)},
		{name: "page cache is not memory Go does not manage", limitMB: 1024, overheadMB: 64, m: sidecar,
			cs:   cgroupSample{workingSet: mib(802), activeFile: mib(150), statOK: true, limit: mib(1024)},
			want: mib(708), derived: true, wantNonGo: mib(252), wantHard: mib(1024)},
		{name: "memory Go does not manage grown by 100 MiB", limitMB: 1024, overheadMB: 64, m: sidecar,
			cs:   cgroupSample{workingSet: mib(752), statOK: true, limit: mib(1024)},
			want: mib(608), derived: true, wantNonGo: mib(352), wantHard: mib(1024)},
		{name: "a cgroup limit over --memory-limit", limitMB: 1024, overheadMB: 64, m: sidecar,
			cs:   cgroupSample{workingSet: mib(652), statOK: true, limit: mib(2048)},
			want: mib(708), derived: true, wantNonGo: mib(252), wantHard: mib(1024)},
		// The budget is the container's 768 MiB: margin 35.2 + 16 MiB.
		{name: "a cgroup limit under --memory-limit", limitMB: 1024, overheadMB: 64, m: sidecar,
			cs:   cgroupSample{workingSet: mib(652), statOK: true, limit: mib(768)},
			want: mib(768) - mib(252) - (mib(704)-thresholdBytes(mib(704), pauseThresholdRatio))/4 - softLimitSlack, derived: true, wantNonGo: mib(252), wantHard: mib(768)},
		// 4 GiB proxy mode: margin 204.8 + 16 MiB. The room is over 90% of
		// the budget with 50 MiB not Go's, under it with 600.
		{name: "4 GiB: never over 90% of the budget", limitMB: 4096, m: goFig(1500, 1200, 50, 1600),
			cs:   cgroupSample{workingSet: mib(1650), statOK: true, limit: mib(4096)},
			want: ninety(4096), derived: true, wantNonGo: mib(50), wantHard: mib(4096)},
		{name: "4 GiB, 600 MiB not Go's", limitMB: 4096, m: goFig(1500, 1200, 50, 1600),
			cs:   cgroupSample{workingSet: mib(2200), statOK: true, limit: mib(4096)},
			want: mib(4096) - mib(600) - (mib(4096)-thresholdBytes(mib(4096), pauseThresholdRatio))/4 - softLimitSlack, derived: true, wantNonGo: mib(600), wantHard: mib(4096)},
		// Live memory under the pause point, as recording ordinarily runs,
		// on three budgets: under the room with 64 MiB to spare, or the 90%
		// of the budget installed.
		{name: "1 GiB sidecar, 560 MiB live: 570 MiB no GC can free, under the 708 MiB room with 64 MiB", limitMB: 1024, overheadMB: 64,
			m: goFig(600, 560, 10, 620), cs: cgroupSample{workingSet: mib(872), statOK: true, limit: mib(1024)},
			want: mib(708), derived: true, wantNonGo: mib(252), wantHard: mib(1024)},
		{name: "2 GiB proxy mode, 1550 MiB live", limitMB: 2048, m: goFig(1580, 1550, 10, 1610),
			cs:   cgroupSample{workingSet: mib(1660), statOK: true, limit: mib(2048)},
			want: ninety(2048), derived: true, wantNonGo: mib(50), wantHard: mib(2048)},
		{name: "4 GiB proxy mode, 3100 MiB live", limitMB: 4096, m: goFig(3150, 3100, 20, 3200),
			cs:   cgroupSample{workingSet: mib(3250), statOK: true, limit: mib(4096)},
			want: ninety(4096), derived: true, wantNonGo: mib(50), wantHard: mib(4096)},
		// 512 MiB proxy mode, 20 MiB not Go's: margin 25.6 + 16 MiB, a room of
		// 450.4 MiB; 365 MiB no GC can free.
		{name: "512 MiB proxy mode, 360 MiB live", limitMB: 512, m: goFig(370, 360, 5, 380),
			cs:   cgroupSample{workingSet: mib(400), statOK: true, limit: mib(512)},
			want: mib(512) - mib(20) - (mib(512)-thresholdBytes(mib(512), pauseThresholdRatio))/4 - softLimitSlack, derived: true, wantNonGo: mib(20), wantHard: mib(512)},
		// Go's live memory with its floor over the room, and the room over 90%
		// of the budget: the budget, not the container, is what is short.
		{name: "the floor over a room that is over 90% of the budget", limitMB: 4096, m: goFig(3820, 3800, 10, 3840),
			cs:   cgroupSample{workingSet: mib(3890), statOK: true, limit: mib(4096)},
			want: ninety(4096), derived: true, wantNonGo: mib(50), wantHard: mib(4096)},
		// 350 MiB not Go's leaves Go 610 MiB; 610 MiB no GC can free: a floor
		// of 674 MiB, under 90% of the budget (864 MiB).
		{name: "a floor over the room, under 90% of the budget", limitMB: 1024, overheadMB: 64, m: goFig(610, 600, 0, 620),
			cs:   cgroupSample{workingSet: mib(970), statOK: true, limit: mib(1024)},
			want: mib(610) + mib(64), derived: true, short: true, wantNonGo: mib(350), wantHard: mib(1024)},
		{name: "a floor over the room and over 90% of the budget", limitMB: 1024, overheadMB: 64, m: goFig(810, 800, 0, 820),
			cs:   cgroupSample{workingSet: mib(1020), statOK: true, limit: mib(1024)},
			want: ninety(960), derived: true, short: true, wantNonGo: mib(200), wantHard: mib(1024)},
		// 700 MiB not Go's leaves Go 260 MiB; it holds 320 MiB no GC can free.
		{name: "a container too small for Go's live memory", limitMB: 1024, overheadMB: 64, m: goFig(330, 300, 10, 360),
			cs:   cgroupSample{workingSet: mib(1060), statOK: true, limit: mib(1024)},
			want: mib(320) + mib(64), derived: true, short: true, wantNonGo: mib(700), wantHard: mib(1024)},
		// The sidecar's 252 MiB not Go's, against --memory-limit: 90% of it,
		// 864 MiB, would have Go's next GC due at 1116 MiB, over its 976 MiB
		// ceiling.
		{name: "no container limit: the room --memory-limit leaves Go", limitMB: 1024, overheadMB: 64, m: sidecar,
			cs:   cgroupSample{workingSet: mib(652), statOK: true},
			want: mib(708), derived: true, wantNonGo: mib(252), wantHard: mib(1024)},
		{name: "no container limit, short", limitMB: 1024, overheadMB: 64, m: goFig(330, 300, 10, 360),
			cs:   cgroupSample{workingSet: mib(1060), statOK: true},
			want: mib(320) + mib(64), derived: true, short: true, wantNonGo: mib(700), wantHard: mib(1024)},
		{name: "memory.stat unreadable", limitMB: 1024, overheadMB: 64, m: sidecar,
			cs:   cgroupSample{workingSet: mib(652), limit: mib(1024)},
			want: ninety(960)},
		{name: "runtime figures unreadable", limitMB: 1024, overheadMB: 64, m: goMemory{mapped: mib(400)},
			cs:   cgroupSample{workingSet: mib(652), statOK: true, limit: mib(1024)},
			want: ninety(960)},
		{name: "a budget too small to record", limitMB: 512, overheadMB: 256, m: sidecar,
			cs:   cgroupSample{workingSet: mib(652), statOK: true, limit: mib(512)},
			want: ninety(256)},
	}
	for _, tt := range tests {
		// The budget run() judges against: the lower of the container's
		// limit and --memory-limit.
		g := &guard{logger: zap.NewNop(), limitBytes: mib(tt.limitMB)}
		dec := evaluateMemory(tt.cs.workingSet, g.budget(tt.cs), mib(tt.overheadMB))
		s := deriveSoftLimit(dec, tt.cs, tt.m)
		if s.bytes != tt.want || s.derived != tt.derived || s.short != tt.short ||
			(tt.derived && (s.nonGo != tt.wantNonGo || s.hard != tt.wantHard)) {
			t.Errorf("%s: %d MiB (%d B), derived %v, short %v, not Go's %d MiB, limit %d MiB; want %d MiB (%d B), %v, %v, %d, %d",
				tt.name, s.bytes>>20, s.bytes, s.derived, s.short, s.nonGo>>20, s.hard>>20,
				tt.want>>20, tt.want, tt.derived, tt.short, tt.wantNonGo>>20, tt.wantHard>>20)
		}
		if s.derived && s.bytes < min(s.need, dec.goMemTarget) {
			t.Errorf("%s: %d MiB under the %d MiB of Go memory no collection can free", tt.name, s.bytes>>20, s.need>>20)
		}
		if s.bytes > dec.goMemTarget {
			t.Errorf("%s: %d MiB over 90%% of the budget", tt.name, s.bytes>>20)
		}
		if s.short && s.bytes <= s.room {
			t.Errorf("%s: short, but %d MiB is not over the %d MiB room", tt.name, s.bytes>>20, s.room>>20)
		}
	}
}

// limitPhase is the container's memory limit from a time on (0: none, or
// unreadable).
type limitPhase struct {
	from  time.Duration
	limit int64
}

// sidecarMemory is a 1 GiB sidecar with a 64 MiB ring buffer: 252 MiB not
// Go's (the ring buffer among it) plus Go's 400 MiB: a soft limit of 708 MiB.
func sidecarMemory() *fakeMemory {
	return &fakeMemory{ws: mib(652), limit: mib(1024), m: goFig(350, 200, 20, 400)}
}

// TestTheGuardDerivesTheSoftLimitFromTheContainer: run() installs the
// derived GOMEMLIMIT on its first sample, from --memory-limit where the
// container has no limit or loses it, lowers it on the next sample as
// memory Go does not manage grows, raises it at the first sample
// reclaimCooldown after the last change, and leaves it alone as that memory
// comes and goes by a few MiB.
func TestTheGuardDerivesTheSoftLimitFromTheContainer(t *testing.T) {
	over90 := goFig(810, 800, 0, 820)
	ms := time.Millisecond
	tests := []struct {
		name   string
		limit  int64 // the container's; -1: none
		ws     int64 // 0: the sidecar's
		m      *goMemory
		phases []phase // the working set from then on
		limits []limitPhase
		run    time.Duration
		want   []int64
		at     []time.Duration
	}{
		{name: "the first sample installs it", run: twoSamples, want: []int64{mib(708)}, at: []time.Duration{500 * ms}},
		// 810 MiB no GC can free and 200 MiB not Go's: a floor over 90% of
		// the budget, which is installed once.
		{name: "Go's live memory with its floor over 90% of the budget", ws: mib(1020), m: &over90, run: twoSamples,
			want: []int64{int64(float64(mib(960)) * 0.9)}, at: []time.Duration{500 * ms}},
		{name: "no container limit: the room --memory-limit leaves Go", limit: -1, run: twoSamples,
			want: []int64{mib(708)}, at: []time.Duration{500 * ms}},
		// The sidecar's limit is --memory-limit's: losing it leaves the room.
		{name: "the container's limit stops being readable: the same room, from --memory-limit", run: 2200 * ms,
			limits: []limitPhase{{1200 * ms, 0}},
			want:   []int64{mib(708)}, at: []time.Duration{500 * ms}},
		{name: "a container limit read, then lost: the same room throughout", limit: -1, run: 3200 * ms,
			limits: []limitPhase{{1200 * ms, mib(1024)}, {2200 * ms, 0}},
			want:   []int64{mib(708)}, at: []time.Duration{500 * ms}},
		{name: "memory Go does not manage grows", phases: []phase{{from: 1200 * ms, ws: mib(752)}}, run: 2200 * ms,
			want: []int64{mib(708), mib(608)}, at: []time.Duration{500 * ms, 1500 * ms}},
		{name: "memory Go does not manage shrinks", phases: []phase{{from: 1200 * ms, ws: mib(552)}}, run: 6700 * ms,
			want: []int64{mib(708), mib(808)}, at: []time.Duration{500 * ms, 5500 * ms}},
		{name: "memory Go does not manage moves by 3 MiB", run: 6200 * ms, want: []int64{mib(708)}, at: []time.Duration{500 * ms}, phases: []phase{
			{from: 700 * ms, ws: mib(655)}, {from: 1200 * ms, ws: mib(649)}, {from: 1700 * ms, ws: mib(655)},
			{from: 2200 * ms, ws: mib(649)}, {from: 2700 * ms, ws: mib(655)}, {from: 3200 * ms, ws: mib(649)},
			{from: 3700 * ms, ws: mib(655)}, {from: 4200 * ms, ws: mib(649)}, {from: 4700 * ms, ws: mib(655)},
			{from: 5200 * ms, ws: mib(649)}}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := sidecarMemory()
			if tt.limit < 0 {
				f.limit = 0
			}
			if tt.m != nil {
				f.ws, f.m = tt.ws, *tt.m
			}
			for i := range tt.phases {
				tt.phases[i].m = f.m
			}
			g := fakeGuard(t, 1024, 64, f)
			setPhases := phases(f, tt.phases...)
			simulate(t, g, tt.run, func(now time.Duration) {
				setPhases(now)
				for _, l := range tt.limits {
					if now >= l.from {
						f.limit = l.limit
					}
				}
			})
			if len(f.installs) != len(tt.want) || len(f.installAt) != len(tt.at) {
				t.Fatalf("installed %v at %v; want %v at %v", f.installs, f.installAt, tt.want, tt.at)
			}
			for i := range tt.want {
				if f.installs[i] != tt.want[i] || f.installAt[i] != tt.at[i] {
					t.Fatalf("installed %v at %v; want %v at %v", f.installs, f.installAt, tt.want, tt.at)
				}
			}
		})
	}
}

// TestTheFloorIsRaisedAtOnceOverARoomThatCameBack: a soft limit lowered for
// a spike of memory Go does not manage is raised at the first sample at
// which Go's live memory with its floor is over it, not reclaimCooldown
// after the last change, though the room has come back and the container is
// not too small: until then the GC would run with less than 64 MiB to
// allocate between its collections.
func TestTheFloorIsRaisedAtOnceOverARoomThatCameBack(t *testing.T) {
	ms := time.Millisecond
	f := sidecarMemory()
	g := fakeGuard(t, 1024, 64, f)
	simulate(t, g, 2500*ms, phases(f,
		// 200 MiB more not Go's: a room of 508 MiB.
		phase{from: 1000 * ms, ws: mib(852), m: goFig(350, 200, 20, 400)},
		// Back to 252 MiB not Go's, and live memory grown: 490 MiB no GC can
		// free, a floor of 554 MiB over the 508 MiB installed.
		phase{from: 2000 * ms, ws: mib(782), m: goFig(500, 470, 10, 530)}))
	want, at := []int64{mib(708), mib(508), mib(708)}, []time.Duration{500 * ms, 1000 * ms, 2000 * ms}
	if len(f.installs) != 3 || len(f.installAt) != 3 {
		t.Fatalf("installed %v at %v; want %v at %v", f.installs, f.installAt, want, at)
	}
	for i := range want {
		if f.installs[i] != want[i] || f.installAt[i] != at[i] {
			t.Fatalf("installed %v at %v; want %v at %v", f.installs, f.installAt, want, at)
		}
	}
}

// tooSmallLines is the error lines that report the container too small.
func tooSmallLines(logs *observer.ObservedLogs) []observer.LoggedEntry {
	return logs.FilterLevelExact(zapcore.ErrorLevel).FilterMessageSnippet("too small for the keploy-agent").All()
}

// TestTooSmallIsReportedOnlyWhenPausedRecordingDoesNotFit: the container is
// reported too small only where Go's live memory with its 64 MiB floor is
// over the room the container leaves it with recording paused, after the
// guard's collection reclaimCooldown into the pause; never on live memory
// under the pause point, nor on a capture burst that a GC finds live and the
// pause drops, at once or seconds later.
func TestTooSmallIsReportedOnlyWhenPausedRecordingDoesNotFit(t *testing.T) {
	ms := time.Millisecond
	// The 1 GiB sidecar with a GC that found 700 MiB no GC can free: a floor
	// of 764 MiB over the 708 MiB room; and that capture burst dropped.
	burstWS, burst := mib(252+710), goFig(700, 690, 0, 710)
	droppedWS, dropped := mib(252+230), goFig(210, 200, 10, 230)
	// Held memory at 852 MiB, over the 832 MiB pause point; 600 MiB no GC can
	// free, a floor of 664 MiB under the room.
	episodeWS, episode := mib(252+610), withGoal(goFig(600, 590, 0, 610), 677)
	tests := []struct {
		name            string
		limitMB, ringMB int64
		f               *fakeMemory
		phases          []phase
		dropOnPause     bool
		dropOn          int           // the collection, by count, that drops the burst; 0: none
		run             time.Duration // 0: 7 s
		pauses          int
		reportAt        time.Duration // 0: none
		source          string        // the too-small line's limit_source; "": --memory-limit
	}{
		// Live memory under the pause point, each heap goal 97% of the soft
		// limit installed less Go's memory outside the heap.
		{name: "1 GiB sidecar, 560 MiB live", limitMB: 1024, ringMB: 64,
			f: &fakeMemory{ws: mib(872), limit: mib(1024), m: withGoal(goFig(600, 560, 10, 620), 677)}},
		{name: "2 GiB proxy mode, 1550 MiB live", limitMB: 2048,
			f: &fakeMemory{ws: mib(1660), limit: mib(2048), m: withGoal(goFig(1580, 1550, 10, 1610), 1788)}},
		{name: "4 GiB proxy mode, 3100 MiB live", limitMB: 4096,
			f: &fakeMemory{ws: mib(3250), limit: mib(4096), m: withGoal(goFig(3150, 3100, 20, 3200), 3575)}},
		{name: "512 MiB proxy mode, 20 MiB not Go's, 360 MiB live", limitMB: 512,
			f: &fakeMemory{ws: mib(400), limit: mib(512), m: withGoal(goFig(370, 360, 5, 380), 431)}},
		{name: "1 GiB sidecar: a GC finds a capture burst live; the pause drops it", limitMB: 1024, ringMB: 64,
			f: sidecarMemory(), phases: []phase{{from: 1000 * ms, ws: burstWS, m: burst}}, dropOnPause: true, pauses: 1},
		{name: "1 GiB sidecar: a GC finds a capture burst live; it is dropped 2 s into the pause", limitMB: 1024, ringMB: 64,
			f: sidecarMemory(), phases: []phase{{from: 1000 * ms, ws: burstWS, m: burst}, {from: 3000 * ms, ws: droppedWS, m: dropped}}, pauses: 1},
		// Two pressure episodes on the 1 GiB sidecar: held memory over the
		// pause point, the floor under the room, pauses at 500 ms, is
		// collected again at 5.5 s, and resumes at 6 s; at 8 s a GC finds a
		// capture burst live. The first episode's collection says nothing
		// of the second: no report while recording, nor at the second pause.
		{name: "1 GiB sidecar, a second episode: a burst the second pause drops", limitMB: 1024, ringMB: 64,
			f: &fakeMemory{ws: episodeWS, limit: mib(1024), m: episode}, run: 14 * time.Second, dropOn: 3, pauses: 2,
			phases: []phase{{from: 6000 * ms, ws: droppedWS, m: dropped}, {from: 8000 * ms, ws: burstWS, m: burst}}},
		// The burst stays: reported 5.5 s into the second pause, after its
		// own collection at 13 s.
		{name: "1 GiB sidecar, a second episode: the burst stays over the room with recording paused", limitMB: 1024, ringMB: 64,
			f: &fakeMemory{ws: episodeWS, limit: mib(1024), m: episode}, run: 14 * time.Second, pauses: 2, reportAt: 13500 * ms,
			phases: []phase{{from: 6000 * ms, ws: droppedWS, m: dropped}, {from: 8000 * ms, ws: burstWS, m: burst}}},
		// 700 MiB not Go's leaves Go a room of 260 MiB; it holds 320 MiB no GC
		// can free with recording paused. Paused at 500 ms; collected again
		// at 5500 ms; reported at the next sample.
		{name: "1 GiB sidecar, 700 MiB not Go's: live memory stays over the room with recording paused", limitMB: 1024, ringMB: 64,
			f: &fakeMemory{ws: mib(1060), limit: mib(1024), m: goFig(330, 300, 10, 360)}, pauses: 1, reportAt: 6000 * ms},
		// The same container under a 2 GiB --memory-limit: its limit binds,
		// and is the one the line names and advises raising.
		{name: "a 1 GiB container under a 2 GiB --memory-limit, 700 MiB not Go's: live memory stays over the room", limitMB: 2048, ringMB: 64,
			f: &fakeMemory{ws: mib(1060), limit: mib(1024), m: goFig(330, 300, 10, 360)}, pauses: 1, reportAt: 6000 * ms, source: "container"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := tt.f
			dropOn := tt.dropOn
			if tt.dropOnPause {
				dropOn = 1
			}
			if dropOn > 0 {
				f.onReclaim = func(f *fakeMemory) {
					if f.forced() == dropOn {
						f.set(droppedWS, dropped)
					}
				}
			}
			run := tt.run
			if run == 0 {
				run = 7 * time.Second
			}
			g := fakeGuard(t, tt.limitMB, tt.ringMB, f)
			logs := observe(g, zapcore.InfoLevel)
			edges, _ := simulate(t, g, run, phases(f, tt.phases...))
			if n := len(edges) - len(edges)/2; n != tt.pauses {
				t.Errorf("pause edges %v; want %d pause(s)", edges, tt.pauses)
			}
			lines := tooSmallLines(logs)
			if tt.reportAt == 0 {
				if len(lines) != 0 {
					t.Errorf("reported too small: %q %v", lines[0].Message, lines[0].ContextMap())
				}
				return
			}
			if len(lines) != 1 || lines[0].Time.Sub(simStart) != tt.reportAt {
				t.Fatalf("%d too-small lines; want one, at %v", len(lines), tt.reportAt)
			}
			c := lines[0].ContextMap()
			if tt.f.limit == mib(1024) && c["non_go_memory_bytes"] == mib(252) {
				if c["go_mem_limit_bytes"].(int64) <= c["go_room_bytes"].(int64) || c["go_live_memory_bytes"] != mib(700) {
					t.Errorf("too-small line's sizes %v; want GOMEMLIMIT over the 708 MiB room, 700 MiB live", c)
				}
				return
			}
			source, raise := "--memory-limit", "--memory-limit"
			if tt.source == "container" {
				source, raise = "container", "the container's memory limit"
			}
			if c["go_mem_limit_bytes"] != mib(384) || c["go_room_bytes"] != mib(260) || c["go_live_memory_bytes"] != mib(320) ||
				c["limit_bytes"] != mib(1024) || c["limit_source"] != source || c["non_go_memory_bytes"] != mib(700) {
				t.Errorf("too-small line's sizes %v; want GOMEMLIMIT 384 MiB over a 260 MiB room, 320 MiB live, 1024 MiB limit from %s, 700 MiB not Go's", c, source)
			}
			if !strings.HasSuffix(lines[0].Message, "; raise "+raise) {
				t.Errorf("too-small line %q; want it to advise raising %s", lines[0].Message, raise)
			}
		})
	}
}

// TestTheGuardBudgetsAgainstTheLowerOfTheContainersLimitAndTheFlag: with the
// container's memory limit under --memory-limit, the guard pauses, and sets
// its ceiling, against the container's limit, and says so once with both
// sizes; with the container's limit over --memory-limit, or none, against
// --memory-limit as before, silently.
func TestTheGuardBudgetsAgainstTheLowerOfTheContainersLimitAndTheFlag(t *testing.T) {
	// --memory-limit 1024 MiB with a 64 MiB ring buffer. Against a 768 MiB
	// container: pause point 627.2 MiB, ceiling 732.8 MiB; against 1024 MiB:
	// 832 and 976 MiB. 252 MiB not Go's in each.
	heldOver := withGoal(goFig(400, 390, 0, 410), 780) // held 652 MiB
	nextOver := withGoal(goFig(450, 200, 0, 448), 600) // held 450 MiB; working set 700, next GC due at 850
	room768 := mib(768) - mib(252) - (mib(704)-thresholdBytes(mib(704), pauseThresholdRatio))/4 - softLimitSlack
	tests := []struct {
		name      string
		container int64
		ws        int64
		m         goMemory
		pause     bool
		install   int64
	}{
		{name: "a 768 MiB container: held memory over its pause point", container: mib(768), ws: mib(662), m: heldOver, pause: true, install: room768},
		{name: "a 768 MiB container: the next GC due over its ceiling", container: mib(768), ws: mib(700), m: nextOver, pause: true, install: room768},
		{name: "a 2 GiB container: held memory under the pause point", container: mib(2048), ws: mib(662), m: heldOver, install: mib(708)},
		{name: "a 2 GiB container: the working set under the pause point", container: mib(2048), ws: mib(700), m: nextOver, install: mib(708)},
		{name: "no container limit: held memory under the pause point", ws: mib(662), m: heldOver, install: mib(708)},
		{name: "no container limit: the working set under the pause point", ws: mib(700), m: nextOver, install: mib(708)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeMemory{ws: tt.ws, limit: tt.container, m: tt.m}
			g := fakeGuard(t, 1024, 64, f)
			logs := observe(g, zapcore.WarnLevel)
			edges, _ := simulate(t, g, twoSamples, nil)
			if paused := len(edges) == 1 && edges[0].paused && edges[0].after == defaultCheckInterval; paused != tt.pause || len(edges) > 1 {
				t.Errorf("pause edges %v; want a pause on the first sample: %v", edges, tt.pause)
			}
			if len(f.installs) == 0 || f.installs[0] != tt.install {
				t.Errorf("installed %v; want %d first", f.installs, tt.install)
			}
			lines := logs.FilterMessageSnippet("under --memory-limit").All()
			if !tt.pause {
				if len(lines) != 0 {
					t.Errorf("said %q with the container's limit %d MiB", lines[0].Message, tt.container>>20)
				}
				return
			}
			if len(lines) != 1 || lines[0].ContextMap()["container_limit_bytes"] != mib(768) || lines[0].ContextMap()["memory_limit_bytes"] != mib(1024) {
				t.Fatalf("%d lines naming the container's limit under --memory-limit; want one, with both sizes", len(lines))
			}
		})
	}
	// Said again once the container's limit is under --memory-limit again.
	f := &fakeMemory{ws: mib(652), limit: mib(768), m: goFig(350, 200, 20, 400)}
	g := fakeGuard(t, 1024, 64, f)
	logs := observe(g, zapcore.WarnLevel)
	setLimits := func(now time.Duration) {
		switch {
		case now >= 2200*time.Millisecond:
			f.limit = mib(768)
		case now >= 1200*time.Millisecond:
			f.limit = mib(2048)
		}
	}
	simulate(t, g, 3*time.Second, setLimits)
	if n := logs.FilterMessageSnippet("under --memory-limit").Len(); n != 2 {
		t.Errorf("%d lines for the container's limit under --memory-limit twice; want 2", n)
	}
}

// TestTheLinesNameTheLimitTheGuardJudgesAgainst: the pause line, the
// non-viable line and the floored warning carry the limit the guard judged
// against and where it came from, and advise raising that one: the
// container's limit where it is under --memory-limit, else --memory-limit.
func TestTheLinesNameTheLimitTheGuardJudgesAgainst(t *testing.T) {
	const (
		pausing   = "Pausing keploy-agent recording due to memory pressure"
		nonViable = "recording cannot proceed"
		floored   = "GOMEMLIMIT floored"
	)
	tests := []struct {
		name                       string
		limitMB, ringMB, container int64
		ws                         int64
		m                          goMemory
		line, advice, source       string
		limit                      int64
	}{
		{name: "paused against a 768 MiB container under a 1024 MiB --memory-limit", limitMB: 1024, ringMB: 64, container: mib(768),
			ws: mib(662), m: withGoal(goFig(400, 390, 0, 410), 780), line: pausing,
			advice: "Consider increasing the container's memory limit,", source: "container", limit: mib(768)},
		{name: "paused against --memory-limit, no container limit", limitMB: 1024, ringMB: 64,
			ws: mib(900), m: heap(600, 580, 0, 830), line: pausing,
			advice: "Consider increasing --memory-limit,", source: "--memory-limit", limit: mib(1024)},
		// A 64 MiB ring buffer leaves 192 MiB of 256, and 236 of 300: under 320 MiB.
		{name: "non-viable against a 256 MiB container under a 1024 MiB --memory-limit", limitMB: 1024, ringMB: 64, container: mib(256),
			ws: mib(200), m: goFig(60, 40, 5, 80), line: nonViable,
			advice: "raise the container's memory limit or lower record.ringbufSizeMB", source: "container", limit: mib(256)},
		{name: "non-viable against a 300 MiB --memory-limit", limitMB: 300, ringMB: 64,
			ws: mib(200), m: goFig(60, 40, 5, 80), line: nonViable,
			advice: "raise --memory-limit or lower record.ringbufSizeMB", source: "--memory-limit", limit: mib(300)},
		{name: "floored against a 64 MiB container under a 1024 MiB --memory-limit", limitMB: 1024, container: mib(64),
			ws: mib(40), m: goFig(20, 10, 2, 30), line: floored,
			advice: "raise the container's memory limit or lower record.ringbufSizeMB", source: "container", limit: mib(64)},
		{name: "floored against a 64 MiB --memory-limit", limitMB: 64,
			ws: mib(40), m: goFig(20, 10, 2, 30), line: floored,
			advice: "raise --memory-limit or lower record.ringbufSizeMB", source: "--memory-limit", limit: mib(64)},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := &fakeMemory{ws: tt.ws, limit: tt.container, m: tt.m}
			g := fakeGuard(t, tt.limitMB, tt.ringMB, f)
			logs := observe(g, zapcore.InfoLevel)
			simulate(t, g, twoSamples, nil)
			lines := logs.FilterMessageSnippet(tt.line).All()
			if len(lines) != 1 {
				t.Fatalf("%d lines %q; want one", len(lines), tt.line)
			}
			ctx := lines[0].ContextMap()
			if ctx["limit_bytes"] != tt.limit || ctx["limit_source"] != tt.source {
				t.Errorf("limit_bytes %v, limit_source %v; want %d (%d MiB), %q", ctx["limit_bytes"], ctx["limit_source"], tt.limit, tt.limit>>20, tt.source)
			}
			if !strings.Contains(lines[0].Message, tt.advice) {
				t.Errorf("line %q; want it to advise %q", lines[0].Message, tt.advice)
			}
			for _, k := range []string{"memory_limit_bytes", "memory_limit_mb"} {
				if v, ok := ctx[k]; ok {
					t.Errorf("the line labels %v as --memory-limit's (%s)", v, k)
				}
			}
		})
	}
}

// TestATooSmallContainerIsReportedOnceEachWay: one error line on entering,
// none while live memory moves about the room's edge, one info line once
// the container has room for Go's live memory plus 64 MiB again; no path in
// any line.
func TestATooSmallContainerIsReportedOnceEachWay(t *testing.T) {
	ms := time.Millisecond
	f := &fakeMemory{ws: mib(1060), limit: mib(1024), m: goFig(330, 300, 10, 360)}
	g := fakeGuard(t, 1024, 64, f)
	logs := observe(g, zapcore.InfoLevel)
	// Paused from 500 ms, sampled every 500 ms: collected again at 5.5 s,
	// reported at 6 s.
	simulate(t, g, 9*time.Second, phases(f,
		// 190 MiB no GC can free: a floor of 254 MiB, within the 260 MiB room
		// but not by softLimitRaiseStep.
		phase{from: 6500 * ms, ws: mib(700 + 240), m: goFig(230, 190, 10, 240)},
		phase{from: 7000 * ms, ws: mib(1060), m: goFig(330, 300, 10, 360)},
		// 252 MiB not Go's: a room of 708 MiB.
		phase{from: 7500 * ms, ws: mib(252 + 360), m: goFig(330, 300, 10, 360)}))
	if lines := tooSmallLines(logs); len(lines) != 1 || lines[0].Time.Sub(simStart) != 6000*ms {
		t.Errorf("%d too-small lines; want one, at 6 s", len(lines))
	}
	room := logs.FilterMessageSnippet("has room for the keploy-agent's live Go memory plus 64 MiB again").All()
	if len(room) != 1 || room[0].Time.Sub(simStart) != 7500*ms || room[0].ContextMap()["go_room_bytes"] != mib(708) {
		t.Errorf("%d room-again lines; want one, at 7.5 s, with the 708 MiB room", len(room))
	}
	for _, e := range logs.All() {
		for _, fld := range e.Context {
			if fld.Type == zapcore.StringType && strings.Contains(fld.String, "/") {
				t.Errorf("log %q carries a path: %s=%s", e.Message, fld.Key, fld.String)
			}
		}
	}
}

// TestTheCeilingRuleLeavesAHeapItsSoftLimitHoldsAlone: on the 1 GiB sidecar
// with Go at its derived soft limit and 40 MiB of active page cache, garbage
// over the pause point neither pauses nor collects: the memory the kernel
// cannot reclaim is due under the ceiling by the runtime's next GC.
func TestTheCeilingRuleLeavesAHeapItsSoftLimitHoldsAlone(t *testing.T) {
	// 252 MiB not Go's, 40 active page cache, Go's 680 mapped: 660 objects
	// (400 live), 10 free, the heap goal 677 MiB (97% of the 708 MiB soft
	// limit less Go's 10 MiB outside the heap).
	m := goFig(660, 400, 10, 680)
	m.goal = mib(677)
	f := &fakeMemory{ws: mib(972), active: mib(40), limit: mib(1024), m: m}
	g := fakeGuard(t, 1024, 64, f)
	if edges, _ := simulate(t, g, twoSamples, nil); len(edges) != 0 || f.forced() != 0 {
		t.Errorf("pause edges %v, %d forced collections; want none", edges, f.forced())
	}
	if len(f.installs) != 1 || f.installs[0] != mib(708) {
		t.Errorf("installed %v; want 708 MiB", f.installs)
	}
}

// TestRealSoftLimitKeepsTheWorkingSetUnderTheContainerLimit: on the real
// runtime, in a 512 MiB container with 120 MiB not Go's, 230 MiB live and
// churned garbage, the derived soft limit (350.4 MiB) has the runtime
// collect before the container's limit: the working set stays under it,
// recording never pauses and the guard forces no collection. 90% of the
// budget (460.8 MiB) lets the runtime take it to about 580 MiB.
func TestRealSoftLimitKeepsTheWorkingSetUnderTheContainerLimit(t *testing.T) {
	g, maxWS := realGuard(t, 120, 512)
	debug.FreeOSMemory()
	live := make([][]byte, 230)
	for i := range live {
		live[i] = make([]byte, 1<<20)
	}
	f0 := forcedGCs()
	var edges []pauseEdge
	done := make(chan struct{})
	go func() { edges = runGuardForEdges(t, g, 3500*time.Millisecond); close(done) }()
	time.Sleep(700 * time.Millisecond) // the first sample installs the soft limit
	soft := debug.SetMemoryLimit(-1)
	churn(2700 * time.Millisecond)
	<-done
	forced := forcedGCs() - f0
	runtime.KeepAlive(live)
	if want := mib(512) - mib(120) - (mib(512)-thresholdBytes(mib(512), pauseThresholdRatio))/4 - softLimitSlack; soft != want {
		t.Fatalf("soft limit %d; want %d", soft, want)
	}
	if maxWS() >= mib(512) {
		t.Errorf("the working set reached %d MiB, the container's 512 MiB limit", maxWS()>>20)
	}
	if len(edges) != 0 || forced != 0 {
		t.Errorf("pause edges %v, %d forced collections; want none", edges, forced)
	}
}

// TestTheDerivedLimitIsInForceFromTheStart: Start's sample installs the
// derived GOMEMLIMIT before run's first sample, from --memory-limit where the
// container has no limit.
func TestTheDerivedLimitIsInForceFromTheStart(t *testing.T) {
	// A 768 MiB container under the 1024 MiB --memory-limit: its own budget,
	// margin 35.2 + 16 MiB.
	f := sidecarMemory()
	f.limit = mib(768)
	g := fakeGuard(t, 1024, 64, f)
	g.sampleSoftLimit()
	if want := mib(768) - mib(252) - (mib(704)-thresholdBytes(mib(704), pauseThresholdRatio))/4 - softLimitSlack; len(f.installs) != 1 || f.installs[0] != want {
		t.Errorf("container limit 768 MiB: installed %v; want %d", f.installs, want)
	}
	for _, limit := range []int64{mib(1024), 0} {
		f := sidecarMemory()
		f.limit = limit
		g := fakeGuard(t, 1024, 64, f)
		g.softLimit = int64(float64(mib(1024)) * 0.9)
		g.sampleSoftLimit()
		if len(f.installs) != 1 || f.installs[0] != mib(708) {
			t.Errorf("container limit %d MiB (0: none): installed %v; want 708 MiB", limit>>20, f.installs)
		}
	}
}

// TestGoMemoryBudgetIsTheBudgetNotTheFittedLimit: GoMemoryBudget is 90% of
// the budget left after the ring buffer while the guard runs (the lower of
// the container's limit and --memory-limit), floored at 64 MiB as GOMEMLIMIT
// is, whatever GOMEMLIMIT is fitted to, from Start's sample on; 0 once the
// guard stops, and when none starts.
func TestGoMemoryBudgetIsTheBudgetNotTheFittedLimit(t *testing.T) {
	t.Cleanup(SetGoMemoryBudgetForTest(0))
	for _, tt := range []struct {
		name            string
		limitMB, ringMB int64
		container       int64
		ws              int64 // 0: the sidecar's figures
		m               goMemory
		want            int64
	}{
		{name: "1 GiB sidecar: GOMEMLIMIT fitted to 708 MiB", limitMB: 1024, ringMB: 64, container: mib(1024), want: int64(float64(mib(960)) * 0.9)},
		{name: "a 768 MiB container under --memory-limit", limitMB: 1024, ringMB: 64, container: mib(768), want: int64(float64(mib(704)) * 0.9)},
		{name: "no container limit", limitMB: 1024, ringMB: 64, want: int64(float64(mib(960)) * 0.9)},
		// 90% of 70 MiB is 63 MiB.
		{name: "a 70 MiB proxy budget: floored at 64 MiB", limitMB: 70, ws: mib(40), m: goFig(20, 10, 2, 30), want: minGoMemLimitBytes},
	} {
		t.Run(tt.name, func(t *testing.T) {
			SetGoMemoryBudgetForTest(0)
			f := sidecarMemory()
			if tt.ws != 0 {
				f.ws, f.m = tt.ws, tt.m
			}
			f.limit = tt.container
			g := fakeGuard(t, tt.limitMB, tt.ringMB, f)
			g.sampleSoftLimit()
			if got := GoMemoryBudget(); got != tt.want {
				t.Errorf("after Start's sample: %d MiB; want %d MiB", got>>20, tt.want>>20)
			}
			SetGoMemoryBudgetForTest(0)
			var during int64
			simulate(t, g, twoSamples, func(now time.Duration) {
				if now == 2*defaultCheckInterval {
					during = GoMemoryBudget()
				}
			})
			if during != tt.want {
				t.Errorf("while running: %d MiB (GOMEMLIMIT installed %v); want %d MiB", during>>20, f.installs, tt.want>>20)
			}
			if got := GoMemoryBudget(); got != 0 {
				t.Errorf("after the guard stopped: %d MiB; want 0", got>>20)
			}
		})
	}
	SetGoMemoryBudgetForTest(mib(864))
	if err := Start(context.Background(), zap.NewNop(), false, 1024); err != nil || GoMemoryBudget() != 0 {
		t.Errorf("no guard started (not docker): %d MiB, err %v; want 0", GoMemoryBudget()>>20, err)
	}
}

// TestAFlooredLimitIsReportedWhereItIsDerivedToo: a budget whose 90% is
// under minGoMemLimitBytes is reported once, as without a container limit,
// where the limit is derived from the container.
func TestAFlooredLimitIsReportedWhereItIsDerivedToo(t *testing.T) {
	for _, limit := range []int64{mib(64), 0} {
		// 64 MiB proxy mode: 90% of it is 57.6 MiB, floored at 64 MiB.
		f := &fakeMemory{ws: mib(40), limit: limit, m: goFig(20, 10, 2, 30)}
		g := fakeGuard(t, 64, 0, f)
		logs := observe(g, zapcore.WarnLevel)
		simulate(t, g, 2*time.Second, nil)
		if n := logs.FilterMessageSnippet("GOMEMLIMIT floored").Len(); n != 1 || len(f.installs) != 1 || f.installs[0] != minGoMemLimitBytes {
			t.Errorf("container limit %d MiB: %d floored warnings, installed %v; want one, and 64 MiB", limit>>20, n, f.installs)
		}
	}
}
