//go:build linux && pidmap_integration

// Root-only, beside skipwatch_integration_linux_test.go: exercises AddScopedPID
// and sweepScopedPIDs against keploy's real compiled BPF object's scoped_pids
// map in the running kernel (works in both host and container PID namespaces).

package linux

import (
	"errors"
	"os"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"github.com/stretchr/testify/require"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/config"
)

func TestScopedPidsAddAndSweepAgainstRealBPFMap(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("this test needs root (it loads BPF programs); run the test binary with sudo")
	}
	require.NoError(t, rlimit.RemoveMemlock())
	spec, err := loadBpf()
	require.NoError(t, err)
	if p := spec.Programs["k_sockops"]; p != nil {
		p.Type = ebpf.SockOps
		p.AttachType = ebpf.AttachCGroupSockOps
	}
	h := NewHooks(zap.NewNop(), &config.Config{})
	require.NoError(t, spec.LoadAndAssign(&h.objects, nil))
	t.Cleanup(func() {
		_ = h.closeScopedFork()
		_ = h.objects.Close()
	})

	scoped := h.objects.ScopedPids
	require.NotNil(t, scoped, "loaded BPF object missing scoped_pids map")

	self := uint32(os.Getpid())
	selfStart, err := scopedProcStartTicks(self)
	require.NoError(t, err, "read /proc/self/stat start ticks")

	// Live process: AddScopedPID stores its start ticks and attaches k_scoped_fork.
	require.NoError(t, h.AddScopedPID(self, selfStart))
	require.NotNil(t, h.scopedFork, "AddScopedPID must attach k_scoped_fork")
	var got uint64
	require.NoError(t, scoped.Lookup(self, &got))
	require.Equal(t, selfStart, got)

	// Exited process: AddScopedPID fails closed and leaves no entry.
	dead := goneTGID(t)
	require.Error(t, h.AddScopedPID(dead, 99999))
	require.ErrorIs(t, scoped.Lookup(dead, &got), ebpf.ErrKeyNotExist)

	// Fail-closed under transient /proc read error (e.g. EMFILE): sweepScopedPIDs
	// must NOT delete a live scoped process and must return a non-nil error so
	// sweepIfExitsSkipped reschedules another sweep.
	t.Run("EMFILE keeps live entry and requests retry", func(t *testing.T) {
		origRead := readScopedProcStat
		t.Cleanup(func() { readScopedProcStat = origRead })
		readScopedProcStat = func(tgid uint32) ([]byte, error) {
			return nil, &os.PathError{Op: "open", Path: "/proc/stat", Err: unix.EMFILE}
		}
		err := sweepScopedPIDs(scoped, &h.scopedMu)
		require.Error(t, err, "sweepScopedPIDs must return retry error on EMFILE")
		require.True(t, errors.Is(err, unix.EMFILE), "expected EMFILE error, got %v", err)
		require.NoError(t, scoped.Lookup(self, &got), "live scoped PID must NOT be deleted on EMFILE")
		require.Equal(t, selfStart, got)
	})

	// Seed an exited PID and a recycled-PID stale entry (wrong start ticks for PID 1)
	// directly in the map, simulating skipped k_process_exit runs.
	require.NoError(t, scoped.Update(dead, uint64(42), ebpf.UpdateAny))
	const stalePID uint32 = 1
	pid1Start, err := scopedProcStartTicks(stalePID)
	require.NoError(t, err)
	require.NoError(t, scoped.Update(stalePID, pid1Start+999, ebpf.UpdateAny))

	// Sweep must reap BOTH the exited PID and the recycled-start-time entry,
	// while keeping the live process's matching entry intact.
	require.NoError(t, sweepScopedPIDs(scoped, &h.scopedMu))
	require.ErrorIs(t, scoped.Lookup(dead, &got), ebpf.ErrKeyNotExist, "exited PID should be swept")
	require.ErrorIs(t, scoped.Lookup(stalePID, &got), ebpf.ErrKeyNotExist, "recycled PID with mismatched start ticks should be swept")
	require.NoError(t, scoped.Lookup(self, &got), "live scoped PID with matching start ticks must survive sweep")
	require.Equal(t, selfStart, got)
}
