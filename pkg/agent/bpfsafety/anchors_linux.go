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
)

type pendingAnchor struct {
	funcName string
	linkPath string
	lnk      link.Link
}

// ensureAnchor ensures one pinned no-op fentry anchor link exists under
// <host-bpffs>/keploy-grace/anchors/<funcName>-link (alongside <funcName>-prog)
// BEFORE the first real tracing attach on funcName. Works for both vmlinux and
// kernel-module functions (e.g. tls.ko).
//
// Safety invariants:
//   - No Keploy path ever removes an anchor's last link on any kernel — anchors
//     are released only by node reboot (preventing last-link -ENOMEM ftrace_ops
//     UAF on 6.0+ and static-trampoline page free on 5.10–5.11).
//   - If LoadPinnedLink fails with any non-ENOENT error (e.g. EMFILE, ENOMEM,
//     EACCES), linkPath is NEVER unlinked because it may hold a live anchor.
//   - If l.Pin(linkPath) fails after attachPinnedAnchorNoop succeeds, l is NEVER
//     closed (closing it would drop the function's only link under memory
//     pressure). Instead l is retained on the owner and re-pinned on subsequent
//     reaper rounds and at shutdown.
func (g *graceOwner) ensureAnchor(funcName string, logger *zap.Logger) {
	if g == nil || funcName == "" {
		return
	}
	if !g.rootIsBPFFS {
		return
	}
	g.anchorMu.Lock()
	if g.anchoredFuncs[funcName] {
		g.anchorMu.Unlock()
		return
	}
	unpinned := g.hasUnpinnedAnchorLocked(funcName)
	g.anchorMu.Unlock()
	if unpinned {
		// An anchor link for funcName is already attached in-process; retry
		// pinning that existing FD instead of attaching a second anchor link.
		g.retryUnpinnedAnchors()
		return
	}

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
	// NEVER unlink linkPath on EMFILE, ENOMEM, EACCES, or any non-ENOENT error
	// because it may be a live pinned anchor link!
	if existing, lerr := link.LoadPinnedLink(linkPath, nil); lerr == nil {
		_ = existing.Close()
		g.markAnchored(funcName)
		return
	} else if !errors.Is(lerr, os.ErrNotExist) {
		if logger != nil {
			logger.Warn("bpf-safety: failed to inspect existing anchor link on host bpffs; leaving it untouched",
				zap.String("function", funcName), zap.String("linkPath", linkPath), zap.Error(lerr))
		}
		return
	}

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

	// Pin the no-op program BEFORE attaching so the anchor prog stays pinned
	// until reboot.
	if _, serr := os.Stat(progPath); serr == nil {
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

	if g.onBeforeAnchorPin != nil {
		g.onBeforeAnchorPin()
	}

	// Pin l BEFORE closing our process FD. If l.Pin fails (e.g. ENOMEM under
	// memcg pressure), NEVER call l.Close(): closing the function's only link
	// under memory pressure hits Linux 6.0+'s last-link ftrace_ops UAF and
	// Linux 5.10's static-trampoline page free. Retain the live link FD on the
	// owner and retry pinning on every reaper round and at shutdown.
	if err := l.Pin(linkPath); err != nil {
		g.anchorMu.Lock()
		// Do NOT mark anchoredFuncs[funcName]=true here: anchoredFuncs records
		// only anchors confirmed pinned on host bpffs. hasUnpinnedAnchorLocked
		// prevents attaching a second anchor link while pinProgram can tell
		// that this anchor is still unpinned on static-trampoline kernels.
		g.unpinnedAnchors = append(g.unpinnedAnchors, pendingAnchor{
			funcName: funcName,
			linkPath: linkPath,
			lnk:      l,
		})
		g.anchorMu.Unlock()
		if logger != nil {
			logger.Warn("bpf-safety: failed to pin anchor link on host bpffs; keeping live link FD open and retrying on reaper rounds",
				zap.String("function", funcName), zap.String("linkPath", linkPath), zap.Error(err))
		}
		return
	}

	// Safe to close our process FD now that the bpffs pin holds a permanent ref.
	_ = l.Close()
	g.markAnchored(funcName)
	if logger != nil {
		logger.Debug("bpf-safety: pinned trampoline anchor link on host bpffs",
			zap.String("function", funcName),
			zap.String("linkPath", linkPath))
	}
}

// retryUnpinnedAnchors retries pinning any anchor links whose initial l.Pin
// failed (e.g. transient memcg -ENOMEM). On success, closes the process FD
// (now backed by the bpffs pin); on failure, keeps the live link FD open so
// the trampoline's last link is never dropped.
func (g *graceOwner) retryUnpinnedAnchors() {
	if g == nil {
		return
	}
	g.anchorMu.Lock()
	if len(g.unpinnedAnchors) == 0 {
		g.anchorMu.Unlock()
		return
	}
	pending := g.unpinnedAnchors
	g.unpinnedAnchors = nil
	g.anchorMu.Unlock()

	var remaining []pendingAnchor
	for _, p := range pending {
		if p.lnk == nil {
			continue
		}
		err := p.lnk.Pin(p.linkPath)
		if err != nil && errors.Is(err, os.ErrExist) {
			altPath := filepath.Join(filepath.Dir(p.linkPath), fmt.Sprintf("%s-%s%s", sanitizeToken(p.funcName), randomNonce()[:6], anchorLinkSuffix))
			err = p.lnk.Pin(altPath)
		}
		if err != nil {
			remaining = append(remaining, p)
			continue
		}
		_ = p.lnk.Close()
		g.markAnchored(p.funcName)
		if g.logger != nil {
			g.logger.Info("bpf-safety: pinned deferred trampoline anchor link on retry",
				zap.String("function", p.funcName), zap.String("linkPath", p.linkPath))
		}
	}

	if len(remaining) > 0 {
		g.anchorMu.Lock()
		g.unpinnedAnchors = append(remaining, g.unpinnedAnchors...)
		g.anchorMu.Unlock()
	}
}

// retainUnpinnedAnchorsOnShutdown retries pinning deferred anchors and keeps
// any still-unpinned link FDs referenced until process exit so they are never
// closed prematurely before tracing teardown finishes.
func (g *graceOwner) retainUnpinnedAnchorsOnShutdown() {
	if g == nil {
		return
	}
	g.retryUnpinnedAnchors()
	g.anchorMu.Lock()
	left := g.unpinnedAnchors
	g.unpinnedAnchors = nil
	g.anchorMu.Unlock()
	if len(left) == 0 {
		return
	}
	stateMu.Lock()
	for _, p := range left {
		if p.lnk != nil {
			retainedAnchorLinks = append(retainedAnchorLinks, p.lnk)
		}
	}
	stateMu.Unlock()
}

func (g *graceOwner) markAnchored(funcName string) {
	g.anchorMu.Lock()
	if g.anchoredFuncs == nil {
		g.anchoredFuncs = make(map[string]bool)
	}
	g.anchoredFuncs[funcName] = true
	g.anchorMu.Unlock()
}

func (g *graceOwner) isAnchorPinned(funcName string) bool {
	if g == nil || funcName == "" {
		return false
	}
	g.anchorMu.Lock()
	pinned := g.anchoredFuncs[funcName]
	g.anchorMu.Unlock()
	return pinned
}

func (g *graceOwner) hasUnpinnedAnchorLocked(funcName string) bool {
	for _, p := range g.unpinnedAnchors {
		if p.funcName == funcName && p.lnk != nil {
			return true
		}
	}
	return false
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
//  2. Safely reaps dead map-bearing grace directories using the zero-timer
//     image-liveness / BPF_ENABLE_STATS reaper. On Linux 5.12+ with visible
//     kallsyms, it also reaps its temporary rebuild no-op directory once
//     /proc/kallsyms confirms the rebuild image is freed; on Linux 5.10–5.11,
//     hidden kallsyms, or -ENOMEM during cleanup, it never reaps that
//     temporary directory on BPF_ENABLE_STATS alone and leaves at most one
//     tiny mapless 2-insn rebuild no-op directory pinned safely for the next
//     agent's reaper or node reboot.
//  3. NEVER removes or detaches an anchor's last link on ANY kernel. Anchors
//     (mapless idle no-op fentry; at least one 4 KiB bpf_prog page per
//     anchored function on every kernel plus bpf_prog_aux, JIT image, and
//     trampoline image — sub-page bpf_prog_pack trampoline slot on x86 6.8+ /
//     arm64 6.9+, or a 4 KiB trampoline page on x86 5.10–6.7 / arm64
//     5.10–6.8) are released only by node reboot, preventing Linux 6.0+
//     last-link -ENOMEM ftrace_ops UAF and 5.10.0–5.10.27 / 5.11
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
	wrapNoopClose func(closeFn func())
}

func cleanupAnchorsInRoot(ctx context.Context, logger *zap.Logger, root string, waitTimeout time.Duration, hooks *anchorCleanupHooks) error {
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
	var wrapClose func(func())
	if hooks != nil {
		wrapClose = hooks.wrapNoopClose
	}
	reapDeadDirsIdle(ctx, logger, root, waitTimeout, wrapClose)

	// No Keploy path ever removes an anchor's last link on any kernel —
	// anchors stay pinned until the next reboot.
	n := CountAnchorLinks(root)
	msg := fmt.Sprintf("%d idle keploy anchors stay until the next reboot (safe; idle overhead tens of ns/call; on Linux 5.x livepatches touching anchored functions wait for a reboot)", n)
	fmt.Println(msg)
	logger.Info(msg, zap.Int("anchors", n))
	return nil
}

// reapDeadDirsIdle safely reaps any dead grace directories left behind after
// all Keploy agents have stopped (using the same zero-timer image-liveness /
// BPF_ENABLE_STATS reaper with a real pinned no-op owner, never blind removal).
func reapDeadDirsIdle(ctx context.Context, logger *zap.Logger, root string, waitTimeout time.Duration, wrapNoopClose func(func())) {
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
	if wrapNoopClose != nil {
		tmpOwner.reaper.wrapNoopClose = wrapNoopClose
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

	// Reap tmpOwner.dir ONLY when /proc/kallsyms proves every snapshotted
	// trampoline image containing our rebuild no-op has been freed.
	// NEVER reap tmpOwner.dir on BPF_ENABLE_STATS run_cnt alone: if the
	// rebuild no-op's own link close hit -ENOMEM, it stays in the current
	// trampoline image and a flat run_cnt on an idle function would unpin
	// live code and panic the node. On Linux 5.10..5.11, hidden kallsyms, or
	// -ENOMEM during cleanup, tmpOwner.dir (a tiny mapless 2-insn no-op) stays
	// pinned safely for the next agent's reaper or node reboot.
	if len(noopPreCloseRefs) == 0 {
		return
	}
	cleaner := newGraceReaper(root, "", DefaultReapInterval, logger)
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
