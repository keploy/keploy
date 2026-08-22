//go:build windows && amd64

// Package winshim implements Keploy's Windows-native traffic interception.
//
// Windows has no eBPF. Keploy previously reached for a kernel packet filter
// here, which meant a native run needed Administrator and simply refused to
// start without it. This package replaces that: it loads a small DLL
// (shim/keploy_winshim.c) into the application under test and hooks Winsock in
// user space. The shim asks this package, over a named pipe, what to do with
// each outgoing connection, each server bind, and each name it cannot resolve.
// The answers reproduce what the eBPF hooks provide on Linux and the dylib shim
// provides on macOS — egress redirected to the proxy with the original
// destination recoverable by source port, the app's listener moved so Keploy can
// own the port it advertises — so the entire existing proxy, TLS and
// mock-matching stack runs unchanged.
//
// Nothing here needs elevation: no driver is loaded, nothing is installed
// system-wide, and the blast radius is a single process tree — only a process
// the client injected, or one started by such a process, is ever affected.
package winshim

import (
	"fmt"
	"os"
	"path/filepath"
)

// The control protocol spoken over the named pipe by shim/keploy_winshim.c. One
// request line per connection, one reply line, then close. Keeping it
// connection-per-call is what makes the shim side thread-safe without any
// locking, and mirrors the macOS shim's protocol verb for verb.
//
//	HELLO   <pid> <progname>                             -> OK             | BYPASS
//	CONNECT <srcPort> <ipVersion> <destIP> <destPort>    -> OK <proxyPort> | BYPASS
//	BIND    <pid> <origPort>                             -> PORT <newPort> | KEEP
//	LISTEN  <pid> <origPort> <movedPort>                 -> OK
//	DNS     <hostname>                                   -> IP <addr>      | PASS
//
// CONNECT returns the proxy port rather than reading it from the shim's
// environment because the client has to stage the shim before the agent has
// allocated any ports.
//
// HELLO is the proof that instrumentation actually reached the application.
// Without it, a run where injection silently failed is indistinguishable from an
// application that simply made no dependency calls: the run goes green with zero
// mocks and nothing explains why.
//
// BIND and LISTEN are how record mode captures incoming requests. A kernel
// filter can leave the application on its advertised port and redirect inbound
// packets; user space has no such lever, so the application's server bind is
// moved to a port the agent picks and Keploy takes over the advertised one.
// LISTEN exists because at bind time a server socket and a client that merely
// pinned an explicit source port are indistinguishable; the ingress event is
// published only once the socket actually listens.
//
// DNS supplies an address for a name the application could not resolve, so that
// a replay against a dependency that no longer exists reaches connect() — where
// a mock can answer — instead of failing inside the resolver.
//
// Anything unrecognised is answered with BYPASS/KEEP/PASS semantics so that a
// version-skewed shim degrades to leaving the application's traffic alone rather
// than breaking it.
const (
	CmdHello   = "HELLO"
	CmdConnect = "CONNECT"
	CmdBind    = "BIND"
	CmdListen  = "LISTEN"
	CmdDNS     = "DNS"

	ReplyOK     = "OK"
	ReplyBypass = "BYPASS"
	ReplyKeep   = "KEEP"
	ReplyPort   = "PORT"
	ReplyPass   = "PASS"
	ReplyIP     = "IP"
)

// Environment variables read by the shim. They are optional: the control pipe
// name is carried in a sidecar file next to the DLL (see StageShim), because a
// process is free to hand its children a hand-built environment block, which
// would strip these and silently un-instrument everything below it.
const (
	// EnvShimPipe overrides the control pipe name from the sidecar file.
	EnvShimPipe = "KEPLOY_SHIM_PIPE"
	// EnvShimDebug turns on the shim's tracing.
	EnvShimDebug = "KEPLOY_SHIM_DEBUG"
	// EnvShimLog is where the shim writes that tracing.
	EnvShimLog = "KEPLOY_SHIM_LOG"
)

// EnvDisable turns interception off entirely, launching the application
// uninstrumented. An escape hatch, not a supported mode — see Enabled.
const EnvDisable = "KEPLOY_WINDOWS_NO_INTERCEPT"

// Fixed names within a session directory, so the agent and the client can both
// derive every path from the client PID alone with no handshake between the two
// processes.
const (
	shimFileName = "keploy_winshim.dll"
	// shimConfName is read by the shim itself; keep it in sync with
	// KEPLOY_SHIM_CONF in shim/keploy_winshim.c.
	shimConfName = "keploy_shim.conf"
	shimLogName  = "keploy_shim.log"
)

// SessionDir returns the per-run directory holding the staged shim and its
// sidecar configuration.
//
// It is keyed on the PID of the keploy CLIENT process, which is the one value
// every side already knows at the moment it needs it: the client has it from
// os.Getpid() while it is still preparing the application launch, and the agent
// receives the same number as --client-pid. Ports cannot serve this role — the
// client must stage the shim before the agent has allocated its proxy port.
//
// Unlike macOS, both processes here run as the same user (the Windows agent is
// never elevated — see pkg/platform/http/utils/elevate.go), so the per-user
// temporary directory is a safe and correct root for both.
func SessionDir(clientPID uint32) string {
	return filepath.Join(os.TempDir(), fmt.Sprintf("keploy-native-%d", clientPID))
}

// EnsureSessionDir creates the session directory and returns it.
func EnsureSessionDir(clientPID uint32) (string, error) {
	dir := SessionDir(clientPID)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return "", fmt.Errorf("failed to create the keploy session directory %s: %w", dir, err)
	}
	return dir, nil
}

// ControlPipeName returns the named pipe the shim talks to.
//
// Named pipes live in a flat, machine-wide namespace, so the client PID is what
// keeps concurrent keploy runs from colliding on each other's control plane.
func ControlPipeName(clientPID uint32) string {
	return fmt.Sprintf(`\\.\pipe\keploy-shim-%d`, clientPID)
}

// ShimPath returns the staged DLL inside a session directory.
func ShimPath(sessionDir string) string { return filepath.Join(sessionDir, shimFileName) }

// ShimConfPath returns the sidecar configuration inside a session directory.
func ShimConfPath(sessionDir string) string { return filepath.Join(sessionDir, shimConfName) }

// ShimLogPath returns the shim's debug log inside a session directory.
func ShimLogPath(sessionDir string) string { return filepath.Join(sessionDir, shimLogName) }
