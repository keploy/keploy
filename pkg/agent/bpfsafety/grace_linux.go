//go:build linux

// Package bpfsafety implements host-bpffs grace pinning for eBPF fentry/fexit
// trampoline programs (CVE-2026-98800 defense on Linux 5.10.28..7.2.9).
//
// On unpatched kernels prior to Linux 7.2.10 / 7.3 (commit 0fb5751a6e41),
// detaching an fentry/fexit program while another program remains attached to
// the same kernel function frees the detached struct bpf_prog after a single
// normal RCU grace period rather than an RCU-tasks grace period. A task
// preempted inside the old trampoline image before __bpf_prog_enter_recur or
// prog->bpf_func then faults on freed vmalloc/JIT memory and panics the node.
//
// Defense: pin every trampoline program into a per-instance directory on the
// HOST bpffs superblock (<host-bpffs>/keploy-grace/grace-<nonce>/) BEFORE
// calling link.AttachTracing, hold flock(LOCK_EX) on that directory for the
// lifetime of the process, and fail closed at PinBeforeAttach if Init was never
// called. Placing the root at <host-bpffs>/keploy-grace/ keeps grace
// directories completely outside <host-bpffs>/keploy/ so enterprise
// selfupdate.WipeBPFPins, inst-* sweeps, and legacy map-pin cleanups never
// touch live or recently-exited grace dirs.
//
// On every exit (clean SIGTERM, self-update swap, rollout, or SIGKILL/OOM),
// the exiting agent NEVER blocks exit: it closes the attach gate, runs all
// registered detach hooks synchronously, and exits immediately while leaving
// its grace directory pinned on host bpffs for the cross-process reaper (the
// kernel releases flock on process exit). A shared 16-byte CLOCK_BOOTTIME
// stamp pinned inside each dead grace directory lets any subsequent agent
// process reap the dead directory after DefaultGracePeriod (15m, strictly
// exceeding Linux's 10m RCU-tasks stall bound) confirmed on two passes >= 1m
// apart.
//
// Where no host bpffs is reachable (e.g. a customer's own manifest or an older
// chart lacking /run/keploy/host-bpffs), Init does not abort startup: it keeps
// recording in honest unprotected mode, logs one clear WARN naming the missing
// mount, and surfaces unprotected status through metrics and telemetry.
package bpfsafety

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
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
	// cannot arm host-bpffs grace pinning and degrades to honest unprotected mode.
	UnprotectedWarnMessage = "eBPF crash protection is OFF: mount the host's /sys/fs/bpf at /run/keploy/host-bpffs (see docs/kernel-safety.md)"

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

	// EnvGraceDuration optionally configures the reaper grace period.
	// Production builds clamp values to [MinProdGracePeriod, MaxProdGracePeriod].
	EnvGraceDuration = "KEPLOY_BPF_GRACE_DURATION"

	// EnvGraceTestTag scopes short-grace test builds (-tags bpfsafety_short_grace)
	// to their own directory prefix so a test run on a shared host can never
	// reap another lane's or production agent's grace-* directories.
	EnvGraceTestTag = "KEPLOY_BPF_GRACE_TEST_TAG"

	// GraceDirPrefix names production grace directories under <host-bpffs>/keploy-grace/.
	GraceDirPrefix = "grace-"

	// GraceTestDirPrefix names isolated short-grace test directories.
	GraceTestDirPrefix = "grace-test-"

	stampFileName = "first_free_boottime"

	// DefaultGracePeriod (15m) strictly exceeds Linux's 10-minute
	// CONFIG_RCU_TASKS_STALL_TIMEOUT. Under kubelet's 5-minute CrashLoopBackOff
	// cap, steady state holds ~5-7 dirs (~9 peak during initial backoff ramp).
	DefaultGracePeriod  = 15 * time.Minute
	DefaultReapInterval = 3 * time.Minute
	DefaultConfirmGap   = 1 * time.Minute

	// Production bounds enforced unless shortGraceTestMode or -tags bpfsafety_short_grace.
	MinProdGracePeriod = 15 * time.Minute
	MaxProdGracePeriod = 24 * time.Hour
	MinProdConfirmGap  = 1 * time.Minute
	MaxProdConfirmGap  = 30 * time.Minute
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

