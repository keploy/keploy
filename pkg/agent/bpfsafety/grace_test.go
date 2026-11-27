//go:build linux

package bpfsafety

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/utils"
)

func resetStateForTest(t *testing.T) {
	t.Helper()
	stateMu.Lock()
	prevOwner := activeOwner
	prevObs := metricsObserver
	prevHooks := detachHooks
	prevHolds := inProcessHolds
	prevLockFDs := unpinnedLockFDs
	prevOverlays := make(map[uint64]bool, len(privateOverlayDevs))
	for k, v := range privateOverlayDevs {
		prevOverlays[k] = v
	}
	activeOwner = nil
	metricsObserver = nil
	detachHooks = nil
	inProcessHolds = nil
	unpinnedLockFDs = nil
	privateOverlayDevs = map[uint64]bool{}
	stateMu.Unlock()
	prevUnprot := unprotectedTrampolines.Swap(false)
	prevShuttingDown := shuttingDown.Swap(false)
	prevBypass := testBypassPin.Swap(false)
	t.Cleanup(func() {
		stateMu.Lock()
		for _, fd := range unpinnedLockFDs {
			_ = unix.Close(fd)
		}
		activeOwner = prevOwner
		metricsObserver = prevObs
		detachHooks = prevHooks
		inProcessHolds = prevHolds
		unpinnedLockFDs = prevLockFDs
		privateOverlayDevs = prevOverlays
		stateMu.Unlock()
		unprotectedTrampolines.Store(prevUnprot)
		shuttingDown.Store(prevShuttingDown)
		testBypassPin.Store(prevBypass)
	})
}

func TestDetectMode_K8sCIPodIsNative(t *testing.T) {
	t.Setenv(EnvLaunchMode, "")
	t.Setenv("KEPLOY_DAEMONSET_ENABLED", "")
	t.Setenv("KEPLOY_INDOCKER", "")
	t.Setenv("KEPLOY_POD_UID", "")
	t.Setenv("KEPLOY_APP_CONTAINER_NAME", "")
	t.Setenv("KEPLOY_COMPOSE_MODE", "")
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")

	if got := DetectMode(false); got != ModeNative {
		t.Fatalf("DetectMode(false) in k8s CI pod with KUBERNETES_SERVICE_HOST = %q, want %q", got, ModeNative)
	}
}

func TestDetectMode_ExplicitLaunchModeAndFallbacks(t *testing.T) {
	clear := func(t *testing.T) {
		for _, k := range []string{
			EnvLaunchMode, "KEPLOY_DAEMONSET_ENABLED", "KEPLOY_INDOCKER",
			"KEPLOY_POD_UID", "KEPLOY_APP_CONTAINER_NAME", "KEPLOY_COMPOSE_MODE",
			"KUBERNETES_SERVICE_HOST",
		} {
			t.Setenv(k, "")
		}
	}

	t.Run("explicit_launch_mode", func(t *testing.T) {
		for envVal, want := range map[string]Mode{
			"daemonset":   ModeDaemonSet,
			"k8s-sidecar": ModeK8sSidecar,
			"k8s-replay":  ModeK8sSidecar,
			"compose":     ModeCompose,
			"docker":      ModeDocker,
			"native":      ModeNative,
		} {
			clear(t)
			t.Setenv(EnvLaunchMode, envVal)
			if got := DetectMode(false); got != want {
				t.Fatalf("KEPLOY_LAUNCH_MODE=%q -> %q, want %q", envVal, got, want)
			}
		}
	})

	t.Run("fallback_webhook_sidecar_requires_indocker_and_pod_uid", func(t *testing.T) {
		clear(t)
		t.Setenv("KEPLOY_POD_UID", "uid-123")
		if got := DetectMode(false); got != ModeNative {
			t.Fatalf("KEPLOY_POD_UID without KEPLOY_INDOCKER = %q, want native", got)
		}
		t.Setenv("KEPLOY_INDOCKER", "true")
		if got := DetectMode(false); got != ModeK8sSidecar {
			t.Fatalf("KEPLOY_POD_UID + KEPLOY_INDOCKER = %q, want k8s-sidecar", got)
		}
	})

	t.Run("fallback_compose_and_docker", func(t *testing.T) {
		clear(t)
		t.Setenv("KEPLOY_INDOCKER", "true")
		if got := DetectMode(false); got != ModeDocker {
			t.Fatalf("KEPLOY_INDOCKER=true -> %q, want docker", got)
		}
		t.Setenv("KEPLOY_COMPOSE_MODE", "true")
		if got := DetectMode(false); got != ModeCompose {
			t.Fatalf("KEPLOY_COMPOSE_MODE=true -> %q, want compose", got)
		}
	})
}

