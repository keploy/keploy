//go:build linux

package bpfsafety

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/asm"
	"github.com/cilium/ebpf/link"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

const (
	// AnchorsSubdir holds pinned no-op fentry anchor links and programs under
	// <host-bpffs>/keploy-grace/anchors/. Anchors keep each hooked function's
	// kernel bpf_trampoline refcount >= 1 while Keploy is installed so an
	// exiting or SIGKILL'd agent is never the last link on the trampoline
	// (preventing Linux 6.12's fatal last-link ftrace_ops UAF + permanent EBUSY
	// until reboot).
	AnchorsSubdir = "anchors"

	// bpffs forbids '.' in pinned object names (kernel/bpf/inode.c returns EPERM),
	// so anchor link and prog pins use '-' suffixes.
	anchorLinkSuffix = "-link"
	anchorProgSuffix = "-prog"

	// anchorIdlePerCallCostNs is the measured median per-call overhead (ns) of
	// an idle 2-instruction no-op fentry anchor on Linux 6.12 (15x100k
	// interleaved unix_dgram_sendmsg benchmark: +196.2 ns/call).
	anchorIdlePerCallCostNs = 200
)

// ensureAnchor ensures one pinned no-op fentry anchor link exists under
// <host-bpffs>/keploy-grace/anchors/<funcName>-link (alongside <funcName>-prog)
// BEFORE the first real tracing attach on funcName. Works for both vmlinux and
// kernel-module functions (e.g. tls.ko).
//
// No Keploy path ever removes an anchor's last link on any kernel — anchors are
// released only by node reboot (preventing last-link -ENOMEM ftrace_ops UAF on
// 6.0+ and static-trampoline page free on 5.10–5.11).
func (g *graceOwner) ensureAnchor(funcName string, logger *zap.Logger) {
	if g == nil || funcName == "" {
		return
	}
	if !autoStateStore.isBPFFS(autoStateStore{}, g.root) {
		return
	}
	g.anchorMu.Lock()
	if g.anchoredFuncs[funcName] {
		g.anchorMu.Unlock()
		return
	}
	g.anchorMu.Unlock()

	anchorsDir := filepath.Join(g.root, AnchorsSubdir)
	if err := os.MkdirAll(anchorsDir, 0o755); err != nil {
		if logger != nil {
			logger.Warn("bpf-safety: failed to create anchors directory on host bpffs", zap.String("dir", anchorsDir), zap.Error(err))
		}
		return
	}

	// Serialize anchor creation across concurrent agents on the same node.
	lockFD, err := unix.Open(anchorsDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	defer func() { _ = unix.Close(lockFD) }()
	if err := unix.Flock(lockFD, unix.LOCK_EX); err != nil {
		return
	}

	tok := sanitizeToken(funcName)
	linkPath := filepath.Join(anchorsDir, tok+anchorLinkSuffix)
	progPath := filepath.Join(anchorsDir, tok+anchorProgSuffix)

	// Fast path: anchor link already pinned by an earlier agent on this node.
	if existing, lerr := link.LoadPinnedLink(linkPath, nil); lerr == nil {
		_ = existing.Close()
		g.markAnchored(funcName)
		return
	}

	// Clean any orphaned .link (without touching a .prog that may be held for a
	// prior stale last-link condition!).
	_ = os.Remove(linkPath)

	prog, err := ebpf.NewProgram(&ebpf.ProgramSpec{
		Name:       "kp_anchor",
		Type:       ebpf.Tracing,
		AttachType: ebpf.AttachTraceFEntry,
		AttachTo:   funcName,
		License:    "GPL",
		Instructions: asm.Instructions{
			asm.Mov.Imm(asm.R0, 0),
			asm.Return(),
		},
	})
	if err != nil {
		if logger != nil {
			logger.Warn("bpf-safety: failed to load trampoline anchor no-op fentry program (trampoline running unanchored)", zap.String("function", funcName), zap.Error(err))
		}
		return
	}
	defer func() { _ = prog.Close() }()

	// Pin the no-op program BEFORE attaching so that even if a future calm
	// removal of the anchor link hits -ENOMEM, the program stays pinned until
	// reboot.
	if _, serr := os.Stat(progPath); serr == nil {
		// Previous .prog already exists (e.g. preserved from a stale removal);
		// use a unique suffix so we never unpin a previously preserved prog.
		progPath = filepath.Join(anchorsDir, fmt.Sprintf("%s-%s%s", tok, randomNonce()[:6], anchorProgSuffix))
	}
	if err := prog.Pin(progPath); err != nil {
		if logger != nil {
			logger.Warn("bpf-safety: failed to pin anchor program on host bpffs", zap.String("function", funcName), zap.Error(err))
		}
		return
	}

	l, err := attachPinnedAnchorNoop(prog)
	if err != nil {
		_ = os.Remove(progPath)
		if errors.Is(err, unix.EBUSY) {
			ReportStaleTrampoline(logger, funcName, err)
			return
		}
		if logger != nil {
			logger.Warn("bpf-safety: failed to attach trampoline anchor no-op fentry (trampoline running unanchored)", zap.String("function", funcName), zap.Error(err))
		}
		return
	}
	defer func() { _ = l.Close() }()

	if err := l.Pin(linkPath); err != nil {
		if logger != nil {
			logger.Warn("bpf-safety: failed to pin anchor link on host bpffs", zap.String("function", funcName), zap.Error(err))
		}
		return
	}

	g.markAnchored(funcName)
	if logger != nil {
		logger.Debug("bpf-safety: pinned trampoline anchor link on host bpffs",
			zap.String("function", funcName),
			zap.String("linkPath", linkPath))
	}
}

func (g *graceOwner) markAnchored(funcName string) {
	g.anchorMu.Lock()
	if g.anchoredFuncs == nil {
		g.anchoredFuncs = make(map[string]bool)
	}
	g.anchoredFuncs[funcName] = true
	g.anchorMu.Unlock()
}

// CountAnchorLinks returns the number of pinned anchor links currently present
// under <root>/anchors/.
func CountAnchorLinks(root string) int {
	entries, err := os.ReadDir(filepath.Join(root, AnchorsSubdir))
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if strings.HasSuffix(e.Name(), anchorLinkSuffix) {
			n++
		}
	}
	return n
}