// MetricsObserver receives updated host-bpffs grace counts and unprotected status.
type MetricsObserver func(dirsPresent, pinnedProgs int, unprotected bool)

var (
	initMu                 sync.Mutex
	stateMu                sync.Mutex
	activeOwner            *graceOwner
	detachHooks            []func()
	unpinnedLockFDs        []int
	privateOverlayDevs     = map[uint64]bool{}
	metricsObserver        MetricsObserver
	unprotectedTrampolines atomic.Bool
	unprotectedWarned      atomic.Bool
	shuttingDown           atomic.Bool
	shortGraceTestMode     atomic.Bool
	testBypassPin          atomic.Bool
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

func notifyMetrics(dirs, progs int) {
	stateMu.Lock()
	fn := metricsObserver
	stateMu.Unlock()
	if fn != nil {
		fn(dirs, progs, unprotectedTrampolines.Load())
	}
}

// UnprotectedTrampolinesActive reports whether this process is running without
// host-bpffs trampoline grace pinning (either because no host bpffs was
// reachable or because KEPLOY_ALLOW_UNPROTECTED_TRAMPOLINES=1 was set).
func UnprotectedTrampolinesActive() bool {
	return unprotectedTrampolines.Load()
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

// AllowShortGraceForTest lifts the 15m production floor on KEPLOY_BPF_GRACE_DURATION
// for Go tests.
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

// ResetUnprotectedForTest resets the unprotected-mode state and warn-once flag
// for the duration of a test and restores them on cleanup.
func ResetUnprotectedForTest(tb TestCleaner) {
	tb.Helper()
	prevUnprot := unprotectedTrampolines.Swap(false)
	prevWarned := unprotectedWarned.Swap(false)
	tb.Cleanup(func() {
		unprotectedTrampolines.Store(prevUnprot)
		unprotectedWarned.Store(prevWarned)
	})
}

// HasActiveWork reports whether this process has armed host-bpffs grace pins or
// registered tracing detach hooks that GracefulShutdown must tear down on exit.
func HasActiveWork() bool {
	stateMu.Lock()
	ok := activeOwner != nil || len(detachHooks) > 0
	stateMu.Unlock()
	return ok
}

// Init verifies a writable host bpffs mount, creates and locks this process's
// <host-bpffs>/keploy-grace/grace-<nonce> directory (blocking flock(LOCK_EX)
// and clearing any transient first_free_boottime stamp), and starts the
// background reaper.
//
// On native Linux (ModeNative), if /sys/fs/bpf is not yet mounted as bpffs,
// Init attempts to mount it so native hosts always use real host bpffs.
//
// Where no usable host bpffs mount is reachable (for example a customer's own
// manifest or an older chart lacking /run/keploy/host-bpffs), Init never aborts
// recording: it enters honest unprotected mode, logs UnprotectedWarnMessage
// once, sets UnprotectedTrampolinesActive()=true, updates metrics, and returns
// nil.
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
		enterUnprotectedMode(logger, mode, errors.New("KEPLOY_ALLOW_UNPROTECTED_TRAMPOLINES=1 set"))
		return nil
	}

	owner, err := openGraceOwner(logger, mode)
	if err != nil {
		enterUnprotectedMode(logger, mode, err)
		return nil
	}

	reaperCtx, reaperCancel := context.WithCancel(ctx)
	owner.reaperCancel = reaperCancel

	unprotectedTrampolines.Store(false)
	stateMu.Lock()
	activeOwner = owner
	stateMu.Unlock()

	fields := []zap.Field{
		zap.String("mode", string(mode)),
		zap.String("graceDir", owner.dir),
		zap.Duration("reapAfter", owner.reaper.gracePeriod),
		zap.Duration("reapInterval", owner.reaper.reapInterval),
	}
	if owner.reaper.gracePeriod < MinProdGracePeriod {
		fields = append(fields, zap.String("shortGraceReapPrefix", owner.reaper.reapPrefix))
	}
	logger.Info("bpf-safety: trampoline program grace pinning armed on host bpffs", fields...)

	owner.reaper.start(reaperCtx)
	return nil
}

