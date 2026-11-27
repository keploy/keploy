//go:build linux

package bpfsafety

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cilium/ebpf"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"
)

type nopCloser struct{}

func (nopCloser) Close() error { return nil }

func resetStateForTest(t *testing.T) {
	t.Helper()
	stateMu.Lock()
	prevOwner := activeOwner
	prevObs := metricsObserver
	prevDisarms := disarmHooks
	prevHooks := detachHooks
	prevLockFDs := unpinnedLockFDs
	prevRetained := retainedAnchorLinks
	prevOverlays := make(map[uint64]bool, len(privateOverlayDevs))
	for k, v := range privateOverlayDevs {
		prevOverlays[k] = v
	}
	prevUnprotLogger := unprotectedLogger
	prevWarnedFuncs := unprotectedWarnedFuncs
	activeOwner = nil
	metricsObserver = nil
	disarmHooks = nil
	detachHooks = nil
	unpinnedLockFDs = nil
	retainedAnchorLinks = nil
	privateOverlayDevs = map[uint64]bool{}
	unprotectedLogger = nil
	unprotectedWarnedFuncs = map[string]bool{}
	stateMu.Unlock()
	prevUnprot := unprotectedTrampolines.Swap(false)
	prevOptOut := explicitUnprotectedOptOut.Swap(false)
	prevWarned := unprotectedWarned.Swap(false)
	prevStale := staleTrampolineDetected.Swap(false)
	prevShuttingDown := shuttingDown.Swap(false)
	prevBypass := testBypassPin.Swap(false)
	prevDynFn := dynamicTrampImageCheckFn
	prevValFn := validateHostBPFFSPathFn
	prevOpenFn := openGraceOwnerInRootFn
	prevInContainerFn := runningInContainerFn
	prevSharedFn := hasHostSharedBPFFSFn
	prevStatfsFn := nativeBPFFSStatfsFn
	prevMkdirFn := nativeBPFFSMkdirAllFn
	prevMountFn := nativeBPFFSMountFn
	t.Cleanup(func() {
		stateMu.Lock()
		for _, fd := range unpinnedLockFDs {
			_ = unix.Close(fd)
		}
		activeOwner = prevOwner
		metricsObserver = prevObs
		disarmHooks = prevDisarms
		detachHooks = prevHooks
		unpinnedLockFDs = prevLockFDs
		retainedAnchorLinks = prevRetained
		privateOverlayDevs = prevOverlays
		unprotectedLogger = prevUnprotLogger
		unprotectedWarnedFuncs = prevWarnedFuncs
		stateMu.Unlock()
		unprotectedTrampolines.Store(prevUnprot)
		explicitUnprotectedOptOut.Store(prevOptOut)
		unprotectedWarned.Store(prevWarned)
		staleTrampolineDetected.Store(prevStale)
		shuttingDown.Store(prevShuttingDown)
		testBypassPin.Store(prevBypass)
		dynamicTrampImageCheckFn = prevDynFn
		validateHostBPFFSPathFn = prevValFn
		openGraceOwnerInRootFn = prevOpenFn
		runningInContainerFn = prevInContainerFn
		hasHostSharedBPFFSFn = prevSharedFn
		nativeBPFFSStatfsFn = prevStatfsFn
		nativeBPFFSMkdirAllFn = prevMkdirFn
		nativeBPFFSMountFn = prevMountFn
	})
}

