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

// ensureAnchor ensures one pinned no-op fentry anchor link exists under
// <host-bpffs>/keploy-grace/anchors/<funcName>.link (alongside <funcName>.prog)
// BEFORE the first real tracing attach on funcName. Works for both vmlinux and
// kernel-module functions (e.g. tls.ko).
//
// Anchors are NEVER removed by an exiting, dying, or SIGKILL'd agent — only by
// a calm CleanupAnchors pass when no live agent holds a grace dir lock.
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
			logger.Debug("bpf-safety: skipped anchor no-op fentry load", zap.String("function", funcName), zap.Error(err))
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
			logger.Debug("bpf-safety: anchor fentry attach failed", zap.String("function", funcName), zap.Error(err))
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

// CleanupAnchors performs a CALM removal of pinned anchor links under
// <hostBPFFS>/keploy-grace/anchors/ (intended ONLY for explicit
// `keploy agent cleanup`, chart pre-delete hooks, or calm idle maintenance —
// NEVER called by a dying, exiting, or killed agent).
//
// Safety invariants:
//  1. Refuses to remove anchors if ANY live grace-* directory currently holds
//     flock(LOCK_EX) (an active Keploy agent is running on the node).
//  2. For each anchor link, snapshots /proc/kallsyms BEFORE detaching, unpins
//     and closes the link, then verifies via /proc/kallsyms that every
//     snapshotted trampoline image address is freed BEFORE unpinning the anchor
//     program (.prog).
//  3. If a snapshotted image address STILL persists after detaching the anchor
//     link (the permanent last-link -ENOMEM stale state), keeps the anchor
//     program (.prog) pinned until reboot, raises StaleTrampolineDetected() and
//     logs a loud "drain and reboot this node" warning, and returns an error.
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
	beforeLinkClose  func(funcName string)
	afterLinkClose   func(funcName string)
	kallsymsSysctlFn func() bool
	readKallsymsFn   func() (map[uint64][]uint64, bool, error)
}