func TestPinBeforeAttachFailsClosedByConstruction(t *testing.T) {
	resetStateForTest(t)

	var dummy ebpf.Program
	err := PinBeforeAttach(&dummy, "proxyless", "keploy_tcp_sendmsg", "p")
	if !errors.Is(err, ErrGraceNotArmed) {
		t.Fatalf("PinBeforeAttach unarmed = %v, want ErrGraceNotArmed", err)
	}

	SetBypassForTest(t)
	if err := PinBeforeAttach(&dummy, "proxyless", "keploy_tcp_sendmsg", "p"); err != nil {
		t.Fatalf("PinBeforeAttach with SetBypassForTest = %v, want nil", err)
	}

	// Once shutdown gate closes, PinBeforeAttach MUST fail closed even with bypass.
	shuttingDown.Store(true)
	if err := PinBeforeAttach(&dummy, "proxyless", "keploy_tcp_sendmsg", "p"); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("PinBeforeAttach after shutdown gate = %v, want ErrShuttingDown", err)
	}
}

func TestInitFailClosedPerModeMessagesAndOptOut(t *testing.T) {
	resetStateForTest(t)
	notBPFFS := t.TempDir()
	t.Setenv(EnvHostBPFFS, notBPFFS)
	t.Setenv(EnvAllowUnprotectedTrampolines, "")

	cases := []struct {
		mode Mode
		want string
	}{
		{ModeDaemonSet, "k8s-proxy Helm chart"},
		{ModeK8sSidecar, "k8s-proxy webhook"},
		{ModeCompose, "Docker host's /sys/fs/bpf is not a mounted bpffs"},
		{ModeDocker, "Docker host's /sys/fs/bpf is not a mounted bpffs"},
		{ModeNative, "mount -t bpf"},
	}
	for _, tc := range cases {
		err := Init(context.Background(), zap.NewNop(), tc.mode)
		if err == nil {
			t.Fatalf("mode %s: expected fail-closed error", tc.mode)
		}
		if !errors.Is(err, utils.ErrEnvironmentUnsupported) {
			t.Fatalf("mode %s: error %v must wrap utils.ErrEnvironmentUnsupported", tc.mode, err)
		}
		if !strings.Contains(err.Error(), tc.want) {
			t.Fatalf("mode %s: error %q missing mode-specific remediation %q", tc.mode, err.Error(), tc.want)
		}
		if tc.mode == ModeDocker && strings.Contains(err.Error(), "Helm") {
			t.Fatalf("docker mode error wrongly mentions Helm: %q", err.Error())
		}
	}

	t.Setenv(EnvAllowUnprotectedTrampolines, "1")
	core, logs := observer.New(zapcore.ErrorLevel)
	var obsUnprot bool
	SetMetricsObserver(func(_, _ int, unprot bool) { obsUnprot = unprot })

	if err := Init(context.Background(), zap.New(core), ModeDaemonSet); err != nil {
		t.Fatalf("Init with opt-out returned error: %v", err)
	}
	if !UnprotectedTrampolinesActive() || !obsUnprot {
		t.Fatalf("UnprotectedTrampolinesActive=%v obsUnprot=%v, want both true", UnprotectedTrampolinesActive(), obsUnprot)
	}
	if logs.Len() == 0 || !strings.Contains(logs.All()[0].Message, EnvAllowUnprotectedTrampolines) {
		t.Fatalf("expected ERROR log naming %s, got %+v", EnvAllowUnprotectedTrampolines, logs.All())
	}
	var dummy ebpf.Program
	if err := PinBeforeAttach(&dummy, "proxyless", "keploy_tcp_sendmsg", "p"); err != nil {
		t.Fatalf("PinBeforeAttach under explicit opt-out returned %v, want nil", err)
	}
}