// CleanupAnchors safely reaps dead grace directories left behind by exited
// Keploy agents under <hostBPFFS>/keploy-grace/ (intended for explicit
// `keploy agent cleanup` or idle maintenance — NEVER called by a dying,
// exiting, or killed agent) and reports any idle trampoline anchors.
//
// Safety invariants (identical on every Linux kernel):
//  1. Refuses to run if ANY live grace-* / grace2-* directory currently holds
//     flock(LOCK_EX) (an active Keploy agent is running on the node).
//  2. Safely reaps all dead grace directories using the zero-timer
//     image-liveness / BPF_ENABLE_STATS reaper (leaving 0 leftover grace dirs,
//     including on Linux 5.10–5.11 and hidden-kallsyms nodes).
//  3. NEVER removes or detaches an anchor's last link on ANY kernel. Anchors
//     (~4 KiB total, idle no-op fentry) are released only by node reboot,
//     preventing Linux 6.0+ last-link -ENOMEM ftrace_ops UAF and 5.10–5.11
//     static-trampoline page free.
func CleanupAnchors(ctx context.Context, logger *zap.Logger, hostBPFFS string) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	if strings.TrimSpace(hostBPFFS) == "" {
		if err := ValidateHostBPFFSPath(DefaultHostBPFFSPath, false); err == nil {
			hostBPFFS = DefaultHostBPFFSPath
		} else {
			hostBPFFS = DefaultNativeBPFFSPath
		}
	}
	root := filepath.Join(hostBPFFS, GraceRootSubdir)
	return cleanupAnchorsInRoot(ctx, logger, root, 10*time.Second, nil)
}

type anchorCleanupHooks struct {
	beforeLinkClose     func(funcName string)
	afterLinkClose      func(funcName string)
	dynamicTrampImageFn func() bool
	kallsymsSysctlFn    func() bool
	readKallsymsFn      func() (map[uint64][]uint64, bool, error)
}

func cleanupAnchorsInRoot(ctx context.Context, logger *zap.Logger, root string, waitTimeout time.Duration, _ *anchorCleanupHooks) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	if waitTimeout <= 0 {
		waitTimeout = 10 * time.Second
	}

	// Refuse if any live agent holds flock on a grace-* / grace2-* directory under root.
	if live, err := hasAnyLiveGraceDir(root); err != nil {
		return fmt.Errorf("check live grace directories under %s: %w", root, err)
	} else if live {
		return errors.New("refusing to run cleanup while a live Keploy agent is running on this host")
	}

	// Safely reap any dead grace directories left behind by exited agents
	// (using the same zero-timer image-liveness / BPF_ENABLE_STATS reaper,
	// never blind removal).
	reapDeadDirsIdle(ctx, logger, root, waitTimeout)

	// No Keploy path ever removes an anchor's last link on any kernel —
	// anchors stay pinned until the next reboot.
	n := CountAnchorLinks(root)
	msg := fmt.Sprintf("%d idle keploy anchors stay until the next reboot (safe; per-call cost %d ns)", n, anchorIdlePerCallCostNs)
	fmt.Println(msg)
	logger.Info(msg, zap.Int("anchors", n), zap.Int("perCallCostNs", anchorIdlePerCallCostNs))
	return nil
}

