package memoryguard

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

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