func TestResolveGraceTimingEnforces15mFloorAnd24hUpperClamp(t *testing.T) {
	resetStateForTest(t)

	// Sub-15m values (including 10m = RCU_TASK_STALL_TIMEOUT) MUST clamp up to 15m.
	for _, raw := range []string{"5s", "10m", "14m59s"} {
		t.Setenv(EnvGraceDuration, raw)
		gp, _, cg := resolveGraceTiming()
		if gp != MinProdGracePeriod {
			t.Fatalf("EnvGraceDuration=%q -> gracePeriod=%v, want %v", raw, gp, MinProdGracePeriod)
		}
		if cg < MinProdConfirmGap {
			t.Fatalf("EnvGraceDuration=%q -> confirmGap=%v, want >= %v", raw, cg, MinProdConfirmGap)
		}
	}

	// Excessively large values clamp down to MaxProdGracePeriod (24h).
	t.Setenv(EnvGraceDuration, "1000h")
	gp, _, _ := resolveGraceTiming()
	if gp != MaxProdGracePeriod {
		t.Fatalf("EnvGraceDuration=1000h -> gracePeriod=%v, want %v", gp, MaxProdGracePeriod)
	}

	// Short-grace test mode lifts the floor.
	t.Setenv(EnvGraceDuration, "5s")
	AllowShortGraceForTest(t)
	gp, _, cg := resolveGraceTiming()
	if gp != 5*time.Second || cg != 2500*time.Millisecond {
		t.Fatalf("test short gracePeriod=%v confirmGap=%v, want 5s / 2.5s", gp, cg)
	}
}

func TestShortGraceBuildReapsOnlyOwnTestPrefix(t *testing.T) {
	resetStateForTest(t)
	AllowShortGraceForTest(t)
	t.Setenv(EnvGraceDuration, "2s")
	t.Setenv(EnvGraceTestTag, "e2e")

	root := t.TempDir()
	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatalf("openGraceOwnerInRoot: %v", err)
	}
	defer func() { _ = unix.Close(owner.lockFD) }()

	if !strings.HasPrefix(owner.self, GraceTestDirPrefix+"e2e-") {
		t.Fatalf("short-grace owner.self = %q, want prefix %q", owner.self, GraceTestDirPrefix+"e2e-")
	}
	// Verify sanitizeTag strips '-' so "e2e-2" becomes "e2e2" and cannot collide with "e2e-".
	if got := sanitizeTag("e2e-2"); got != "e2e2" {
		t.Fatalf("sanitizeTag(\"e2e-2\") = %q, want \"e2e2\"", got)
	}

	// Place a dead production grace-* dir, a dead sibling test dir with another
	// tag (including "e2e2" which came from "e2e-2"), plus a dead dir belonging
	// to THIS test tag ("e2e").
	prodDead := filepath.Join(root, GraceDirPrefix+"prod-dead-agent")
	otherTestDead := filepath.Join(root, GraceTestDirPrefix+"e2e2-dead")
	ownTestDead := filepath.Join(root, GraceTestDirPrefix+"e2e-dead")
	for _, d := range []string{prodDead, otherTestDead, ownTestDead} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "000001-p"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	now := uint64(500 * time.Second)
	owner.reaper.bootNowNs = func() uint64 { return now }

	owner.reaper.round() // stamps ownTestDead
	now += uint64(3 * time.Second)
	owner.reaper.round() // hits deadline
	now += uint64(2 * time.Second)
	owner.reaper.round() // reaps ONLY ownTestDead

	if _, err := os.Stat(ownTestDead); !os.IsNotExist(err) {
		t.Fatalf("ownTestDead was not reaped by matching short-grace tag: err=%v", err)
	}
	if _, err := os.Stat(prodDead); err != nil {
		t.Fatalf("short-grace reaper MUST NOT touch production grace-* dir: %v", err)
	}
	if _, err := os.Stat(otherTestDead); err != nil {
		t.Fatalf("short-grace reaper MUST NOT touch another test tag's dir: %v", err)
	}
}

func TestOpenGraceOwnerBlockingFlockNoStartupRace(t *testing.T) {
	root := t.TempDir()
	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatalf("openGraceOwnerInRoot: %v", err)
	}
	defer func() { _ = unix.Close(owner.lockFD) }()

	if !strings.HasPrefix(owner.self, GraceDirPrefix) {
		t.Fatalf("owner.self = %q, want prefix %q", owner.self, GraceDirPrefix)
	}
	if strings.HasPrefix(owner.self, "inst-") {
		t.Fatalf("owner.self = %q must NOT start with inst-", owner.self)
	}
	if GraceRootSubdir == "keploy" {
		t.Fatalf("GraceRootSubdir must be outside keploy/ (selfupdate.WipeBPFPins wipes keploy/)")
	}

	fd, err := unix.Open(owner.dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err == nil {
		t.Fatal("expected EWOULDBLOCK on published grace dir; flock was not held")
	}
}