// reapDeadDirsIdle safely reaps any dead grace directories left behind after
// all Keploy agents have stopped (using the same zero-timer image-liveness /
// BPF_ENABLE_STATS reaper with a real pinned no-op owner, never blind removal).
func reapDeadDirsIdle(ctx context.Context, logger *zap.Logger, root string, waitTimeout time.Duration) {
	if !autoStateStore.isBPFFS(autoStateStore{}, root) {
		return
	}
	if !hasAnyDeadGraceDir(root) {
		return
	}
	tmpOwner, err := openGraceOwnerInRoot(root, logger)
	if err != nil {
		return
	}
	var (
		preAttachRefs    []trampImageRef
		noopPreCloseRefs []trampImageRef
	)
	tmpOwner.reaper.onBeforeNoopAttach = func() {
		preAttachRefs = tmpOwner.captureKallsymsSnapshot()
	}
	tmpOwner.reaper.onBeforeNoopClose = func() {
		after := tmpOwner.captureKallsymsSnapshot()
		// Exclude addresses already present before attachPinnedReaperNoop
		// (which belong to the pre-existing anchor image and may be reused by
		// bpf_prog_pack once freed); keep only the newly allocated image(s)
		// that actually contain our rebuild no-op.
		preSet := make(map[trampImageRef]struct{}, len(preAttachRefs))
		for _, ref := range preAttachRefs {
			preSet[ref] = struct{}{}
		}
		var added []trampImageRef
		for _, ref := range after {
			if _, existed := preSet[ref]; !existed {
				added = append(added, ref)
			}
		}
		if len(added) == 0 {
			added = after
		}
		noopPreCloseRefs = mergeSnapshotRefs(noopPreCloseRefs, added)
	}
	deadline := time.Now().Add(waitTimeout)
	for time.Now().Before(deadline) {
		if ctx.Err() != nil {
			break
		}
		dirs, _ := tmpOwner.reaper.round()
		// dirs includes tmpOwner.self (1) while tmpOwner holds flock.
		if dirs <= 1 {
			break
		}
		time.Sleep(300 * time.Millisecond)
	}
	// Release tmpOwner cleanly: if tmpOwner pinned no rebuild no-op, remove
	// tmpOwner.dir immediately.
	tmpOwner.reaper.closeNoopCache()
	if countPinnedProgs(tmpOwner.dir) == 0 {
		_ = unix.Close(tmpOwner.lockFD)
		_ = os.RemoveAll(tmpOwner.dir)
		return
	}
	tmpOwner.persistCleanSnapshot(noopPreCloseRefs, logger)
	_ = unix.Close(tmpOwner.lockFD)

	cleaner := newGraceReaper(root, "", DefaultReapInterval, logger)
	// On Linux 5.12+ with visible kallsyms, poll until every rebuild no-op
	// trampoline image has cleared from /proc/kallsyms (typically ~1s).
	if len(noopPreCloseRefs) > 0 {
		cleanDeadline := time.Now().Add(5 * time.Second)
		if deadline.After(cleanDeadline) {
			cleanDeadline = deadline
		}
		for time.Now().Before(cleanDeadline) {
			if ctx.Err() != nil {
				return
			}
			cleaner.round()
			if _, serr := os.Stat(tmpOwner.dir); os.IsNotExist(serr) {
				return
			}
			time.Sleep(300 * time.Millisecond)
		}
	}

	// Fallback for Linux 5.10..5.11 (static trampoline page), hidden kallsyms,
	// or 6.12 bpf_prog_pack slot reuse: verify tmpOwner.dir's own pinned
	// rebuild no-op is no longer executing via BPF_ENABLE_STATS (~2s flat
	// run_cnt) before removing tmpOwner.dir (fail closed if stats fail or
	// run_cnt moves).
	if ctx.Err() != nil {
		return
	}
	if _, serr := os.Stat(tmpOwner.dir); os.IsNotExist(serr) {
		return
	}
	closer, serr := cleaner.enableStatsFn()
	if serr != nil {
		logger.Warn("bpf-safety: BPF_ENABLE_STATS unavailable during idle cleanup; keeping temporary rebuild grace dir",
			zap.String("graceDir", tmpOwner.dir), zap.Error(serr))
		return
	}
	c1, err1 := cleaner.sumDirRunCnt(tmpOwner.dir)
	time.Sleep(cleaner.statsProbeWait)
	c2, err2 := cleaner.sumDirRunCnt(tmpOwner.dir)
	_ = closer.Close()
	if err1 != nil || err2 != nil || c2 > c1 {
		logger.Warn("bpf-safety: temporary rebuild no-op still active or unreadable; keeping temporary grace dir",
			zap.String("graceDir", tmpOwner.dir), zap.Uint64("runCntBefore", c1), zap.Uint64("runCntAfter", c2))
		return
	}
	cleaner.reapDir(tmpOwner.dir, countPinnedProgs(tmpOwner.dir), "idle cleanup rebuild noop confirmed idle via BPF_ENABLE_STATS")
}

func hasAnyDeadGraceDir(root string) bool {
	entries, err := os.ReadDir(root)
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && isManagedGraceDirName(e.Name()) {
			return true
		}
	}
	return false
}

func hasAnyLiveGraceDir(root string) (bool, error) {
	entries, err := os.ReadDir(root)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return false, err
	}
	for _, e := range entries {
		if !e.IsDir() || !isManagedGraceDirName(e.Name()) {
			continue
		}
		dirPath := filepath.Join(root, e.Name())
		fd, oerr := unix.Open(dirPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if oerr != nil {
			continue
		}
		ferr := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB)
		_ = unix.Close(fd)
		if ferr != nil {
			return true, nil
		}
	}
	return false, nil
}
