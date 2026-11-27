//go:build !linux

package bpfsafety

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"
)

const (
	DefaultHostBPFFSPath                = "/run/keploy/host-bpffs"
	DefaultNativeBPFFSPath              = "/sys/fs/bpf"
	GraceRootSubdir                     = "keploy-grace"
	AnchorsSubdir                       = "anchors"
	GraceModeHostBPFFS                  = "host-bpffs"
	GraceModeUnprotected                = "unprotected"
	UnprotectedWarnMessage              = "eBPF crash protection is OFF: mount the host's /sys/fs/bpf at /run/keploy/host-bpffs (see docs/kernel-safety.md)"
	UnprotectedMountCheckErrWarnMessage = "eBPF crash protection is OFF: host bpffs mount check failed with a system error (see docs/kernel-safety.md)"
	UnprotectedPermWarnMessage          = "eBPF crash protection is OFF: permission denied accessing host bpffs mount"
	UnprotectedInitErrWarnMessage       = "eBPF crash protection is OFF: host bpffs mount is present but grace directory initialization failed (see docs/kernel-safety.md)"
	UnprotectedOptOutWarnMessage        = "eBPF crash protection is OFF: KEPLOY_ALLOW_UNPROTECTED_TRAMPOLINES=1 is set (see docs/kernel-safety.md)"
	EnvHostBPFFS                        = "KEPLOY_HOST_BPFFS"
	EnvHostBPFFSDev                     = "KEPLOY_HOST_BPFFS_DEV"
	EnvLaunchMode                       = "KEPLOY_LAUNCH_MODE"
	EnvAllowUnprotectedTrampolines      = "KEPLOY_ALLOW_UNPROTECTED_TRAMPOLINES"
	EnvGraceTestTag                     = "KEPLOY_BPF_GRACE_TEST_TAG"
	GraceDirPrefix                      = "grace2-"
	LegacyGraceDirPrefix                = "grace-"
	GraceTestDirPrefix                  = "grace2-test-"
	DefaultReapInterval                 = 15 * time.Second
)

var (
	ErrGraceNotArmed    = errors.New("bpf trampoline grace pinning is not armed")
	ErrShuttingDown     = errors.New("bpf trampoline safety gate is closed for shutdown")
	ErrPinFailed        = errors.New("bpf trampoline grace pin failed")
	ErrUnresolvedTarget = errors.New("bpf tracing program target function could not be resolved")
	ErrAnchorUnpinned   = errors.New("bpf trampoline anchor is not pinned on host bpffs")
)

type Mode string

const (
	ModeDaemonSet  Mode = "daemonset"
	ModeK8sSidecar Mode = "k8s-sidecar"
	ModeCompose    Mode = "compose"
	ModeDocker     Mode = "docker"
	ModeNative     Mode = "native"
)

type TestCleaner interface {
	Helper()
	Cleanup(func())
}

type MetricsObserver func(dirsPresent, pinnedProgs, anchorLinks int, unprotected, staleTrampoline bool)

func DetectMode(daemonSet bool) Mode                                            { return ModeNative }
func SetMetricsObserver(fn MetricsObserver)                                     {}
func UnprotectedTrampolinesActive() bool                                        { return false }
func StaleTrampolineDetected() bool                                             { return false }
func ReportStaleTrampoline(logger *zap.Logger, funcName string, err error)      {}
func CountAnchorLinks(root string) int                                          { return 0 }
func CleanupAnchors(ctx context.Context, logger *zap.Logger, path string) error { return nil }
func ActiveGraceMode() string                                                   { return "" }
func Armed() bool                                                               { return false }
func ActiveGraceDir() string                                                    { return "" }
func RecordPrivateOverlayDev(dev uint64)                                        {}
func CaptureHostBPFFSDev() (uint64, bool)                                       { return 0, false }
func ValidateHostBPFFSPath(path string, checkContainerHostMount bool) error     { return nil }
func SetBypassForTest(tb TestCleaner)                                           {}
func AllowShortGraceForTest(tb TestCleaner)                                     {}
func ResetShutdownForTest(tb TestCleaner)                                       {}
func ResetUnprotectedForTest(tb TestCleaner)                                    {}
func HasActiveWork() bool                                                       { return false }
func Init(ctx context.Context, logger *zap.Logger, m Mode) error                { return nil }
func RegisterDisarmHook(fn func())                                              {}
func RegisterDetachHook(fn func())                                              {}
func GracefulShutdown(ctx context.Context, logger *zap.Logger)                  {}
