//go:build linux

package linux

import (
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.uber.org/zap"
)

func TestScopedCgroupAgentInfoMatchesBPFObjects(t *testing.T) {
	mainSpec, err := loadBpf()
	require.NoError(t, err)
	unixSpec, err := loadUnixconn()
	require.NoError(t, err)

	// Confirm legacy PID-map / raw_tracepoint scoping artifacts are gone.
	require.Nil(t, mainSpec.Maps["scoped_pids"], "scoped_pids map must be removed in favor of cgroup scoping")
	require.Nil(t, unixSpec.Maps["scoped_pids"], "unixconn must not carry scoped_pids map")
	require.Nil(t, mainSpec.Programs["k_scoped_fork"], "k_scoped_fork raw_tracepoint must be removed")

	mainReg := mainSpec.Maps["keploy_agent_registration_map"]
	unixReg := unixSpec.Maps["keploy_agent_registration_map"]
	require.NotNil(t, mainReg)
	require.NotNil(t, unixReg)
	require.Equal(t, uint32(unsafe.Sizeof(structs.AgentInfo{})), mainReg.ValueSize,
		"structs.AgentInfo size must match BPF struct agent_info_t (with scoped_cgroup_id)")
	require.Equal(t, mainReg.ValueSize, unixReg.ValueSize)
}

func TestScopedCgroupActiveStatusAndInputValidation(t *testing.T) {
	h := &Hooks{}
	require.False(t, h.ScopedPidsActive(), "inactive before agent info is sent")
	require.False(t, h.ScopedStatus().Requested)

	h.agentInfoSent = true
	h.agentInfo = structs.AgentInfo{IsDocker: 0, Flags: structs.FlagScopedPids, ScopedCgroupID: 0}
	require.False(t, h.ScopedPidsActive(), "zero scoped_cgroup_id must not report active")

	h.agentInfo.ScopedCgroupID = 4242
	require.True(t, h.ScopedPidsActive())

	h.agentInfo.IsDocker = 1
	require.False(t, h.ScopedPidsActive(), "docker mode must ignore FlagScopedPids")

	h.scopedRequested = true
	h.scopedReady = false
	h.scopedReason = "read-only file system"
	st := h.ScopedStatus()
	require.True(t, st.Requested)
	require.False(t, st.Ready)
	require.Equal(t, "read-only file system", st.Reason)
	require.Contains(t, st.Remedy, "sudo")
	require.Contains(t, st.Remedy, "systemd")

	require.Error(t, h.AddScopedPID(0, 123))
	require.Error(t, h.AddScopedPID(100, 0))
	require.ErrorContains(t, h.AddScopedPID(100, 123), "not armed")
}

func TestResolveScopedCgroupParent(t *testing.T) {
	origRead := scopedReadProcCgroup
	t.Cleanup(func() { scopedReadProcCgroup = origRead })

	t.Run("uses client leaf cgroup inside cGroupPath", func(t *testing.T) {
		root := t.TempDir()
		leaf := filepath.Join(root, "user.slice", "user-1000.slice", "session.scope")
		require.NoError(t, os.MkdirAll(leaf, 0o755))
		scopedReadProcCgroup = func(uint32) ([]byte, error) {
			return []byte("0::/user.slice/user-1000.slice/session.scope\n"), nil
		}
		got, err := resolveScopedCgroupParent(root, 1234)
		require.NoError(t, err)
		require.Equal(t, leaf, got)
	})

	t.Run("pure cgroup v1 (no 0:: line) uses mounted cGroupPath root", func(t *testing.T) {
		root := t.TempDir()
		scopedReadProcCgroup = func(uint32) ([]byte, error) {
			return []byte("12:memory:/docker/abc\n1:name=systemd:/docker/abc\n"), nil
		}
		got, err := resolveScopedCgroupParent(root, 1234)
		require.NoError(t, err)
		require.Equal(t, root, got)
	})

	t.Run("threaded cgroup steps up to nearest domain ancestor inside container", func(t *testing.T) {
		root := t.TempDir()
		domain := filepath.Join(root, "docker-abc.scope")
		threaded := filepath.Join(domain, "workers")
		require.NoError(t, os.MkdirAll(threaded, 0o755))
		require.NoError(t, os.WriteFile(filepath.Join(domain, "cgroup.type"), []byte("domain\n"), 0o644))
		require.NoError(t, os.WriteFile(filepath.Join(threaded, "cgroup.type"), []byte("threaded\n"), 0o644))
		scopedReadProcCgroup = func(uint32) ([]byte, error) {
			return []byte("0::/docker-abc.scope/workers\n"), nil
		}
		got, err := resolveScopedCgroupParent(root, 1234)
		require.NoError(t, err)
		require.Equal(t, domain, got)
	})
}

func TestScopedCgroupConcurrencySweepAndFallback(t *testing.T) {
	origGone := scopedProcessGone
	origEUID := scopedGetEUID
	origWait := scopedRemoveWait
	origStep := scopedRemoveStep
	t.Cleanup(func() {
		scopedProcessGone = origGone
		scopedGetEUID = origEUID
		scopedRemoveWait = origWait
		scopedRemoveStep = origStep
	})
	scopedRemoveWait = 60 * time.Millisecond
	scopedRemoveStep = 10 * time.Millisecond

	t.Run("sweeps dead-agent dirs and preserves live concurrent run dirs", func(t *testing.T) {
		parent := t.TempDir()
		deadDir := filepath.Join(parent, "keploy-scoped-999999-deadbeef")
		liveDir := filepath.Join(parent, "keploy-scoped-888888-cafebabe")
		otherDir := filepath.Join(parent, "unrelated-dir")
		require.NoError(t, os.Mkdir(deadDir, 0o755))
		require.NoError(t, os.Mkdir(liveDir, 0o755))
		require.NoError(t, os.Mkdir(otherDir, 0o755))

		scopedProcessGone = func(pid uint32) bool { return pid == 999999 }
		removed := sweepStaleScopedCgroups(zap.NewNop(), parent, 777777)
		require.Equal(t, 1, removed)
		_, err := os.Stat(deadDir)
		require.True(t, os.IsNotExist(err), "dead-agent dir must be swept")
		_, err = os.Stat(liveDir)
		require.NoError(t, err, "live concurrent agent dir must NOT be swept")
		_, err = os.Stat(otherDir)
		require.NoError(t, err, "unrelated dir must NOT be swept")
	})

	t.Run("non-root without writable parent cgroup.procs fails preflight cleanly", func(t *testing.T) {
		parent := t.TempDir()
		// Create read-only cgroup.procs (mode 0o444) and simulate non-root UID.
		require.NoError(t, os.WriteFile(filepath.Join(parent, "cgroup.procs"), nil, 0o444))
		scopedGetEUID = func() int { return 1000 }
		if os.Geteuid() == 0 {
			// Root ignores 0o444 DAC permissions; skip file-mode check when running as root.
			return
		}
		_, _, err := createScopedCgroup(zap.NewNop(), parent, os.Getpid())
		require.Error(t, err)
		require.Contains(t, err.Error(), "not delegated")
	})

	t.Run("read-only parent directory fails cleanly", func(t *testing.T) {
		if os.Geteuid() == 0 {
			return
		}
		parent := t.TempDir()
		require.NoError(t, os.Chmod(parent, 0o555))
		t.Cleanup(func() { _ = os.Chmod(parent, 0o755) })
		scopedGetEUID = func() int { return 0 } // exercise mkdir failure directly
		_, _, err := createScopedCgroup(zap.NewNop(), parent, os.Getpid())
		require.Error(t, err)
	})
}
