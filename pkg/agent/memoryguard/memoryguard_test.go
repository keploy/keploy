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

	actualPath, err := resolveMemoryCurrentPath(cgroupRoot, procSelfCgroup, procMountInfo)
	if err != nil {
		t.Fatalf("resolveMemoryCurrentPath returned error: %v", err)
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

	actualPath, err := resolveMemoryCurrentPath(cgroupRoot, procSelfCgroup, procMountInfo)
	if err != nil {
		t.Fatalf("resolveMemoryCurrentPath returned error: %v", err)
	}

	if actualPath != expectedPath {
		t.Fatalf("expected %s, got %s", expectedPath, actualPath)
	}
}
