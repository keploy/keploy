//go:build linux

package linux

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"
	"unsafe"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
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

	require.NotNil(t, mainSpec.Maps["keploy_scoped_cgroup_level_map"], "scoped cgroup level calibration map must exist in main BPF object")
	require.Nil(t, mainSpec.Maps["keploy_agent_cgroup_level_map"], "deprecated agent level probe map must be removed")

	mainReg := mainSpec.Maps["keploy_agent_registration_map"]
	unixReg := unixSpec.Maps["keploy_agent_registration_map"]
	require.NotNil(t, mainReg)
	require.NotNil(t, unixReg)
	require.Equal(t, uint32(unsafe.Sizeof(structs.AgentInfo{})), mainReg.ValueSize,
		"structs.AgentInfo size must match BPF struct agent_info_t (with scoped_cgroup_id and scoped_cgroup_level)")
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
	h.scopedReason = "create scoped cgroup /sys/fs/cgroup/keploy-scoped-1: read-only file system"
	h.scopedRemedy = scopedCgroupRemedyFor(fmt.Errorf("create: %w", unix.EROFS))
	st := h.ScopedStatus()
	require.True(t, st.Requested)
	require.False(t, st.Ready)
	require.Equal(t, h.scopedReason, st.Reason)
	require.Contains(t, st.Remedy, "writable cgroup v2")
	require.Contains(t, st.Remedy, "go mod download")
	require.NotContains(t, st.Remedy, "sudo", "EROFS remedy must not blame sudo")

	require.Error(t, h.AddScopedPID(0, 123))
	require.Error(t, h.AddScopedPID(100, 0))
	require.ErrorContains(t, h.AddScopedPID(100, 123), "not armed")
}

func TestApplyScopedCgroupSetupDecision(t *testing.T) {
	t.Run("failure clears FlagScopedPids and zeroes ID/level and records scoped_ready=false", func(t *testing.T) {
		h := &Hooks{logger: zap.NewNop()}
		info := structs.AgentInfo{
			IsDocker:          0,
			Flags:             structs.FlagScopedPids | structs.FlagProvenUDP,
			ScopedCgroupID:    999,
			ScopedCgroupLevel: 4,
		}
		roErr := fmt.Errorf("create scoped cgroup /sys/fs/cgroup/keploy-scoped-1: %w", unix.EROFS)
		h.applyScopedCgroupSetup(&info, "/sys/fs/cgroup", 1234, func(string, uint32) (string, uint64, error) {
			return "", 0, roErr
		})
		// Kills F1: FlagScopedPids must be cleared on fallback while other flags survive.
		require.Zero(t, info.Flags&structs.FlagScopedPids, "FlagScopedPids must be cleared on setup error")
		require.NotZero(t, info.Flags&structs.FlagProvenUDP, "unrelated flags must stay set")
		require.Zero(t, info.ScopedCgroupID)
		require.Zero(t, info.ScopedCgroupLevel)
		// Kills F2: ScopedStatus must report Ready=false with cause-specific remedy.
		st := h.ScopedStatus()
		require.True(t, st.Requested)
		require.False(t, st.Ready)
		require.Contains(t, st.Reason, "read-only file system")
		require.Contains(t, st.Remedy, "writable cgroup v2")
		require.Contains(t, st.Remedy, "go mod download")
	})

	t.Run("success populates ScopedCgroupID, leaves ScopedCgroupLevel=0 for BPF self-calibration, and sets scoped_ready=true", func(t *testing.T) {
		h := &Hooks{logger: zap.NewNop()}
		info := structs.AgentInfo{
			IsDocker:          0,
			Flags:             structs.FlagScopedPids,
			ScopedCgroupLevel: 99,
		}
		h.applyScopedCgroupSetup(&info, "/sys/fs/cgroup", 1234, func(string, uint32) (string, uint64, error) {
			return "/sys/fs/cgroup/keploy-scoped-1-abcd", 778899, nil
		})
		// Kills F3: ScopedCgroupID must be written into info and ScopedCgroupLevel must be zeroed.
		require.NotZero(t, info.Flags&structs.FlagScopedPids)
		require.Equal(t, uint64(778899), info.ScopedCgroupID)
		require.Zero(t, info.ScopedCgroupLevel)
		st := h.ScopedStatus()
		require.True(t, st.Requested)
		require.True(t, st.Ready)
		require.Empty(t, st.Reason)
		require.Empty(t, st.Remedy)
	})

	t.Run("docker mode ignores scoped setup", func(t *testing.T) {
		h := &Hooks{logger: zap.NewNop()}
		info := structs.AgentInfo{IsDocker: 1, Flags: structs.FlagScopedPids}
		called := false
		h.applyScopedCgroupSetup(&info, "/sys/fs/cgroup", 1234, func(string, uint32) (string, uint64, error) {
			called = true
			return "", 0, errors.New("should not run")
		})
		require.False(t, called)
		require.False(t, h.ScopedStatus().Requested)
	})
}

func TestScopedCgroupRemedyFor(t *testing.T) {
	cases := []struct {
		name    string
		err     error
		want    string
		notWant string
	}{
		{"erofs", fmt.Errorf("mkdir: %w", unix.EROFS), "--privileged", "CAP_SYS_ADMIN"},
		{"threaded", fmt.Errorf("%w: domain invalid", errScopedCgroupNotDomain), "non-threaded", "CAP_SYS_ADMIN"},
		{"eopnotsupp_not_threaded", fmt.Errorf("write: %w", unix.EOPNOTSUPP), "delegation", "non-threaded"},
		{"eacces", fmt.Errorf("open: %w", unix.EACCES), "CAP_SYS_ADMIN", ""},
		{"other", errors.New("unexpected cgroupfs error"), "delegation", ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := scopedCgroupRemedyFor(tc.err)
			require.Contains(t, got, tc.want)
			require.Contains(t, got, "go mod download")
			if tc.notWant != "" {
				require.NotContains(t, got, tc.notWant)
			}
		})
	}
}

func TestCreateScopedCgroupFailsClosedWhenCgroupTypeUnreadable(t *testing.T) {
	origEUID := scopedGetEUID
	t.Cleanup(func() { scopedGetEUID = origEUID })
	scopedGetEUID = func() int { return 0 }

	// A plain temp directory has no kernel-generated cgroup.type file:
	// createScopedCgroup must fail closed with a read error (not misclassified
	// as threaded/non-domain) and remove the directory it created.
	parent := t.TempDir()
	_, _, err := createScopedCgroup(zap.NewNop(), parent, os.Getpid())
	require.Error(t, err)
	require.Contains(t, err.Error(), "read cgroup.type")
	require.NotErrorIs(t, err, errScopedCgroupNotDomain)
	entries, rerr := os.ReadDir(parent)
	require.NoError(t, rerr)
	require.Empty(t, entries, "unusable directory must be removed when cgroup.type is unreadable")
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
		scopedGetEUID = func() int { return 1000 }
		// Missing parent cgroup.procs fails O_WRONLY preflight even when the
		// test binary runs as root (and also rejects 0o444 under non-root).
		_, _, err := createScopedCgroup(zap.NewNop(), parent, os.Getpid())
		require.Error(t, err)
		require.Contains(t, err.Error(), "not delegated")
		if os.Geteuid() != 0 {
			require.NoError(t, os.WriteFile(filepath.Join(parent, "cgroup.procs"), nil, 0o444))
			_, _, err = createScopedCgroup(zap.NewNop(), parent, os.Getpid())
			require.Error(t, err)
			require.Contains(t, err.Error(), "not delegated")
		}
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
