//go:build linux

// Package bpfsafety implements host-bpffs grace pinning for eBPF fentry/fexit
// trampoline programs (CVE-2026-98800 and -ENOMEM unlink-failure defense on
// Linux 5.10.28..7.2.9).
//
// On unpatched kernels prior to Linux 7.2.10 / 7.3 (commits 0fb5751a6e41 and
// 17637e1a581a):
//  1. Detaching an fentry/fexit program while another program remains attached
//     frees the detached struct bpf_prog after a single normal RCU grace period
//     rather than an RCU-tasks grace period (CVE-2026-98800).
//  2. If bpf_trampoline_unlink_prog -> bpf_trampoline_update fails with -ENOMEM
//     (e.g. SIGKILL interrupting pcpu_alloc's killable mutex during link close,
//     or memory pressure), the kernel leaves tr->cur_image installed and still
//     calling the unlinked struct bpf_prog while dropping link->prog's refcount.
//
// Defense:
//   - Pin every trampoline program into a per-instance directory on the HOST
//     bpffs superblock (<host-bpffs>/keploy-grace/grace-<nonce>/) BEFORE calling
//     link.AttachTracing, and hold flock(LOCK_EX) on that directory for the
//     lifetime of the process.
//   - Before closing tracing links on shutdown, run registered disarm hooks so
//     filter/targeting maps are cleared (a prog stuck in a stale image does
//     nothing), snapshot the live /proc/kallsyms `bpf_trampoline_<key> [bpf]`
//     image addresses into the grace directory, close tracing links immediately,
//     and exit without blocking while leaving pins for the cross-process reaper.
//   - The cross-process reaper NEVER uses a blind timer to unpin: it unpins a
//     dead grace directory ONLY once every snapshotted trampoline image address
//     is gone from /proc/kallsyms (confirming bpf_tramp_image_free completed
//     after RCU-tasks and percpu_ref drain). If a snapshotted image persists
//     (stale image from a failed -ENOMEM unlink), the reaper forces a trampoline
//     rebuild via a cached no-op fentry pinned in the reaper's OWN grace dir and
//     waits for the old address to vanish. If forced rebuild returns EBUSY
//     (permanent last-link ftrace UAF on 6.12), the reaper keeps the programs
//     pinned until reboot and raises keploy_bpf_stale_trampoline=1 + WARN.
//   - When /proc/kallsyms addresses are unavailable (bpf_jit_kallsyms=0,
//     bpf_jit_harden!=0, kptr_restrict, or pre-6.0 kernel), the reaper falls
//     back to forced rebuild + BPF_ENABLE_STATS (~2s) + flat run_cnt check;
//     otherwise it fails closed and keeps the pins indefinitely.
//   - Where no host bpffs is reachable, Init never aborts startup: it degrades
//     honestly to unprotected mode, logs one WARN, and surfaces unprotected
//     status in metrics and telemetry.
package bpfsafety

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/link"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/utils"
)

const (
	// DefaultHostBPFFSPath is the host bpffs mount point placed outside /sys
	// by DaemonSet, K8s sidecar webhook, replay pods, Docker, and Compose
	// launchers so container-private /sys/fs/bpf overlays and containerd's
	// AppArmor /sys/fs/[^c]*/** deny rules never affect grace pins.
	DefaultHostBPFFSPath = "/run/keploy/host-bpffs"

	// DefaultNativeBPFFSPath is the standard host bpffs mount on bare-metal/VM hosts.
	DefaultNativeBPFFSPath = "/sys/fs/bpf"

	// GraceRootSubdir is the dedicated top-level directory on host bpffs for
	// trampoline grace pins (<host-bpffs>/keploy-grace/). It deliberately lives
	// OUTSIDE <host-bpffs>/keploy/ so selfupdate.WipeBPFPins, inst-* sweeps,
	// and legacy map-pin cleanups can never delete live or cooling grace dirs.
	GraceRootSubdir = "keploy-grace"

	// GraceModeHostBPFFS indicates host-bpffs grace pinning is active.
	GraceModeHostBPFFS = "host-bpffs"

	// GraceModeUnprotected indicates no host bpffs was reachable (or the
	// explicit break-glass opt-out was set), so trampoline detach crash
	// protection is OFF.
	GraceModeUnprotected = "unprotected"

	// UnprotectedWarnMessage is the exact single warning logged when Init
	// cannot find a writable host bpffs mount and degrades to honest
	// unprotected mode.
	UnprotectedWarnMessage = "eBPF crash protection is OFF: mount the host's /sys/fs/bpf at /run/keploy/host-bpffs (see docs/kernel-safety.md)"

	// UnprotectedMountCheckErrWarnMessage is logged when checking the host
	// bpffs mount itself failed with an operational system error (for example
	// transient statfs/stat ENOMEM or EIO) even after retry, rather than bpffs
	// being absent or misconfigured.
	UnprotectedMountCheckErrWarnMessage = "eBPF crash protection is OFF: host bpffs mount check failed with a system error (see docs/kernel-safety.md)"

	// UnprotectedPermWarnMessage is the prefix logged when accessing the host
	// bpffs mount failed with EACCES or EPERM (for example userns, AppArmor, or
	// SELinux denying traversal when the path is mounted but unreachable).
	// unprotectedPermWarnMessage formats the complete message naming the actual
	// denied path (including any KEPLOY_HOST_BPFFS override).
	UnprotectedPermWarnMessage = "eBPF crash protection is OFF: permission denied accessing host bpffs mount"

	// UnprotectedInitErrWarnMessage is logged when a host bpffs mount validated
	// but grace directory initialization failed (for example under memory
	// pressure or ENOSPC), so the warning names the real cause rather than
	// claiming bpffs is unmounted.
	UnprotectedInitErrWarnMessage = "eBPF crash protection is OFF: host bpffs mount is present but grace directory initialization failed (see docs/kernel-safety.md)"

	// UnprotectedOptOutWarnMessage is logged when the operator explicitly set
	// KEPLOY_ALLOW_UNPROTECTED_TRAMPOLINES=1.
	UnprotectedOptOutWarnMessage = "eBPF crash protection is OFF: KEPLOY_ALLOW_UNPROTECTED_TRAMPOLINES=1 is set (see docs/kernel-safety.md)"

	// EnvHostBPFFS overrides the host bpffs mount path.
	EnvHostBPFFS = "KEPLOY_HOST_BPFFS"

	// EnvHostBPFFSDev carries the host bpffs st_dev captured by PID 1 (e.g.
	// the self-update supervisor) before any child overlays /sys/fs/bpf.
	EnvHostBPFFSDev = "KEPLOY_HOST_BPFFS_DEV"

	// EnvLaunchMode explicitly tells DetectMode which launcher started the
	// agent ("daemonset", "k8s-sidecar", "k8s-replay", "compose", "docker",
	// "native"), avoiding brittle ambient heuristics like KUBERNETES_SERVICE_HOST.
	EnvLaunchMode = "KEPLOY_LAUNCH_MODE"

	// EnvAllowUnprotectedTrampolines forces unprotected mode even when host
	// bpffs is present (break-glass / diagnostics only).
	EnvAllowUnprotectedTrampolines = "KEPLOY_ALLOW_UNPROTECTED_TRAMPOLINES"

	// EnvGraceTestTag scopes test runs to their own directory prefix
	// (grace-test-<tag>-*) so a test run on a shared host can never reap
	// another lane's or production agent's grace-* directories.
	EnvGraceTestTag = "KEPLOY_BPF_GRACE_TEST_TAG"

	// GraceDirPrefix names v3.7.28+ grace directories under
	// <host-bpffs>/keploy-grace/. Using a distinct prefix ("grace2-") ensures
	// older v3.7.27 timer-only reapers (which only scan "grace-") never touch
	// or prematurely unpin v3.7.28+ directories during mixed-version rollouts.
	GraceDirPrefix = "grace2-"

	// LegacyGraceDirPrefix is the v3.7.27 directory prefix, which the v3.7.28+
	// reaper still scans and safely reaps via kallsyms + forced rebuild.
	LegacyGraceDirPrefix = "grace-"

	// GraceTestDirPrefix names isolated test directories.
	GraceTestDirPrefix = "grace2-test-"

	// DefaultReapInterval controls how often the cross-process reaper scans
	// <host-bpffs>/keploy-grace/ for dead directories whose trampoline images
	// have been freed in /proc/kallsyms. Clean detaches typically free their
	// image within ~1s, so a 15s scan interval reclaims dead pins promptly.
	DefaultReapInterval = 15 * time.Second

	defaultStatsProbeWait = 2 * time.Second
)

// Mode identifies the runtime environment so Init selects the right mount path.
type Mode string

const (
	ModeDaemonSet  Mode = "daemonset"
	ModeK8sSidecar Mode = "k8s-sidecar"
	ModeCompose    Mode = "compose"
	ModeDocker     Mode = "docker"
	ModeNative     Mode = "native"
)

// TestCleaner matches testing.TB without importing the testing package into
// production binaries.
type TestCleaner interface {
	Helper()
	Cleanup(func())
}

// ErrGraceNotArmed is returned by PinBeforeAttach when bpfsafety.Init has not
// been called (neither host-bpffs grace pinning nor honest unprotected mode is
// initialized).
var ErrGraceNotArmed = errors.New("bpf trampoline grace pinning is not armed (refusing unprotected fentry/fexit attach; call bpfsafety.Init first)")

// ErrShuttingDown is returned by PinBeforeAttach once GracefulShutdown has
// closed the attach gate.
var ErrShuttingDown = errors.New("bpf trampoline safety gate is closed for shutdown (refusing late fentry/fexit attach)")

// ErrPinFailed is wrapped around bpffs pin errors in PinBeforeAttach so callers
// can distinguish host-bpffs pin failures (e.g. EACCES, ENOSPC) from kernel
// link.AttachTracing failures (e.g. missing trampoline support).
var ErrPinFailed = errors.New("bpf trampoline grace pin failed")

