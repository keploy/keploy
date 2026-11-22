package models

import (
	"errors"
	"time"
)

// ErrReadinessUnsupported says keploy's agent cannot watch the application's
// readiness at all (another platform's agent, an older one, one that does not
// run keploy test): keploy test sends the first test as it is, and its own
// connect decides.
var ErrReadinessUnsupported = errors.New("keploy's agent cannot tell when the application listens")

// AppReadiness is the agent's answer to whether the application keploy test
// runs is ready, and whether its start is going on (GET
// /agent/app/readiness, under the proven switch; design v14 §8.4, row W6).
// keploy test fires its first test once the application listens where that
// test dials, and calls a start that moves none of the progress below across
// its watchdog's count a stall: its test set is not judged.
type AppReadiness struct {
	// Listens are the application's TCP listens the kernel reported (a
	// member's socket entering LISTEN), from the count asked for on (GET
	// /agent/app/readiness?since=N), one for each port, address, namespace
	// and liveness, the latest of each, in the order they came.
	Listens []AppListen `json:"listens"`
	// ListenCount is how many listens the agent was told in all: the count
	// a set's mark is (the next listen's Seq).
	ListenCount uint64 `json:"listen_count"`
	// ListensLost counts the listens the kernel had no room to report: a
	// listen the agent cannot vouch for.
	ListensLost uint64 `json:"listens_lost"`
	// Progress counts what the application's processes did: listens,
	// connects, process forks and execs.
	Progress ReadyProgress `json:"progress"`
	// CPU is the CPU time the application's processes have used, each its
	// own, as it grew (a process its pid and start time): a start that
	// computes moves it. keploy test weighs it until the start restarts
	// (Progress.Restarts).
	CPU time.Duration `json:"cpu"`
	// CPUKnown says CPU could be read.
	CPUKnown bool `json:"cpu_known"`
	// LastRestart is the program of the last restart (Progress.Restarts).
	LastRestart string `json:"last_restart,omitempty"`
}

// AppListen is one TCP listen of the application's.
type AppListen struct {
	Port uint16 `json:"port"`
	// Addr is the address it listens on ("0.0.0.0", "::" for every one).
	Addr string `json:"addr"`
	// Here says it listens in the agent's network namespace: where keploy
	// test's requests reach it (a native app's host, or the namespace a
	// container app shares with the agent).
	Here bool   `json:"here"`
	TGID uint32 `json:"tgid"`
	// Cookie is its socket's cookie (bpf_get_socket_cookie's, SO_COOKIE's).
	Cookie uint64 `json:"cookie"`
	// Seq is its place among the listens the agent was told, from 0: a set
	// whose application keploy started anew looks only at those after the
	// count it read before it started it (an earlier set's application, or
	// what outlived it, is not this one).
	Seq uint64 `json:"seq"`
	// Live says its socket still listens (the kernel's ready_live): a listen
	// whose socket has since closed (a start that probes a port by listening on it and
	// closing it, one that dies and is restarted) is no readiness.
	Live bool `json:"live"`
}

// ReadyProgress counts the application's processes' listens, connects,
// process forks and execs, each only growing.
type ReadyProgress struct {
	Listens  uint64 `json:"listens"`
	Connects uint64 `json:"connects"`
	Forks    uint64 `json:"forks"`
	Execs    uint64 `json:"execs"`
	// Restarts counts the restarts in the set: an exec of a command line
	// (its program, its command line's length and its first eight reads of
	// it, 63 bytes each) whose every earlier run in the set exited
	// (one run again while an earlier run lives, or exec'd again in place,
	// is none).
	Restarts uint64 `json:"restarts"`
}