func cleanupAnchorsInRoot(ctx context.Context, logger *zap.Logger, root string, waitTimeout time.Duration, hooks *anchorCleanupHooks) error {
	if logger == nil {
		logger = zap.NewNop()
	}
	if waitTimeout <= 0 {
		waitTimeout = 10 * time.Second
	}
	ksysctl := kallsymsSysctlAllowsJIT
	readKsym := func() (map[uint64][]uint64, bool, error) { return parseKallsymsTrampolines(kallsymsPath) }
	if hooks != nil {
		if hooks.kallsymsSysctlFn != nil {
			ksysctl = hooks.kallsymsSysctlFn
		}
		if hooks.readKallsymsFn != nil {
			readKsym = hooks.readKallsymsFn
		}
	}

	// Refuse if any live agent holds flock on a grace-* directory under root.
	if live, err := hasAnyLiveGraceDir(root); err != nil {
		return fmt.Errorf("check live grace directories under %s: %w", root, err)
	} else if live {
		return errors.New("refusing to remove trampoline anchors while a live Keploy agent is running on this host")
	}

	anchorsDir := filepath.Join(root, AnchorsSubdir)
	entries, err := os.ReadDir(anchorsDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return fmt.Errorf("read anchors directory %s: %w", anchorsDir, err)
	}

	lockFD, err := unix.Open(anchorsDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open anchors dir for flock: %w", err)
	}
	defer func() { _ = unix.Close(lockFD) }()
	if err := unix.Flock(lockFD, unix.LOCK_EX); err != nil {
		return fmt.Errorf("flock anchors dir: %w", err)
	}

	// Re-check live agents after acquiring anchorsDir lock.
	if live, err := hasAnyLiveGraceDir(root); err != nil {
		return err
	} else if live {
		return errors.New("refusing to remove trampoline anchors while a live Keploy agent is running on this host")
	}

	var staleFuncs []string
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), anchorLinkSuffix) {
			continue
		}
		funcToken := strings.TrimSuffix(e.Name(), anchorLinkSuffix)
		linkPath := filepath.Join(anchorsDir, e.Name())
		progPath := filepath.Join(anchorsDir, funcToken+anchorProgSuffix)

		// Resolve key and function name from the pinned anchor prog if present.
		var key uint64
		funcName := funcToken
		if p, perr := ebpf.LoadPinnedProgram(progPath, nil); perr == nil {
			if info, ierr := queryProgInfo(p.FD()); ierr == nil {
				if k, fn, isTramp, _, rerr := resolveTrampolineTarget(info); rerr == nil && isTramp {
					key = k
					if fn != "" {
						funcName = fn
					}
				}
			}
			_ = p.Close()
		}

		// Snapshot /proc/kallsyms BEFORE detaching the anchor link!
		var snap []trampImageRef
		useKallsyms := false
		if key != 0 && ksysctl() {
			if tramps, vis, kerr := readKsym(); kerr == nil && vis {
				snap = snapshotKeysFromKallsyms(tramps, map[uint64]struct{}{key: {}})
				if len(snap) > 0 {
					useKallsyms = true
				}
			}
		}

		// Open the pinned link, unlink its bpffs pin, then close it cleanly.
		l, lerr := link.LoadPinnedLink(linkPath, nil)
		if lerr != nil {
			if errors.Is(lerr, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf("load pinned anchor link %s: %w", linkPath, lerr)
		}
		if rerr := os.Remove(linkPath); rerr != nil && !errors.Is(rerr, os.ErrNotExist) {
			_ = l.Close()
			return fmt.Errorf("unpin anchor link %s: %w", linkPath, rerr)
		}
		if hooks != nil && hooks.beforeLinkClose != nil {
			hooks.beforeLinkClose(funcName)
		}
		_ = l.Close()
		if hooks != nil && hooks.afterLinkClose != nil {
			hooks.afterLinkClose(funcName)
		}

		// Apply kallsyms image-liveness rule BEFORE unpinning the anchor .prog!
		if useKallsyms {
			cleared := false
			deadline := time.Now().Add(waitTimeout)
			for time.Now().Before(deadline) {
				if ctx.Err() != nil {
					break
				}
				if tramps, vis, kerr := readKsym(); kerr == nil && vis {
					if len(survivingSnapshotKeys(snap, tramps)) == 0 {
						cleared = true
						break
					}
				}
				time.Sleep(200 * time.Millisecond)
			}
			if !cleared {
				// Trampoline image persisted after detaching the last link!
				// Keep .prog pinned until reboot and raise the loud alert.
				ReportStaleTrampoline(logger, funcName, errors.New("trampoline image persisted in /proc/kallsyms after calm anchor link detach"))
				logger.Warn("bpf-safety: keeping anchor program pinned until node reboot — trampoline image persisted after anchor detach; drain and reboot this node",
					zap.String("function", funcName),
					zap.String("progPath", progPath))
				staleFuncs = append(staleFuncs, funcName)
				continue
			}
			_ = os.Remove(progPath)
			logger.Info("bpf-safety: calmly removed trampoline anchor after kallsyms confirmed image freed",
				zap.String("function", funcName))
			continue
		}

		// Fallback when kallsyms is unavailable: BPF_ENABLE_STATS for 2s + flat run_cnt.
		// If stats fail or prog still ran, fail closed and keep .prog pinned.
		if p, perr := ebpf.LoadPinnedProgram(progPath, nil); perr == nil {
			closer, serr := defaultEnableStats()
			if serr != nil {
				_ = p.Close()
				logger.Warn("bpf-safety: BPF_ENABLE_STATS unavailable during anchor cleanup; keeping anchor prog pinned",
					zap.String("function", funcName), zap.Error(serr))
				continue
			}
			info1, err1 := queryProgInfo(p.FD())
			time.Sleep(defaultStatsProbeWait)
			info2, err2 := queryProgInfo(p.FD())
			_ = closer.Close()
			_ = p.Close()
			if err1 != nil || err2 != nil || info2.RunCnt > info1.RunCnt {
				ReportStaleTrampoline(logger, funcName, errors.New("anchor prog still executing after calm detach"))
				staleFuncs = append(staleFuncs, funcName)
				continue
			}
			_ = os.Remove(progPath)
		}
	}

	if len(staleFuncs) > 0 {
		return fmt.Errorf("stale trampoline image persisted after calm anchor removal on %v; anchor programs kept pinned — drain and reboot this node", staleFuncs)
	}
	return nil
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
		if !e.IsDir() || !strings.HasPrefix(e.Name(), GraceDirPrefix) {
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