// ErrUnresolvedTarget is returned by PinBeforeAttach when a Tracing/LSM
// program's kernel target function cannot be resolved (caller must pass
// ProgramSpec.AttachTo so Linux 5.10..5.19 and CAP_BPF-only module targets
// can be anchored and safely reaped per program).
var ErrUnresolvedTarget = errors.New("bpf tracing program target function could not be resolved")

// ErrAnchorUnpinned is returned by PinBeforeAttach on static-trampoline
// kernels (Linux 5.10.0–5.10.27 and 5.11.0–5.11.10, lacking struct
// bpf_tramp_image) when the host-bpffs trampoline anchor link for the target
// function could not be pinned. Failing closed prevents attaching fexit
// programs onto an unpinned anchor whose clean exit-time close would free the
// static trampoline page while tasks are sleeping in the hooked function.
var ErrAnchorUnpinned = errors.New("bpf trampoline anchor is not pinned on host bpffs")

// MetricsObserver receives updated host-bpffs grace counts, anchor link count,
// unprotected status, and permanent stale-trampoline status (EBUSY last-link failure).
type MetricsObserver func(dirsPresent, pinnedProgs, anchorLinks int, unprotected, staleTrampoline bool)

var (
	initMu                    sync.Mutex
	stateMu                   sync.Mutex
	activeOwner               *graceOwner
	disarmHooks               []func()
	detachHooks               []func()
	unpinnedLockFDs           []int
	retainedAnchorLinks       []link.Link
	privateOverlayDevs        = map[uint64]bool{}
	staleTargets              = map[string]bool{}
	unprotectedWarnedFuncs    = map[string]bool{}
	unprotectedLogger         *zap.Logger
	metricsObserver           MetricsObserver
	unprotectedTrampolines    atomic.Bool
	explicitUnprotectedOptOut atomic.Bool
	unprotectedWarned         atomic.Bool
	staleTrampolineDetected   atomic.Bool
	shuttingDown              atomic.Bool
	shortGraceTestMode        atomic.Bool
	testBypassPin             atomic.Bool

	// dynamicTrampImageCheckFn, validateHostBPFFSPathFn,
	// openGraceOwnerInRootFn, hasHostSharedBPFFSFn, nativeBPFFSStatfsFn,
	// nativeBPFFSMkdirAllFn, and nativeBPFFSMountFn are swappable in tests.
	dynamicTrampImageCheckFn = kernelHasDynamicTrampImage
	validateHostBPFFSPathFn  = ValidateHostBPFFSPath
	openGraceOwnerInRootFn   = openGraceOwnerInRoot
	hasHostSharedBPFFSFn     = hasHostSharedBPFFS
	nativeBPFFSStatfsFn      = unix.Statfs
	nativeBPFFSMkdirAllFn    = os.MkdirAll
	nativeBPFFSMountFn       = unix.Mount
)

// DetectMode infers the launch Mode from the daemonSet flag, explicit
// KEPLOY_LAUNCH_MODE, and container launcher signals.
//
// Deliberately does NOT inspect KUBERNETES_SERVICE_HOST: kubelet injects that
// into every pod, including generic Kubernetes CI runners (GitHub ARC, GitLab
// k8s executor, Tekton, Jenkins) running keploy natively with host /sys/fs/bpf.
func DetectMode(daemonSet bool) Mode {
	if daemonSet {
		return ModeDaemonSet
	}
	if explicit := strings.ToLower(strings.TrimSpace(os.Getenv(EnvLaunchMode))); explicit != "" {
		switch explicit {
		case "daemonset":
			return ModeDaemonSet
		case "k8s-sidecar", "k8s-replay", "sidecar":
			return ModeK8sSidecar
		case "compose":
			return ModeCompose
		case "docker":
			return ModeDocker
		case "native":
			return ModeNative
		}
	}
	if strings.EqualFold(strings.TrimSpace(os.Getenv("KEPLOY_DAEMONSET_ENABLED")), "true") {
		return ModeDaemonSet
	}
	inDocker := strings.EqualFold(strings.TrimSpace(os.Getenv("KEPLOY_INDOCKER")), "true")
	if inDocker && strings.TrimSpace(os.Getenv("KEPLOY_POD_UID")) != "" {
		return ModeK8sSidecar
	}
	if inDocker {
		if strings.TrimSpace(os.Getenv("KEPLOY_APP_CONTAINER_NAME")) != "" ||
			strings.EqualFold(strings.TrimSpace(os.Getenv("KEPLOY_COMPOSE_MODE")), "true") {
			return ModeCompose
		}
		return ModeDocker
	}
	return ModeNative
}

// SetMetricsObserver registers an optional callback (used by enterprise to
// export Prometheus gauges without adding Prometheus to OSS go.mod).
func SetMetricsObserver(fn MetricsObserver) {
	stateMu.Lock()
	metricsObserver = fn
	stateMu.Unlock()
}

func notifyMetrics(dirs, progs, anchors int) {
	stateMu.Lock()
	fn := metricsObserver
	stateMu.Unlock()
	if fn != nil {
		fn(dirs, progs, anchors, unprotectedTrampolines.Load(), staleTrampolineDetected.Load())
	}
}

// UnprotectedTrampolinesActive reports whether this process is running without
// host-bpffs trampoline grace pinning (either because no host bpffs was
// reachable or because KEPLOY_ALLOW_UNPROTECTED_TRAMPOLINES=1 was set).
func UnprotectedTrampolinesActive() bool {
	return unprotectedTrampolines.Load()
}

// StaleTrampolineDetected reports whether this process (either at attach time
// or during reaper forced rebuild) observed EBUSY indicating a permanent
// last-link stale trampoline on this node that requires draining and rebooting
// the node.
func StaleTrampolineDetected() bool {
	return staleTrampolineDetected.Load()
}

// ReportStaleTrampoline records a permanent stale-trampoline condition (EBUSY
// from an fentry/fexit attach on a kernel function), blocks further grace-pin
// leaks on funcName until reboot, logs a clear warning instructing the operator
// to drain and reboot the node, and updates metrics.
func ReportStaleTrampoline(logger *zap.Logger, funcName string, err error) {
	staleTrampolineDetected.Store(true)
	if funcName != "" {
		stateMu.Lock()
		staleTargets[funcName] = true
		staleTargets[sanitizeToken(funcName)] = true
		stateMu.Unlock()
	}
	notifyMetrics(0, 0, 0)
	if logger != nil {
		logger.Warn("bpf-safety: permanent stale BPF trampoline detected (EBUSY on tracing attach — hook unavailable until node reboot; drain and reboot this node)",
			zap.String("function", funcName),
			zap.Error(err))
	}
}

func isStaleTarget(funcName string) bool {
	if funcName == "" {
		return false
	}
	stateMu.Lock()
	defer stateMu.Unlock()
	return staleTargets[funcName] || staleTargets[sanitizeToken(funcName)]
}

// ActiveGraceMode returns GraceModeHostBPFFS when host-bpffs grace pinning is
// armed, GraceModeUnprotected when Init fell back to unprotected mode, or ""
// before Init has run.
func ActiveGraceMode() string {
	if Armed() {
		return GraceModeHostBPFFS
	}
	if unprotectedTrampolines.Load() {
		return GraceModeUnprotected
	}
	return ""
}

// Armed reports whether host bpffs trampoline grace pinning is currently active.
func Armed() bool {
	stateMu.Lock()
	ok := activeOwner != nil
	stateMu.Unlock()
	return ok
}

// ActiveGraceDir returns this process's host bpffs grace directory path, or "".
func ActiveGraceDir() string {
	stateMu.Lock()
	defer stateMu.Unlock()
	if activeOwner == nil {
		return ""
	}
	return activeOwner.dir
}

// RecordPrivateOverlayDev records the st_dev of a container-private bpffs
// overlay mounted over /sys/fs/bpf so ValidateHostBPFFSPath never accepts it
// as the host bpffs superblock.
func RecordPrivateOverlayDev(dev uint64) {
	if dev == 0 {
		return
	}
	stateMu.Lock()
	privateOverlayDevs[dev] = true
	stateMu.Unlock()
}

// CaptureHostBPFFSDev returns the st_dev of the host bpffs mount before any
// private overlay is stacked on /sys/fs/bpf. Always probes /run/keploy/host-bpffs
// first (so a supervisor re-captures the current host device after a node-side
// bpffs remount), then KEPLOY_HOST_BPFFS_DEV, then /sys/fs/bpf.
func CaptureHostBPFFSDev() (uint64, bool) {
	if dev, ok := bpffsDevice(DefaultHostBPFFSPath); ok {
		return dev, true
	}
	if raw := strings.TrimSpace(os.Getenv(EnvHostBPFFSDev)); raw != "" {
		if dev, err := strconv.ParseUint(raw, 10, 64); err == nil && dev != 0 {
			return dev, true
		}
	}
	return bpffsDevice(DefaultNativeBPFFSPath)
}

func bpffsDevice(path string) (uint64, bool) {
	var stfs unix.Statfs_t
	if err := unix.Statfs(path, &stfs); err != nil || stfs.Type != unix.BPF_FS_MAGIC {
		return 0, false
	}
	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return 0, false
	}
	return st.Dev, true
}

// SetBypassForTest permits unit tests that exercise BPF loaders without a host
// bpffs mount to bypass PinBeforeAttach's fail-closed check.
func SetBypassForTest(tb TestCleaner) {
	tb.Helper()
	prev := testBypassPin.Swap(true)
	tb.Cleanup(func() { testBypassPin.Store(prev) })
}

// AllowShortGraceForTest enables tagged test directory isolation in Go tests.
func AllowShortGraceForTest(tb TestCleaner) {
	tb.Helper()
	prev := shortGraceTestMode.Swap(true)
	tb.Cleanup(func() { shortGraceTestMode.Store(prev) })
}

// ResetShutdownForTest explicitly re-arms the attach gate closed by
// GracefulShutdown. Only tests may re-open the gate; production Init refuses
// to reopen it once shutdown has begun.
func ResetShutdownForTest(tb TestCleaner) {
	tb.Helper()
	prev := shuttingDown.Swap(false)
	tb.Cleanup(func() { shuttingDown.Store(prev) })
}

