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
// lifetime of the process, and fail closed at PinBeforeAttach if grace pinning
// is not armed (unless the explicit emergency override is set). Placing the
// root at <host-bpffs>/keploy-grace/ keeps grace directories completely outside
// <host-bpffs>/keploy/ so enterprise selfupdate.WipeBPFPins, inst-* sweeps,
// and legacy map-pin cleanups never touch live or recently-exited grace dirs.
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
// In OSS hold mode without host bpffs (e.g. DinD / plain container under
// KEPLOY_PROVEN), GracefulShutdown hands only the cloned BPF program FDs to a
// detached holder process (re-exec of /proc/self/exe with setsid and
// ExtraFiles) that sleeps >= 60s and exits while the main process exits
// immediately.
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
	"os/exec"
	"os/signal"
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
	// by DaemonSet, K8s sidecar webhook, Docker, and Compose launchers so
	// container-private /sys/fs/bpf overlays and containerd's AppArmor
	// /sys/fs/[^c]*/** deny rules never affect grace pins.
	DefaultHostBPFFSPath = "/run/keploy/host-bpffs"

	// DefaultNativeBPFFSPath is the standard host bpffs mount on bare-metal/VM hosts.
	DefaultNativeBPFFSPath = "/sys/fs/bpf"

	// GraceRootSubdir is the dedicated top-level directory on host bpffs for
	// trampoline grace pins (<host-bpffs>/keploy-grace/). It deliberately lives
	// OUTSIDE <host-bpffs>/keploy/ so selfupdate.WipeBPFPins, inst-* sweeps,
	// and legacy map-pin cleanups can never delete live or cooling grace dirs.
	GraceRootSubdir = "keploy-grace"

	// DetachedHolderComm is the /proc/<pid>/comm (<= 15 bytes) and argv[0]
	// assigned to the detached BPF program-FD holder process. Deliberately
	// omits "keploy" so pgrep/pkill/pidof keploy and HasOtherKeployProcesses
	// do not match or terminate the holder during its grace sleep.
	DetachedHolderComm = "kp-bpf-hold"

	// EnvHostBPFFS overrides the host bpffs mount path.
	EnvHostBPFFS = "KEPLOY_HOST_BPFFS"

	// EnvHostBPFFSDev carries the host bpffs st_dev captured by PID 1 (e.g.
	// the self-update supervisor) before any child overlays /sys/fs/bpf.
	EnvHostBPFFSDev = "KEPLOY_HOST_BPFFS_DEV"

	// EnvLaunchMode explicitly tells DetectMode which launcher started the
	// agent ("daemonset", "k8s-sidecar", "k8s-replay", "compose", "docker",
	// "native"), avoiding brittle ambient heuristics like KUBERNETES_SERVICE_HOST.
	EnvLaunchMode = "KEPLOY_LAUNCH_MODE"

	// EnvAllowUnprotectedTrampolines is the explicit break-glass opt-out that
	// permits attaching fentry/fexit trampolines without host bpffs grace
	// pinning. Logged as ERROR on every start and surfaced in metrics/telemetry.
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

	// DefaultInProcessHoldWait is the detached-holder prog-FD retention window
	// (>= 60s) used ONLY by OSS unixconn when no host bpffs is available.
	DefaultInProcessHoldWait = 60 * time.Second

	// Internal env vars used exclusively when re-execing /proc/self/exe as a
	// detached BPF program-FD holder process.
	envDetachedHolderMS    = "KEPLOY_INTERNAL_BPF_HOLD_MS"
	envDetachedHolderCount = "KEPLOY_INTERNAL_BPF_HOLD_COUNT"

	// Production bounds enforced unless shortGraceTestMode or -tags bpfsafety_short_grace.
	MinProdGracePeriod = 15 * time.Minute
	MaxProdGracePeriod = 24 * time.Hour
	MinProdConfirmGap  = 1 * time.Minute
	MaxProdConfirmGap  = 30 * time.Minute
)

// Mode identifies the runtime environment so Init selects the right mount path
// and produces accurate per-mode remediation messages.
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

// ErrGraceNotArmed is returned by PinBeforeAttach when trampoline grace
// pinning has not been armed and the explicit break-glass opt-out is not set.
var ErrGraceNotArmed = errors.New("bpf trampoline grace pinning is not armed (refusing unprotected fentry/fexit attach; call bpfsafety.Init first or set KEPLOY_ALLOW_UNPROTECTED_TRAMPOLINES=1)")

