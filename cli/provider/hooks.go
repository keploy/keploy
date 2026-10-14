package provider

// Runtime extension points for builds that wrap this one.
//
// Same shape as pkg/agent/runtime_hooks.go and pkg/client/app/hooks.go: a
// package-level default plus a Register function, set from an importing
// package's init(). Go guarantees those complete before main, and the CLI reads
// them on a single goroutine, so they are read without synchronization —
// anything that installs one lazily (from a goroutine, or a PersistentPreRun)
// would be an unsynchronized write and is not supported.

import (
	"fmt"
	"runtime"
)

// NativeCommandSupported reports whether this build can instrument an
// application running directly on the host, as opposed to requiring the app to
// run in Docker. ValidateFlags consults it to decide whether to accept a
// non-docker command at all.
//
// Install a wider predicate with RegisterNativeCommandSupport.
var NativeCommandSupported = DefaultNativeCommandSupported

// DefaultNativeCommandSupported is the set of platforms with an in-tree
// interception backend: eBPF on Linux (pkg/agent/hooks/linux), and nothing
// else.
//
// macOS and Windows have no eBPF, and their userspace interception backends
// ship in Keploy as installed from keploy.io rather than here. That build
// widens this predicate from its own init() via RegisterNativeCommandSupport.
// In this build both resolve to the pkg/agent/hooks/others stub, whose Load
// returns "eBPF hooks are not supported on non-Linux platforms".
//
// Rejecting a native command up front is deliberate: nativeUnsupportedError
// says what does work on the platform, instead of that stub's confusing eBPF
// error much later in the run.
//
// goos and goarch are parameters rather than reads of runtime.GOOS/GOARCH so
// the policy can be tested for every platform from any host.
func DefaultNativeCommandSupported(goos, _ string) bool {
	return goos == "linux"
}

// RegisterNativeCommandSupport installs the predicate used to decide whether a
// non-docker command is accepted. A build shipping an interception backend this
// module does not know about (for example a macOS one) calls this from init()
// to widen the platform set, normally by delegating to
// DefaultNativeCommandSupported and adding its own.
//
// A nil fn restores the default.
func RegisterNativeCommandSupport(fn func(goos, goarch string) bool) {
	if fn == nil {
		NativeCommandSupported = DefaultNativeCommandSupported
		return
	}
	NativeCommandSupported = fn
}

// nativeCommandSupportedHere applies the installed predicate to the platform
// this binary is actually running on.
func nativeCommandSupportedHere() bool {
	return NativeCommandSupported(runtime.GOOS, runtime.GOARCH)
}

// nativeUnsupportedError is ValidateFlags' refusal of a non-docker command on
// a platform whose predicate said no. It says what does work there rather than
// only stating the refusal, and it depends on the platform because the answer
// does: every build that wraps this one shares this path, including ones that
// run natively on macOS and Windows but not on every architecture of them.
//
// User-facing text names no editions: the product is just "keploy".
func nativeUnsupportedError(goos, goarch string) error {
	const docker = "  - Or run your application in Docker: keploy record -c \"docker run ...\""
	switch {
	// Docker is offered here, unlike in the installer's Intel-Mac refusal. The
	// installer turns an Intel Mac away because its only macOS CLI is arm64;
	// whoever sees THIS message is already running an amd64 CLI (a source
	// build, such as homebrew-core's), which can drive the Docker route.
	case goos == "darwin" && goarch != "arm64":
		return fmt.Errorf("running an application directly on %s/%s is not supported: Keploy records apps running natively on a Mac only on Apple Silicon.\n\n"+
			"  - On an Intel Mac, run Keploy inside Lima: https://keploy.io/docs/installation/macos-installation/#option-2-install-keploy-with-lima\n"+
			docker, goos, goarch)
	case goos == "windows" && goarch != "amd64":
		return fmt.Errorf("running an application directly on %s/%s is not supported: Keploy records apps running natively on Windows only on x86-64.\n\n"+
			"  - On Windows on ARM, run Keploy inside WSL: https://keploy.io/docs/keploy-explained/windows-wsl/",
			goos, goarch)
	case goos == "darwin" || goos == "windows":
		return fmt.Errorf("running an application directly on %s/%s is not supported by this build of Keploy, which intercepts traffic with eBPF (Linux only).\n\n"+
			"  - To record and replay an app running natively on macOS or Windows, install Keploy from https://keploy.io/docs/server/installation/ -- it runs natively there.\n"+
			docker, goos, goarch)
	default:
		return fmt.Errorf("running an application directly on %s/%s is not supported: Keploy records apps running natively on Linux, macOS (Apple Silicon) and Windows (x86-64).\n\n"+
			docker, goos, goarch)
	}
}