// ResetUnprotectedForTest resets the unprotected-mode, stale-trampoline,
// stale-target set, and warn-once flags for the duration of a test and restores
// them on cleanup.
func ResetUnprotectedForTest(tb TestCleaner) {
	tb.Helper()
	prevUnprot := unprotectedTrampolines.Swap(false)
	prevOptOut := explicitUnprotectedOptOut.Swap(false)
	prevWarned := unprotectedWarned.Swap(false)
	prevStale := staleTrampolineDetected.Swap(false)
	stateMu.Lock()
	prevTargets := staleTargets
	staleTargets = map[string]bool{}
	prevLogger := unprotectedLogger
	unprotectedLogger = nil
	prevWarnedFuncs := unprotectedWarnedFuncs
	unprotectedWarnedFuncs = map[string]bool{}
	stateMu.Unlock()
	tb.Cleanup(func() {
		unprotectedTrampolines.Store(prevUnprot)
		explicitUnprotectedOptOut.Store(prevOptOut)
		unprotectedWarned.Store(prevWarned)
		staleTrampolineDetected.Store(prevStale)
		stateMu.Lock()
		staleTargets = prevTargets
		unprotectedLogger = prevLogger
		unprotectedWarnedFuncs = prevWarnedFuncs
		stateMu.Unlock()
	})
}

// HasActiveWork reports whether this process has armed host-bpffs grace pins,
// registered disarm hooks, or registered tracing detach hooks that
// GracefulShutdown must execute on exit.
func HasActiveWork() bool {
	stateMu.Lock()
	ok := activeOwner != nil || len(disarmHooks) > 0 || len(detachHooks) > 0
	stateMu.Unlock()
	return ok
}

// Init verifies a writable host bpffs mount, creates and locks this process's
// <host-bpffs>/keploy-grace/grace-<nonce> directory (blocking flock(LOCK_EX)),
// and starts the background image-liveness reaper.
//
// On native Linux (ModeNative), if /sys/fs/bpf is not yet mounted as bpffs,
// Init attempts to mount it so native hosts always use real host bpffs.
//
// Where no usable host bpffs mount is reachable (for example a customer's own
// manifest or an older chart lacking /run/keploy/host-bpffs), access is denied
// by permissions/LSM, or mount validation / grace setup fails even after
// retrying transient system errors, Init never aborts recording: it enters
// honest unprotected mode, logs a cause-specific warning once
// (UnprotectedWarnMessage, UnprotectedPermWarnMessage,
// UnprotectedMountCheckErrWarnMessage, UnprotectedInitErrWarnMessage, or
// UnprotectedOptOutWarnMessage), sets UnprotectedTrampolinesActive()=true,
// updates metrics, and returns nil. On static-trampoline kernels
// (5.10.0–5.10.27 and 5.11.0–5.11.10), PinBeforeAttach still refuses tracing
// attaches with ErrAnchorUnpinned in Init-entered unprotected mode unless
// KEPLOY_ALLOW_UNPROTECTED_TRAMPOLINES=1 was explicitly set.
//
// Serialized and idempotent when already armed. Refuses to reopen the attach
// gate once GracefulShutdown has started (returns ErrShuttingDown).
func Init(ctx context.Context, logger *zap.Logger, mode Mode) error {
	if logger == nil {
		logger = zap.NewNop()
	}

	initMu.Lock()
	defer initMu.Unlock()

	if shuttingDown.Load() {
		return ErrShuttingDown
	}

	stateMu.Lock()
	alreadyArmed := activeOwner != nil
	stateMu.Unlock()
	if alreadyArmed {
		return nil
	}

	if strings.TrimSpace(os.Getenv(EnvAllowUnprotectedTrampolines)) == "1" {
		explicitUnprotectedOptOut.Store(true)
		enterUnprotectedMode(logger, mode, UnprotectedOptOutWarnMessage, errors.New("KEPLOY_ALLOW_UNPROTECTED_TRAMPOLINES=1 set"))
		return nil
	}

	owner, warnMsg, err := openGraceOwner(logger, mode)
	if err != nil {
		explicitUnprotectedOptOut.Store(false)
		enterUnprotectedMode(logger, mode, warnMsg, err)
		return nil
	}

	reaperCtx, reaperCancel := context.WithCancel(ctx)
	owner.reaperCancel = reaperCancel

	unprotectedTrampolines.Store(false)
	explicitUnprotectedOptOut.Store(false)
	stateMu.Lock()
	activeOwner = owner
	unprotectedLogger = nil
	unprotectedWarnedFuncs = map[string]bool{}
	stateMu.Unlock()

	fields := []zap.Field{
		zap.String("mode", string(mode)),
		zap.String("graceDir", owner.dir),
		zap.Duration("reapInterval", owner.reaper.reapInterval),
	}
	if owner.reaper.reapPrefix != GraceDirPrefix {
		fields = append(fields, zap.String("testReapPrefix", owner.reaper.reapPrefix))
	}
	logger.Info("bpf-safety: trampoline program grace pinning armed on host bpffs", fields...)

	owner.reaper.start(reaperCtx)
	return nil
}

func enterUnprotectedMode(logger *zap.Logger, mode Mode, msg string, cause error) {
	unprotectedTrampolines.Store(true)
	if logger != nil {
		stateMu.Lock()
		unprotectedLogger = logger
		stateMu.Unlock()
	}
	notifyMetrics(0, 0, 0)
	if unprotectedWarned.CompareAndSwap(false, true) && logger != nil {
		fields := []zap.Field{
			zap.String("mode", string(mode)),
			zap.String("graceMode", GraceModeUnprotected),
		}
		if cause != nil {
			fields = append(fields, zap.Error(cause))
		}
		logger.Warn(msg, fields...)
	}
}

func warnUnprotectedStaticRefusalOnce(funcName string) {
	stateMu.Lock()
	if unprotectedWarnedFuncs[funcName] {
		stateMu.Unlock()
		return
	}
	unprotectedWarnedFuncs[funcName] = true
	logger := unprotectedLogger
	stateMu.Unlock()
	if logger != nil {
		logger.Warn("bpf-safety: refusing tracing attach on static-trampoline kernel in unprotected mode because trampoline anchors cannot be pinned on host bpffs",
			zap.String("function", funcName))
	}
}

// RegisterDisarmHook registers a synchronous pre-detach callback that
// GracefulShutdown invokes BEFORE closing any tracing links. Loaders use this
// to clear targeting/filter maps or flip global disable flags so that if a
// subsequent link close fails with -ENOMEM and leaves a program running in a
// stale trampoline image, that program exits immediately without doing work.
func RegisterDisarmHook(fn func()) {
	if fn == nil {
		return
	}
	stateMu.Lock()
	if shuttingDown.Load() {
		stateMu.Unlock()
		fn()
		return
	}
	disarmHooks = append(disarmHooks, fn)
	stateMu.Unlock()
}

// RegisterDetachHook registers a synchronous link-teardown callback that
// GracefulShutdown invokes immediately after disarming and snapshotting live
// trampoline image addresses.
//
// If shutdown has already begun when a late loader calls RegisterDetachHook,
// fn is invoked synchronously right away so late-attached links are never left
// orphaned after shutdown.
func RegisterDetachHook(fn func()) {
	if fn == nil {
		return
	}
	stateMu.Lock()
	if shuttingDown.Load() {
		stateMu.Unlock()
		fn()
		utils.TeardownStep()
		return
	}
	detachHooks = append(detachHooks, fn)
	stateMu.Unlock()
}

// PinBeforeAttach pins prog into the active host-bpffs grace directory BEFORE
// link.AttachTracing is called, and records prog's trampoline key for clean-exit
// /proc/kallsyms image-liveness snapshotting.
//
// Fail-closed BY CONSTRUCTION: if GracefulShutdown has started, returns
// ErrShuttingDown; if Init has never been called (neither host-bpffs pinning
// nor honest unprotected mode is initialized) and SetBypassForTest is not
// active, returns ErrGraceNotArmed so no production path can silently bypass
// bpfsafety.Init.
func PinBeforeAttach(prog *ebpf.Program, object, name, role string) error {
	if prog == nil {
		return errors.New("cannot pin nil BPF program for trampoline grace")
	}
	stateMu.Lock()
	if shuttingDown.Load() {
		stateMu.Unlock()
		return ErrShuttingDown
	}
	owner := activeOwner
	stateMu.Unlock()
	if owner == nil {
		if testBypassPin.Load() || explicitUnprotectedOptOut.Load() {
			return nil
		}
		if unprotectedTrampolines.Load() {
			if !dynamicTrampImageCheckFn() {
				warnUnprotectedStaticRefusalOnce(name)
				return fmt.Errorf("%w: refusing to attach tracing program %s (%s) on static-trampoline kernel in unprotected mode (host bpffs grace pinning unavailable)", ErrAnchorUnpinned, name, object)
			}
			return nil
		}
		return ErrGraceNotArmed
	}
	return owner.pinProgram(prog, object, name, role)
}

// AttachTracing pins prog into the armed host-bpffs grace directory and then
// attaches it via link.AttachTracing. If link.AttachTracing fails, prog was
// never attached to any trampoline so its unused pin is immediately removed
// (preventing pin/map leaks on repeated EBUSY/ENOENT attempts). If the kernel
// returns EBUSY, it raises ReportStaleTrampoline before returning the error.
func AttachTracing(prog *ebpf.Program, object, name, role string) (link.Link, error) {
	if err := PinBeforeAttach(prog, object, name, role); err != nil {
		return nil, err
	}
	l, err := link.AttachTracing(link.TracingOptions{Program: prog})
	if err != nil {
		UnpinFailedAttach(prog)
		if errors.Is(err, unix.EBUSY) {
			stateMu.Lock()
			var logger *zap.Logger
			if activeOwner != nil {
				logger = activeOwner.logger
			}
			stateMu.Unlock()
			ReportStaleTrampoline(logger, name, err)
		}
	}
	return l, err
}