func TestSharedBoottimeStampAllowsCrossProcessReapOfCrashLoopsAndShortLanes(t *testing.T) {
	root := t.TempDir()
	deadDir := filepath.Join(root, GraceDirPrefix+"dead-shortlane")
	liveDir := filepath.Join(root, GraceDirPrefix+"live-sibling")
	selfDir := filepath.Join(root, GraceDirPrefix+"self")
	for _, d := range []string{deadDir, liveDir, selfDir} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "000001-proxyless-tcp_sendmsg-p"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	liveFD, err := unix.Open(liveDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(liveFD) }()
	if err := unix.Flock(liveFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		t.Fatal(err)
	}

	now := uint64(1000 * time.Second)
	newShortProcessReaper := func() *graceReaper {
		r := newGraceReaper(root, GraceDirPrefix+"self", 15*time.Minute, 3*time.Minute, 1*time.Minute, zap.NewNop())
		r.bootNowNs = func() uint64 { return now }
		return r
	}

	dirs, progs := newShortProcessReaper().round()
	if dirs != 3 || progs != 3 {
		t.Fatalf("proc1: dirs=%d progs=%d, want 3/3", dirs, progs)
	}

	now += uint64(14 * time.Minute)
	newShortProcessReaper().round()
	if _, err := os.Stat(deadDir); err != nil {
		t.Fatalf("deadDir reaped early at 14m: %v", err)
	}

	now += uint64(1*time.Minute + time.Second)
	newShortProcessReaper().round()
	if _, err := os.Stat(deadDir); err != nil {
		t.Fatalf("deadDir reaped on first confirm pass: %v", err)
	}

	now += uint64(30 * time.Second)
	newShortProcessReaper().round()
	if _, err := os.Stat(deadDir); err != nil {
		t.Fatalf("deadDir reaped before confirmGap: %v", err)
	}

	now += uint64(31 * time.Second)
	dirs, progs = newShortProcessReaper().round()
	if _, err := os.Stat(deadDir); !os.IsNotExist(err) {
		t.Fatalf("deadDir was NOT reaped by fresh process after 15m + 1m confirm: err=%v", err)
	}
	if _, err := os.Stat(liveDir); err != nil {
		t.Fatalf("liveDir was wrongly reaped despite held flock: %v", err)
	}
	if dirs != 2 || progs != 2 {
		t.Fatalf("proc5 after reap: dirs=%d progs=%d, want 2/2", dirs, progs)
	}
}

func TestGracefulShutdown_HostBPFFSExitsImmediatelyAndLeavesPinForReaper(t *testing.T) {
	resetStateForTest(t)
	root := t.TempDir()
	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	reaperCancelled := false
	owner.reaperCancel = func() { reaperCancelled = true }
	stateMu.Lock()
	activeOwner = owner
	stateMu.Unlock()

	detached := false
	RegisterDetachHook(func() {
		if _, statErr := os.Stat(owner.dir); statErr != nil {
			t.Errorf("detach hook saw missing owner.dir: %v", statErr)
		}
		// Attach gate must ALREADY be closed when detach hooks run.
		var dummy ebpf.Program
		if perr := PinBeforeAttach(&dummy, "proxyless", "late", "p"); !errors.Is(perr, ErrShuttingDown) {
			t.Errorf("PinBeforeAttach during detach hook = %v, want ErrShuttingDown", perr)
		}
		detached = true
	})

	start := time.Now()
	GracefulShutdown(context.Background(), zap.NewNop(), DefaultInProcessHoldWait)
	elapsed := time.Since(start)
	if elapsed > 500*time.Millisecond {
		t.Fatalf("GracefulShutdown with host bpffs blocked exit for %v; must return immediately", elapsed)
	}
	if !detached {
		t.Fatal("GracefulShutdown did not invoke registered detach hook")
	}
	if !reaperCancelled {
		t.Fatal("GracefulShutdown did not cancel background reaper")
	}
	if Armed() {
		t.Fatal("Armed() = true after GracefulShutdown; want false")
	}
	// Host-bpffs pins MUST stay intact for the >= 15m cross-process reaper, and
	// owner.lockFD MUST stay open until process exit so flock releases only
	// once the process is truly gone.
	if _, err := os.Stat(owner.dir); err != nil {
		t.Fatalf("GracefulShutdown prematurely deleted grace dir %s: %v", owner.dir, err)
	}
	fd, err := unix.Open(owner.dir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(fd) }()
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err == nil {
		t.Fatal("owner.lockFD was closed early while process is still alive; expected flock held until exit")
	}
}

