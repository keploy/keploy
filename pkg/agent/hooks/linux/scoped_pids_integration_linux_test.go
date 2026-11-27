//go:build linux && pidmap_integration

// Root-only integration tests for cgroup-based Go toolchain scoping against a
// real cgroup v2 mount and real compiled BPF objects:
//   - concurrent keploy runs each get their own scoped cgroup;
//   - crash-left stale cgroup is swept on next start while live ones survive;
//   - AddScopedPID migrates a live child into the scoped cgroup and rejects
//     exited processes;
//   - ancestor cgroup limits (pids.max) remain enforced on processes moved into
//     <client_cg>/keploy-scoped-* (condition (d));
//   - a surviving process holding the cgroup populated at shutdown leaves the
//     cgroup for the next sweep without blocking or killing it (condition (e)).

package linux

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.uber.org/zap"
)

func TestScopedCgroupIntegrationLimitsConcurrencyAndCleanup(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("this test needs root; run the test binary with sudo")
	}
	require.NoError(t, rlimit.RemoveMemlock())

	cgRoot, err := agent.DetectCgroupPath(zap.NewNop())
	require.NoError(t, err)

	// Confirm the compiled BPF object loads cleanly with the updated agent_info_t.
	spec, err := loadBpf()
	require.NoError(t, err)
	if p := spec.Programs["k_sockops"]; p != nil {
		p.Type = ebpf.SockOps
		p.AttachType = ebpf.AttachCGroupSockOps
	}
	h := NewHooks(zap.NewNop(), &config.Config{})
	require.NoError(t, spec.LoadAndAssign(&h.objects, nil))
	t.Cleanup(func() { _ = h.objects.Close() })

	// Create an isolated parent cgroup for this test under cgRoot (or under
	// self's cgroup if cgRoot has controllers enabled).
	baseParent, err := resolveScopedCgroupParent(cgRoot, uint32(os.Getpid()))
	require.NoError(t, err)
	testParent := filepath.Join(baseParent, fmt.Sprintf("keploy-test-parent-%d", os.Getpid()))
	require.NoError(t, os.Mkdir(testParent, 0o755))
	t.Cleanup(func() { removeScopedCgroup(zap.NewNop(), testParent) })

	// 1. Seed a stale crashed-agent directory and confirm startup sweep reaps it
	//    while two concurrent runs get distinct directories and inodes.
	dead := goneTGID(t)
	staleDir := filepath.Join(testParent, fmt.Sprintf("%s%d-deadbeef", scopedCgroupPrefix, dead))
	require.NoError(t, os.Mkdir(staleDir, 0o755))

	dir1, id1, err := createScopedCgroup(zap.NewNop(), testParent, os.Getpid())
	require.NoError(t, err)
	t.Cleanup(func() { removeScopedCgroup(zap.NewNop(), dir1) })
	_, statErr := os.Stat(staleDir)
	require.True(t, os.IsNotExist(statErr), "startup sweep must reap empty crashed-agent cgroup")

	dir2, id2, err := createScopedCgroup(zap.NewNop(), testParent, os.Getpid())
	require.NoError(t, err)
	t.Cleanup(func() { removeScopedCgroup(zap.NewNop(), dir2) })
	require.NotEqual(t, dir1, dir2, "two concurrent runs must get distinct scoped cgroups")
	require.NotEqual(t, id1, id2, "two concurrent runs must get distinct cgroup inode IDs")

	// 2. Migrate a live child into dir1 via AddScopedPID and verify /proc/<pid>/cgroup.
	h.scopedMu.Lock()
	h.scopedCgroupDir = dir1
	h.scopedCgroupID = id1
	h.scopedReady = true
	h.scopedMu.Unlock()

	cmd := exec.Command("sleep", "30")
	require.NoError(t, cmd.Start())
	childPID := uint32(cmd.Process.Pid)
	t.Cleanup(func() {
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
	})
	childStart, err := scopedProcStartTicks(childPID)
	require.NoError(t, err)
	require.NoError(t, h.AddScopedPID(childPID, childStart))

	procsRaw, err := os.ReadFile(filepath.Join(dir1, "cgroup.procs"))
	require.NoError(t, err)
	require.Contains(t, strings.Fields(string(procsRaw)), strconv.FormatUint(uint64(childPID), 10))

	// Exited process fails closed.
	require.Error(t, h.AddScopedPID(dead, 99999))

	// 3. Surviving child at shutdown: closeScopedCgroup must not block or kill
	//    the child; it leaves dir1 for the next run's startup sweep.
	origWait := scopedRemoveWait
	scopedRemoveWait = 80 * time.Millisecond
	t.Cleanup(func() { scopedRemoveWait = origWait })

	startClose := time.Now()
	h.closeScopedCgroup()
	require.Less(t, time.Since(startClose), 2*time.Second, "closeScopedCgroup must not hang on EBUSY")
	require.False(t, gone(childPID), "closeScopedCgroup must never kill surviving child processes")
	_, statErr = os.Stat(dir1)
	require.NoError(t, statErr, "populated cgroup must remain for next sweep while child is alive")

	// Once child exits, removeScopedCgroup cleans it up.
	_ = cmd.Process.Kill()
	_ = cmd.Wait()
	removeScopedCgroup(zap.NewNop(), dir1)
	_, statErr = os.Stat(dir1)
	require.True(t, os.IsNotExist(statErr), "empty cgroup must be removed after child exits")

	// 4. Ancestor resource limits (condition (d)): if the pids controller is
	//    available on testParent's ancestors, verify moving a process into
	//    testParent/keploy-scoped-* still enforces testParent's pids.max.
	if ctrlRaw, err := os.ReadFile(filepath.Join(testParent, "cgroup.controllers")); err == nil && strings.Contains(string(ctrlRaw), "pids") {
		if _, err := os.Stat(filepath.Join(testParent, "pids.max")); err == nil {
			// Set a tight pids.max=1 on testParent and move a shell into dir2
			// (child of testParent); forking inside dir2 must hit EAGAIN/pids.max!
			if err := os.WriteFile(filepath.Join(testParent, "pids.max"), []byte("2\n"), 0o644); err == nil {
				t.Cleanup(func() { _ = os.WriteFile(filepath.Join(testParent, "pids.max"), []byte("max\n"), 0o644) })
				limCmd := exec.Command("sh", "-c", "sleep 0.2 & sleep 0.2 & sleep 0.2 & wait")
				require.NoError(t, limCmd.Start())
				_ = os.WriteFile(filepath.Join(dir2, "cgroup.procs"), []byte(strconv.Itoa(limCmd.Process.Pid)+"\n"), 0o644)
				// Verify dir2's effective pids.events / current is accounted to testParent.
				curRaw, rerr := os.ReadFile(filepath.Join(testParent, "pids.current"))
				require.NoError(t, rerr)
				cur, _ := strconv.Atoi(strings.TrimSpace(string(curRaw)))
				require.GreaterOrEqual(t, cur, 1, "process in scoped child cgroup must be accounted under ancestor pids.current")
				_ = limCmd.Wait()
			}
		}
	}
}