// UnpinFailedAttach removes the host-bpffs grace pin created by PinBeforeAttach
// for prog when the subsequent link.AttachTracing call FAILED (meaning prog was
// never attached to any kernel trampoline image). Never call this for a program
// whose attach succeeded.
func UnpinFailedAttach(prog *ebpf.Program) {
	if prog == nil {
		return
	}
	stateMu.Lock()
	owner := activeOwner
	stateMu.Unlock()
	if owner == nil {
		return
	}
	owner.unpinFailed(prog)
}

// attachPinnedReaperNoop attaches a cached reaper no-op program that was
// already pinned into the reaper's own active grace directory by
// graceReaper.forceRebuildTarget.
func attachPinnedReaperNoop(prog *ebpf.Program) (link.Link, error) {
	return link.AttachTracing(link.TracingOptions{Program: prog})
}

// attachPinnedAnchorNoop attaches a no-op anchor fentry program that was
// already pinned into <host-bpffs>/keploy-grace/anchors/ by
// graceOwner.ensureAnchor.
func attachPinnedAnchorNoop(prog *ebpf.Program) (link.Link, error) {
	return link.AttachTracing(link.TracingOptions{Program: prog})
}

// GracefulShutdown closes the attach gate, cancels and drains the background
// reaper BEFORE detaching (so the reaper cannot rebuild a trampoline during
// link close), runs all disarm hooks FIRST (so any prog caught in a stale
// trampoline image does nothing), closes all tracing links synchronously and
// fast, snapshots live /proc/kallsyms `bpf_trampoline_<key>` addresses (the
// union of before-detach and after-detach so images created during teardown
// are never missed), and returns IMMEDIATELY without blocking process exit.
func GracefulShutdown(_ context.Context, logger *zap.Logger) {
	if logger == nil {
		logger = zap.NewNop()
	}

	stateMu.Lock()
	shuttingDown.Store(true)
	disarms := disarmHooks
	disarmHooks = nil
	hooks := detachHooks
	detachHooks = nil
	owner := activeOwner
	activeOwner = nil
	stateMu.Unlock()

	utils.TeardownStep()

	// 1. Cancel and drain the background reaper BEFORE detaching any links so
	//    it cannot trigger a trampoline rebuild mid-teardown, and retry pinning
	//    any anchor link whose earlier pin failed under memory pressure.
	if owner != nil {
		if owner.reaper != nil {
			owner.reaper.stopAndWait(owner.reaperCancel)
			owner.reaper.closeNoopCache()
		} else if owner.reaperCancel != nil {
			owner.reaperCancel()
		}
		owner.retainUnpinnedAnchorsOnShutdown()
	}

	// 2. Disarm filter/targeting maps BEFORE closing any tracing links.
	for _, fn := range disarms {
		fn()
	}

	// 3. Capture pre-detach trampoline image addresses (in memory only).
	var preRefs []trampImageRef
	if owner != nil {
		preRefs = owner.captureKallsymsSnapshot()
	}

	// 4. Detach every registered tracing-link holder synchronously and fast.
	for _, fn := range hooks {
		fn()
		utils.TeardownStep()
	}

	// 5. Capture post-detach trampoline image addresses AFTER all detach hooks
	//    complete and persist the union (pre ∪ post). Any trampoline image built
	//    during teardown (by a concurrent agent, an earlier link close in this
	//    process, etc.) that could still call one of our progs is guaranteed to
	//    be present in postRefs, while preRefs preserves any image already in
	//    RCU-tasks teardown.
	if owner != nil {
		postRefs := owner.captureKallsymsSnapshot()
		owner.persistCleanSnapshot(mergeSnapshotRefs(preRefs, postRefs), logger)

		// Keep owner.lockFD open until process exit so the kernel releases flock
		// only once this process is truly gone.
		stateMu.Lock()
		unpinnedLockFDs = append(unpinnedLockFDs, owner.lockFD)
		stateMu.Unlock()
		logger.Info("bpf-safety: all tracing links detached cleanly; leaving grace directory pinned on host bpffs for image-liveness reaper",
			zap.String("graceDir", owner.dir))
	}
	utils.TeardownStep()
}

// runningInContainerFn detects whether the current process is executing inside
// a container mount namespace (Docker, Podman, containerd, CRI-O, Kubernetes).
// Swappable in unit tests.
var runningInContainerFn = runningInContainer

func runningInContainer() bool {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("KEPLOY_INDOCKER")), "true") {
		return true
	}
	if strings.TrimSpace(os.Getenv("KUBERNETES_SERVICE_HOST")) != "" {
		return true
	}
	for _, marker := range []string{"/.dockerenv", "/run/.containerenv"} {
		if _, err := os.Stat(marker); err == nil {
			return true
		}
	}
	if b, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		s := string(b)
		for _, tok := range []string{"docker", "kubepods", "containerd", "crio", "libpod"} {
			if strings.Contains(s, tok) {
				return true
			}
		}
	}
	return false
}

// ensureNativeBPFFS mounts bpffs at /sys/fs/bpf ONLY on bare native Linux hosts
// where /sys/fs/bpf is confirmed absent (ENOENT) or non-bpffs (statfs succeeded
// with a different filesystem magic). Returns any non-ENOENT statfs, MkdirAll,
// or Mount error so resolveHostBPFFSMount can propagate operational/permission
// failures (instead of falling through to a misleading non-bpffs config error
// on an unmounted host) while still never stacking a mount on top of an
// existing bpffs.
//
// Invariants:
//   - NEVER mounts a container-private bpffs when running inside a container
//     (pins inside a private mount namespace die with the container, giving
//     false protection).
//   - NEVER stacks a new bpffs mount on top of /sys/fs/bpf when statfs fails
//     with an operational or permission error (ENOMEM, EINTR, EIO, EACCES, etc.):
//     doing so hides every existing pin (other tools' and Keploy's own grace
//     dirs and anchors) behind an empty mount.
//   - NEVER remounts an operator's read-only bpffs mount read-write.
func ensureNativeBPFFS() error {
	if runningInContainerFn() {
		return nil
	}
	var stfs unix.Statfs_t
	err := nativeBPFFSStatfsFn(DefaultNativeBPFFSPath, &stfs)
	if err == nil {
		if stfs.Type == unix.BPF_FS_MAGIC {
			return nil
		}
	} else if !errors.Is(err, unix.ENOENT) {
		return fmt.Errorf("statfs %s: %w", DefaultNativeBPFFSPath, err)
	}
	if err := nativeBPFFSMkdirAllFn(DefaultNativeBPFFSPath, 0o755); err != nil {
		return fmt.Errorf("mkdir %s: %w", DefaultNativeBPFFSPath, err)
	}
	if err := nativeBPFFSMountFn("bpf", DefaultNativeBPFFSPath, "bpf", 0, ""); err != nil {
		return fmt.Errorf("mount bpffs at %s: %w", DefaultNativeBPFFSPath, err)
	}
	return nil
}

func unprotectedPermWarnMessage(path string) string {
	if path == "" {
		return UnprotectedPermWarnMessage + " (check permissions/AppArmor/SELinux; see docs/kernel-safety.md)"
	}
	return fmt.Sprintf("%s %s (check permissions/AppArmor/SELinux on %s; see docs/kernel-safety.md)", UnprotectedPermWarnMessage, path, path)
}

// resolveHostBPFFSMount locates and validates the host bpffs mount point for mode.
// On failure, the second return value names the path that failed (for permission
// diagnostics, including KEPLOY_HOST_BPFFS overrides).
func resolveHostBPFFSMount(mode Mode) (string, string, error) {
	inContainer := mode != ModeNative || runningInContainerFn()
	if override := strings.TrimSpace(os.Getenv(EnvHostBPFFS)); override != "" {
		if err := validateHostBPFFSPathFn(override, inContainer); err != nil {
			return "", override, fmt.Errorf("%s=%s: %w", EnvHostBPFFS, override, err)
		}
		return override, "", nil
	}

	if mode != ModeNative {
		if err := validateHostBPFFSPathFn(DefaultHostBPFFSPath, true); err != nil {
			return "", DefaultHostBPFFSPath, fmt.Errorf("host bpffs mount %s unavailable: %w", DefaultHostBPFFSPath, err)
		}
		return DefaultHostBPFFSPath, "", nil
	}

	// Native mode (including native CLI inside a CI container):
	// prefer /run/keploy/host-bpffs if mounted.
	hostErr := validateHostBPFFSPathFn(DefaultHostBPFFSPath, inContainer)
	if hostErr == nil {
		return DefaultHostBPFFSPath, "", nil
	}
	if inContainer {
		// Inside a container without /run/keploy/host-bpffs: NEVER self-mount a
		// private bpffs. Only accept /sys/fs/bpf if it is already an explicit
		// writable host-shared bpffs mount in mountinfo.
		if err := validateHostBPFFSPathFn(DefaultNativeBPFFSPath, true); err != nil {
			if isMountCheckSysErr(hostErr) && !isMountCheckSysErr(err) {
				return "", DefaultHostBPFFSPath, fmt.Errorf("host bpffs mount %s check failed: %w", DefaultHostBPFFSPath, hostErr)
			}
			if isMountCheckPermErr(hostErr) && !isMountCheckSysErr(err) && !isMountCheckPermErr(err) {
				return "", DefaultHostBPFFSPath, fmt.Errorf("host bpffs mount %s check failed: %w", DefaultHostBPFFSPath, hostErr)
			}
			return "", DefaultNativeBPFFSPath, fmt.Errorf("host bpffs mount unavailable in container: %w", err)
		}
		ok, merr := hasHostSharedBPFFSFn("/proc/self/mountinfo", DefaultNativeBPFFSPath)
		if merr != nil {
			return "", DefaultNativeBPFFSPath, fmt.Errorf("check host-shared bpffs mountinfo for %s: %w", DefaultNativeBPFFSPath, merr)
		}
		if !ok {
			if isMountCheckSysErr(hostErr) || isMountCheckPermErr(hostErr) {
				return "", DefaultHostBPFFSPath, fmt.Errorf("host bpffs mount %s check failed: %w", DefaultHostBPFFSPath, hostErr)
			}
			return "", DefaultNativeBPFFSPath, fmt.Errorf("%s inside container is not a host-shared bpffs mount", DefaultNativeBPFFSPath)
		}
		return DefaultNativeBPFFSPath, "", nil
	}
	ensureErr := ensureNativeBPFFS()
	if err := validateHostBPFFSPathFn(DefaultNativeBPFFSPath, false); err != nil {
		if isMountCheckSysErr(err) {
			return "", DefaultNativeBPFFSPath, fmt.Errorf("host bpffs mount %s unavailable: %w", DefaultNativeBPFFSPath, err)
		}
		if isMountCheckSysErr(ensureErr) {
			return "", DefaultNativeBPFFSPath, fmt.Errorf("ensure host bpffs mount %s failed: %w", DefaultNativeBPFFSPath, ensureErr)
		}
		if isMountCheckSysErr(hostErr) {
			return "", DefaultHostBPFFSPath, fmt.Errorf("host bpffs mount %s check failed: %w", DefaultHostBPFFSPath, hostErr)
		}
		if isMountCheckPermErr(err) {
			return "", DefaultNativeBPFFSPath, fmt.Errorf("host bpffs mount %s unavailable: %w", DefaultNativeBPFFSPath, err)
		}
		if isMountCheckPermErr(ensureErr) {
			return "", DefaultNativeBPFFSPath, fmt.Errorf("ensure host bpffs mount %s failed: %w", DefaultNativeBPFFSPath, ensureErr)
		}
		if isMountCheckPermErr(hostErr) {
			return "", DefaultHostBPFFSPath, fmt.Errorf("host bpffs mount %s check failed: %w", DefaultHostBPFFSPath, hostErr)
		}
		return "", DefaultNativeBPFFSPath, fmt.Errorf("host bpffs mount %s unavailable: %w", DefaultNativeBPFFSPath, err)
	}
	return DefaultNativeBPFFSPath, "", nil
}