func TestLateRegisterDetachHookRunsSynchronouslyAfterShutdown(t *testing.T) {
	resetStateForTest(t)
	GracefulShutdown(context.Background(), zap.NewNop(), 0)

	ran := false
	RegisterDetachHook(func() { ran = true })
	if !ran {
		t.Fatal("late RegisterDetachHook after GracefulShutdown did not run synchronously")
	}
}

func TestInitAfterGracefulShutdownRefusesToReopenGate(t *testing.T) {
	resetStateForTest(t)
	GracefulShutdown(context.Background(), zap.NewNop(), 0)

	// Production Init MUST NOT reopen the attach gate once GracefulShutdown ran.
	if err := Init(context.Background(), zap.NewNop(), ModeNative); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("Init after GracefulShutdown = %v, want ErrShuttingDown", err)
	}
	var dummy ebpf.Program
	if err := PinBeforeAttach(&dummy, "proxyless", "late", "p"); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("PinBeforeAttach after failed post-shutdown Init = %v, want ErrShuttingDown", err)
	}

	// Only explicit test-only re-arm helper may clear the gate.
	ResetShutdownForTest(t)
	SetBypassForTest(t)
	if err := PinBeforeAttach(&dummy, "proxyless", "rearmed", "p"); err != nil {
		t.Fatalf("PinBeforeAttach after ResetShutdownForTest = %v, want nil", err)
	}
}

func TestValidateHostBPFFSPath_RejectsNonBPFFSAndOverlay(t *testing.T) {
	resetStateForTest(t)

	// Non-bpffs temp directory rejected in both modes.
	d := t.TempDir()
	if err := ValidateHostBPFFSPath(d, false); err == nil || !strings.Contains(err.Error(), "not a bpffs filesystem") {
		t.Fatalf("ValidateHostBPFFSPath(tmpfs, false) = %v, want not-a-bpffs error", err)
	}

	// If /sys/fs/bpf is a real bpffs on this host, prove RecordPrivateOverlayDev
	// AND EnvHostBPFFSDev mismatch both reject it!
	if dev, ok := bpffsDevice(DefaultNativeBPFFSPath); ok {
		RecordPrivateOverlayDev(dev)
		if err := ValidateHostBPFFSPath(DefaultNativeBPFFSPath, false); err == nil || !strings.Contains(err.Error(), "private bpffs overlay") {
			t.Fatalf("ValidateHostBPFFSPath(recorded overlay, native) = %v, want overlay rejection", err)
		}
		resetStateForTest(t)
		t.Setenv(EnvHostBPFFSDev, strconv.FormatUint(dev+99999, 10))
		if err := ValidateHostBPFFSPath(DefaultNativeBPFFSPath, true); err == nil || !strings.Contains(err.Error(), "not the supervisor-recorded host bpffs device") {
			t.Fatalf("ValidateHostBPFFSPath(mismatched dev) = %v, want device mismatch error", err)
		}
	}
}

func TestInitSerializedConcurrentCalls(t *testing.T) {
	resetStateForTest(t)
	notBPFFS := t.TempDir()
	t.Setenv(EnvHostBPFFS, notBPFFS)
	t.Setenv(EnvAllowUnprotectedTrampolines, "")

	var wg sync.WaitGroup
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_ = Init(context.Background(), zap.NewNop(), ModeNative)
		}()
	}
	wg.Wait()
}