func enterUnprotectedMode(logger *zap.Logger, mode Mode, cause error) {
	unprotectedTrampolines.Store(true)
	notifyMetrics(0, 0)
	if unprotectedWarned.CompareAndSwap(false, true) && logger != nil {
		fields := []zap.Field{
			zap.String("mode", string(mode)),
			zap.String("graceMode", GraceModeUnprotected),
		}
		if cause != nil {
			fields = append(fields, zap.Error(cause))
		}
		logger.Warn(UnprotectedWarnMessage, fields...)
	}
}

// RegisterDetachHook registers a synchronous link-teardown callback that
// GracefulShutdown invokes after closing the attach gate. Every tracing-program
// holder (unixconn, standalone netio, classic scope lifecycle, DaemonSet kTLS
// and lifecycle hooks, low-latency lifecycle hooks) registers here so all links
// are guaranteed closed synchronously on shutdown.
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
// link.AttachTracing is called.
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
		if unprotectedTrampolines.Load() || testBypassPin.Load() {
			return nil
		}
		return ErrGraceNotArmed
	}
	return owner.pinProgram(prog, object, name, role)
}

// AttachTracing pins prog into the armed host-bpffs grace directory and then
// attaches it via link.AttachTracing.
func AttachTracing(prog *ebpf.Program, object, name, role string) (link.Link, error) {
	if err := PinBeforeAttach(prog, object, name, role); err != nil {
		return nil, err
	}
	return link.AttachTracing(link.TracingOptions{Program: prog})
}

// attachPinnedReaperNoop attaches a cached reaper no-op program that was
// already pinned into the reaper's own active grace directory by
// graceReaper.forceRebuildTarget.
func attachPinnedReaperNoop(prog *ebpf.Program) (link.Link, error) {
	return link.AttachTracing(link.TracingOptions{Program: prog})
}

// GracefulShutdown closes the attach gate, runs all registered link-detach
// hooks synchronously, cancels the background reaper, and returns IMMEDIATELY
// without blocking process exit.
//
// With host bpffs armed, it leaves the grace directory pinned on host bpffs
// and keeps owner.lockFD open until process exit (kernel releases flock on
// exit) so the >= 15m cross-process reaper safely reaps it. No self-unpin and
// zero exit stall.
func GracefulShutdown(_ context.Context, logger *zap.Logger) {
	if logger == nil {
		logger = zap.NewNop()
	}

	// Close the attach gate and snapshot state atomically under stateMu so no
	// concurrent RegisterDetachHook or PinBeforeAttach can race past shutdown.
	stateMu.Lock()
	shuttingDown.Store(true)
	hooks := detachHooks
	detachHooks = nil
	owner := activeOwner
	activeOwner = nil
	stateMu.Unlock()

	utils.TeardownStep()

	// Detach every registered tracing-link holder synchronously.
	for _, fn := range hooks {
		fn()
		utils.TeardownStep()
	}

	if owner != nil {
		if owner.reaperCancel != nil {
			owner.reaperCancel()
		}
		if owner.reaper != nil {
			owner.reaper.closeNoopCache()
		}
		// Leave the directory pinned on host bpffs and keep owner.lockFD open
		// until process exit so the kernel releases flock only once this
		// process is truly gone; the >= 15m cross-process reaper then safely
		// stamps and reaps it.
		stateMu.Lock()
		unpinnedLockFDs = append(unpinnedLockFDs, owner.lockFD)
		stateMu.Unlock()
		logger.Info("bpf-safety: all tracing links detached cleanly; leaving grace directory pinned on host bpffs for cross-process reaper",
			zap.String("graceDir", owner.dir),
			zap.Duration("reapAfter", owner.reaper.gracePeriod))
	}
	utils.TeardownStep()
}

// ensureNativeBPFFS mounts bpffs at /sys/fs/bpf on native Linux hosts when it
// is not yet mounted (or remounts read-write if mounted read-only). Best-effort:
// any error is surfaced by the subsequent ValidateHostBPFFSPath check.
func ensureNativeBPFFS() {
	var stfs unix.Statfs_t
	if err := unix.Statfs(DefaultNativeBPFFSPath, &stfs); err == nil && stfs.Type == unix.BPF_FS_MAGIC {
		if stfs.Flags&unix.ST_RDONLY != 0 {
			_ = unix.Mount("bpf", DefaultNativeBPFFSPath, "bpf", uintptr(unix.MS_REMOUNT), "")
		}
		return
	}
	_ = os.MkdirAll(DefaultNativeBPFFSPath, 0o755)
	_ = unix.Mount("bpf", DefaultNativeBPFFSPath, "bpf", 0, "")
}