// isMountCheckPermErr reports whether err came from a persistent permission
// denial (EACCES or EPERM) while checking the host bpffs mount.
func isMountCheckPermErr(err error) bool {
	return errors.Is(err, unix.EACCES) || errors.Is(err, unix.EPERM)
}

// isMountCheckSysErr reports whether err came from an operational syscall
// failure while checking the host bpffs mount (such as ENOMEM, EINTR, EAGAIN,
// or EIO) rather than a missing, unmounted, non-bpffs, read-only, or
// non-host-shared mount configuration.
func isMountCheckSysErr(err error) bool {
	if err == nil {
		return false
	}
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case unix.ENOENT, unix.ENOTDIR, unix.EACCES, unix.EPERM, unix.ELOOP, unix.ENAMETOOLONG:
		return false
	default:
		return true
	}
}

// hasHostSharedBPFFS reports whether target in /proc/self/mountinfo is a bpffs
// mount that carries mount propagation (shared: or master:), distinguishing a
// host bind-mounted /sys/fs/bpf from a container-private `mount -t bpf` instance.
func hasHostSharedBPFFS(mountinfoPath, target string) (bool, error) {
	f, err := os.Open(mountinfoPath)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		sep := strings.Index(line, " - ")
		if sep < 0 {
			continue
		}
		left := strings.Fields(line[:sep])
		right := strings.Fields(line[sep+3:])
		if len(left) < 6 || len(right) < 1 {
			continue
		}
		if left[4] != target || right[0] != "bpf" || left[3] != "/" {
			continue
		}
		for _, opt := range left[6:] {
			if strings.HasPrefix(opt, "shared:") || strings.HasPrefix(opt, "master:") {
				return true, nil
			}
		}
	}
	return false, sc.Err()
}

// ValidateHostBPFFSPath checks that path is a writable BPF_FS_MAGIC filesystem,
// is never a private overlay recorded by RecordPrivateOverlayDev, and when
// checkContainerHostMount is true, confirms it matches the supervisor-recorded
// host device (if any) and is an explicit bpffs mount point in /proc/self/mountinfo.
func ValidateHostBPFFSPath(path string, checkContainerHostMount bool) error {
	var stfs unix.Statfs_t
	if err := unix.Statfs(path, &stfs); err != nil {
		return fmt.Errorf("statfs %s: %w", path, err)
	}
	if stfs.Type != unix.BPF_FS_MAGIC {
		return fmt.Errorf("%s is not a bpffs filesystem (type=0x%x, want BPF_FS_MAGIC=0x%x)", path, uint64(stfs.Type), uint64(unix.BPF_FS_MAGIC))
	}
	if stfs.Flags&unix.ST_RDONLY != 0 {
		return fmt.Errorf("%s is mounted read-only", path)
	}

	var st unix.Stat_t
	if err := unix.Stat(path, &st); err != nil {
		return fmt.Errorf("stat %s: %w", path, err)
	}

	stateMu.Lock()
	isPrivate := privateOverlayDevs[st.Dev]
	stateMu.Unlock()
	if isPrivate {
		return fmt.Errorf("%s resolves to container-private bpffs overlay device %d", path, st.Dev)
	}

	if !checkContainerHostMount {
		return nil
	}

	if raw := strings.TrimSpace(os.Getenv(EnvHostBPFFSDev)); raw != "" {
		if wantDev, err := strconv.ParseUint(raw, 10, 64); err == nil && wantDev != 0 && st.Dev != wantDev {
			return fmt.Errorf("%s is on device %d, not the supervisor-recorded host bpffs device %d", path, st.Dev, wantDev)
		}
	}

	if clean := filepath.Clean(path); clean == DefaultHostBPFFSPath {
		if ok, err := hasMountInfoBPFFS("/proc/self/mountinfo", clean); err == nil && !ok {
			return fmt.Errorf("%s is not a distinct bpffs mount point in /proc/self/mountinfo", clean)
		}
	}
	return nil
}

func hasMountInfoBPFFS(mountinfoPath, target string) (bool, error) {
	f, err := os.Open(mountinfoPath)
	if err != nil {
		return false, err
	}
	defer func() { _ = f.Close() }()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := sc.Text()
		sep := strings.Index(line, " - ")
		if sep < 0 {
			continue
		}
		left := strings.Fields(line[:sep])
		right := strings.Fields(line[sep+3:])
		if len(left) < 5 || len(right) < 1 {
			continue
		}
		if left[4] == target && right[0] == "bpf" && left[3] == "/" {
			return true, nil
		}
	}
	return false, sc.Err()
}

type graceOwner struct {
	root               string
	rootIsBPFFS        bool
	dir                string
	self               string
	lockFD             int
	seq                atomic.Uint64
	keysMu             sync.Mutex
	attachedKeys       map[uint64]struct{}
	attachedModBTFIDs  map[uint32]struct{}
	recordedPinTargets map[string]string
	pinPaths           map[*ebpf.Program]string
	anchorMu           sync.Mutex
	anchoredFuncs      map[string]bool
	warnedUnpinned     map[string]bool
	unpinnedAnchors    []pendingAnchor
	onBeforeAnchorPin  func()
	logger             *zap.Logger
	reaper             *graceReaper
	reaperCancel       context.CancelFunc
}

func (g *graceOwner) unpinFailed(prog *ebpf.Program) {
	g.keysMu.Lock()
	p := g.pinPaths[prog]
	delete(g.pinPaths, prog)
	if p != "" && g.recordedPinTargets != nil {
		delete(g.recordedPinTargets, filepath.Base(p))
	}
	g.keysMu.Unlock()
	if p != "" {
		_ = os.Remove(p)
	}
}

func (g *graceOwner) recordPinTarget(pinName, target string) error {
	if pinName == "" || target == "" {
		return nil
	}
	g.keysMu.Lock()
	if g.recordedPinTargets == nil {
		g.recordedPinTargets = make(map[string]string)
	}
	g.recordedPinTargets[pinName] = target
	entries := make([]string, 0, len(g.recordedPinTargets))
	for pName, fn := range g.recordedPinTargets {
		entries = append(entries, pName+"="+fn)
	}
	g.keysMu.Unlock()

	if g.reaper != nil && g.reaper.store != nil {
		return g.reaper.store.writeTargets(g.dir, entries)
	}
	return nil
}