func TestHasMountInfoBPFFS(t *testing.T) {
	mi := filepath.Join(t.TempDir(), "mountinfo")
	content := strings.Join([]string{
		"120 88 0:42 / /sys/fs/bpf rw,nosuid,nodev,noexec,relatime - bpf bpf rw",
		"121 88 0:41 / /run/keploy/host-bpffs rw,nosuid,nodev,noexec,relatime shared:12 - bpf bpf rw",
		"122 88 0:41 /keploy /run/keploy/subpath-bpffs rw,nosuid,nodev,noexec,relatime shared:12 - bpf bpf rw",
	}, "\n") + "\n"
	if err := os.WriteFile(mi, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	ok, err := hasMountInfoBPFFS(mi, DefaultHostBPFFSPath)
	if err != nil || !ok {
		t.Fatalf("hasMountInfoBPFFS(host-bpffs) = (%v, %v), want (true, nil)", ok, err)
	}
	ok, err = hasMountInfoBPFFS(mi, "/run/keploy/subpath-bpffs")
	if err != nil || ok {
		t.Fatalf("hasMountInfoBPFFS(subpath /keploy bind) = (%v, %v), want (false, nil)", ok, err)
	}
	ok, err = hasMountInfoBPFFS(mi, "/run/keploy/missing")
	if err != nil || ok {
		t.Fatalf("hasMountInfoBPFFS(missing) = (%v, %v), want (false, nil)", ok, err)
	}
}

func TestDetachedHolderClosesInheritedNonCLOEXECPipeImmediately(t *testing.T) {
	runCase := func(t *testing.T, count int, passInExtraFiles bool) {
		t.Helper()
		r, w, err := os.Pipe()
		if err != nil {
			t.Fatalf("os.Pipe: %v", err)
		}
		defer func() { _ = r.Close() }()

		cmd := exec.Command("/proc/self/exe")
		cmd.Args = []string{DetachedHolderComm}
		cmd.Env = []string{
			fmt.Sprintf("%s=%d", envDetachedHolderMS, 60000),
			fmt.Sprintf("%s=%d", envDetachedHolderCount, count),
		}
		cmd.Stdin = nil
		cmd.Stdout = nil
		cmd.Stderr = nil
		cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

		if passInExtraFiles {
			cmd.ExtraFiles = []*os.File{w}
		} else {
			// Clear FD_CLOEXEC so w is genuinely inherited across exec at an FD >= 3+count.
			if _, ferr := unix.FcntlInt(w.Fd(), unix.F_SETFD, 0); ferr != nil {
				_ = w.Close()
				t.Fatalf("clear FD_CLOEXEC: %v", ferr)
			}
		}

		if err := cmd.Start(); err != nil {
			_ = w.Close()
			t.Fatalf("start holder: %v", err)
		}
		// Close parent's write end immediately; if the holder fails to close
		// its inherited copy, reading r will block for 60s instead of returning EOF.
		_ = w.Close()
		t.Cleanup(func() {
			_ = cmd.Process.Kill()
			_ = cmd.Wait()
		})

		eofCh := make(chan error, 1)
		go func() {
			var buf [1]byte
			_, rerr := r.Read(buf[:])
			eofCh <- rerr
		}()

		select {
		case rerr := <-eofCh:
			if !errors.Is(rerr, io.EOF) {
				t.Fatalf("expected io.EOF when holder closes inherited pipe, got %v", rerr)
			}
		case <-time.After(3 * time.Second):
			t.Fatalf("timed out waiting for EOF: detached holder kept inherited non-BPF pipe open (count=%d, extraFiles=%v)", count, passInExtraFiles)
		}

		// Verify /proc/<pid>/comm was renamed away from "keploy" to DetachedHolderComm.
		commPath := fmt.Sprintf("/proc/%d/comm", cmd.Process.Pid)
		deadline := time.Now().Add(2 * time.Second)
		var comm string
		for time.Now().Before(deadline) {
			if b, cerr := os.ReadFile(commPath); cerr == nil {
				comm = strings.TrimSpace(string(b))
				if comm == DetachedHolderComm {
					break
				}
			}
			time.Sleep(10 * time.Millisecond)
		}
		if comm != DetachedHolderComm {
			t.Fatalf("holder /proc/%d/comm = %q, want %q", cmd.Process.Pid, comm, DetachedHolderComm)
		}
	}

	t.Run("inherited_non_cloexec_pipe_closed_by_close_range", func(t *testing.T) {
		runCase(t, 0, false)
	})
	t.Run("non_bpf_fd_inside_count_slot_closed_immediately", func(t *testing.T) {
		runCase(t, 1, true)
	})
}