// resolveHostBPFFSMount locates and validates the host bpffs mount point for mode.
func resolveHostBPFFSMount(mode Mode) (string, error) {
	containerMode := mode != ModeNative
	if override := strings.TrimSpace(os.Getenv(EnvHostBPFFS)); override != "" {
		if err := ValidateHostBPFFSPath(override, containerMode); err != nil {
			return "", fmt.Errorf("%s=%s: %w", EnvHostBPFFS, override, err)
		}
		return override, nil
	}

	// Containerized modes (DaemonSet, K8s sidecar, replay pods, Compose, Docker)
	// MUST use /run/keploy/host-bpffs outside /sys so EnsureBPFFS's private
	// /sys/fs/bpf overlay and containerd AppArmor /sys/fs/[^c]*/** rules never
	// affect pins.
	if containerMode {
		if err := ValidateHostBPFFSPath(DefaultHostBPFFSPath, true); err != nil {
			return "", fmt.Errorf("host bpffs mount %s unavailable: %w", DefaultHostBPFFSPath, err)
		}
		return DefaultHostBPFFSPath, nil
	}

	// Native host CLI/agent: prefer /run/keploy/host-bpffs if mounted, else
	// ensure /sys/fs/bpf is mounted as bpffs and use it.
	if err := ValidateHostBPFFSPath(DefaultHostBPFFSPath, false); err == nil {
		return DefaultHostBPFFSPath, nil
	}
	ensureNativeBPFFS()
	if err := ValidateHostBPFFSPath(DefaultNativeBPFFSPath, false); err != nil {
		return "", fmt.Errorf("host bpffs mount %s unavailable: %w", DefaultNativeBPFFSPath, err)
	}
	return DefaultNativeBPFFSPath, nil
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

	// Always reject container-private bpffs overlays recorded by EnsureBPFFS,
	// even in ModeNative or when KEPLOY_HOST_BPFFS is explicitly set.
	stateMu.Lock()
	isPrivate := privateOverlayDevs[st.Dev]
	stateMu.Unlock()
	if isPrivate {
		return fmt.Errorf("%s resolves to container-private bpffs overlay device %d", path, st.Dev)
	}

	if !checkContainerHostMount {
		return nil
	}

	// If PID 1 (supervisor) captured the host bpffs device before Child 1
	// overlaid /sys/fs/bpf, confirm this mount matches that host device.
	if raw := strings.TrimSpace(os.Getenv(EnvHostBPFFSDev)); raw != "" {
		if wantDev, err := strconv.ParseUint(raw, 10, 64); err == nil && wantDev != 0 && st.Dev != wantDev {
			return fmt.Errorf("%s is on device %d, not the supervisor-recorded host bpffs device %d", path, st.Dev, wantDev)
		}
	}

	// Confirm /run/keploy/host-bpffs is an explicit bpffs mount point in
	// /proc/self/mountinfo (not a symlink into /sys/fs/bpf's private overlay).
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
		// Require mountinfo root field (left[3]) == "/" so a subpath bind of
		// /sys/fs/bpf/keploy cannot silently place keploy-grace/ back inside
		// keploy/ where WipeBPFPins deletes it.
		if left[4] == target && right[0] == "bpf" && left[3] == "/" {
			return true, nil
		}
	}
	return false, sc.Err()
}

type graceOwner struct {
	root         string
	dir          string
	self         string
	lockFD       int
	seq          atomic.Uint64
	reaper       *graceReaper
	reaperCancel context.CancelFunc
}