func (g *graceOwner) pinProgram(prog *ebpf.Program, object, name, role string) error {
	// Resolve target function + candidate trampoline keys BEFORE pinning so:
	//  1. Already-permanent-EBUSY targets fail immediately without leaking a pin.
	//  2. Both Linux 6.0+ (attach_btf_id) and Linux 5.10..5.19 / CAP_BPF-only
	//     module targets (BTF lookup of caller-provided AttachTo name) record
	//     the exact per-pin target function and pin an anchor link BEFORE the
	//     first real attach.
	//  3. Any Tracing/LSM program whose kernel target cannot be resolved is
	//     REFUSED BEFORE PINNING OR ATTACHING (fail closed per program so one
	//     resolved program in a grace dir can never hide an unresolved one).
	if object != "bpfsafety_reaper" && isStaleTarget(name) {
		return fmt.Errorf("stale BPF trampoline on %s (hook unavailable until node reboot; drain and reboot this node): %w", name, unix.EBUSY)
	}

	var (
		resolvedFunc     string
		resolvedNonTramp bool
		candKeys         []uint64
		modBTFID         uint32
	)
	info, err := queryProgInfo(prog.FD())
	if err != nil {
		return fmt.Errorf("%w: query BPF program info for %s (%s): %w", ErrUnresolvedTarget, name, object, err)
	}
	if progType := ebpf.ProgramType(info.Type); progType == ebpf.Tracing || progType == ebpf.LSM {
		objID, btfID, fnName, isTramp, pre60, rerr := resolveTrampolineTarget(info)
		if rerr != nil {
			return fmt.Errorf("%w: resolve tracing target for %s (%s): %v", ErrUnresolvedTarget, name, object, rerr)
		}
		if isTramp && fnName != "" {
			resolvedFunc = fnName
			candKeys = trampolineCandidateKeys(objID, btfID)
		} else if !pre60 {
			// Valid non-trampoline Tracing prog (e.g. tp_btf on 6.0+).
			resolvedNonTramp = true
		} else {
			// Linux 5.10..5.19 or CAP_BPF-only 6.0+ module target: resolve
			// caller-provided AttachTo symbol directly against /sys/kernel/btf/*.
			bObjID, bID, isMod, ok, berr := resolveFuncNameInBTF(name)
			if berr != nil {
				return fmt.Errorf("%w: lookup %q in kernel BTF: %v", ErrUnresolvedTarget, name, berr)
			}
			if !ok {
				if isRawBTFTracepointName(name) {
					resolvedNonTramp = true
				} else {
					return fmt.Errorf("%w: %q (object %s) is not a kernel BTF function or tracepoint; pass ProgramSpec.AttachTo", ErrUnresolvedTarget, name, object)
				}
			} else {
				resolvedFunc = name
				if isMod {
					modBTFID = bID
					if info.AttachBtfObjID > 1 && info.AttachBtfID != 0 {
						candKeys = trampolineCandidateKeys(info.AttachBtfObjID, info.AttachBtfID)
					}
				} else {
					candKeys = trampolineCandidateKeys(bObjID, bID)
				}
			}
		}
	}
	if object != "bpfsafety_reaper" && resolvedFunc != "" && isStaleTarget(resolvedFunc) {
		return fmt.Errorf("stale BPF trampoline on %s (hook unavailable until node reboot; drain and reboot this node): %w", resolvedFunc, unix.EBUSY)
	}

	if resolvedFunc != "" && object != "bpfsafety_reaper" {
		// Pin a no-op anchor link on this function BEFORE the first real
		// attach (skip for the reaper's own rebuild no-op so we don't recurse).
		g.ensureAnchor(resolvedFunc, g.logger)
		// On static-trampoline kernels (5.10.0–5.10.27 and 5.11.0–5.11.10,
		// lacking struct bpf_tramp_image), a clean exit-time close of an
		// unpinned anchor frees tr->image after synchronize_rcu_tasks(), which
		// does not wait for tasks sleeping inside an fexit-hooked function.
		// Fail closed unconditionally whenever the anchor is not pinned, for
		// any reason; an fentry-only anchor image has no sleepers on the stack,
		// so its exit close is safe.
		if !g.dynamicTrampImage() && !g.isAnchorPinned(resolvedFunc) {
			g.warnAnchorUnpinnedOnce(resolvedFunc)
			return fmt.Errorf("%w: refusing to attach tracing program %s (%s) on static-trampoline kernel until anchor for %s is pinned on host bpffs", ErrAnchorUnpinned, name, object, resolvedFunc)
		}
	}

	seq := g.seq.Add(1)
	pinName := fmt.Sprintf("%06d-%s-%s-%s", seq, sanitizeToken(object), sanitizeToken(name), role)
	pinPath := filepath.Join(g.dir, pinName)
	if err := prog.Pin(pinPath); err != nil {
		return fmt.Errorf("%w: pin BPF trampoline program %s (%s) into host bpffs grace dir %s: %w", ErrPinFailed, name, object, pinPath, err)
	}

	// Record per-pin target metadata BEFORE returning so the reaper can always
	// map this exact pin on 5.10..5.19 and CAP_BPF-only 6.0+ module paths.
	pinTargetVal := resolvedFunc
	if pinTargetVal == "" && resolvedNonTramp {
		pinTargetVal = nonTrampolineTargetSentinel
	}
	if pinTargetVal != "" {
		if rerr := g.recordPinTarget(pinName, pinTargetVal); rerr != nil {
			_ = os.Remove(pinPath)
			return fmt.Errorf("%w: record per-pin target %s for %s: %v", ErrPinFailed, pinTargetVal, pinName, rerr)
		}
	}

	g.keysMu.Lock()
	if g.pinPaths == nil {
		g.pinPaths = make(map[*ebpf.Program]string)
	}
	g.pinPaths[prog] = pinPath
	if len(candKeys) > 0 {
		if g.attachedKeys == nil {
			g.attachedKeys = make(map[uint64]struct{})
		}
		for _, k := range candKeys {
			g.attachedKeys[k] = struct{}{}
		}
	}
	if modBTFID != 0 {
		if g.attachedModBTFIDs == nil {
			g.attachedModBTFIDs = make(map[uint32]struct{})
		}
		g.attachedModBTFIDs[modBTFID] = struct{}{}
	}
	g.keysMu.Unlock()

	return nil
}

func (g *graceOwner) dynamicTrampImage() bool {
	if g != nil && g.reaper != nil && g.reaper.dynamicTrampImageFn != nil {
		return g.reaper.dynamicTrampImageFn()
	}
	return kernelHasDynamicTrampImage()
}

func (g *graceOwner) warnAnchorUnpinnedOnce(funcName string) {
	if g == nil || g.logger == nil || funcName == "" {
		return
	}
	g.anchorMu.Lock()
	if g.warnedUnpinned == nil {
		g.warnedUnpinned = make(map[string]bool)
	}
	if g.warnedUnpinned[funcName] {
		g.anchorMu.Unlock()
		return
	}
	g.warnedUnpinned[funcName] = true
	g.anchorMu.Unlock()
	g.logger.Warn("bpf-safety: refusing tracing attach on static-trampoline kernel because trampoline anchor is not pinned on host bpffs",
		zap.String("function", funcName))
}

func (g *graceOwner) captureKallsymsSnapshot() []trampImageRef {
	g.keysMu.Lock()
	keys := make(map[uint64]struct{}, len(g.attachedKeys))
	for k := range g.attachedKeys {
		keys[k] = struct{}{}
	}
	modIDs := make(map[uint32]struct{}, len(g.attachedModBTFIDs))
	for id := range g.attachedModBTFIDs {
		modIDs[id] = struct{}{}
	}
	g.keysMu.Unlock()
	if (len(keys) == 0 && len(modIDs) == 0) || g.reaper == nil {
		return nil
	}
	if g.reaper.dynamicTrampImageFn != nil && !g.reaper.dynamicTrampImageFn() {
		return nil
	}
	if !g.reaper.kallsymsSysctlFn() {
		return nil
	}
	tramps, visible, err := g.reaper.readKallsymsFn()
	if err != nil || !visible {
		return nil
	}
	return snapshotKeysFromKallsyms(tramps, keys, modIDs)
}

func (g *graceOwner) persistCleanSnapshot(refs []trampImageRef, logger *zap.Logger) {
	if g == nil || g.reaper == nil {
		return
	}
	if len(refs) == 0 || len(refs) > maxSnapshotEntries {
		// If we had attached keys but saw zero kallsyms entries (or exceeded
		// cap), do NOT persist an empty clean snapshot — let the reaper take
		// the safe dead-owner pass-2 snapshot + forced rebuild path.
		return
	}
	if err := g.reaper.store.writeSnapshot(g.dir, refs); err != nil {
		if logger != nil {
			logger.Warn("bpf-safety: failed to persist clean-exit kallsyms snapshot", zap.String("graceDir", g.dir), zap.Error(err))
		}
		return
	}
	nowNs := g.reaper.bootNowNs()
	if err := g.reaper.store.writeState(g.dir, reaperState{
		Flags:           stateCleanSnapshot,
		SnapshotCount:   uint64(len(refs)),
		FirstSeenBootNs: nowNs,
	}); err != nil && logger != nil {
		logger.Warn("bpf-safety: failed to persist clean-exit reaper state", zap.String("graceDir", g.dir), zap.Error(err))
	}
}

func sanitizeToken(s string) string {
	if s == "" {
		return "prog"
	}
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range s {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '_' || r == '-' {
			b.WriteRune(r)
		} else {
			b.WriteByte('_')
		}
	}
	return b.String()
}

// sanitizeTag restricts test tags strictly to [a-z0-9] (no '-') so the '-'
// delimiter in grace-test-<tag>-<nonce> can never cause prefix collisions.
func sanitizeTag(s string) string {
	var b strings.Builder
	b.Grow(len(s))
	for _, r := range strings.ToLower(s) {
		if (r >= 'a' && r <= 'z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
	}
	return b.String()
}

func openGraceOwner(logger *zap.Logger, mode Mode) (*graceOwner, string, error) {
	mountPath, failedPath, err := resolveHostBPFFSMount(mode)
	if err != nil && isMountCheckSysErr(err) {
		// Operational syscall failure during mount validation or ensure (e.g.
		// transient statfs/stat/mount ENOMEM): retry once before degrading.
		mountPath, failedPath, err = resolveHostBPFFSMount(mode)
	}
	if err != nil {
		if isMountCheckSysErr(err) {
			return nil, UnprotectedMountCheckErrWarnMessage, err
		}
		if isMountCheckPermErr(err) {
			return nil, unprotectedPermWarnMessage(failedPath), err
		}
		return nil, UnprotectedWarnMessage, err
	}
	root := filepath.Join(mountPath, GraceRootSubdir)
	owner, err := openGraceOwnerInRootFn(root, logger)
	if err != nil {
		// The host bpffs mount itself validated; retry once in case a
		// transient allocation fault interrupted grace directory setup.
		owner, err = openGraceOwnerInRootFn(root, logger)
	}
	if err != nil {
		return nil, UnprotectedInitErrWarnMessage, err
	}
	return owner, "", nil
}

func openGraceOwnerInRoot(root string, logger *zap.Logger) (*graceOwner, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create grace root %s: %w", root, err)
	}
	reapPrefix := GraceDirPrefix
	dirPrefix := GraceDirPrefix
	tagRaw := strings.TrimSpace(os.Getenv(EnvGraceTestTag))
	if tagRaw != "" || shortGraceTestMode.Load() || shortGraceBuildEnabled {
		tag := sanitizeTag(tagRaw)
		if tag == "" {
			tag = randomNonce()[:8]
		}
		dirPrefix = GraceTestDirPrefix + tag + "-"
		reapPrefix = dirPrefix
	}

	nonce := randomNonce()
	self := dirPrefix + nonce
	finalDir := filepath.Join(root, self)

	if err := os.Mkdir(finalDir, 0o755); err != nil {
		return nil, fmt.Errorf("create grace directory %s: %w", finalDir, err)
	}
	fd, err := unix.Open(finalDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		_ = os.Remove(finalDir)
		return nil, fmt.Errorf("open grace directory %s for flock: %w", finalDir, err)
	}
	if err := unix.Flock(fd, unix.LOCK_EX); err != nil {
		_ = unix.Close(fd)
		_ = os.Remove(finalDir)
		return nil, fmt.Errorf("flock grace directory %s: %w", finalDir, err)
	}
	// Clear any transient metadata written between Mkdir and Flock(LOCK_EX).
	for _, meta := range []string{reaperStateFileName, kallsymsSnapshotFileName, targetFuncsFileName, legacyStampFileName} {
		if err := os.Remove(filepath.Join(finalDir, meta)); err != nil && !errors.Is(err, os.ErrNotExist) {
			_ = unix.Close(fd)
			_ = os.RemoveAll(finalDir)
			return nil, fmt.Errorf("clear transient grace metadata %s in %s: %w", meta, finalDir, err)
		}
	}

	var stfs unix.Statfs_t
	if err := unix.Statfs(root, &stfs); err != nil {
		_ = unix.Close(fd)
		_ = os.RemoveAll(finalDir)
		return nil, fmt.Errorf("statfs grace root %s: %w", root, err)
	}

	owner := &graceOwner{
		root:        root,
		rootIsBPFFS: stfs.Type == unix.BPF_FS_MAGIC,
		dir:         finalDir,
		self:        self,
		lockFD:      fd,
		logger:      logger,
	}
	reaper := newGraceReaper(root, self, DefaultReapInterval, logger)
	reaper.reapPrefix = reapPrefix
	reaper.pinNoop = func(prog *ebpf.Program, funcName string) error {
		return owner.pinProgram(prog, "bpfsafety_reaper", funcName, "rb")
	}
	reaper.retryAnchorsFn = owner.retryUnpinnedAnchors
	owner.reaper = reaper
	return owner, nil
}

