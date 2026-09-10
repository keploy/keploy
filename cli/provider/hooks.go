package provider

// Runtime extension points for builds that wrap this one.
//
// Same shape as pkg/agent/runtime_hooks.go and pkg/client/app/hooks.go: a
// package-level default plus a Register function, set from an importing
// package's init(). Go guarantees those complete before main, and the CLI reads
// them on a single goroutine, so they are read without synchronization —
// anything that installs one lazily (from a goroutine, or a PersistentPreRun)
// would be an unsynchronized write and is not supported.

import "runtime"

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
// ship in the Community and Enterprise editions rather than here. Those builds
// widen this predicate from their own init() via RegisterNativeCommandSupport.
// In this build both resolve to the pkg/agent/hooks/others stub, whose Load
// returns "eBPF hooks are not supported on non-Linux platforms".
//
// Rejecting a native command up front is deliberate: it produces a clear
// message naming the editions that can do it, instead of that stub's confusing
// eBPF error much later in the run.
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