func (g *graceOwner) pinProgram(prog *ebpf.Program, object, name, role string) error {
	seq := g.seq.Add(1)
	pinName := fmt.Sprintf("%06d-%s-%s-%s", seq, sanitizeToken(object), sanitizeToken(name), role)
	pinPath := filepath.Join(g.dir, pinName)
	if err := prog.Pin(pinPath); err != nil {
		return fmt.Errorf("%w: pin BPF trampoline program %s (%s) into host bpffs grace dir %s: %w", ErrPinFailed, name, object, pinPath, err)
	}
	return nil
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

// sanitizeTag restricts short-grace test tags strictly to [a-z0-9] (no '-')
// so the '-' delimiter in grace-test-<tag>-<nonce> can never cause prefix
// collisions such as tag "e2e" matching dirs created by tag "e2e-2".
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

func openGraceOwner(logger *zap.Logger, mode Mode) (*graceOwner, error) {
	mountPath, err := resolveHostBPFFSMount(mode)
	if err != nil {
		return nil, err
	}
	root := filepath.Join(mountPath, GraceRootSubdir)
	return openGraceOwnerInRoot(root, logger)
}

// openGraceOwnerInRoot creates grace-<nonce> (or grace-test-<tag>-<nonce> when
// running a short-grace test build) and acquires a blocking flock(LOCK_EX) on
// it. Fails if clearing any stale stamp returns an error other than ENOENT.
func openGraceOwnerInRoot(root string, logger *zap.Logger) (*graceOwner, error) {
	if err := os.MkdirAll(root, 0o755); err != nil {
		return nil, fmt.Errorf("create grace root %s: %w", root, err)
	}
	gracePeriod, reapInterval, confirmGap := resolveGraceTiming()
	reapPrefix := GraceDirPrefix
	dirPrefix := GraceDirPrefix
	if gracePeriod < MinProdGracePeriod {
		// Short-grace test builds MUST isolate their directories to a tagged
		// prefix so they can NEVER reap another lane's or production agent's
		// grace-* directories on a shared host.
		tag := sanitizeTag(strings.TrimSpace(os.Getenv(EnvGraceTestTag)))
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
	// Clear any stamp a concurrent reaper may have written in the window
	// between Mkdir and Flock(LOCK_EX). Fail on any error other than ENOENT
	// so a surviving stamp can never shorten this directory's grace window.
	if err := os.Remove(filepath.Join(finalDir, stampFileName)); err != nil && !errors.Is(err, os.ErrNotExist) {
		_ = unix.Close(fd)
		_ = os.RemoveAll(finalDir)
		return nil, fmt.Errorf("clear transient grace stamp in %s: %w", finalDir, err)
	}

	owner := &graceOwner{
		root:   root,
		dir:    finalDir,
		self:   self,
		lockFD: fd,
	}
	reaper := newGraceReaper(root, self, gracePeriod, reapInterval, confirmGap, logger)
	reaper.reapPrefix = reapPrefix
	reaper.pinNoop = func(prog *ebpf.Program, funcName string) error {
		return owner.pinProgram(prog, "bpfsafety_reaper", funcName, "rb")
	}
	owner.reaper = reaper
	return owner, nil
}

func resolveGraceTiming() (gracePeriod, reapInterval, confirmGap time.Duration) {
	gracePeriod = DefaultGracePeriod
	reapInterval = DefaultReapInterval
	confirmGap = DefaultConfirmGap
	shortAllowed := shortGraceTestMode.Load() || shortGraceBuildEnabled

	if raw := strings.TrimSpace(os.Getenv(EnvGraceDuration)); raw != "" {
		if d, err := time.ParseDuration(raw); err == nil && d > 0 {
			if !shortAllowed {
				if d < MinProdGracePeriod {
					d = MinProdGracePeriod
				}
				if d > MaxProdGracePeriod {
					d = MaxProdGracePeriod
				}
			}
			gracePeriod = d
			if shortAllowed {
				if confirmGap > d/2 {
					confirmGap = d / 2
				}
				if confirmGap <= 0 {
					confirmGap = time.Millisecond
				}
				if reapInterval > confirmGap {
					reapInterval = confirmGap
				}
			} else {
				if confirmGap < MinProdConfirmGap {
					confirmGap = MinProdConfirmGap
				}
				if confirmGap > MaxProdConfirmGap {
					confirmGap = MaxProdConfirmGap
				}
			}
		}
	}
	return gracePeriod, reapInterval, confirmGap
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

// sharedStamp is persisted in <graceDir>/first_free_boottime across processes.
// Layout (32 bytes; backward-compatible with 16-byte stamps from v3.7.27):
//   - [0:8]   firstFreeBootNs: CLOCK_BOOTTIME when rebuild succeeded
//   - [8:16]  deadlineHitBootNs: CLOCK_BOOTTIME when gracePeriod elapsed
//   - [16:24] rebuiltBootNs: non-zero once every fentry/fexit target in dir was rebuilt
//   - [24:32] runCntAtRebuild: cumulative bpf_prog_info.run_cnt right after rebuild
type sharedStamp struct {
	firstFreeBootNs   uint64
	deadlineHitBootNs uint64
	rebuiltBootNs     uint64
	runCntAtRebuild   uint64
}

// stampStore reads and writes the 32-byte shared CLOCK_BOOTTIME stamp inside a
// grace directory. On real bpffs (BPF_FS_MAGIC), it uses a pinned 1-entry
// 32-byte BPF_MAP_TYPE_ARRAY so no extra host filesystem mount is required. On
// non-bpffs test filesystems (t.TempDir()), it uses a 32-byte regular file.
type stampStore interface {
	read(dir string) (sharedStamp, bool, error)
	write(dir string, s sharedStamp) error
}

type autoStampStore struct{}

func (autoStampStore) isBPFFS(dir string) bool {
	var stfs unix.Statfs_t
	return unix.Statfs(dir, &stfs) == nil && stfs.Type == unix.BPF_FS_MAGIC
}

func (a autoStampStore) read(dir string) (sharedStamp, bool, error) {
	p := filepath.Join(dir, stampFileName)
	if a.isBPFFS(dir) {
		m, err := ebpf.LoadPinnedMap(p, nil)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				return sharedStamp{}, false, nil
			}
			return sharedStamp{}, false, err
		}
		defer func() { _ = m.Close() }()
		valSize := m.ValueSize()
		if valSize < 16 {
			return sharedStamp{}, false, nil
		}
		buf := make([]byte, valSize)
		if err := m.Lookup(uint32(0), buf); err != nil {
			return sharedStamp{}, false, err
		}
		return decodeStamp(buf), true, nil
	}
	b, err := os.ReadFile(p)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return sharedStamp{}, false, nil
		}
		return sharedStamp{}, false, err
	}
	if len(b) < 16 {
		return sharedStamp{}, false, nil
	}
	return decodeStamp(b), true, nil
}