func randomNonce() string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	return fmt.Sprintf("p%dt%d", os.Getpid(), time.Now().UnixNano())
}

// clockBoottimeNs returns CLOCK_BOOTTIME in nanoseconds.
func clockBoottimeNs() uint64 {
	var ts unix.Timespec
	if err := unix.ClockGettime(unix.CLOCK_BOOTTIME, &ts); err != nil {
		return uint64(time.Now().UnixNano())
	}
	return uint64(ts.Sec)*1e9 + uint64(ts.Nsec)
}

// graceReaper scans <host-bpffs>/keploy-grace/ at startup and periodically,
// unpinning dead grace directories ONLY once every trampoline image that could
// call their programs has been freed in /proc/kallsyms (or confirmed idle via
// forced rebuild + BPF_ENABLE_STATS fallback). Never uses a blind timer.
type graceReaper struct {
	root                string
	self                string
	reapPrefix          string
	reapInterval        time.Duration
	statsProbeWait      time.Duration
	logger              *zap.Logger
	bootNowNs           func() uint64
	store               stateStore
	pinNoop             func(prog *ebpf.Program, funcName string) error
	inspectTargetsFn    func(dirPath string) (dirTargets, error)
	rebuildTargetFn     func(funcName string) error
	runCntFn            func(dirPath string) (uint64, error)
	enableStatsFn       func() (io.Closer, error)
	dynamicTrampImageFn func() bool
	kallsymsSysctlFn    func() bool
	readKallsymsFn      func() (map[uint64][]uint64, bool, error)
	onBeforeNoopAttach  func()
	onBeforeNoopClose   func()
	wrapNoopClose       func(closeFn func())
	retryAnchorsFn      func()
	noopMu              sync.Mutex
	noopCache           map[string]*ebpf.Program
	warnedMu            sync.Mutex
	warnedUnresolved    map[string]bool
	doneMu              sync.Mutex
	doneCh              chan struct{}
}

func newGraceReaper(root, self string, reapInterval time.Duration, logger *zap.Logger) *graceReaper {
	if logger == nil {
		logger = zap.NewNop()
	}
	if reapInterval <= 0 {
		reapInterval = DefaultReapInterval
	}
	r := &graceReaper{
		root:                root,
		self:                self,
		reapPrefix:          GraceDirPrefix,
		reapInterval:        reapInterval,
		statsProbeWait:      defaultStatsProbeWait,
		logger:              logger,
		bootNowNs:           clockBoottimeNs,
		store:               autoStateStore{},
		enableStatsFn:       defaultEnableStats,
		dynamicTrampImageFn: kernelHasDynamicTrampImage,
		kallsymsSysctlFn:    kallsymsSysctlAllowsJIT,
		readKallsymsFn:      func() (map[uint64][]uint64, bool, error) { return parseKallsymsTrampolines(kallsymsPath) },
		warnedUnresolved:    make(map[string]bool),
	}
	r.inspectTargetsFn = r.inspectDirTrampolineTargets
	r.rebuildTargetFn = r.forceRebuildTarget
	r.runCntFn = r.sumDirRunCnt
	return r
}

func (r *graceReaper) start(ctx context.Context) {
	done := make(chan struct{})
	r.doneMu.Lock()
	r.doneCh = done
	r.doneMu.Unlock()
	go func() {
		defer close(done)
		r.round()
		tick := time.NewTicker(r.reapInterval)
		defer tick.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-tick.C:
				r.round()
			}
		}
	}()
}

// stopAndWait cancels the reaper context and waits for any in-flight round()
// to finish so the reaper can never perform a trampoline rebuild while
// GracefulShutdown is detaching links.
func (r *graceReaper) stopAndWait(cancel context.CancelFunc) {
	if cancel != nil {
		cancel()
	}
	r.doneMu.Lock()
	done := r.doneCh
	r.doneMu.Unlock()
	if done != nil {
		<-done
	}
}

func (r *graceReaper) warnOnceUnresolved(dirPath string, err error) {
	r.warnedMu.Lock()
	if r.warnedUnresolved == nil {
		r.warnedUnresolved = make(map[string]bool)
	}
	already := r.warnedUnresolved[dirPath]
	r.warnedUnresolved[dirPath] = true
	r.warnedMu.Unlock()
	if !already {
		r.logger.Warn("bpf-safety: failed to inspect dead grace dir targets; keeping pins (logged once per directory)",
			zap.String("graceDir", dirPath), zap.Error(err))
	} else {
		r.logger.Debug("bpf-safety: dead grace dir targets remain unresolved; keeping pins",
			zap.String("graceDir", dirPath), zap.Error(err))
	}
}

// isManagedGraceDirName reports whether a directory entry under keploy-grace/
// is a production or test grace directory (either v3.7.28+ grace2-* or legacy
// v3.7.27 grace-*).
func isManagedGraceDirName(name string) bool {
	return strings.HasPrefix(name, GraceDirPrefix) || strings.HasPrefix(name, LegacyGraceDirPrefix)
}

func matchesReapScope(name, reapPrefix string) bool {
	if reapPrefix == GraceDirPrefix {
		if strings.HasPrefix(name, GraceTestDirPrefix) || strings.HasPrefix(name, "grace-test-") {
			return false
		}
		return strings.HasPrefix(name, GraceDirPrefix) || strings.HasPrefix(name, LegacyGraceDirPrefix)
	}
	return strings.HasPrefix(name, reapPrefix)
}

// round executes one image-liveness reaper scan and notifies the metrics observer.
func (r *graceReaper) round() (dirsPresent, pinnedProgs int) {
	if r.retryAnchorsFn != nil {
		r.retryAnchorsFn()
	}
	entries, err := os.ReadDir(r.root)
	if err != nil {
		return 0, 0
	}
	nowNs := r.bootNowNs()
	prefix := r.reapPrefix
	if prefix == "" {
		prefix = GraceDirPrefix
	}

	// Read /proc/kallsyms at most once per round when needed.
	kallsymsChecked := false
	kallsymsOK := false
	var currentTramps map[uint64][]uint64
	loadKallsyms := func() (map[uint64][]uint64, bool) {
		if kallsymsChecked {
			return currentTramps, kallsymsOK
		}
		kallsymsChecked = true
		if r.dynamicTrampImageFn != nil && !r.dynamicTrampImageFn() {
			kallsymsOK = false
			return nil, false
		}
		if !r.kallsymsSysctlFn() {
			kallsymsOK = false
			return nil, false
		}
		tramps, visible, kerr := r.readKallsymsFn()
		if kerr != nil || !visible {
			kallsymsOK = false
			return nil, false
		}
		currentTramps = tramps
		kallsymsOK = true
		return currentTramps, true
	}

	// Deduplicate forced trampoline rebuilds per kernel function within a single
	// round so N dead grace directories targeting the same function only perform
	// one trampoline update per round.
	rebuiltThisRound := make(map[string]error)
	rebuildOnce := func(fn string) error {
		if err, done := rebuiltThisRound[fn]; done {
			return err
		}
		err := r.rebuildTargetFn(fn)
		rebuiltThisRound[fn] = err
		return err
	}

	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !isManagedGraceDirName(name) {
			continue
		}
		dirPath := filepath.Join(r.root, name)
		if name == r.self || !matchesReapScope(name, prefix) {
			dirsPresent++
			pinnedProgs += countPinnedProgs(dirPath)
			continue
		}

		fd, oerr := unix.Open(dirPath, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
		if oerr != nil {
			continue
		}
		if ferr := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); ferr != nil {
			_ = unix.Close(fd)
			dirsPresent++
			pinnedProgs += countPinnedProgs(dirPath)
			continue
		}

		reaped := r.processDeadDir(dirPath, nowNs, loadKallsyms, rebuildOnce)
		_ = unix.Close(fd)
		if !reaped {
			dirsPresent++
			pinnedProgs += countPinnedProgs(dirPath)
		}
	}

	notifyMetrics(dirsPresent, pinnedProgs, CountAnchorLinks(r.root))
	return dirsPresent, pinnedProgs
}