func TestTrampolineKeyAndKallsymsParsing(t *testing.T) {
	// Matches Debian 6.12.117 unix_dgram_sendmsg (btf_obj_id=1, btf_id=104305).
	if got := trampolineKey(1, 104305); got != 6442555249 {
		t.Fatalf("trampolineKey(1, 104305) = %d, want 6442555249", got)
	}
	if got := trampolineKey(0, 104305); got != 6442555249 {
		t.Fatalf("trampolineKey(0, 104305) = %d, want 6442555249 (default vmlinux obj 1)", got)
	}

	ks := filepath.Join(t.TempDir(), "kallsyms")
	content := strings.Join([]string{
		"ffffffff81000000 T _text",
		"ffffffffc031a300 t bpf_trampoline_6442555249\t[bpf]",
		"ffffffffc031b400 t bpf_trampoline_6442555249\t[bpf]",
		"ffffffffb5ecb260 t bpf_trampoline_lookup",
	}, "\n") + "\n"
	if err := os.WriteFile(ks, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	tramps, vis, err := parseKallsymsTrampolines(ks)
	if err != nil || !vis {
		t.Fatalf("parseKallsymsTrampolines = (%v, %v, %v), want visible", tramps, vis, err)
	}
	addrs := tramps[6442555249]
	if len(addrs) != 2 || addrs[0] != 0xffffffffc031a300 || addrs[1] != 0xffffffffc031b400 {
		t.Fatalf("tramps[6442555249] = %x, want 2 addresses", addrs)
	}

	// Linux 5.12..6.0 generation-suffixed trampoline symbols (bpf_trampoline_<key>_<gen>),
	// including same-page address reuse across generations (observed on Linux 5.19).
	ksGen0 := filepath.Join(t.TempDir(), "kallsyms-gen0")
	if err := os.WriteFile(ksGen0, []byte("ffffffffc03ac000 t bpf_trampoline_6442572795_0\t[bpf]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	ksGen2 := filepath.Join(t.TempDir(), "kallsyms-gen2")
	if err := os.WriteFile(ksGen2, []byte("ffffffffc03ac000 t bpf_trampoline_6442572795_2\t[bpf]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	t0, vis0, err := parseKallsymsTrampolines(ksGen0)
	if err != nil || !vis0 || len(t0[6442572795]) != 1 {
		t.Fatalf("parseKallsymsTrampolines(gen0) = (%v, %v, %v), want 1 entry", t0, vis0, err)
	}
	t2, vis2, err := parseKallsymsTrampolines(ksGen2)
	if err != nil || !vis2 || len(t2[6442572795]) != 1 {
		t.Fatalf("parseKallsymsTrampolines(gen2) = (%v, %v, %v), want 1 entry", t2, vis2, err)
	}
	snap0 := snapshotKeysFromKallsyms(t0, map[uint64]struct{}{6442572795: {}}, nil)
	if got := survivingSnapshotKeys(snap0, t0); len(got) != 1 {
		t.Fatalf("survivingSnapshotKeys(gen0 vs gen0) = %d, want 1", len(got))
	}
	if got := survivingSnapshotKeys(snap0, t2); len(got) != 0 {
		t.Fatalf("same-address gen2 MUST NOT match gen0 snapshot (address reuse on 5.19), got %d surviving", len(got))
	}

	// kptr_restrict zeroed addresses MUST report visible=false.
	ksZero := filepath.Join(t.TempDir(), "kallsyms-zero")
	if err := os.WriteFile(ksZero, []byte("0000000000000000 t bpf_trampoline_6442555249\t[bpf]\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, visZero, err := parseKallsymsTrampolines(ksZero)
	if err != nil || visZero {
		t.Fatalf("zeroed kallsyms must return visible=false, got vis=%v err=%v", visZero, err)
	}
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

	shuttingDown.Store(true)
	if err := PinBeforeAttach(&dummy, "proxyless", "keploy_tcp_sendmsg", "p"); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("PinBeforeAttach after shutdown gate = %v, want ErrShuttingDown", err)
	}
}

func TestInitWithoutHostBPFFSEntersHonestUnprotectedModeOnce(t *testing.T) {
	resetStateForTest(t)
	notBPFFS := t.TempDir()
	t.Setenv(EnvHostBPFFS, notBPFFS)
	t.Setenv(EnvAllowUnprotectedTrampolines, "")

	core, logs := observer.New(zapcore.WarnLevel)
	var obsUnprot bool
	var obsCalls int
	SetMetricsObserver(func(_, _, _ int, unprot, _ bool) {
		obsUnprot = unprot
		obsCalls++
	})

	for _, mode := range []Mode{ModeDaemonSet, ModeK8sSidecar, ModeCompose, ModeDocker, ModeNative} {
		if err := Init(context.Background(), zap.New(core), mode); err != nil {
			t.Fatalf("mode %s: Init without host bpffs returned error %v, want nil (honest unprotected mode)", mode, err)
		}
	}
	if !UnprotectedTrampolinesActive() || !obsUnprot || obsCalls == 0 {
		t.Fatalf("UnprotectedTrampolinesActive=%v obsUnprot=%v obsCalls=%d, want true/true/>0", UnprotectedTrampolinesActive(), obsUnprot, obsCalls)
	}
	if got := ActiveGraceMode(); got != GraceModeUnprotected {
		t.Fatalf("ActiveGraceMode() = %q, want %q", got, GraceModeUnprotected)
	}
	if Armed() {
		t.Fatal("Armed() = true without host bpffs, want false")
	}
	if logs.Len() != 1 {
		t.Fatalf("expected exactly 1 WARN across repeated Init calls, got %d: %+v", logs.Len(), logs.All())
	}
	msg := logs.All()[0].Message
	if msg != UnprotectedWarnMessage {
		t.Fatalf("WARN message = %q, want %q", msg, UnprotectedWarnMessage)
	}
	if strings.Contains(msg, EnvAllowUnprotectedTrampolines) {
		t.Fatalf("WARN message must not mention %s: %q", EnvAllowUnprotectedTrampolines, msg)
	}

	var dummy ebpf.Program
	// Dynamic-trampoline kernel: honest unprotected mode permits PinBeforeAttach.
	dynamicTrampImageCheckFn = func() bool { return true }
	if err := PinBeforeAttach(&dummy, "proxyless", "keploy_tcp_sendmsg", "p"); err != nil {
		t.Fatalf("PinBeforeAttach under honest unprotected mode on dynamic kernel returned %v, want nil", err)
	}

	// Static-trampoline kernel: Init-entered unprotected mode MUST STILL fail
	// closed with ErrAnchorUnpinned (and log one refusal WARN per function)
	// unless KEPLOY_ALLOW_UNPROTECTED_TRAMPOLINES=1 was explicitly set.
	dynamicTrampImageCheckFn = func() bool { return false }
	for i := 0; i < 3; i++ {
		err := PinBeforeAttach(&dummy, "proxyless", "unix_stream_recvmsg", "p")
		if !errors.Is(err, ErrAnchorUnpinned) {
			t.Fatalf("attempt %d: PinBeforeAttach under Init unprotected mode on static kernel = %v, want ErrAnchorUnpinned", i, err)
		}
	}
	if logs.Len() != 2 {
		t.Fatalf("expected 1 init WARN + 1 per-function static refusal WARN across 3 attempts, got %d: %+v", logs.Len(), logs.All())
	}

	// Explicit KEPLOY_ALLOW_UNPROTECTED_TRAMPOLINES=1 break-glass opt-out still overrides.
	ResetUnprotectedForTest(t)
	t.Setenv(EnvAllowUnprotectedTrampolines, "1")
	optCore, optLogs := observer.New(zapcore.WarnLevel)
	if err := Init(context.Background(), zap.New(optCore), ModeNative); err != nil {
		t.Fatalf("Init with explicit opt-out returned %v, want nil", err)
	}
	if optLogs.Len() != 1 || optLogs.All()[0].Message != UnprotectedOptOutWarnMessage {
		t.Fatalf("explicit opt-out WARN = %+v, want %q", optLogs.All(), UnprotectedOptOutWarnMessage)
	}
	if err := PinBeforeAttach(&dummy, "proxyless", "unix_stream_recvmsg", "p"); err != nil {
		t.Fatalf("PinBeforeAttach with explicit %s=1 on static kernel = %v, want nil", EnvAllowUnprotectedTrampolines, err)
	}
}

func TestInitRetriesTransientMountCheckAndGraceSetupErrors(t *testing.T) {
	t.Run("transient_mount_check_enomem_recovers_on_retry", func(t *testing.T) {
		resetStateForTest(t)
		fakeMount := t.TempDir()
		t.Setenv(EnvHostBPFFS, fakeMount)
		t.Setenv(EnvAllowUnprotectedTrampolines, "")

		var valCalls int
		validateHostBPFFSPathFn = func(path string, _ bool) error {
			valCalls++
			if valCalls == 1 {
				return fmt.Errorf("statfs %s: %w", path, unix.ENOMEM)
			}
			return nil
		}
		openGraceOwnerInRootFn = openGraceOwnerInRoot

		core, logs := observer.New(zapcore.WarnLevel)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := Init(ctx, zap.New(core), ModeDaemonSet); err != nil {
			t.Fatalf("Init = %v, want nil", err)
		}
		defer func() {
			stateMu.Lock()
			o := activeOwner
			activeOwner = nil
			stateMu.Unlock()
			if o != nil {
				if o.reaper != nil {
					o.reaper.stopAndWait(o.reaperCancel)
				}
				_ = unix.Close(o.lockFD)
			}
		}()

		if valCalls != 2 {
			t.Fatalf("validateHostBPFFSPathFn calls = %d, want 2 (1 retry on ENOMEM)", valCalls)
		}
		if !Armed() || UnprotectedTrampolinesActive() {
			t.Fatalf("expected Armed=true Unprotected=false after mount-check retry, got armed=%v unprot=%v", Armed(), UnprotectedTrampolinesActive())
		}
		if logs.Len() != 0 {
			t.Fatalf("expected 0 WARNs after transient retry recovery, got %+v", logs.All())
		}
	})

	t.Run("persistent_mount_check_enomem_logs_system_error_not_mount_hint", func(t *testing.T) {
		resetStateForTest(t)
		fakeMount := t.TempDir()
		t.Setenv(EnvHostBPFFS, fakeMount)
		t.Setenv(EnvAllowUnprotectedTrampolines, "")

		var valCalls int
		validateHostBPFFSPathFn = func(path string, _ bool) error {
			valCalls++
			return fmt.Errorf("statfs %s: %w", path, unix.ENOMEM)
		}

		core, logs := observer.New(zapcore.WarnLevel)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := Init(ctx, zap.New(core), ModeDaemonSet); err != nil {
			t.Fatalf("Init = %v, want nil", err)
		}
		if valCalls != 2 {
			t.Fatalf("validateHostBPFFSPathFn calls = %d, want 2 (initial + 1 retry)", valCalls)
		}
		if Armed() || !UnprotectedTrampolinesActive() {
			t.Fatalf("expected unprotected mode, got armed=%v unprot=%v", Armed(), UnprotectedTrampolinesActive())
		}
		if logs.Len() != 1 || logs.All()[0].Message != UnprotectedMountCheckErrWarnMessage {
			t.Fatalf("WARN = %+v, want %q", logs.All(), UnprotectedMountCheckErrWarnMessage)
		}
		if strings.Contains(logs.All()[0].Message, "mount the host's /sys/fs/bpf") {
			t.Fatalf("persistent mount-check ENOMEM logged misleading mount hint: %q", logs.All()[0].Message)
		}

		dynamicTrampImageCheckFn = func() bool { return false }
		var dummy ebpf.Program
		if err := PinBeforeAttach(&dummy, "proxyless", "unix_stream_recvmsg", "p"); !errors.Is(err, ErrAnchorUnpinned) {
			t.Fatalf("static kernel PinBeforeAttach after persistent mount-check ENOMEM = %v, want ErrAnchorUnpinned", err)
		}
	})

	t.Run("config_absent_mount_error_does_not_retry_and_logs_mount_hint", func(t *testing.T) {
		resetStateForTest(t)
		t.Setenv(EnvHostBPFFS, "/nonexistent/keploy-bpffs-test")
		t.Setenv(EnvAllowUnprotectedTrampolines, "")

		var valCalls int
		validateHostBPFFSPathFn = func(path string, _ bool) error {
			valCalls++
			return fmt.Errorf("statfs %s: %w", path, unix.ENOENT)
		}

		core, logs := observer.New(zapcore.WarnLevel)
		if err := Init(context.Background(), zap.New(core), ModeDaemonSet); err != nil {
			t.Fatalf("Init = %v, want nil", err)
		}
		if valCalls != 1 {
			t.Fatalf("ENOENT should not retry mount check; calls = %d, want 1", valCalls)
		}
		if logs.Len() != 1 || logs.All()[0].Message != UnprotectedWarnMessage {
			t.Fatalf("WARN = %+v, want %q", logs.All(), UnprotectedWarnMessage)
		}
	})

	t.Run("transient_grace_dir_init_error_recovers_on_retry", func(t *testing.T) {
		resetStateForTest(t)
		fakeMount := t.TempDir()
		t.Setenv(EnvHostBPFFS, fakeMount)
		t.Setenv(EnvAllowUnprotectedTrampolines, "")

		validateHostBPFFSPathFn = func(string, bool) error { return nil }
		var openCalls int
		openGraceOwnerInRootFn = func(root string, logger *zap.Logger) (*graceOwner, error) {
			openCalls++
			if openCalls == 1 {
				return nil, fmt.Errorf("statfs grace root %s: %w", root, unix.ENOMEM)
			}
			return openGraceOwnerInRoot(root, logger)
		}

		core, logs := observer.New(zapcore.WarnLevel)
		ctx, cancel := context.WithCancel(context.Background())
		defer cancel()
		if err := Init(ctx, zap.New(core), ModeDaemonSet); err != nil {
			t.Fatalf("Init = %v, want nil", err)
		}
		defer func() {
			stateMu.Lock()
			o := activeOwner
			activeOwner = nil
			stateMu.Unlock()
			if o != nil {
				if o.reaper != nil {
					o.reaper.stopAndWait(o.reaperCancel)
				}
				_ = unix.Close(o.lockFD)
			}
		}()

		if openCalls != 2 {
			t.Fatalf("openGraceOwnerInRootFn calls = %d, want 2 (kills M54)", openCalls)
		}
		if !Armed() || UnprotectedTrampolinesActive() {
			t.Fatalf("expected Armed=true Unprotected=false after grace-dir retry, got armed=%v unprot=%v", Armed(), UnprotectedTrampolinesActive())
		}
		if logs.Len() != 0 {
			t.Fatalf("expected 0 WARNs after grace-dir retry recovery, got %+v", logs.All())
		}
	})

	t.Run("persistent_grace_dir_init_error_logs_init_err_warn", func(t *testing.T) {
		resetStateForTest(t)
		fakeMount := t.TempDir()
		t.Setenv(EnvHostBPFFS, fakeMount)
		t.Setenv(EnvAllowUnprotectedTrampolines, "")

		validateHostBPFFSPathFn = func(string, bool) error { return nil }
		var openCalls int
		openGraceOwnerInRootFn = func(root string, _ *zap.Logger) (*graceOwner, error) {
			openCalls++
			return nil, fmt.Errorf("statfs grace root %s: %w", root, unix.ENOMEM)
		}

		core, logs := observer.New(zapcore.WarnLevel)
		if err := Init(context.Background(), zap.New(core), ModeDaemonSet); err != nil {
			t.Fatalf("Init = %v, want nil", err)
		}
		if openCalls != 2 {
			t.Fatalf("openGraceOwnerInRootFn calls = %d, want 2", openCalls)
		}
		if Armed() || !UnprotectedTrampolinesActive() {
			t.Fatalf("expected unprotected mode, got armed=%v unprot=%v", Armed(), UnprotectedTrampolinesActive())
		}
		if logs.Len() != 1 || logs.All()[0].Message != UnprotectedInitErrWarnMessage {
			t.Fatalf("WARN = %+v, want %q", logs.All(), UnprotectedInitErrWarnMessage)
		}
	})
}

func TestMountCheckErrorClassificationAndOpenGraceOwner(t *testing.T) {
	t.Run("errno_classification_and_openGraceOwner_table", func(t *testing.T) {
		cases := []struct {
			name      string
			err       error
			wantSys   bool
			wantPerm  bool
			wantCalls int
			wantWarn  string
		}{
			{"ENOMEM", fmt.Errorf("statfs /run/keploy/host-bpffs: %w", unix.ENOMEM), true, false, 2, UnprotectedMountCheckErrWarnMessage},
			{"EINTR", fmt.Errorf("statfs /run/keploy/host-bpffs: %w", unix.EINTR), true, false, 2, UnprotectedMountCheckErrWarnMessage},
			{"EIO", fmt.Errorf("statfs /run/keploy/host-bpffs: %w", unix.EIO), true, false, 2, UnprotectedMountCheckErrWarnMessage},
			{"EAGAIN", fmt.Errorf("statfs /run/keploy/host-bpffs: %w", unix.EAGAIN), true, false, 2, UnprotectedMountCheckErrWarnMessage},
			{"ENOSYS", fmt.Errorf("statfs /run/keploy/host-bpffs: %w", unix.ENOSYS), true, false, 2, UnprotectedMountCheckErrWarnMessage},
			{"EOVERFLOW", fmt.Errorf("statfs /run/keploy/host-bpffs: %w", unix.EOVERFLOW), true, false, 2, UnprotectedMountCheckErrWarnMessage},
			{"ESTALE", fmt.Errorf("statfs /run/keploy/host-bpffs: %w", unix.ESTALE), true, false, 2, UnprotectedMountCheckErrWarnMessage},
			{"ENODEV", fmt.Errorf("statfs /run/keploy/host-bpffs: %w", unix.ENODEV), true, false, 2, UnprotectedMountCheckErrWarnMessage},
			{"EINVAL", fmt.Errorf("statfs /run/keploy/host-bpffs: %w", unix.EINVAL), true, false, 2, UnprotectedMountCheckErrWarnMessage},
			{"ENOENT", fmt.Errorf("statfs /run/keploy/host-bpffs: %w", unix.ENOENT), false, false, 1, UnprotectedWarnMessage},
			{"ENOTDIR", fmt.Errorf("statfs /run/keploy/host-bpffs: %w", unix.ENOTDIR), false, false, 1, UnprotectedWarnMessage},
			{"EACCES", fmt.Errorf("statfs /run/keploy/host-bpffs: %w", unix.EACCES), false, true, 1, unprotectedPermWarnMessage(DefaultHostBPFFSPath)},
			{"EPERM", fmt.Errorf("statfs /run/keploy/host-bpffs: %w", unix.EPERM), false, true, 1, unprotectedPermWarnMessage(DefaultHostBPFFSPath)},
			{"ELOOP", fmt.Errorf("statfs /run/keploy/host-bpffs: %w", unix.ELOOP), false, false, 1, UnprotectedWarnMessage},
			{"ENAMETOOLONG", fmt.Errorf("statfs /run/keploy/host-bpffs: %w", unix.ENAMETOOLONG), false, false, 1, UnprotectedWarnMessage},
			{"non_bpffs", errors.New("/run/keploy/host-bpffs is not a bpffs filesystem"), false, false, 1, UnprotectedWarnMessage},
			{"read_only", errors.New("/run/keploy/host-bpffs is mounted read-only"), false, false, 1, UnprotectedWarnMessage},
			{"not_host_shared", errors.New("/sys/fs/bpf inside container is not a host-shared bpffs mount"), false, false, 1, UnprotectedWarnMessage},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				resetStateForTest(t)
				t.Setenv(EnvHostBPFFS, "")
				t.Setenv(EnvAllowUnprotectedTrampolines, "")

				if got := isMountCheckSysErr(tc.err); got != tc.wantSys {
					t.Fatalf("isMountCheckSysErr(%v) = %v, want %v", tc.err, got, tc.wantSys)
				}
				if got := isMountCheckPermErr(tc.err); got != tc.wantPerm {
					t.Fatalf("isMountCheckPermErr(%v) = %v, want %v", tc.err, got, tc.wantPerm)
				}

				var calls int
				validateHostBPFFSPathFn = func(string, bool) error {
					calls++
					return tc.err
				}
				owner, warnMsg, err := openGraceOwner(zap.NewNop(), ModeDaemonSet)
				if owner != nil || err == nil {
					t.Fatalf("openGraceOwner = (%v, %q, %v), want nil owner + error", owner, warnMsg, err)
				}
				if calls != tc.wantCalls {
					t.Fatalf("validate calls = %d, want %d", calls, tc.wantCalls)
				}
				if warnMsg != tc.wantWarn {
					t.Fatalf("warnMsg = %q, want %q", warnMsg, tc.wantWarn)
				}
			})
		}
	})

	t.Run("container_native_branches", func(t *testing.T) {
		enomem := func(p string) error { return fmt.Errorf("statfs %s: %w", p, unix.ENOMEM) }
		enoent := func(p string) error { return fmt.Errorf("statfs %s: %w", p, unix.ENOENT) }
		eacces := func(p string) error { return fmt.Errorf("statfs %s: %w", p, unix.EACCES) }
		notBPF := func(p string) error {
			return fmt.Errorf("%s is not a bpffs filesystem (type=0x62656572, want BPF_FS_MAGIC=0xcafe4a11)", p)
		}
		h, n := DefaultHostBPFFSPath, DefaultNativeBPFFSPath

		type sharedRes struct {
			ok  bool
			err error
		}
		cases := []struct {
			name        string
			hostErrs    []error
			nativeErrs  []error
			sharedSeq   []sharedRes
			wantArmed   bool
			wantRootSub string
			wantWarn    string
			wantHostN   int
		}{
			{
				name:        "transient_enomem_on_host_bpffs_detached_sysbpf",
				hostErrs:    []error{enomem(h), nil},
				nativeErrs:  []error{notBPF(n)},
				wantArmed:   true,
				wantRootSub: h,
				wantHostN:   2,
			},
			{
				name:        "transient_enomem_on_host_bpffs_private_sysbpf",
				hostErrs:    []error{enomem(h), nil},
				nativeErrs:  []error{nil},
				sharedSeq:   []sharedRes{{false, nil}},
				wantArmed:   true,
				wantRootSub: h,
				wantHostN:   2,
			},
			{
				name:       "persistent_enomem_on_host_bpffs_detached_sysbpf",
				hostErrs:   []error{enomem(h)},
				nativeErrs: []error{notBPF(n)},
				wantArmed:  false,
				wantWarn:   UnprotectedMountCheckErrWarnMessage,
				wantHostN:  2,
			},
			{
				name:       "persistent_enomem_on_host_bpffs_private_sysbpf",
				hostErrs:   []error{enomem(h)},
				nativeErrs: []error{nil},
				sharedSeq:  []sharedRes{{false, nil}},
				wantArmed:  false,
				wantWarn:   UnprotectedMountCheckErrWarnMessage,
				wantHostN:  2,
			},
			{
				name:       "eacces_on_host_bpffs_detached_sysbpf_no_retry",
				hostErrs:   []error{eacces(h)},
				nativeErrs: []error{notBPF(n)},
				wantArmed:  false,
				wantWarn:   unprotectedPermWarnMessage(h),
				wantHostN:  1,
			},
			{
				// Kills M87: attempt 1 fails with ENOMEM on /sys/fs/bpf (failedPath=n),
				// retry sees not-bpffs on /sys/fs/bpf and surfaces hostErr EACCES on
				// /run/keploy/host-bpffs — failedPath MUST refresh to h on retry.
				name:       "container_eacces_host_fallback_transient",
				hostErrs:   []error{eacces(h)},
				nativeErrs: []error{enomem(n), notBPF(n)},
				wantArmed:  false,
				wantWarn:   unprotectedPermWarnMessage(h),
				wantHostN:  2,
			},
			{
				name:        "transient_mountinfo_enomem_recovers_on_retry",
				hostErrs:    []error{enoent(h)},
				nativeErrs:  []error{nil},
				sharedSeq:   []sharedRes{{false, unix.ENOMEM}, {true, nil}},
				wantArmed:   true,
				wantRootSub: n,
				wantHostN:   2,
			},
			{
				name:       "persistent_mountinfo_eio_logs_syserr_not_mount_hint",
				hostErrs:   []error{enoent(h)},
				nativeErrs: []error{nil},
				sharedSeq:  []sharedRes{{false, unix.EIO}},
				wantArmed:  false,
				wantWarn:   UnprotectedMountCheckErrWarnMessage,
				wantHostN:  2,
			},
			{
				name:       "absent_everywhere_no_retry_mount_hint",
				hostErrs:   []error{enoent(h)},
				nativeErrs: []error{notBPF(n)},
				wantArmed:  false,
				wantWarn:   UnprotectedWarnMessage,
				wantHostN:  1,
			},
		}

		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				resetStateForTest(t)
				t.Setenv(EnvHostBPFFS, "")
				t.Setenv(EnvAllowUnprotectedTrampolines, "")
				runningInContainerFn = func() bool { return true }

				pickErr := func(errs []error, idx int) error {
					if len(errs) == 0 {
						return nil
					}
					if idx >= len(errs) {
						idx = len(errs) - 1
					}
					return errs[idx]
				}
				var hostN, nativeN, sharedN int
				validateHostBPFFSPathFn = func(path string, _ bool) error {
					switch path {
					case DefaultHostBPFFSPath:
						e := pickErr(tc.hostErrs, hostN)
						hostN++
						return e
					case DefaultNativeBPFFSPath:
						e := pickErr(tc.nativeErrs, nativeN)
						nativeN++
						return e
					default:
						return fmt.Errorf("unexpected validate path %s", path)
					}
				}
				hasHostSharedBPFFSFn = func(string, string) (bool, error) {
					if len(tc.sharedSeq) == 0 {
						return false, nil
					}
					idx := sharedN
					if idx >= len(tc.sharedSeq) {
						idx = len(tc.sharedSeq) - 1
					}
					sharedN++
					return tc.sharedSeq[idx].ok, tc.sharedSeq[idx].err
				}

				tmp := t.TempDir()
				var gotRoot string
				openGraceOwnerInRootFn = func(root string, lg *zap.Logger) (*graceOwner, error) {
					gotRoot = root
					return openGraceOwnerInRoot(filepath.Join(tmp, GraceRootSubdir), lg)
				}

				core, logs := observer.New(zapcore.WarnLevel)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if err := Init(ctx, zap.New(core), ModeNative); err != nil {
					t.Fatalf("Init = %v, want nil", err)
				}
				defer func() {
					stateMu.Lock()
					o := activeOwner
					activeOwner = nil
					stateMu.Unlock()
					if o != nil {
						if o.reaper != nil {
							o.reaper.stopAndWait(o.reaperCancel)
						}
						_ = unix.Close(o.lockFD)
					}
				}()

				if Armed() != tc.wantArmed {
					t.Fatalf("Armed = %v, want %v (logs=%+v)", Armed(), tc.wantArmed, logs.All())
				}
				if hostN != tc.wantHostN {
					t.Fatalf("DefaultHostBPFFSPath validate calls = %d, want %d", hostN, tc.wantHostN)
				}
				if tc.wantArmed {
					if wantRoot := filepath.Join(tc.wantRootSub, GraceRootSubdir); gotRoot != wantRoot {
						t.Fatalf("openGraceOwnerInRoot root = %q, want %q", gotRoot, wantRoot)
					}
					if logs.Len() != 0 {
						t.Fatalf("expected 0 WARNs when armed, got %+v", logs.All())
					}
				} else {
					if logs.Len() != 1 || logs.All()[0].Message != tc.wantWarn {
						t.Fatalf("WARN = %+v, want %q", logs.All(), tc.wantWarn)
					}
				}
			})
		}
	})

	t.Run("override_eacces_names_override_path", func(t *testing.T) {
		resetStateForTest(t)
		const customPath = "/custom/keploy/host-bpffs"
		t.Setenv(EnvHostBPFFS, customPath)
		t.Setenv(EnvAllowUnprotectedTrampolines, "")
		validateHostBPFFSPathFn = func(path string, _ bool) error {
			if path != customPath {
				return fmt.Errorf("unexpected path %s", path)
			}
			return fmt.Errorf("statfs %s: %w", path, unix.EACCES)
		}
		core, logs := observer.New(zapcore.WarnLevel)
		if err := Init(context.Background(), zap.New(core), ModeDaemonSet); err != nil {
			t.Fatalf("Init = %v, want nil", err)
		}
		if Armed() {
			t.Fatal("expected unprotected mode on override EACCES")
		}
		want := unprotectedPermWarnMessage(customPath)
		if logs.Len() != 1 || logs.All()[0].Message != want || !strings.Contains(logs.All()[0].Message, customPath) {
			t.Fatalf("WARN = %+v, want %q naming %q", logs.All(), want, customPath)
		}
	})

	t.Run("bare_native_host_bpffs_and_ensure_branches", func(t *testing.T) {
		enomem := func(p string) error { return fmt.Errorf("statfs %s: %w", p, unix.ENOMEM) }
		enoent := func(p string) error { return fmt.Errorf("statfs %s: %w", p, unix.ENOENT) }
		eacces := func(p string) error { return fmt.Errorf("statfs %s: %w", p, unix.EACCES) }
		notBPF := func(p string) error {
			return fmt.Errorf("%s is not a bpffs filesystem (type=0x62656572, want BPF_FS_MAGIC=0xcafe4a11)", p)
		}
		h, n := DefaultHostBPFFSPath, DefaultNativeBPFFSPath

		// M79: bare native, /run/keploy/host-bpffs transient ENOMEM once then OK,
		// /sys/fs/bpf not bpffs -> retries and ends ARMED on /run/keploy/host-bpffs.
		t.Run("m79_transient_host_enomem_recovers_armed", func(t *testing.T) {
			resetStateForTest(t)
			t.Setenv(EnvHostBPFFS, "")
			t.Setenv(EnvAllowUnprotectedTrampolines, "")
			runningInContainerFn = func() bool { return false }
			nativeBPFFSStatfsFn = func(_ string, buf *unix.Statfs_t) error {
				buf.Type = 0x62656572
				return nil
			}
			nativeBPFFSMkdirAllFn = func(string, os.FileMode) error { return nil }
			nativeBPFFSMountFn = func(_, _, _ string, _ uintptr, _ string) error { return nil }

			var hostN, nativeN int
			validateHostBPFFSPathFn = func(path string, _ bool) error {
				switch path {
				case h:
					hostN++
					if hostN == 1 {
						return enomem(h)
					}
					return nil
				case n:
					nativeN++
					return notBPF(n)
				default:
					return fmt.Errorf("unexpected path %s", path)
				}
			}
			tmp := t.TempDir()
			var gotRoot string
			openGraceOwnerInRootFn = func(root string, lg *zap.Logger) (*graceOwner, error) {
				gotRoot = root
				return openGraceOwnerInRoot(filepath.Join(tmp, GraceRootSubdir), lg)
			}
			core, logs := observer.New(zapcore.WarnLevel)
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if err := Init(ctx, zap.New(core), ModeNative); err != nil {
				t.Fatalf("Init = %v, want nil", err)
			}
			defer func() {
				stateMu.Lock()
				o := activeOwner
				activeOwner = nil
				stateMu.Unlock()
				if o != nil {
					if o.reaper != nil {
						o.reaper.stopAndWait(o.reaperCancel)
					}
					_ = unix.Close(o.lockFD)
				}
			}()
			if !Armed() {
				t.Fatalf("expected Armed after bare-native host-bpffs retry, logs=%+v", logs.All())
			}
			if hostN != 2 || nativeN != 1 {
				t.Fatalf("validate calls host=%d native=%d, want host=2 native=1", hostN, nativeN)
			}
			if want := filepath.Join(h, GraceRootSubdir); gotRoot != want {
				t.Fatalf("root = %q, want %q", gotRoot, want)
			}
		})

		// M79 persistent: bare native, /run/keploy/host-bpffs persistent ENOMEM,
		// /sys/fs/bpf not bpffs -> 4 total validate calls (2 host + 2 native) + syserr WARN.
		t.Run("m79_persistent_host_enomem_syserr_4_validates", func(t *testing.T) {
			resetStateForTest(t)
			t.Setenv(EnvHostBPFFS, "")
			t.Setenv(EnvAllowUnprotectedTrampolines, "")
			runningInContainerFn = func() bool { return false }
			nativeBPFFSStatfsFn = func(_ string, buf *unix.Statfs_t) error {
				buf.Type = 0x62656572
				return nil
			}
			nativeBPFFSMkdirAllFn = func(string, os.FileMode) error { return nil }
			nativeBPFFSMountFn = func(_, _, _ string, _ uintptr, _ string) error { return nil }

			var totalCalls int
			validateHostBPFFSPathFn = func(path string, _ bool) error {
				totalCalls++
				if path == h {
					return enomem(h)
				}
				return notBPF(n)
			}
			core, logs := observer.New(zapcore.WarnLevel)
			if err := Init(context.Background(), zap.New(core), ModeNative); err != nil {
				t.Fatalf("Init = %v, want nil", err)
			}
			if Armed() {
				t.Fatal("expected unprotected on persistent host-bpffs ENOMEM")
			}
			if totalCalls != 4 {
				t.Fatalf("total validate calls = %d, want 4", totalCalls)
			}
			if logs.Len() != 1 || logs.All()[0].Message != UnprotectedMountCheckErrWarnMessage {
				t.Fatalf("WARN = %+v, want %q", logs.All(), UnprotectedMountCheckErrWarnMessage)
			}
		})

		// Bare native, /run/keploy/host-bpffs EACCES, /sys/fs/bpf not bpffs -> perm WARN naming host path, no retry.
		t.Run("bare_native_host_eacces_perm_warn", func(t *testing.T) {
			resetStateForTest(t)
			t.Setenv(EnvHostBPFFS, "")
			t.Setenv(EnvAllowUnprotectedTrampolines, "")
			runningInContainerFn = func() bool { return false }
			nativeBPFFSStatfsFn = func(_ string, buf *unix.Statfs_t) error {
				buf.Type = 0x62656572
				return nil
			}
			nativeBPFFSMkdirAllFn = func(string, os.FileMode) error { return nil }
			nativeBPFFSMountFn = func(_, _, _ string, _ uintptr, _ string) error { return nil }
			var hostN int
			validateHostBPFFSPathFn = func(path string, _ bool) error {
				if path == h {
					hostN++
					return eacces(h)
				}
				return notBPF(n)
			}
			core, logs := observer.New(zapcore.WarnLevel)
			if err := Init(context.Background(), zap.New(core), ModeNative); err != nil {
				t.Fatalf("Init = %v, want nil", err)
			}
			if hostN != 1 {
				t.Fatalf("host validate calls = %d, want 1", hostN)
			}
			want := unprotectedPermWarnMessage(h)
			if logs.Len() != 1 || logs.All()[0].Message != want {
				t.Fatalf("WARN = %+v, want %q", logs.All(), want)
			}
		})

		// S1: unmounted host where ensureNativeBPFFS hits a transient fault (statfs,
		// MkdirAll, or Mount) must retry and end ARMED; persistent faults must log
		// the system-error or permission WARN rather than the mount hint; mount
		// counts are asserted so stacked mounts or spurious mount attempts fail.
		ensureCases := []struct {
			name           string
			statfsErrs     []error
			mkdirErrs      []error
			mountErrs      []error
			nativeErrs     []error
			wantArmed      bool
			wantWarn       string
			wantEnsureN    int
			wantMountCalls int
			wantMounted    int
		}{
			{"transient_statfs_enomem_recovers", []error{unix.ENOMEM, nil}, nil, nil, nil, true, "", 2, 1, 1},
			{"transient_mkdir_enomem_recovers", nil, []error{unix.ENOMEM, nil}, nil, nil, true, "", 2, 1, 1},
			{"transient_mount_enomem_recovers", nil, nil, []error{unix.ENOMEM, nil}, nil, true, "", 2, 2, 1},
			{"persistent_statfs_enomem_syserr", []error{unix.ENOMEM}, nil, nil, nil, false, UnprotectedMountCheckErrWarnMessage, 2, 0, 0},
			{"persistent_mount_enomem_syserr", nil, nil, []error{unix.ENOMEM}, nil, false, UnprotectedMountCheckErrWarnMessage, 2, 2, 0},
			{"mount_eperm_perm_warn_no_retry", nil, nil, []error{unix.EPERM}, nil, false, unprotectedPermWarnMessage(n), 1, 1, 0},
			{"mounted_then_validate_enomem_once_no_stack", nil, nil, nil, []error{enomem(n), nil}, true, "", 2, 1, 1},
			// Kills M88: ensureErr operational ENOMEM takes precedence over validate EACCES
			// (retries once and logs system-error WARN rather than permission WARN without retry).
			{"unmounted_statfs_enomem_validate_eacces", []error{unix.ENOMEM}, nil, nil, []error{eacces(n)}, false, UnprotectedMountCheckErrWarnMessage, 2, 0, 0},
		}
		for _, tc := range ensureCases {
			t.Run(tc.name, func(t *testing.T) {
				resetStateForTest(t)
				t.Setenv(EnvHostBPFFS, "")
				t.Setenv(EnvAllowUnprotectedTrampolines, "")
				runningInContainerFn = func() bool { return false }

				pick := func(errs []error, idx int) error {
					if len(errs) == 0 {
						return nil
					}
					if idx >= len(errs) {
						idx = len(errs) - 1
					}
					return errs[idx]
				}
				var mounts, stacked, statN, mkdirN, mountN, natN int
				nativeBPFFSStatfsFn = func(_ string, buf *unix.Statfs_t) error {
					e := pick(tc.statfsErrs, statN)
					statN++
					if e != nil {
						return e
					}
					if mounts > 0 {
						buf.Type = unix.BPF_FS_MAGIC
					} else {
						buf.Type = 0x62656572
					}
					return nil
				}
				nativeBPFFSMkdirAllFn = func(string, os.FileMode) error {
					e := pick(tc.mkdirErrs, mkdirN)
					mkdirN++
					return e
				}
				nativeBPFFSMountFn = func(_, _, _ string, _ uintptr, _ string) error {
					e := pick(tc.mountErrs, mountN)
					mountN++
					if e != nil {
						return e
					}
					if mounts > 0 {
						stacked++
					}
					mounts++
					return nil
				}
				validateHostBPFFSPathFn = func(path string, _ bool) error {
					if path == h {
						return enoent(h)
					}
					e := pick(tc.nativeErrs, natN)
					natN++
					if e != nil {
						return e
					}
					if mounts > 0 {
						return nil
					}
					return notBPF(n)
				}
				tmp := t.TempDir()
				openGraceOwnerInRootFn = func(_ string, lg *zap.Logger) (*graceOwner, error) {
					return openGraceOwnerInRoot(filepath.Join(tmp, GraceRootSubdir), lg)
				}

				core, logs := observer.New(zapcore.WarnLevel)
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if err := Init(ctx, zap.New(core), ModeNative); err != nil {
					t.Fatalf("Init = %v, want nil", err)
				}
				defer func() {
					stateMu.Lock()
					o := activeOwner
					activeOwner = nil
					stateMu.Unlock()
					if o != nil {
						if o.reaper != nil {
							o.reaper.stopAndWait(o.reaperCancel)
						}
						_ = unix.Close(o.lockFD)
					}
				}()

				if Armed() != tc.wantArmed {
					t.Fatalf("Armed = %v, want %v (logs=%+v)", Armed(), tc.wantArmed, logs.All())
				}
				if statN != tc.wantEnsureN {
					t.Fatalf("ensure statfs calls = %d, want %d", statN, tc.wantEnsureN)
				}
				if mountN != tc.wantMountCalls || mounts != tc.wantMounted || stacked != 0 {
					t.Fatalf("mount calls=%d (want %d), mounted=%d (want %d), stacked=%d (want 0)", mountN, tc.wantMountCalls, mounts, tc.wantMounted, stacked)
				}
				if tc.wantArmed {
					if logs.Len() != 0 {
						t.Fatalf("expected 0 WARNs when armed, got %+v", logs.All())
					}
				} else if logs.Len() != 1 || logs.All()[0].Message != tc.wantWarn {
					t.Fatalf("WARN = %+v, want %q", logs.All(), tc.wantWarn)
				}
			})
		}
	})
}

func TestEnsureNativeBPFFSNeverStacksOnStatfsError(t *testing.T) {
	cases := []struct {
		name        string
		inContainer bool
		statfsMagic int64
		statfsErr   error
		wantMount   int
		wantErr     error
	}{
		{"already_bpffs", false, unix.BPF_FS_MAGIC, nil, 0, nil},
		{"statfs_enomem_no_stack", false, 0, unix.ENOMEM, 0, unix.ENOMEM},
		{"statfs_eintr_no_stack", false, 0, unix.EINTR, 0, unix.EINTR},
		{"statfs_eio_no_stack", false, 0, unix.EIO, 0, unix.EIO},
		{"statfs_eagain_no_stack", false, 0, unix.EAGAIN, 0, unix.EAGAIN},
		{"statfs_eacces_no_stack", false, 0, unix.EACCES, 0, unix.EACCES},
		{"statfs_eperm_no_stack", false, 0, unix.EPERM, 0, unix.EPERM},
		{"statfs_enotdir_no_stack", false, 0, unix.ENOTDIR, 0, unix.ENOTDIR},
		{"non_bpffs_mounts", false, 0x62656572, nil, 1, nil},
		{"enoent_mounts", false, 0, unix.ENOENT, 1, nil},
		{"in_container_never_mounts_non_bpffs", true, 0x62656572, nil, 0, nil},
		{"in_container_never_mounts_enoent", true, 0, unix.ENOENT, 0, nil},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resetStateForTest(t)
			runningInContainerFn = func() bool { return tc.inContainer }
			nativeBPFFSStatfsFn = func(_ string, buf *unix.Statfs_t) error {
				if tc.statfsErr != nil {
					return tc.statfsErr
				}
				buf.Type = tc.statfsMagic
				return nil
			}
			nativeBPFFSMkdirAllFn = func(string, os.FileMode) error { return nil }
			var mountCalls int
			nativeBPFFSMountFn = func(_, _, _ string, _ uintptr, _ string) error {
				mountCalls++
				return nil
			}
			gotErr := ensureNativeBPFFS()
			if mountCalls != tc.wantMount {
				t.Fatalf("mount calls = %d, want %d", mountCalls, tc.wantMount)
			}
			if tc.wantErr != nil {
				if !errors.Is(gotErr, tc.wantErr) {
					t.Fatalf("ensureNativeBPFFS err = %v, want %v", gotErr, tc.wantErr)
				}
			} else if gotErr != nil {
				t.Fatalf("ensureNativeBPFFS err = %v, want nil", gotErr)
			}
		})
	}
}

