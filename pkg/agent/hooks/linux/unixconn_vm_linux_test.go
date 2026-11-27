//go:build linux && integration

package linux

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/agent/bpfsafety"
)

// TestOnKernelUnixconnRealObjectFexitPinsResolve loads the real embedded
// unixconn BPF object through loadUnixConnWith(..., false) alongside the real
// bpfsafety.Init on host bpffs. Proves across Linux 5.10, 5.15, 5.19, and 6.12
// that every fexit program passes its real kernel AttachTo symbol (never its
// ELF program name or nil spec — kills M12, M12b, and M29 on real 5.x kernels),
// attaches without falling back to syscall tracepoints, creates all 3 anchors,
// and records all 3 per-pin target_funcs entries.
func TestOnKernelUnixconnRealObjectFexitPinsResolve(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Skip("requires root/CAP_BPF")
	}
	var stfs unix.Statfs_t
	if err := unix.Statfs(bpfsafety.DefaultNativeBPFFSPath, &stfs); err != nil || stfs.Type != unix.BPF_FS_MAGIC {
		t.Skipf("%s is not a bpffs mount", bpfsafety.DefaultNativeBPFFSPath)
	}
	if err := rlimit.RemoveMemlock(); err != nil {
		t.Fatalf("RemoveMemlock: %v", err)
	}
	bpfsafety.ResetShutdownForTest(t)
	bpfsafety.ResetUnprotectedForTest(t)

	mode := bpfsafety.DetectMode(false)
	if err := bpfsafety.Init(context.Background(), zap.NewNop(), mode); err != nil {
		t.Fatalf("bpfsafety.Init: %v", err)
	}
	if !bpfsafety.Armed() {
		t.Fatalf("expected bpfsafety.Armed()=true on host bpffs, graceMode=%q", bpfsafety.ActiveGraceMode())
	}

	u, err := loadUnixConnWith(zap.NewNop(), &bpfObjects{}, false)
	if err != nil {
		t.Fatalf("loadUnixConnWith(fexit) refused or failed (M29 nil-spec or wrong AttachTo?): %v", err)
	}
	defer func() {
		u.close()
		bpfsafety.GracefulShutdown(context.Background(), zap.NewNop())
	}()

	if u.fallback {
		t.Fatalf("expected unixconn fexit path (fallback=false), got fallback=true (why=%q)", u.why)
	}
	if len(u.links) != len(unixFexitPrograms) {
		t.Fatalf("attached links = %d, want %d", len(u.links), len(unixFexitPrograms))
	}

	root := filepath.Join(bpfsafety.DefaultNativeBPFFSPath, bpfsafety.GraceRootSubdir)
	for _, fn := range []string{"unix_stream_connect", "unix_dgram_connect", "unix_dgram_sendmsg"} {
		anchorPath := filepath.Join(root, bpfsafety.AnchorsSubdir, fn+"-link")
		if _, serr := os.Stat(anchorPath); serr != nil {
			t.Fatalf("expected anchor link %s for unixconn target %s: %v", anchorPath, fn, serr)
		}
	}

	graceDir := bpfsafety.ActiveGraceDir()
	if graceDir == "" {
		t.Fatal("expected non-empty ActiveGraceDir")
	}
	m, err := ebpf.LoadPinnedMap(filepath.Join(graceDir, "target_funcs"), nil)
	if err != nil {
		t.Fatalf("load target_funcs in %s: %v", graceDir, err)
	}
	defer func() { _ = m.Close() }()
	info, err := m.Info()
	if err != nil {
		t.Fatalf("target_funcs Info: %v", err)
	}
	slot := make([]byte, info.ValueSize)
	found := map[string]bool{}
	for i := uint32(0); i < info.MaxEntries; i++ {
		if m.Lookup(i, slot) != nil || slot[0] == 0 {
			break
		}
		entry := strings.TrimRight(string(slot), "\x00")
		if idx := strings.IndexByte(entry, '='); idx > 0 {
			found[entry[idx+1:]] = true
		}
	}
	for _, fn := range []string{"unix_stream_connect", "unix_dgram_connect", "unix_dgram_sendmsg"} {
		if !found[fn] {
			t.Fatalf("target_funcs missing per-pin record for %s (found=%v)", fn, found)
		}
	}
}
