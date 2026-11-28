//go:build linux && integration

package bpfsafety

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// TestAnchorIdleOverheadBenchmark measures the per-call overhead of one idle
// 2-instruction no-op fentry anchor (created by ensureAnchor) using interleaved
// baseline/anchored rounds on a single pinned CPU, plus the standalone anchor
// program execution time via BPF_ENABLE_STATS.
//
// Run inside a disposable non-debug Linux VM (never on a production host, since
// tearing down anchors between rounds unlinks the test anchor):
//
//	KEPLOY_RUN_ANCHOR_BENCH=1 go test -v -tags=integration -run TestAnchorIdleOverheadBenchmark ./pkg/agent/bpfsafety
func TestAnchorIdleOverheadBenchmark(t *testing.T) {
	if os.Getenv("KEPLOY_RUN_ANCHOR_BENCH") != "1" {
		t.Skip("set KEPLOY_RUN_ANCHOR_BENCH=1 inside a disposable VM to run the idle anchor benchmark")
	}
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatal(err)
	}
	var stfs unix.Statfs_t
	if err := unix.Statfs(DefaultNativeBPFFSPath, &stfs); err != nil || stfs.Type != unix.BPF_FS_MAGIC {
		t.Skipf("%s is not a bpffs mount", DefaultNativeBPFFSPath)
	}

	runtime.LockOSThread()
	defer runtime.UnlockOSThread()
	var set unix.CPUSet
	set.Zero()
	set.Set(0)
	_ = unix.SchedSetaffinity(0, &set)

	resetStateForTest(t)
	root := filepath.Join(DefaultNativeBPFFSPath, "keploy-anchor-bench-"+randomNonce()[:8], GraceRootSubdir)
	if err := os.MkdirAll(root, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Dir(root)) })

	sp, err := unix.Socketpair(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		_ = unix.Close(sp[0])
		_ = unix.Close(sp[1])
	}()
	msg, buf := []byte("x"), make([]byte, 8)
	sendIter := func(n int) float64 {
		st := time.Now()
		for i := 0; i < n; i++ {
			_ = unix.Send(sp[0], msg, 0)
			_, _, _ = unix.Recvfrom(sp[1], buf, 0)
		}
		return float64(time.Since(st).Nanoseconds()) / float64(n)
	}

	cfd, err := unix.Socket(unix.AF_UNIX, unix.SOCK_DGRAM, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(cfd) }()
	connIter := func(n int) float64 {
		st := time.Now()
		for i := 0; i < n; i++ {
			_ = unix.Connect(cfd, &unix.SockaddrUnix{Name: "@kp-anchor-bench-none"})
		}
		return float64(time.Since(st).Nanoseconds()) / float64(n)
	}

	quantiles := func(xs []float64) (med, p25, p75 float64) {
		s := append([]float64(nil), xs...)
		sort.Float64s(s)
		q := func(f float64) float64 { return s[int(f*float64(len(s)-1)+0.5)] }
		return q(0.5), q(0.25), q(0.75)
	}

	type benchCase struct {
		fn   string
		iter func(int) float64
		n    int
	}
	for _, bc := range []benchCase{
		{"unix_dgram_sendmsg", sendIter, 200000},
		{"unix_dgram_connect", connIter, 200000},
	} {
		bc.iter(bc.n / 4) // warmup
		owner, err := openGraceOwnerInRoot(root, zap.NewNop())
		if err != nil {
			t.Fatal(err)
		}
		var base, anch, deltas []float64
		var runCnt, runNs uint64
		for r := 0; r < 21; r++ {
			x := bc.iter(bc.n)
			owner.anchorMu.Lock()
			owner.anchoredFuncs = nil
			owner.anchorMu.Unlock()
			owner.ensureAnchor(bc.fn, zap.NewNop())
			y := bc.iter(bc.n)
			if r == 20 {
				if closer, serr := defaultEnableStats(); serr == nil {
					_ = bc.iter(bc.n)
					_ = closer.Close()
				}
				if p, perr := ebpf.LoadPinnedProgram(filepath.Join(root, AnchorsSubdir, bc.fn+anchorProgSuffix), nil); perr == nil {
					if info, ierr := queryProgInfo(p.FD()); ierr == nil {
						runCnt, runNs = info.RunCnt, info.RunTimeNs
					}
					_ = p.Close()
				}
			}
			// Benchmark-only teardown inside disposable VM.
			_ = os.Remove(filepath.Join(root, AnchorsSubdir, bc.fn+anchorLinkSuffix))
			_ = os.Remove(filepath.Join(root, AnchorsSubdir, bc.fn+anchorProgSuffix))
			time.Sleep(50 * time.Millisecond)
			base = append(base, x)
			anch = append(anch, y)
			deltas = append(deltas, y-x)
		}
		_ = unix.Close(owner.lockFD)
		bm, b25, b75 := quantiles(base)
		am, a25, a75 := quantiles(anch)
		dm, d25, d75 := quantiles(deltas)
		perProg := 0.0
		if runCnt > 0 {
			perProg = float64(runNs) / float64(runCnt)
		}
		t.Logf("fn=%s rounds=21x%d baseline=%.1f [%.1f..%.1f] anchored=%.1f [%.1f..%.1f] paired_delta=%+.1f ns/call [IQR %+.1f..%+.1f] prog_only=%.1f ns/run (%d runs)",
			bc.fn, bc.n, bm, b25, b75, am, a25, a75, dm, d25, d75, perProg, runCnt)
	}
}