func (a autoStampStore) write(dir string, s sharedStamp) error {
	p := filepath.Join(dir, stampFileName)
	buf := encodeStamp(s)
	if a.isBPFFS(dir) {
		m, err := ebpf.LoadPinnedMap(p, nil)
		if err == nil && m.ValueSize() == uint32(len(buf)) {
			defer func() { _ = m.Close() }()
			return m.Update(uint32(0), buf[:], ebpf.UpdateAny)
		}
		if err == nil {
			// Upgrade legacy 16-byte stamp map to 32 bytes.
			_ = m.Close()
			_ = os.Remove(p)
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
		m, err = ebpf.NewMap(&ebpf.MapSpec{
			Name:       "grace_stamp",
			Type:       ebpf.Array,
			KeySize:    4,
			ValueSize:  uint32(len(buf)),
			MaxEntries: 1,
		})
		if err != nil {
			return err
		}
		if err := m.Update(uint32(0), buf[:], ebpf.UpdateAny); err != nil {
			_ = m.Close()
			return err
		}
		if err := m.Pin(p); err != nil {
			_ = m.Close()
			return err
		}
		return m.Close()
	}
	return os.WriteFile(p, buf[:], 0o600)
}

func encodeStamp(s sharedStamp) [32]byte {
	var b [32]byte
	binary.LittleEndian.PutUint64(b[0:8], s.firstFreeBootNs)
	binary.LittleEndian.PutUint64(b[8:16], s.deadlineHitBootNs)
	binary.LittleEndian.PutUint64(b[16:24], s.rebuiltBootNs)
	binary.LittleEndian.PutUint64(b[24:32], s.runCntAtRebuild)
	return b
}

func decodeStamp(b []byte) sharedStamp {
	var s sharedStamp
	if len(b) >= 16 {
		s.firstFreeBootNs = binary.LittleEndian.Uint64(b[0:8])
		s.deadlineHitBootNs = binary.LittleEndian.Uint64(b[8:16])
	}
	if len(b) >= 32 {
		s.rebuiltBootNs = binary.LittleEndian.Uint64(b[16:24])
		s.runCntAtRebuild = binary.LittleEndian.Uint64(b[24:32])
	}
	return s
}

// graceReaper scans <host-bpffs>/keploy-grace/ at startup and periodically,
// using flock(LOCK_EX|LOCK_NB) for owner liveness, forcing a kernel trampoline
// rebuild per unique fentry/fexit target before starting the grace clock (so a
// prior -ENOMEM bpf_trampoline_unlink_prog failure cannot leave a stale image
// calling a program that gets unpinned later), and persisting a shared
// CLOCK_BOOTTIME stamp inside each dead directory so short-lived lanes and
// crash-looping DaemonSet pods still reap expired directories in bounded time.
type graceReaper struct {
	root         string
	self         string
	reapPrefix   string
	gracePeriod  time.Duration
	reapInterval time.Duration
	confirmGap   time.Duration
	logger       *zap.Logger
	bootNowNs    func() uint64
	stamps       stampStore
	pinNoop      func(prog *ebpf.Program, funcName string) error
	rebuildFn    func(dirPath string) (uint64, error)
	runCntFn     func(dirPath string) (uint64, error)
	noopMu       sync.Mutex
	noopCache    map[string]*ebpf.Program
}

func newGraceReaper(root, self string, gracePeriod, reapInterval, confirmGap time.Duration, logger *zap.Logger) *graceReaper {
	if logger == nil {
		logger = zap.NewNop()
	}
	r := &graceReaper{
		root:         root,
		self:         self,
		reapPrefix:   GraceDirPrefix,
		gracePeriod:  gracePeriod,
		reapInterval: reapInterval,
		confirmGap:   confirmGap,
		logger:       logger,
		bootNowNs:    clockBoottimeNs,
		stamps:       autoStampStore{},
	}
	r.rebuildFn = r.forceRebuildDirTargets
	r.runCntFn = r.sumDirRunCnt
	return r
}

func (r *graceReaper) start(ctx context.Context) {
	r.round()
	go func() {
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

// round executes one reaper scan and notifies the metrics observer.
func (r *graceReaper) round() (dirsPresent, pinnedProgs int) {
	entries, err := os.ReadDir(r.root)
	if err != nil {
		return 0, 0
	}
	nowNs := r.bootNowNs()
	graceNs := uint64(r.gracePeriod.Nanoseconds())
	confirmNs := uint64(r.confirmGap.Nanoseconds())
	prefix := r.reapPrefix
	if prefix == "" {
		prefix = GraceDirPrefix
	}

	for _, e := range entries {
		name := e.Name()
		if !e.IsDir() || !strings.HasPrefix(name, GraceDirPrefix) {
			continue
		}
		dirPath := filepath.Join(r.root, name)
		if name == r.self {
			dirsPresent++
			pinnedProgs += countPinnedProgs(dirPath)
			continue
		}
		// Short-grace test builds only reap dirs matching their own test prefix
		// so they never unpin unrelated production or sibling-lane grace dirs.
		if !strings.HasPrefix(name, prefix) {
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
			// Live sibling or maxSurge rollout — never touch.
			dirsPresent++
			pinnedProgs += countPinnedProgs(dirPath)
			continue
		}

		// Owner is dead (exclusive flock acquired). Read shared CLOCK_BOOTTIME stamp.
		s, ok, serr := r.stamps.read(dirPath)
		if serr != nil {
			r.logger.Warn("bpf-safety: failed to read shared grace stamp", zap.String("graceDir", dirPath), zap.Error(serr))
			_ = unix.Close(fd)
			dirsPresent++
			pinnedProgs += countPinnedProgs(dirPath)
			continue
		}
		// Phase 1: before starting the grace countdown, force a trampoline
		// rebuild per unique fentry/fexit target in dirPath so any stale
		// trampoline left behind by a prior -ENOMEM bpf_trampoline_unlink_prog
		// failure is replaced before the 15m+1m grace window begins.
		if !ok || s.rebuiltBootNs == 0 || nowNs < s.rebuiltBootNs {
			runCnt, rerr := r.rebuildFn(dirPath)
			if rerr != nil {
				r.logger.Warn("bpf-safety: trampoline rebuild failed for dead grace directory; keeping pins and retrying next pass",
					zap.String("graceDir", dirPath),
					zap.Error(rerr))
				_ = unix.Close(fd)
				dirsPresent++
				pinnedProgs += countPinnedProgs(dirPath)
				continue
			}
			newStamp := sharedStamp{
				firstFreeBootNs: nowNs,
				rebuiltBootNs:   nowNs,
				runCntAtRebuild: runCnt,
			}
			if werr := r.stamps.write(dirPath, newStamp); werr != nil {
				r.logger.Warn("bpf-safety: failed to write shared rebuild stamp", zap.String("graceDir", dirPath), zap.Error(werr))
			}
			_ = unix.Close(fd)
			dirsPresent++
			pinnedProgs += countPinnedProgs(dirPath)
			continue
		}

		elapsedNs := nowNs - s.rebuiltBootNs
		if elapsedNs < graceNs {
			_ = unix.Close(fd)
			dirsPresent++
			pinnedProgs += countPinnedProgs(dirPath)
			continue
		}

		// Verify pinned programs stopped executing (when bpf stats are enabled,
		// run_cnt must remain flat after rebuild). If any prog ran since the
		// rebuild, reset rebuiltBootNs=0, keep pins, and force another rebuild.
		curRunCnt, cerr := r.runCntFn(dirPath)
		if cerr != nil {
			r.logger.Warn("bpf-safety: failed to verify pinned prog run_cnt; keeping pins and retrying next pass",
				zap.String("graceDir", dirPath), zap.Error(cerr))
			_ = unix.Close(fd)
			dirsPresent++
			pinnedProgs += countPinnedProgs(dirPath)
			continue
		}
		if curRunCnt > s.runCntAtRebuild {
			r.logger.Warn("bpf-safety: pinned program still executing after trampoline rebuild; refusing to unpin and scheduling fresh rebuild",
				zap.String("graceDir", dirPath),
				zap.Uint64("runCntAtRebuild", s.runCntAtRebuild),
				zap.Uint64("currentRunCnt", curRunCnt))
			_ = r.stamps.write(dirPath, sharedStamp{firstFreeBootNs: nowNs})
			_ = unix.Close(fd)
			dirsPresent++
			pinnedProgs += countPinnedProgs(dirPath)
			continue
		}

		// Phase 2: grace period elapsed after confirmed rebuild; require
		// confirmation on two separate passes at least confirmGap apart so a
		// VM pause/resume jump cannot reap at once.
		if s.deadlineHitBootNs == 0 || nowNs < s.deadlineHitBootNs {
			s.deadlineHitBootNs = nowNs
			if werr := r.stamps.write(dirPath, s); werr != nil {
				r.logger.Warn("bpf-safety: failed to write shared confirm stamp", zap.String("graceDir", dirPath), zap.Error(werr))
			}
			_ = unix.Close(fd)
			dirsPresent++
			pinnedProgs += countPinnedProgs(dirPath)
			continue
		}
		if nowNs-s.deadlineHitBootNs < confirmNs {
			_ = unix.Close(fd)
			dirsPresent++
			pinnedProgs += countPinnedProgs(dirPath)
			continue
		}

		progs := countPinnedProgs(dirPath)
		rmErr := os.RemoveAll(dirPath)
		_ = unix.Close(fd)
		if rmErr != nil {
			r.logger.Warn("bpf-safety: failed to reap expired trampoline grace directory",
				zap.String("graceDir", dirPath), zap.Error(rmErr))
			dirsPresent++
			pinnedProgs += countPinnedProgs(dirPath)
			continue
		}
		r.logger.Info("bpf-safety: reaped expired trampoline grace directory",
			zap.String("graceDir", dirPath),
			zap.Int("unpinnedProgs", progs),
			zap.Duration("freeFor", time.Duration(elapsedNs)))
	}

	notifyMetrics(dirsPresent, pinnedProgs)
	return dirsPresent, pinnedProgs
}

func countPinnedProgs(dir string) int {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return 0
	}
	n := 0
	for _, e := range entries {
		if e.Name() == stampFileName {
			continue
		}
		n++
	}
	return n
}