func TestTestTagBuildReapsOnlyOwnTestPrefix(t *testing.T) {
	resetStateForTest(t)
	AllowShortGraceForTest(t)
	t.Setenv(EnvGraceTestTag, "e2e")

	root := t.TempDir()
	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatalf("openGraceOwnerInRoot: %v", err)
	}
	defer func() { _ = unix.Close(owner.lockFD) }()

	if !strings.HasPrefix(owner.self, GraceTestDirPrefix+"e2e-") {
		t.Fatalf("test owner.self = %q, want prefix %q", owner.self, GraceTestDirPrefix+"e2e-")
	}
	if got := sanitizeTag("e2e-2"); got != "e2e2" {
		t.Fatalf("sanitizeTag(\"e2e-2\") = %q, want \"e2e2\"", got)
	}

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

	owner.reaper.dynamicTrampImageFn = func() bool { return true }
	owner.reaper.kallsymsSysctlFn = func() bool { return true }
	owner.reaper.readKallsymsFn = func() (map[uint64][]uint64, bool, error) { return map[uint64][]uint64{}, true, nil }

	owner.reaper.round() // Pass 1: marks ownTestDead pass1
	owner.reaper.round() // Pass 2: no active trampoline targets -> reaps ONLY ownTestDead

	if _, err := os.Stat(ownTestDead); !os.IsNotExist(err) {
		t.Fatalf("ownTestDead was not reaped by matching test tag: err=%v", err)
	}
	if _, err := os.Stat(prodDead); err != nil {
		t.Fatalf("tagged test reaper MUST NOT touch production grace-* dir: %v", err)
	}
	if _, err := os.Stat(otherTestDead); err != nil {
		t.Fatalf("tagged test reaper MUST NOT touch another test tag's dir: %v", err)
	}
}