// ErrShuttingDown is returned by PinBeforeAttach once GracefulShutdown has
// closed the attach gate.
var ErrShuttingDown = errors.New("bpf trampoline safety gate is closed for shutdown (refusing late fentry/fexit attach)")

// ErrPinFailed is wrapped around bpffs pin errors in PinBeforeAttach so callers
// such as OSS unixconn can distinguish host-bpffs pin failures (e.g. EACCES)
// from kernel link.AttachTracing failures (e.g. missing trampoline support).
var ErrPinFailed = errors.New("bpf trampoline grace pin failed")

// MetricsObserver receives updated host-bpffs grace counts and opt-out status.
type MetricsObserver func(dirsPresent, pinnedProgs int, unprotected bool)

var (
	initMu                 sync.Mutex
	stateMu                sync.Mutex
	activeOwner            *graceOwner
	detachHooks            []func()
	inProcessHolds         []*ebpf.Program
	unpinnedLockFDs        []int
	privateOverlayDevs     = map[uint64]bool{}
	metricsObserver        MetricsObserver
	unprotectedTrampolines atomic.Bool
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

// UnprotectedTrampolinesActive reports whether this process started under
// KEPLOY_ALLOW_UNPROTECTED_TRAMPOLINES=1 without host bpffs grace pinning.
func UnprotectedTrampolinesActive() bool {
	return unprotectedTrampolines.Load()
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
// and the 60s clean-exit self-unpin floor for Go tests.
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

// HasActiveWork reports whether this process has armed host-bpffs grace pins,
// registered tracing detach hooks, or held in-process BPF program FDs that
// GracefulShutdown must tear down on exit.
func HasActiveWork() bool {
	stateMu.Lock()
	ok := activeOwner != nil || len(inProcessHolds) > 0 || len(detachHooks) > 0
	stateMu.Unlock()
	return ok
}

// Init verifies a writable host bpffs mount, creates and locks this process's
// <host-bpffs>/keploy-grace/grace-<nonce> directory (blocking flock(LOCK_EX)
// and clearing any transient first_free_boottime stamp), and starts the
// background reaper.
//
// Serialized and idempotent when already armed. Refuses to reopen the attach
// gate once GracefulShutdown has started (returns ErrShuttingDown). Fail-closed
// by default, wrapping utils.ErrEnvironmentUnsupported so the process exits
// with ExitEnvironmentUnsupported (6) and supervisors do not indict the binary.
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

	owner, err := openGraceOwner(logger, mode)
	if err != nil {
		if strings.TrimSpace(os.Getenv(EnvAllowUnprotectedTrampolines)) == "1" {
			unprotectedTrampolines.Store(true)
			notifyMetrics(0, 0)
			logger.Error("bpf-safety: running WITHOUT host bpffs trampoline grace pinning because KEPLOY_ALLOW_UNPROTECTED_TRAMPOLINES=1 is set — detaching fentry/fexit programs on unpatched Linux 5.10..7.2.9 kernels can panic the node (CVE-2026-98800)",
				zap.String("mode", string(mode)),
				zap.String("remediation", modeRemediation(mode, err)),
				zap.Error(err))
			return nil
		}
		unprotectedTrampolines.Store(false)
		notifyMetrics(0, 0)
		return fmt.Errorf("bpf trampoline safety preflight failed (%s): %w — %s (for an emergency break-glass override only, set %s=1): %w",
			mode, err, modeRemediation(mode, err), EnvAllowUnprotectedTrampolines, utils.ErrEnvironmentUnsupported)
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

func init() {
	raw := strings.TrimSpace(os.Getenv(envDetachedHolderMS))
	if raw == "" {
		return
	}
	ms, err := strconv.ParseInt(raw, 10, 64)
	if err != nil || ms <= 0 {
		os.Exit(0)
	}
	count := 0
	if rawCount := strings.TrimSpace(os.Getenv(envDetachedHolderCount)); rawCount != "" {
		if n, perr := strconv.Atoi(rawCount); perr == nil && n > 0 {
			count = n
		}
	}
	// Detached BPF program-FD holder mode: parent already detached all tracing
	// links and exited. Inherited ExtraFiles (FDs 3..3+count-1) keep struct
	// bpf_prog referenced across RCU-tasks grace periods until this process
	// exits.
	//
	// 1. Ignore SIGHUP/SIGINT/SIGQUIT so terminal/session teardown or Ctrl-C
	//    cannot terminate the holder early before RCU-tasks finishes.
	signal.Ignore(syscall.SIGHUP, syscall.SIGINT, syscall.SIGQUIT)
	// 2. Rename /proc/self/comm away from "keploy" so pgrep/pkill/pidof keploy
	//    and HasOtherKeployProcesses ignore the holder.
	setHolderProcessName(DetachedHolderComm)
	// 3. Enforce FD hygiene by construction: close every FD >= 3+count via
	//    close_range (so no non-CLOEXEC FD inherited from a launcher stays
	//    open for 60s) and close any FD inside 3..3+count-1 that is not a
	//    genuine BPF program FD.
	sanitizeHolderFDs(count)
	// 4. Sleep using unix.Nanosleep (immune to EINTR and independent of Go's
	//    netpoll after close_range) and exit via exit_group.
	sleepDurationRaw(time.Duration(ms) * time.Millisecond)
	unix.Exit(0)
}

func setHolderProcessName(comm string) {
	fd, err := unix.Open("/proc/self/comm", unix.O_WRONLY|unix.O_CLOEXEC, 0)
	if err != nil {
		return
	}
	_, _ = unix.Write(fd, []byte(comm))
	_ = unix.Close(fd)
}

func sanitizeHolderFDs(count int) {
	if count < 0 {
		count = 0
	}
	firstClose := 3 + count
	if err := closeFDsFrom(firstClose); err != nil {
		closeFDsFromProc(firstClose)
	}
	for fd := 3; fd < firstClose; fd++ {
		if !isBPFProgramFD(fd) {
			_ = unix.Close(fd)
		}
	}
}

func closeFDsFrom(first int) error {
	_, _, errno := unix.Syscall(unix.SYS_CLOSE_RANGE, uintptr(first), uintptr(^uint32(0)), 0)
	if errno != 0 {
		return errno
	}
	return nil
}

func closeFDsFromProc(first int) {
	entries, err := os.ReadDir("/proc/self/fd")
	if err != nil {
		return
	}
	for _, e := range entries {
		fd, perr := strconv.Atoi(e.Name())
		if perr != nil || fd < first {
			continue
		}
		_ = unix.Close(fd)
	}
}

func isBPFProgramFD(fd int) bool {
	target, err := os.Readlink(fmt.Sprintf("/proc/self/fd/%d", fd))
	if err == nil {
		return target == "anon_inode:bpf-prog"
	}
	data, ferr := os.ReadFile(fmt.Sprintf("/proc/self/fdinfo/%d", fd))
	if ferr != nil {
		return false
	}
	return strings.Contains(string(data), "prog_type:")
}

func sleepDurationRaw(d time.Duration) {
	if d <= 0 {
		return
	}
	req := unix.NsecToTimespec(d.Nanoseconds())
	var rem unix.Timespec
	for {
		err := unix.Nanosleep(&req, &rem)
		if !errors.Is(err, unix.EINTR) {
			return
		}
		req = rem
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

func modeRemediation(mode Mode, cause error) string {
	causeStr := ""
	if cause != nil {
		causeStr = cause.Error()
	}
	switch mode {
	case ModeDaemonSet:
		return fmt.Sprintf("mount host /sys/fs/bpf at %s (upgrade the k8s-proxy Helm chart to the matching release)", DefaultHostBPFFSPath)
	case ModeK8sSidecar:
		return fmt.Sprintf("mount host /sys/fs/bpf at %s (upgrade the k8s-proxy webhook to the matching release and ensure the node has bpffs mounted at /sys/fs/bpf)", DefaultHostBPFFSPath)
	case ModeCompose:
		if strings.Contains(causeStr, "not a bpffs filesystem") {
			return fmt.Sprintf("the Docker host's /sys/fs/bpf is not a mounted bpffs (e.g. Docker-in-Docker without a host bpffs bind); mount host bpffs into the Docker daemon at /sys/fs/bpf so -v /sys/fs/bpf:%s resolves to BPF_FS_MAGIC", DefaultHostBPFFSPath)
		}
		return fmt.Sprintf("mount host /sys/fs/bpf at %s in your docker compose agent service (-v /sys/fs/bpf:%s)", DefaultHostBPFFSPath, DefaultHostBPFFSPath)
	case ModeDocker:
		if strings.Contains(causeStr, "not a bpffs filesystem") {
			return fmt.Sprintf("the Docker host's /sys/fs/bpf is not a mounted bpffs (e.g. Docker-in-Docker without a host bpffs bind); mount host bpffs into the Docker daemon at /sys/fs/bpf so -v /sys/fs/bpf:%s resolves to BPF_FS_MAGIC", DefaultHostBPFFSPath)
		}
		return fmt.Sprintf("pass -v /sys/fs/bpf:%s to docker run (or upgrade the keploy CLI)", DefaultHostBPFFSPath)
	default:
		return fmt.Sprintf("ensure host bpffs is mounted writable at %s (mount -t bpf bpf %s on the host) or set %s", DefaultNativeBPFFSPath, DefaultNativeBPFFSPath, EnvHostBPFFS)
	}
}

// PinBeforeAttach pins prog into the active host-bpffs grace directory BEFORE
// link.AttachTracing is called.
//
// Fail-closed BY CONSTRUCTION: if GracefulShutdown has started, returns
// ErrShuttingDown; if Init has not armed grace pinning and neither
// KEPLOY_ALLOW_UNPROTECTED_TRAMPOLINES=1 nor SetBypassForTest is active,
// returns ErrGraceNotArmed so no production path can silently attach an
// unprotected trampoline.
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

// AttachTracingWithInProcessHold attaches prog via link.AttachTracing while
// keeping a cloned program FD alive so GracefulShutdown can hand it off to a
// detached holder process (>= 60s) without blocking process exit. Used ONLY by
// OSS unixconn when no host bpffs is available (e.g. DinD / plain container
// under KEPLOY_PROVEN) so fexit retains its exact in-kernel peer filter with
// zero capture regression.
//
// Enterprise proxyless/netio/classic-scope callers MUST use PinBeforeAttach /
// AttachTracing (fail-closed) and never this helper.
func AttachTracingWithInProcessHold(prog *ebpf.Program) (link.Link, error) {
	if prog == nil {
		return nil, errors.New("cannot attach nil BPF program")
	}
	if shuttingDown.Load() {
		return nil, ErrShuttingDown
	}
	held, err := prog.Clone()
	if err != nil {
		return nil, fmt.Errorf("clone BPF program for detached grace hold: %w", err)
	}
	l, err := link.AttachTracing(link.TracingOptions{Program: prog})
	if err != nil {
		_ = held.Close()
		return nil, err
	}
	stateMu.Lock()
	if shuttingDown.Load() {
		stateMu.Unlock()
		// Shutdown raced with attach: close the link immediately and hand the
		// cloned prog FD to a detached holder before returning ErrShuttingDown.
		_ = l.Close()
		if _, herr := spawnDetachedHolder(zap.L(), []*ebpf.Program{held}, DefaultInProcessHoldWait); herr != nil {
			zap.L().Warn("bpf-safety: failed to start detached holder on shutdown race; exiting without blocking", zap.Error(herr))
		}
		_ = held.Close()
		return nil, ErrShuttingDown
	}
	inProcessHolds = append(inProcessHolds, held)
	stateMu.Unlock()
	return l, nil
}

// GracefulShutdown closes the attach gate, runs all registered link-detach
// hooks synchronously, cancels the background reaper, and returns IMMEDIATELY
// without blocking process exit:
//
//   - With host bpffs armed: leaves the grace directory pinned on host bpffs
//     and keeps owner.lockFD open until process exit (kernel releases flock on
//     exit) so the >= 15m cross-process reaper safely reaps it. No self-unpin
//     and zero exit stall.
//   - Without host bpffs (OSS hold mode): hands only the cloned BPF program
//     FDs via ExtraFiles to a detached holder process (re-exec of
//     /proc/self/exe with setsid, /dev/null stdio) that sleeps >= 60s (wait,
//     clamped >= DefaultInProcessHoldWait in production) and exits while the
//     main process exits immediately. If running as container PID 1 (where the
//     container dies with this process) or spawn fails, logs once and exits
//     immediately without blocking.
func GracefulShutdown(_ context.Context, logger *zap.Logger, wait time.Duration) {
	if logger == nil {
		logger = zap.NewNop()
	}

	// Close the attach gate and snapshot state atomically under stateMu so no
	// concurrent RegisterDetachHook or AttachTracingWithInProcessHold can race
	// past shutdown.
	stateMu.Lock()
	shuttingDown.Store(true)
	hooks := detachHooks
	detachHooks = nil
	owner := activeOwner
	activeOwner = nil
	holds := inProcessHolds
	inProcessHolds = nil
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

	if len(holds) > 0 {
		pid, err := spawnDetachedHolder(logger, holds, wait)
		if err != nil {
			logger.Warn("bpf-safety: failed to start detached BPF program-FD holder; exiting without blocking",
				zap.Int("heldProgs", len(holds)),
				zap.Error(err))
		} else if pid > 0 {
			logger.Info("bpf-safety: handed BPF program FDs to detached holder process; exiting immediately",
				zap.Int("holderPid", pid),
				zap.Int("heldProgs", len(holds)),
				zap.Duration("holdWait", effectiveHoldWait(wait)))
		}
		for _, p := range holds {
			_ = p.Close()
		}
	}
	utils.TeardownStep()
}

func effectiveHoldWait(wait time.Duration) time.Duration {
	shortAllowed := shortGraceTestMode.Load() || shortGraceBuildEnabled
	if !shortAllowed {
		if wait < DefaultInProcessHoldWait {
			return DefaultInProcessHoldWait
		}
		return wait
	}
	if wait <= 0 {
		return DefaultInProcessHoldWait
	}
	return wait
}

// spawnDetachedHolder starts a detached copy of /proc/self/exe in a new
// session (setsid) carrying ONLY the duplicated BPF program FDs via ExtraFiles
// and /dev/null stdio so callers waiting on stdout/stderr pipes never stall.
// Sets argv[0] to DetachedHolderComm so pgrep/pkill keploy ignore the holder.
// When running as container PID 1 (or a direct child of a container init like
// tini/dumb-init), logs a warning once because no child can outlive container
// PID-namespace teardown, and still spawns best-effort without blocking exit.
func spawnDetachedHolder(logger *zap.Logger, holds []*ebpf.Program, wait time.Duration) (int, error) {
	if logger == nil {
		logger = zap.NewNop()
	}
	if os.Getpid() == 1 || (os.Getppid() == 1 && runningInContainerNamespace()) {
		logger.Warn("bpf-safety: running as container PID 1 (or under container init) without host bpffs; detached prog-FD holder cannot outlive PID-namespace exit (exiting without blocking; mount host /sys/fs/bpf at /run/keploy/host-bpffs for crash-surviving protection)",
			zap.Int("pid", os.Getpid()),
			zap.Int("ppid", os.Getppid()),
			zap.Int("heldProgs", len(holds)))
	}
	d := effectiveHoldWait(wait)
	ms := d.Milliseconds()
	if ms <= 0 {
		ms = 1
	}

	extra := make([]*os.File, 0, len(holds))
	closeExtra := func() {
		for _, f := range extra {
			_ = f.Close()
		}
	}
	for i, p := range holds {
		if p == nil {
			continue
		}
		dupFD, derr := unix.FcntlInt(uintptr(p.FD()), unix.F_DUPFD_CLOEXEC, 3)
		if derr != nil {
			closeExtra()
			return 0, fmt.Errorf("dup BPF prog FD %d for detached holder: %w", i, derr)
		}
		extra = append(extra, os.NewFile(uintptr(dupFD), fmt.Sprintf("bpf-prog-hold-%d", i)))
	}
	if len(extra) == 0 {
		return 0, nil
	}

	// Exec /proc/self/exe directly so the spawn survives binary replacement,
	// unlinking, or mount-namespace changes, and set argv[0] to
	// DetachedHolderComm so process-name matching for "keploy" skips it.
	cmd := exec.Command("/proc/self/exe")
	cmd.Args = []string{DetachedHolderComm}
	cmd.Env = []string{
		fmt.Sprintf("%s=%d", envDetachedHolderMS, ms),
		fmt.Sprintf("%s=%d", envDetachedHolderCount, len(extra)),
	}
	cmd.Stdin = nil
	cmd.Stdout = nil
	cmd.Stderr = nil
	cmd.ExtraFiles = extra
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	startErr := cmd.Start()
	closeExtra()
	if startErr != nil {
		return 0, startErr
	}
	pid := cmd.Process.Pid
	_ = cmd.Process.Release()
	return pid, nil
}

func runningInContainerNamespace() bool {
	if _, err := os.Stat("/.dockerenv"); err == nil {
		return true
	}
	if _, err := os.Stat("/run/.containerenv"); err == nil {
		return true
	}
	if b, err := os.ReadFile("/proc/1/cgroup"); err == nil {
		s := string(b)
		if strings.Contains(s, "docker") || strings.Contains(s, "containerd") ||
			strings.Contains(s, "kubepods") || strings.Contains(s, "crio") ||
			strings.Contains(s, "libpod") {
			return true
		}
	}
	return false
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

	// Containerized modes (DaemonSet, K8s sidecar, Compose, Docker) MUST use
	// /run/keploy/host-bpffs outside /sys so EnsureBPFFS's private /sys/fs/bpf
	// overlay and containerd AppArmor /sys/fs/[^c]*/** rules never affect pins.
	if containerMode {
		if err := ValidateHostBPFFSPath(DefaultHostBPFFSPath, true); err != nil {
			return "", fmt.Errorf("host bpffs mount %s unavailable: %w", DefaultHostBPFFSPath, err)
		}
		return DefaultHostBPFFSPath, nil
	}

	// Native host CLI/agent: prefer /run/keploy/host-bpffs if mounted, else /sys/fs/bpf.
	if err := ValidateHostBPFFSPath(DefaultHostBPFFSPath, false); err == nil {
		return DefaultHostBPFFSPath, nil
	}
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

	reaper := newGraceReaper(root, self, gracePeriod, reapInterval, confirmGap, logger)
	reaper.reapPrefix = reapPrefix
	return &graceOwner{
		root:   root,
		dir:    finalDir,
		self:   self,
		lockFD: fd,
		reaper: reaper,
	}, nil
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
type sharedStamp struct {
	firstFreeBootNs   uint64
	deadlineHitBootNs uint64
}

// stampStore reads and writes the 16-byte shared CLOCK_BOOTTIME stamp inside a
// grace directory. On real bpffs (BPF_FS_MAGIC), it uses a pinned 1-entry
// 16-byte BPF_MAP_TYPE_ARRAY so no extra host filesystem mount is required. On
// non-bpffs test filesystems (t.TempDir()), it uses a 16-byte regular file.
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
		var buf [16]byte
		if err := m.Lookup(uint32(0), &buf); err != nil {
			return sharedStamp{}, false, err
		}
		return decodeStamp(buf[:]), true, nil
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
		if err != nil {
			if !errors.Is(err, os.ErrNotExist) {
				return err
			}
			m, err = ebpf.NewMap(&ebpf.MapSpec{
				Name:       "grace_stamp",
				Type:       ebpf.Array,
				KeySize:    4,
				ValueSize:  16,
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
		defer func() { _ = m.Close() }()
		return m.Update(uint32(0), buf[:], ebpf.UpdateAny)
	}
	return os.WriteFile(p, buf[:], 0o600)
}

func encodeStamp(s sharedStamp) [16]byte {
	var b [16]byte
	binary.LittleEndian.PutUint64(b[0:8], s.firstFreeBootNs)
	binary.LittleEndian.PutUint64(b[8:16], s.deadlineHitBootNs)
	return b
}

func decodeStamp(b []byte) sharedStamp {
	return sharedStamp{
		firstFreeBootNs:   binary.LittleEndian.Uint64(b[0:8]),
		deadlineHitBootNs: binary.LittleEndian.Uint64(b[8:16]),
	}
}

// graceReaper scans <host-bpffs>/keploy-grace/ at startup and periodically,
// using flock(LOCK_EX|LOCK_NB) for owner liveness and a shared CLOCK_BOOTTIME
// stamp persisted inside each dead directory so short-lived lanes and
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
}

func newGraceReaper(root, self string, gracePeriod, reapInterval, confirmGap time.Duration, logger *zap.Logger) *graceReaper {
	if logger == nil {
		logger = zap.NewNop()
	}
	return &graceReaper{
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
		if !ok || s.firstFreeBootNs == 0 || nowNs < s.firstFreeBootNs {
			// First observation after owner exit (or boot clock reset): record firstFree.
			if werr := r.stamps.write(dirPath, sharedStamp{firstFreeBootNs: nowNs}); werr != nil {
				r.logger.Warn("bpf-safety: failed to write shared first-free stamp", zap.String("graceDir", dirPath), zap.Error(werr))
			}
			_ = unix.Close(fd)
			dirsPresent++
			pinnedProgs += countPinnedProgs(dirPath)
			continue
		}

		elapsedNs := nowNs - s.firstFreeBootNs
		if elapsedNs < graceNs {
			_ = unix.Close(fd)
			dirsPresent++
			pinnedProgs += countPinnedProgs(dirPath)
			continue
		}

		// Grace period elapsed: require confirmation on two separate passes at
		// least confirmGap apart so a VM pause/resume jump cannot reap at once.
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