func (r *graceReaper) processDeadDir(dirPath string, nowNs uint64, loadKallsyms func() (map[uint64][]uint64, bool), rebuildOnce func(string) error) (reaped bool) {
	st, ok, serr := r.store.readState(dirPath)
	if serr != nil {
		r.logger.Warn("bpf-safety: failed to read grace dir reaper state; keeping pins", zap.String("graceDir", dirPath), zap.Error(serr))
		return false
	}

	// Permanent last-link stale trampoline: NEVER unpin until reboot.
	if ok && st.Flags&stateStalePermanent != 0 {
		staleTrampolineDetected.Store(true)
		return false
	}

	progs := countPinnedProgs(dirPath)
	if progs == 0 {
		return r.reapDir(dirPath, 0, "empty grace directory")
	}

	// Case 1: Dead owner with no snapshot yet (SIGKILL / OOM / legacy dir).
	if !ok || st.Flags&(stateCleanSnapshot|stateDeadSnapshot) == 0 {
		// Pass 1: record dead observation and wait one pass so any in-flight
		// do_exit -> close_files LIFO task_work link closes finish before
		// snapshotting and rebuilding.
		if !ok || st.Flags&stateDeadPass1 == 0 {
			if werr := r.store.writeState(dirPath, reaperState{
				Flags:           stateDeadPass1,
				FirstSeenBootNs: nowNs,
			}); werr != nil {
				r.logger.Warn("bpf-safety: failed to write pass-1 dead state", zap.String("graceDir", dirPath), zap.Error(werr))
			}
			return false
		}

		// Pass 2+: dead owner is confirmed gone. Inspect pinned program targets
		// (fails closed if any Tracing/LSM program has no resolvable target).
		targets, terr := r.inspectTargetsFn(dirPath)
		if terr != nil {
			r.warnOnceUnresolved(dirPath, terr)
			return false
		}
		fnSet := targets.allFuncs()
		if len(fnSet) == 0 {
			return r.reapDir(dirPath, progs, "no active trampoline targets")
		}

		tramps, kOK := loadKallsyms()
		if kOK && targets.hasKallsymsMatchers() {
			wantKeys := make(map[uint64]struct{}, len(targets.keyToFunc))
			for k := range targets.keyToFunc {
				wantKeys[k] = struct{}{}
			}
			snap := snapshotKeysFromKallsyms(tramps, wantKeys, targets.modBTFIDSet())
			if len(snap) == 0 {
				// Dead owner is gone and zero trampoline images for any of its
				// hooked functions exist in /proc/kallsyms.
				return r.reapDir(dirPath, progs, "all trampoline images already freed in kallsyms")
			}
			if len(snap) > maxSnapshotEntries {
				r.logger.Warn("bpf-safety: kallsyms snapshot exceeds cap; failing closed and keeping pins",
					zap.String("graceDir", dirPath), zap.Int("entries", len(snap)))
				return false
			}
			if werr := r.store.writeSnapshot(dirPath, snap); werr != nil {
				r.logger.Warn("bpf-safety: failed to persist pass-2 kallsyms snapshot", zap.String("graceDir", dirPath), zap.Error(werr))
				return false
			}
			// Force a trampoline rebuild for every target on pass 2 so any
			// stale image left by a SIGKILL/-ENOMEM unlink is replaced immediately
			// and its old address disappears from /proc/kallsyms before pass 3.
			for fn := range fnSet {
				if rerr := rebuildOnce(fn); rerr != nil {
					if errors.Is(rerr, unix.EBUSY) {
						r.markPermanentStale(dirPath, fn, st, rerr)
						return false
					}
					r.logger.Warn("bpf-safety: pass-2 forced trampoline rebuild failed; keeping pins and retrying",
						zap.String("graceDir", dirPath), zap.String("function", fn), zap.Error(rerr))
					return false
				}
			}
			st.Flags |= stateDeadSnapshot | stateForcedRebuild
			st.SnapshotCount = uint64(len(snap))
			st.LastProbeBootNs = nowNs
			if werr := r.store.writeState(dirPath, st); werr != nil {
				r.logger.Warn("bpf-safety: failed to persist pass-2 reaper state", zap.String("graceDir", dirPath), zap.Error(werr))
			}
			return false
		}

		// Kallsyms unavailable or key mapping unavailable: fall back to forced
		// rebuild on every resolved target + BPF_ENABLE_STATS (~2s) flat run_cnt.
		return r.handleStatsFallback(dirPath, progs, targets, st, nowNs)
	}

	// Case 2: Snapshot is persisted (either clean-exit or dead-owner pass-2).
	snap, rerr := r.store.readSnapshot(dirPath, st.SnapshotCount)
	if rerr != nil {
		r.logger.Warn("bpf-safety: failed to read persisted kallsyms snapshot; keeping pins", zap.String("graceDir", dirPath), zap.Error(rerr))
		return false
	}
	tramps, kOK := loadKallsyms()
	if !kOK {
		targets, terr := r.inspectTargetsFn(dirPath)
		if terr != nil {
			r.logger.Warn("bpf-safety: kallsyms unavailable and target inspection failed; keeping pins", zap.String("graceDir", dirPath), zap.Error(terr))
			return false
		}
		return r.handleStatsFallback(dirPath, progs, targets, st, nowNs)
	}

	surviving := survivingSnapshotKeys(snap, tramps)
	if len(surviving) == 0 {
		return r.reapDir(dirPath, progs, "all snapshotted trampoline images freed in kallsyms")
	}

	// One or more snapshotted trampoline images are still present in /proc/kallsyms.
	// On a fresh clean exit, give normal RCU-tasks teardown 2s before forcing a
	// trampoline rebuild.
	const cleanSettleNs = uint64(2 * time.Second)
	if st.Flags&stateCleanSnapshot != 0 && st.Flags&stateForcedRebuild == 0 && nowNs >= st.FirstSeenBootNs && nowNs-st.FirstSeenBootNs < cleanSettleNs {
		return false
	}

	// Inspect targets (per-program fail-closed!) before rebuilding.
	targets, terr := r.inspectTargetsFn(dirPath)
	if terr != nil {
		r.warnOnceUnresolved(dirPath, terr)
		return false
	}

	// On Linux 6.12+, bpf_prog_pack allocates trampoline images from first-fit
	// 64-byte slots, so a freshly rebuilt image can reuse the exact address of
	// an old snapshotted image. Once we have ALREADY forced a rebuild on an
	// earlier pass:
	//   - wait calmly during the 2s RCU-tasks settle window (do not thrash
	//     rebuilds or reset LastProbeBootNs), and
	//   - if a snapshotted address is STILL listed after >= 2s, break potential
	//     slot-reuse livelock cleanly via the fail-closed forced-rebuild +
	//     BPF_ENABLE_STATS flat run_cnt check.
	if st.Flags&stateForcedRebuild != 0 {
		if nowNs >= st.LastProbeBootNs && nowNs-st.LastProbeBootNs < cleanSettleNs {
			return false
		}
		return r.handleStatsFallback(dirPath, progs, targets, st, nowNs)
	}

	// Force a trampoline rebuild for every key whose image is still alive
	// (replaces any stale image left behind by a prior -ENOMEM unlink failure).
	stuckFuncs := make(map[string]struct{})
	for _, ref := range surviving {
		if fn, ok := targets.funcForKey(ref.Key); ok && fn != "" {
			stuckFuncs[fn] = struct{}{}
		}
	}
	if len(stuckFuncs) == 0 {
		// Surviving image key could not be mapped back to a function name: fail closed.
		r.logger.Warn("bpf-safety: snapshotted trampoline image still alive in kallsyms with unresolved target; keeping pins",
			zap.String("graceDir", dirPath), zap.Int("survivingImages", len(surviving)))
		return false
	}
	for fn := range stuckFuncs {
		if err := rebuildOnce(fn); err != nil {
			if errors.Is(err, unix.EBUSY) {
				r.markPermanentStale(dirPath, fn, st, err)
				return false
			}
			r.logger.Warn("bpf-safety: forced trampoline rebuild failed for stuck image; keeping pins and retrying",
				zap.String("graceDir", dirPath), zap.String("function", fn), zap.Error(err))
			return false
		}
	}
	st.Flags |= stateForcedRebuild
	st.LastProbeBootNs = nowNs
	_ = r.store.writeState(dirPath, st)
	r.logger.Info("bpf-safety: forced trampoline rebuild for stuck image(s); waiting for old address(es) to clear from kallsyms",
		zap.String("graceDir", dirPath), zap.Int("rebuiltFunctions", len(stuckFuncs)))
	return false
}

func (r *graceReaper) handleStatsFallback(dirPath string, progs int, targets dirTargets, st reaperState, nowNs uint64) bool {
	safe, permanentEBUSY, ebusyFunc, err := r.checkReapViaStatsFallback(dirPath, targets)
	if permanentEBUSY {
		r.markPermanentStale(dirPath, ebusyFunc, st, err)
		return false
	}
	if err != nil {
		r.logger.Warn("bpf-safety: stats fallback failed; failing closed and keeping pins",
			zap.String("graceDir", dirPath), zap.Error(err))
		return false
	}
	if !safe {
		st.LastProbeBootNs = nowNs
		_ = r.store.writeState(dirPath, st)
		return false
	}
	return r.reapDir(dirPath, progs, "forced rebuild + BPF_ENABLE_STATS flat run_cnt")
}

func (r *graceReaper) markPermanentStale(dirPath, funcName string, st reaperState, err error) {
	st.Flags |= stateStalePermanent
	_ = r.store.writeState(dirPath, st)
	ReportStaleTrampoline(r.logger, funcName, err)
	if r.logger != nil {
		r.logger.Warn("bpf-safety: keeping grace directory pinned until node reboot due to permanent stale trampoline (EBUSY)",
			zap.String("graceDir", dirPath), zap.String("function", funcName))
	}
}

func (r *graceReaper) reapDir(dirPath string, progs int, reason string) bool {
	if rmErr := os.RemoveAll(dirPath); rmErr != nil {
		r.logger.Warn("bpf-safety: failed to reap grace directory",
			zap.String("graceDir", dirPath), zap.Error(rmErr))
		return false
	}
	r.logger.Info("bpf-safety: reaped trampoline grace directory",
		zap.String("graceDir", dirPath),
		zap.Int("unpinnedProgs", progs),
		zap.String("reason", reason))
	return true
}

func countPinnedProgs(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if isMetadataFile(e.Name()) {
			continue
		}
		n++
	}
	return n
}
