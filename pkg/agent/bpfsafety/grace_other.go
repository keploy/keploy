//go:build !linux

package bpfsafety

import (
	"context"
	"errors"
	"time"

	"go.uber.org/zap"
)

const (
	DefaultHostBPFFSPath           = "/run/keploy/host-bpffs"
	DefaultNativeBPFFSPath         = "/sys/fs/bpf"
	GraceRootSubdir                = "keploy-grace"
	GraceModeHostBPFFS             = "host-bpffs"
	GraceModeUnprotected           = "unprotected"
	UnprotectedWarnMessage         = "eBPF crash protection is OFF: mount the host's /sys/fs/bpf at /run/keploy/host-bpffs (see docs/kernel-safety.md)"
	EnvHostBPFFS                   = "KEPLOY_HOST_BPFFS"
	EnvHostBPFFSDev                = "KEPLOY_HOST_BPFFS_DEV"
	EnvLaunchMode                  = "KEPLOY_LAUNCH_MODE"
	EnvAllowUnprotectedTrampolines = "KEPLOY_ALLOW_UNPROTECTED_TRAMPOLINES"
	EnvGraceDuration               = "KEPLOY_BPF_GRACE_DURATION"
	EnvGraceTestTag                = "KEPLOY_BPF_GRACE_TEST_TAG"
	GraceDirPrefix                 = "grace-"
	GraceTestDirPrefix             = "grace-test-"
	DefaultGracePeriod             = 15 * time.Minute
	DefaultReapInterval            = 3 * time.Minute
	DefaultConfirmGap              = 1 * time.Minute
	MinProdGracePeriod             = 15 * time.Minute
	MaxProdGracePeriod             = 24 * time.Hour
	MinProdConfirmGap              = 1 * time.Minute
	MaxProdConfirmGap              = 30 * time.Minute
)

var (
	ErrGraceNotArmed = errors.New("bpf trampoline grace pinning is not armed")
	ErrShuttingDown  = errors.New("bpf trampoline safety gate is closed for shutdown")
	ErrPinFailed     = errors.New("bpf trampoline grace pin failed")
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

type MetricsObserver func(dirsPresent, pinnedProgs int, unprotected bool)

func DetectMode(daemonSet bool) Mode                                        { return ModeNative }
func SetMetricsObserver(fn MetricsObserver)                                 {}
func UnprotectedTrampolinesActive() bool                                    { return false }
func ActiveGraceMode() string                                               { return "" }
func Armed() bool                                                           { return false }
func ActiveGraceDir() string                                                { return "" }
func RecordPrivateOverlayDev(dev uint64)                                    {}
func CaptureHostBPFFSDev() (uint64, bool)                                   { return 0, false }
func ValidateHostBPFFSPath(path string, checkContainerHostMount bool) error { return nil }
func SetBypassForTest(tb TestCleaner)                                       {}
func AllowShortGraceForTest(tb TestCleaner)                                 {}
func ResetShutdownForTest(tb TestCleaner)                                   {}
func ResetUnprotectedForTest(tb TestCleaner)                                {}
func HasActiveWork() bool                                                   { return false }
func Init(ctx context.Context, logger *zap.Logger, m Mode) error            { return nil }
func RegisterDetachHook(fn func())                                          {}
func GracefulShutdown(ctx context.Context, logger *zap.Logger)              {}