func TestReaperKallsymsCleanExitUnpinsOnlyWhenImageAddressDisappears(t *testing.T) {
	resetStateForTest(t)
	root := t.TempDir()
	deadDir := filepath.Join(root, GraceDirPrefix+"clean-exit")
	if err := os.Mkdir(deadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deadDir, "000001-p"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	const key = uint64(6442555249)
	const oldAddr = uint64(0xffffffffc031a300)
	const newAddr = uint64(0xffffffffc039f800)

	store := autoStateStore{}
	if err := store.writeSnapshot(deadDir, []trampImageRef{{Key: key, Addr: oldAddr}}); err != nil {
		t.Fatal(err)
	}
	now := uint64(100 * time.Second)
	if err := store.writeState(deadDir, reaperState{
		Flags:           stateCleanSnapshot,
		SnapshotCount:   1,
		FirstSeenBootNs: now,
	}); err != nil {
		t.Fatal(err)
	}

	liveTramps := map[uint64][]uint64{key: {oldAddr}}
	rebuilds := 0
	r := newGraceReaper(root, GraceDirPrefix+"self", DefaultReapInterval, zap.NewNop())
	r.bootNowNs = func() uint64 { return now }
	r.dynamicTrampImageFn = func() bool { return true }
	r.kallsymsSysctlFn = func() bool { return true }
	r.readKallsymsFn = func() (map[uint64][]uint64, bool, error) { return liveTramps, true, nil }
	r.inspectTargetsFn = func(string) (dirTargets, error) {
		return dirTargets{keyToFunc: map[uint64]string{key: "unix_dgram_sendmsg"}}, nil
	}
	r.rebuildTargetFn = func(fn string) error {
		rebuilds++
		return nil
	}

	// While oldAddr is still in kallsyms (< 2s settle), MUST keep pinned without forced rebuild yet.
	r.round()
	if _, err := os.Stat(deadDir); err != nil {
		t.Fatalf("reaped while old trampoline image was still in kallsyms: %v", err)
	}
	if rebuilds != 0 {
		t.Fatalf("unexpected forced rebuild during 2s settle window: %d", rebuilds)
	}

	// After 3s, oldAddr STILL in kallsyms (e.g. ENOMEM unlink) -> forces rebuild, STILL keeps pinned!
	now += uint64(3 * time.Second)
	r.round()
	if rebuilds != 1 {
		t.Fatalf("expected forced rebuild for stuck image, got rebuilds=%d", rebuilds)
	}
	if _, err := os.Stat(deadDir); err != nil {
		t.Fatalf("reaped before old image address actually disappeared from kallsyms: %v", err)
	}

	// Even when a NEW trampoline image (newAddr) is active on the same key,
	// once oldAddr is GONE from kallsyms, deadDir is safely reaped immediately!
	liveTramps = map[uint64][]uint64{key: {newAddr}}
	r.round()
	if _, err := os.Stat(deadDir); !os.IsNotExist(err) {
		t.Fatalf("deadDir not reaped after old image address vanished from kallsyms: err=%v", err)
	}
}

func TestReaperDeadOwnerSnapshotsOnPass2AndNeverUsesTimer(t *testing.T) {
	resetStateForTest(t)
	root := t.TempDir()
	deadDir := filepath.Join(root, GraceDirPrefix+"sigkill-owner")
	if err := os.Mkdir(deadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deadDir, "000001-p"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	const key = uint64(6442555249)
	const stuckAddr = uint64(0xffffffffc031a300)

	now := uint64(1000 * time.Second)
	liveTramps := map[uint64][]uint64{key: {stuckAddr}}
	rebuildErr := error(unix.ENOMEM)
	rebuilds := 0

	r := newGraceReaper(root, GraceDirPrefix+"self", DefaultReapInterval, zap.NewNop())
	r.bootNowNs = func() uint64 { return now }
	r.dynamicTrampImageFn = func() bool { return true }
	r.kallsymsSysctlFn = func() bool { return true }
	r.readKallsymsFn = func() (map[uint64][]uint64, bool, error) { return liveTramps, true, nil }
	r.inspectTargetsFn = func(string) (dirTargets, error) {
		return dirTargets{keyToFunc: map[uint64]string{key: "unix_dgram_sendmsg"}}, nil
	}
	r.rebuildTargetFn = func(string) error {
		rebuilds++
		return rebuildErr
	}

	// Pass 1: records stateDeadPass1 only (does NOT snapshot mid-exit_files).
	r.round()
	st1, ok, err := r.store.readState(deadDir)
	if err != nil || !ok || st1.Flags != stateDeadPass1 {
		t.Fatalf("after pass 1: state=(%+v, %v, %v), want stateDeadPass1 only", st1, ok, err)
	}

	// Pass 2: snapshots stuckAddr from kallsyms AND attempts forced rebuild right away.
	// Rebuild returns ENOMEM -> MUST NOT stamp stateDeadSnapshot or unpin!
	now += uint64(15 * time.Second)
	r.round()
	if rebuilds != 1 {
		t.Fatalf("expected pass-2 rebuild attempt, got %d", rebuilds)
	}
	if _, err := os.Stat(deadDir); err != nil {
		t.Fatalf("deadDir reaped when pass-2 rebuild failed with ENOMEM: %v", err)
	}
	stFail, _, _ := r.store.readState(deadDir)
	if stFail.Flags&(stateDeadSnapshot|stateForcedRebuild) != 0 {
		t.Fatalf("after failed pass-2 rebuild: state flags=%#x must NOT stamp stateDeadSnapshot/stateForcedRebuild (kills M7)", stFail.Flags)
	}

	// Advance clock by 2 HOURS while stuckAddr remains in kallsyms and rebuild hits ENOMEM:
	// MUST NEVER unpin on any timer!
	for i := 0; i < 4; i++ {
		now += uint64(30 * time.Minute)
		r.round()
		if _, err := os.Stat(deadDir); err != nil {
			t.Fatalf("timed unpin violation: deadDir reaped at iter %d while stuckAddr was still in kallsyms!", i)
		}
	}
	if rebuilds != 5 {
		t.Fatalf("expected 5 rebuild attempts (pass 2 + 4 retries), got %d", rebuilds)
	}

	// Rebuild finally succeeds on pass -> writes stateDeadSnapshot|stateForcedRebuild,
	// keeps pinned until stuckAddr actually vanishes from kallsyms, then reaps cleanly!
	rebuildErr = nil
	r.round()
	stSnap, ok, err := r.store.readState(deadDir)
	if err != nil || !ok || stSnap.Flags&stateDeadSnapshot == 0 || stSnap.SnapshotCount != 1 {
		t.Fatalf("after successful rebuild: state=(%+v, %v, %v), want stateDeadSnapshot with count=1", stSnap, ok, err)
	}
	if _, err := os.Stat(deadDir); err != nil {
		t.Fatalf("deadDir reaped before stuckAddr disappeared from kallsyms: %v", err)
	}
	liveTramps = map[uint64][]uint64{}
	r.round()
	if _, err := os.Stat(deadDir); !os.IsNotExist(err) {
		t.Fatalf("deadDir not reaped after rebuild succeeded and stuckAddr vanished: err=%v", err)
	}
}

func TestReaperEBUSYMarksPermanentStaleTrampolineAndNeverUnpins(t *testing.T) {
	resetStateForTest(t)
	root := t.TempDir()
	deadDir := filepath.Join(root, GraceDirPrefix+"last-link-ebusy")
	if err := os.Mkdir(deadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deadDir, "000001-p"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	const key = uint64(6442555249)
	const stuckAddr = uint64(0xffffffffc031a300)

	core, logs := observer.New(zapcore.WarnLevel)
	var obsStale bool
	SetMetricsObserver(func(_, _, _ int, _, stale bool) { obsStale = stale })

	r := newGraceReaper(root, GraceDirPrefix+"self", DefaultReapInterval, zap.New(core))
	r.dynamicTrampImageFn = func() bool { return true }
	r.kallsymsSysctlFn = func() bool { return true }
	r.readKallsymsFn = func() (map[uint64][]uint64, bool, error) { return map[uint64][]uint64{key: {stuckAddr}}, true, nil }
	r.inspectTargetsFn = func(string) (dirTargets, error) {
		return dirTargets{keyToFunc: map[uint64]string{key: "unix_dgram_sendmsg"}}, nil
	}
	rebuildCalls := 0
	r.rebuildTargetFn = func(string) error {
		rebuildCalls++
		return unix.EBUSY
	}

	r.round() // Pass 1: stateDeadPass1
	r.round() // Pass 2: snapshots stuckAddr + forced rebuild hits EBUSY -> marks permanent stale!
	r.round() // Pass 3: stays pinned forever and does NOT keep retrying EBUSY!

	if _, err := os.Stat(deadDir); err != nil {
		t.Fatalf("EBUSY stale dir was wrongly reaped: %v", err)
	}
	if !StaleTrampolineDetected() || !obsStale {
		t.Fatalf("StaleTrampolineDetected=%v obsStale=%v, want both true", StaleTrampolineDetected(), obsStale)
	}
	if rebuildCalls != 1 {
		t.Fatalf("expected 1 rebuild attempt before permanent stale latch, got %d", rebuildCalls)
	}
	foundRebootWarn := false
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "drain and reboot") {
			foundRebootWarn = true
			break
		}
	}
	if !foundRebootWarn {
		t.Fatalf("expected WARN instructing 'drain and reboot', got %+v", logs.All())
	}
}

func TestReaperStatsFallbackAndFailClosedWithoutKallsyms(t *testing.T) {
	resetStateForTest(t)
	root := t.TempDir()
	deadDir := filepath.Join(root, GraceDirPrefix+"no-kallsyms")
	if err := os.Mkdir(deadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deadDir, "000001-p"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	const key = uint64(6442555249)
	statsErr := error(unix.EPERM)
	runCntCalls := 0
	runCntDelta := uint64(5) // still running on first check!

	r := newGraceReaper(root, GraceDirPrefix+"self", DefaultReapInterval, zap.NewNop())
	r.statsProbeWait = time.Millisecond
	r.kallsymsSysctlFn = func() bool { return false } // kallsyms hidden
	r.inspectTargetsFn = func(string) (dirTargets, error) {
		return dirTargets{keyToFunc: map[uint64]string{key: "unix_dgram_sendmsg"}}, nil
	}
	r.rebuildTargetFn = func(string) error { return nil }
	r.enableStatsFn = func() (io.Closer, error) {
		if statsErr != nil {
			return nil, statsErr
		}
		return nopCloser{}, nil
	}
	r.runCntFn = func(string) (uint64, error) {
		runCntCalls++
		if runCntCalls%2 == 1 {
			return 100, nil
		}
		return 100 + runCntDelta, nil
	}

	r.round() // Pass 1
	// Pass 2: kallsyms hidden AND BPF_ENABLE_STATS returns EPERM -> MUST fail closed (keep pins)!
	r.round()
	if _, err := os.Stat(deadDir); err != nil {
		t.Fatalf("deadDir reaped when both kallsyms and BPF_ENABLE_STATS failed: %v", err)
	}

	// Pass 3: BPF_ENABLE_STATS works, but run_cnt increases (100 -> 105) -> MUST keep pins!
	statsErr = nil
	r.round()
	if _, err := os.Stat(deadDir); err != nil {
		t.Fatalf("deadDir reaped while BPF_ENABLE_STATS showed run_cnt increasing: %v", err)
	}

	// Pass 4: run_cnt is flat (100 -> 100) after forced rebuild -> safely reaped!
	runCntDelta = 0
	r.round()
	if _, err := os.Stat(deadDir); !os.IsNotExist(err) {
		t.Fatalf("deadDir not reaped when stats fallback confirmed flat run_cnt after rebuild: err=%v", err)
	}
}

func TestGracefulShutdown_RunsDisarmBeforeDetachAndLeavesPinForReaper(t *testing.T) {
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

	var order []string
	RegisterDisarmHook(func() {
		order = append(order, "disarm")
	})
	RegisterDetachHook(func() {
		order = append(order, "detach")
		var dummy ebpf.Program
		if perr := PinBeforeAttach(&dummy, "proxyless", "late", "p"); !errors.Is(perr, ErrShuttingDown) {
			t.Errorf("PinBeforeAttach during detach hook = %v, want ErrShuttingDown", perr)
		}
	})

	start := time.Now()
	GracefulShutdown(context.Background(), zap.NewNop())
	if elapsed := time.Since(start); elapsed > 500*time.Millisecond {
		t.Fatalf("GracefulShutdown blocked exit for %v; must return immediately", elapsed)
	}
	if len(order) != 2 || order[0] != "disarm" || order[1] != "detach" {
		t.Fatalf("shutdown hook order = %v, want [disarm detach]", order)
	}
	if !reaperCancelled || Armed() {
		t.Fatalf("reaperCancelled=%v Armed=%v, want true/false", reaperCancelled, Armed())
	}
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
	if GraceRootSubdir == "keploy" {
		t.Fatalf("GraceRootSubdir must be outside keploy/")
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

func TestLateRegisterHooksRunSynchronouslyAfterShutdown(t *testing.T) {
	resetStateForTest(t)
	GracefulShutdown(context.Background(), zap.NewNop())

	disarmed := false
	detached := false
	RegisterDisarmHook(func() { disarmed = true })
	RegisterDetachHook(func() { detached = true })
	if !disarmed || !detached {
		t.Fatalf("late hooks after shutdown: disarmed=%v detached=%v, want both true", disarmed, detached)
	}
}

func TestInitAfterGracefulShutdownRefusesToReopenGate(t *testing.T) {
	resetStateForTest(t)
	GracefulShutdown(context.Background(), zap.NewNop())

	if err := Init(context.Background(), zap.NewNop(), ModeNative); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("Init after GracefulShutdown = %v, want ErrShuttingDown", err)
	}
	var dummy ebpf.Program
	if err := PinBeforeAttach(&dummy, "proxyless", "late", "p"); !errors.Is(err, ErrShuttingDown) {
		t.Fatalf("PinBeforeAttach after failed post-shutdown Init = %v, want ErrShuttingDown", err)
	}

	ResetShutdownForTest(t)
	SetBypassForTest(t)
	if err := PinBeforeAttach(&dummy, "proxyless", "rearmed", "p"); err != nil {
		t.Fatalf("PinBeforeAttach after ResetShutdownForTest = %v, want nil", err)
	}
}

func TestValidateHostBPFFSPath_RejectsNonBPFFSAndOverlay(t *testing.T) {
	resetStateForTest(t)

	d := t.TempDir()
	if err := ValidateHostBPFFSPath(d, false); err == nil || !strings.Contains(err.Error(), "not a bpffs filesystem") {
		t.Fatalf("ValidateHostBPFFSPath(tmpfs, false) = %v, want not-a-bpffs error", err)
	}

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

	// Host-shared vs container-private bpffs mount propagation detection.
	shared, err := hasHostSharedBPFFS(mi, DefaultHostBPFFSPath)
	if err != nil || !shared {
		t.Fatalf("hasHostSharedBPFFS(shared:12 host-bpffs) = (%v, %v), want (true, nil)", shared, err)
	}
	priv, err := hasHostSharedBPFFS(mi, DefaultNativeBPFFSPath)
	if err != nil || priv {
		t.Fatalf("hasHostSharedBPFFS(private /sys/fs/bpf) = (%v, %v), want (false, nil)", priv, err)
	}
}

func TestAttachTracingFailsClosedWhenUnarmed(t *testing.T) {
	resetStateForTest(t)
	var dummy ebpf.Program
	if _, err := AttachTracing(&dummy, "unixconn", "unix_dgram_sendmsg", "p"); !errors.Is(err, ErrGraceNotArmed) {
		t.Fatalf("AttachTracing unarmed = %v, want ErrGraceNotArmed (kills M2)", err)
	}
}

func TestMixedVersionGrace2IsolationAndLegacyReap(t *testing.T) {
	resetStateForTest(t)
	root := t.TempDir()
	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatalf("openGraceOwnerInRoot: %v", err)
	}
	defer func() { _ = unix.Close(owner.lockFD) }()

	// New directories MUST use GraceDirPrefix ("grace2-") so v3.7.27's timer-only
	// reaper (which only scans strings.HasPrefix(name, "grace-")) skips them.
	if !strings.HasPrefix(owner.self, GraceDirPrefix) {
		t.Fatalf("owner.self = %q, want prefix %q", owner.self, GraceDirPrefix)
	}
	if strings.HasPrefix(owner.self, LegacyGraceDirPrefix) {
		t.Fatalf("owner.self = %q MUST NOT match legacy prefix %q", owner.self, LegacyGraceDirPrefix)
	}

	// v3.7.28 reaper MUST safely reap BOTH grace2-* and legacy grace-* dead dirs
	// while skipping grace-test-* / grace2-test-* dirs.
	newDead := filepath.Join(root, GraceDirPrefix+"dead-new")
	legacyDead := filepath.Join(root, LegacyGraceDirPrefix+"dead-v3727")
	testDead := filepath.Join(root, "grace-test-other")
	for _, d := range []string{newDead, legacyDead, testDead} {
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "000001-p"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	r := newGraceReaper(root, owner.self, DefaultReapInterval, zap.NewNop())
	r.dynamicTrampImageFn = func() bool { return true }
	r.kallsymsSysctlFn = func() bool { return true }
	r.readKallsymsFn = func() (map[uint64][]uint64, bool, error) { return map[uint64][]uint64{}, true, nil }
	r.inspectTargetsFn = func(string) (dirTargets, error) {
		return dirTargets{
			keyToFunc: map[uint64]string{6442555249: "unix_dgram_sendmsg"},
			funcs:     map[string]struct{}{"unix_dgram_sendmsg": {}},
		}, nil
	}

	r.round() // Pass 1
	r.round() // Pass 2: kallsyms empty -> reaps both newDead and legacyDead

	if _, err := os.Stat(newDead); !os.IsNotExist(err) {
		t.Fatalf("newDead (%s) not reaped: err=%v", newDead, err)
	}
	if _, err := os.Stat(legacyDead); !os.IsNotExist(err) {
		t.Fatalf("legacyDead (%s) not reaped: err=%v", legacyDead, err)
	}
	if _, err := os.Stat(testDead); err != nil {
		t.Fatalf("production reaper wrongly touched test dir %s: %v", testDead, err)
	}
}

func TestReaperFailsClosedOnUnresolvedTargetsAndDeduplicatesRebuilds(t *testing.T) {
	resetStateForTest(t)
	root := t.TempDir()

	// 1. Unresolved target inspection error MUST fail closed (kills M6).
	unresDir := filepath.Join(root, GraceDirPrefix+"unresolved")
	if err := os.Mkdir(unresDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unresDir, "000001-p"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}
	rUnres := newGraceReaper(root, GraceDirPrefix+"self", DefaultReapInterval, zap.NewNop())
	rUnres.dynamicTrampImageFn = func() bool { return true }
	rUnres.kallsymsSysctlFn = func() bool { return true }
	rUnres.readKallsymsFn = func() (map[uint64][]uint64, bool, error) { return map[uint64][]uint64{}, true, nil }
	rUnres.inspectTargetsFn = func(string) (dirTargets, error) {
		return dirTargets{}, errors.New("unresolved tracing program target")
	}
	rUnres.round()
	rUnres.round()
	rUnres.round()
	if _, err := os.Stat(unresDir); err != nil {
		t.Fatalf("reaper unpinned directory despite unresolved tracing target (M6 violation!): %v", err)
	}

	// 2. Two dead directories targeting the same function deduplicate rebuilds per round.
	root2 := t.TempDir()
	for _, n := range []string{"d1", "d2"} {
		d := filepath.Join(root2, GraceDirPrefix+n)
		if err := os.Mkdir(d, 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(d, "000001-p"), []byte("x"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	const key = uint64(6442555249)
	const addr = uint64(0xffffffffc031a300)
	rebuilds := 0
	r2 := newGraceReaper(root2, GraceDirPrefix+"self", DefaultReapInterval, zap.NewNop())
	r2.dynamicTrampImageFn = func() bool { return true }
	r2.kallsymsSysctlFn = func() bool { return true }
	r2.readKallsymsFn = func() (map[uint64][]uint64, bool, error) {
		return map[uint64][]uint64{key: {addr}}, true, nil
	}
	r2.inspectTargetsFn = func(string) (dirTargets, error) {
		return dirTargets{
			keyToFunc: map[uint64]string{key: "unix_dgram_sendmsg"},
			funcs:     map[string]struct{}{"unix_dgram_sendmsg": {}},
		}, nil
	}
	r2.rebuildTargetFn = func(string) error {
		rebuilds++
		return nil
	}
	r2.round() // Pass 1
	r2.round() // Pass 2: both dirs need rebuild of unix_dgram_sendmsg -> called ONCE!
	if rebuilds != 1 {
		t.Fatalf("expected 1 deduplicated rebuild across 2 dead dirs in one round, got %d", rebuilds)
	}
}

func TestStaleTargetBlocksRepeatedPinLeaks(t *testing.T) {
	resetStateForTest(t)
	root := t.TempDir()
	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(owner.lockFD) }()
	stateMu.Lock()
	activeOwner = owner
	stateMu.Unlock()

	ReportStaleTrampoline(zap.NewNop(), "unix_dgram_sendmsg", unix.EBUSY)
	var dummy ebpf.Program
	err = PinBeforeAttach(&dummy, "unixconn", "unix_dgram_sendmsg", "p")
	if !errors.Is(err, unix.EBUSY) {
		t.Fatalf("PinBeforeAttach on known-stale EBUSY target = %v, want EBUSY", err)
	}
	if got := countPinnedProgs(owner.dir); got != 0 {
		t.Fatalf("expected 0 leaked pins after stale target rejection, got %d", got)
	}
}

func TestNativeInContainerDoesNotSelfMountPrivateBPFFS(t *testing.T) {
	resetStateForTest(t)
	prevInContainer := runningInContainerFn
	runningInContainerFn = func() bool { return true }
	t.Cleanup(func() { runningInContainerFn = prevInContainer })

	t.Setenv(EnvHostBPFFS, t.TempDir()) // non-bpffs override ensures we hit honest unprotected mode
	t.Setenv(EnvAllowUnprotectedTrampolines, "")
	if err := Init(context.Background(), zap.NewNop(), ModeNative); err != nil {
		t.Fatalf("Init(ModeNative) inside container = %v, want nil (honest unprotected)", err)
	}
	if Armed() || !UnprotectedTrampolinesActive() {
		t.Fatalf("inside container without host bpffs: Armed=%v Unprotected=%v, want false/true", Armed(), UnprotectedTrampolinesActive())
	}
}

func TestTargetFuncsStoreRoundTripAndMultiVersionCandidateKeys(t *testing.T) {
	dir := t.TempDir()
	store := autoStateStore{}
	if err := store.writeTargets(dir, []string{"unix_dgram_sendmsg", "unix_stream_connect", "unix_dgram_sendmsg"}); err != nil {
		t.Fatalf("writeTargets: %v", err)
	}
	got, err := store.readTargets(dir)
	if err != nil {
		t.Fatalf("readTargets: %v", err)
	}
	if len(got) != 2 || got[0] != "unix_dgram_sendmsg" || got[1] != "unix_stream_connect" {
		t.Fatalf("readTargets = %v, want [unix_dgram_sendmsg unix_stream_connect]", got)
	}

	keys := trampolineCandidateKeys(1, 104305)
	if len(keys) < 2 {
		t.Fatalf("expected multiple candidate keys across kernel versions, got %v", keys)
	}
	has6x := false
	has5x := false
	for _, k := range keys {
		if k == 6442555249 {
			has6x = true
		}
		if k == 104305 {
			has5x = true
		}
	}
	if !has6x || !has5x {
		t.Fatalf("trampolineCandidateKeys(1, 104305) = %v, want both 6.x (6442555249) and 5.x (104305) keys", keys)
	}
}

func TestResolveTrampolineTargetKillsM6(t *testing.T) {
	// Pre-6.0 Tracing prog (AttachBtfID == 0) MUST signal pre60Unknown=true so
	// inspectDirTrampolineTargets resolves via target_funcs or fails closed (kills M6).
	_, _, _, isTramp, pre60, err := resolveTrampolineTarget(uapiProgInfo{
		Type:        uint32(ebpf.Tracing),
		AttachBtfID: 0,
	})
	if err != nil || isTramp || !pre60 {
		t.Fatalf("resolveTrampolineTarget(Tracing, AttachBtfID=0) = (isTramp=%v, pre60=%v, err=%v), want (false, true, nil)", isTramp, pre60, err)
	}
	// Non-tracing prog (e.g. Kprobe) MUST return isTramp=false, pre60=false, err=nil.
	_, _, _, isTramp, pre60, err = resolveTrampolineTarget(uapiProgInfo{
		Type: uint32(ebpf.Kprobe),
	})
	if err != nil || isTramp || pre60 {
		t.Fatalf("resolveTrampolineTarget(Kprobe) = (isTramp=%v, pre60=%v, err=%v), want (false, false, nil)", isTramp, pre60, err)
	}
}

func TestModuleWildcardKallsymsMatching(t *testing.T) {
	// Linux 5.19 tls.ko tls_init: module BTF obj=14, type_id=129749.
	// key = (14 << 32) | 0x80000000 | 129749
	modKey := (uint64(14) << 32) | 0x80000000 | 129749
	vmlinuxKey := (uint64(1) << 32) | 0x80000000 | 129749
	tramps := map[uint64][]uint64{
		modKey:     {0xffffffffc0401000},
		vmlinuxKey: {0xffffffffc0402000},
	}
	modIDs := map[uint32]struct{}{129749: {}}
	snap := snapshotKeysFromKallsyms(tramps, nil, modIDs)
	if len(snap) != 1 || snap[0].Key != modKey || snap[0].Addr != 0xffffffffc0401000 {
		t.Fatalf("snapshotKeysFromKallsyms(module wildcard) = %+v, want single match on modKey %d (not vmlinuxKey)", snap, modKey)
	}
	dt := dirTargets{modIDToFunc: map[uint32]string{129749: "tls_init"}}
	if fn, ok := dt.funcForKey(modKey); !ok || fn != "tls_init" {
		t.Fatalf("funcForKey(modKey) = (%q, %v), want (tls_init, true)", fn, ok)
	}
	if fn, ok := dt.funcForKey(vmlinuxKey); ok {
		t.Fatalf("funcForKey(vmlinuxKey) = (%q, true), want false", fn)
	}
}

func TestGracefulShutdownDrainsReaperBeforeDetachAndSnapshotsPostDetach(t *testing.T) {
	resetStateForTest(t)
	root := t.TempDir()
	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	const key = uint64(6442555249)
	const preAddr = uint64(0xffffffffc0100000)
	const postAddr = uint64(0xffffffffc0200000)

	curAddr := preAddr
	owner.attachedKeys = map[uint64]struct{}{key: {}}
	owner.reaper.dynamicTrampImageFn = func() bool { return true }
	owner.reaper.kallsymsSysctlFn = func() bool { return true }
	owner.reaper.readKallsymsFn = func() (map[uint64][]uint64, bool, error) {
		return map[uint64][]uint64{key: {curAddr}}, true, nil
	}

	reaperCtx, reaperCancel := context.WithCancel(context.Background())
	owner.reaperCancel = reaperCancel
	owner.reaper.start(reaperCtx)

	stateMu.Lock()
	activeOwner = owner
	stateMu.Unlock()

	reaperDrainedBeforeDetach := false
	RegisterDetachHook(func() {
		// Verify reaper goroutine has ALREADY exited before detach hook runs!
		select {
		case <-owner.reaper.doneCh:
			reaperDrainedBeforeDetach = true
		default:
			reaperDrainedBeforeDetach = false
		}
		// Simulate a concurrent agent attach or earlier link close creating a
		// NEW trampoline image during detach.
		curAddr = postAddr
	})

	GracefulShutdown(context.Background(), zap.NewNop())
	if !reaperDrainedBeforeDetach {
		t.Fatal("reaper goroutine was NOT cancelled and drained before detach hooks ran")
	}

	st, ok, err := owner.reaper.store.readState(owner.dir)
	if err != nil || !ok || st.Flags&stateCleanSnapshot == 0 {
		t.Fatalf("clean snapshot state = (%+v, %v, %v), want stateCleanSnapshot", st, ok, err)
	}
	refs, err := owner.reaper.store.readSnapshot(owner.dir, st.SnapshotCount)
	if err != nil {
		t.Fatalf("readSnapshot: %v", err)
	}
	hasPre, hasPost := false, false
	for _, r := range refs {
		if r.Addr == preAddr {
			hasPre = true
		}
		if r.Addr == postAddr {
			hasPost = true
		}
	}
	if !hasPre || !hasPost {
		t.Fatalf("persisted snapshot %+v must contain union of pre (%x) and post (%x) images", refs, preAddr, postAddr)
	}
}

func TestUnpinFailedAttachRemovesUnusedPin(t *testing.T) {
	resetStateForTest(t)
	root := t.TempDir()
	owner, err := openGraceOwnerInRoot(root, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unix.Close(owner.lockFD) }()

	var dummy ebpf.Program
	pinPath := filepath.Join(owner.dir, "000001-test-fn-p")
	if err := os.WriteFile(pinPath, []byte("pin"), 0o600); err != nil {
		t.Fatal(err)
	}
	owner.keysMu.Lock()
	owner.pinPaths = map[*ebpf.Program]string{&dummy: pinPath}
	owner.keysMu.Unlock()
	stateMu.Lock()
	activeOwner = owner
	stateMu.Unlock()

	UnpinFailedAttach(&dummy)
	if _, err := os.Stat(pinPath); !os.IsNotExist(err) {
		t.Fatalf("UnpinFailedAttach did not remove unused pin %s: err=%v", pinPath, err)
	}
}

func TestWarnOnceForUnresolvableDeadDir(t *testing.T) {
	resetStateForTest(t)
	root := t.TempDir()
	unresDir := filepath.Join(root, LegacyGraceDirPrefix+"old-v3727")
	if err := os.Mkdir(unresDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(unresDir, "000001-p"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	core, logs := observer.New(zapcore.WarnLevel)
	r := newGraceReaper(root, GraceDirPrefix+"self", DefaultReapInterval, zap.New(core))
	r.dynamicTrampImageFn = func() bool { return true }
	r.kallsymsSysctlFn = func() bool { return true }
	r.inspectTargetsFn = func(string) (dirTargets, error) {
		return dirTargets{}, errors.New("no resolvable target")
	}

	r.round() // Pass 1
	r.round() // Pass 2: WARN #1
	r.round() // Pass 3: suppressed to Debug
	r.round() // Pass 4: suppressed to Debug
	if logs.Len() != 1 {
		t.Fatalf("expected exactly 1 WARN for unresolvable dir across repeated rounds, got %d: %+v", logs.Len(), logs.All())
	}
}

func TestCleanupAnchorsNeverRemovesAnchorLinksOnAnyKernel(t *testing.T) {
	resetStateForTest(t)
	root := t.TempDir()
	anchorsDir := filepath.Join(root, AnchorsSubdir)
	if err := os.MkdirAll(anchorsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	linkPath := filepath.Join(anchorsDir, "unix_dgram_sendmsg"+anchorLinkSuffix)
	progPath := filepath.Join(anchorsDir, "unix_dgram_sendmsg"+anchorProgSuffix)
	if err := os.WriteFile(linkPath, []byte("l"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(progPath, []byte("p"), 0o600); err != nil {
		t.Fatal(err)
	}

	core, logs := observer.New(zapcore.InfoLevel)
	err := cleanupAnchorsInRoot(context.Background(), zap.New(core), root, time.Second, nil)
	if err != nil {
		t.Fatalf("cleanupAnchorsInRoot = %v, want nil", err)
	}
	if StaleTrampolineDetected() {
		t.Fatalf("cleanupAnchorsInRoot wrongly raised StaleTrampolineDetected")
	}
	if _, serr := os.Stat(linkPath); serr != nil {
		t.Fatalf("cleanupAnchorsInRoot removed anchor link %s (MUST stay until reboot!): %v", linkPath, serr)
	}
	if _, serr := os.Stat(progPath); serr != nil {
		t.Fatalf("cleanupAnchorsInRoot removed anchor prog %s (MUST stay until reboot!): %v", progPath, serr)
	}
	foundMsg := false
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "1 idle keploy anchors stay until the next reboot") &&
			strings.Contains(e.Message, "tens of ns/call") &&
			strings.Contains(e.Message, "livepatches") {
			foundMsg = true
			break
		}
	}
	if !foundMsg {
		t.Fatalf("cleanupAnchorsInRoot missing clear reboot/cost summary line, logs=%+v", logs.All())
	}
}

func TestParsePinTargetMapRequiresPerPinKey(t *testing.T) {
	m := parsePinTargetMap([]string{
		"000001-review-unix_dgram_sendmsg-p=unix_dgram_sendmsg",
		"000002-lifecycle-sched_process_exit-p=<non-trampoline>",
		"unix_stream_connect", // legacy dir-level entry without pinName= MUST NOT populate per-pin map
	})
	if got := m["000001-review-unix_dgram_sendmsg-p"]; got != "unix_dgram_sendmsg" {
		t.Fatalf("pin 1 target = %q, want unix_dgram_sendmsg", got)
	}
	if got := m["000002-lifecycle-sched_process_exit-p"]; got != nonTrampolineTargetSentinel {
		t.Fatalf("pin 2 target = %q, want %q", got, nonTrampolineTargetSentinel)
	}
	if len(m) != 2 {
		t.Fatalf("parsePinTargetMap length = %d (%v), want 2 (legacy un-keyed entry must be ignored)", len(m), m)
	}
}

func TestReaperSlotReuseBreaksLivelockViaStatsFallback(t *testing.T) {
	resetStateForTest(t)
	root := t.TempDir()
	deadDir := filepath.Join(root, GraceDirPrefix+"slot-reuse")
	if err := os.Mkdir(deadDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(deadDir, "000001-p"), []byte("x"), 0o600); err != nil {
		t.Fatal(err)
	}

	const key = uint64(6442555249)
	const reusedAddr = uint64(0xffffffffc031a300) // stays identical on 6.12 bpf_prog_pack slot reuse

	now := uint64(1000 * time.Second)
	rebuilds := 0
	statsCalls := 0
	runCntDelta := uint64(3) // still running on first fallback -> MUST fail closed

	r := newGraceReaper(root, GraceDirPrefix+"self", DefaultReapInterval, zap.NewNop())
	r.bootNowNs = func() uint64 { return now }
	r.statsProbeWait = time.Millisecond
	r.dynamicTrampImageFn = func() bool { return true }
	r.kallsymsSysctlFn = func() bool { return true }
	r.readKallsymsFn = func() (map[uint64][]uint64, bool, error) {
		return map[uint64][]uint64{key: {reusedAddr}}, true, nil
	}
	r.inspectTargetsFn = func(string) (dirTargets, error) {
		return dirTargets{
			keyToFunc: map[uint64]string{key: "unix_dgram_sendmsg"},
			funcs:     map[string]struct{}{"unix_dgram_sendmsg": {}},
		}, nil
	}
	r.rebuildTargetFn = func(string) error {
		rebuilds++
		return nil
	}
	r.enableStatsFn = func() (io.Closer, error) {
		statsCalls++
		return nopCloser{}, nil
	}
	r.runCntFn = func(string) (uint64, error) {
		if runCntDelta > 0 {
			d := runCntDelta
			runCntDelta = 0
			return 50 + d, nil
		}
		return 50, nil
	}

	// Pass 1: stateDeadPass1
	r.round()
	// Pass 2: snapshots reusedAddr + forces rebuild #1 (sets stateForcedRebuild)
	now += uint64(300 * time.Millisecond)
	r.round()
	if rebuilds != 1 {
		t.Fatalf("after pass 2: rebuilds=%d, want 1", rebuilds)
	}
	// Within 2s settle window (every 300ms): MUST wait calmly without thrashing rebuilds or resetting LastProbeBootNs!
	for i := 0; i < 5; i++ {
		now += uint64(300 * time.Millisecond)
		r.round()
	}
	if rebuilds != 1 || statsCalls != 0 {
		t.Fatalf("during 2s post-rebuild settle: rebuilds=%d statsCalls=%d, want 1/0", rebuilds, statsCalls)
	}

	// At >= 2s after pass-2 rebuild: reusedAddr still present -> enters stats fallback.
	// First check sees run_cnt changing -> MUST keep pinned!
	runCntSeq := 0
	r.runCntFn = func(string) (uint64, error) {
		runCntSeq++
		if runCntSeq == 1 {
			return 100, nil
		}
		if runCntSeq == 2 {
			return 105, nil // still executing!
		}
		return 105, nil // flat on next attempt
	}
	now += uint64(600 * time.Millisecond)
	r.round()
	if statsCalls != 1 {
		t.Fatalf("expected stats fallback after >=2s slot reuse, got statsCalls=%d", statsCalls)
	}
	if _, err := os.Stat(deadDir); err != nil {
		t.Fatalf("reaped deadDir despite run_cnt increasing in stats fallback: %v", err)
	}

	// Next check >= 2s later: run_cnt is flat (105 -> 105) -> breaks livelock and reaps cleanly!
	now += uint64(2500 * time.Millisecond)
	r.round()
	if _, err := os.Stat(deadDir); !os.IsNotExist(err) {
		t.Fatalf("deadDir still livelocked after stats fallback confirmed flat run_cnt: err=%v", err)
	}
}
