package util

import (
	"fmt"
	"os"
	"strings"
	"sync"
	"sync/atomic"
)

// envDisableParsing is the environment variable consulted by
// [NewFromEnv]. Exported only as a constant so tests and admin
// tooling can reference it without hard-coding the string.
const envDisableParsing = "KEPLOY_DISABLE_PARSING"

// KillSwitch is a process-wide flag that disables parser dispatch
// in record mode. When Enabled returns true, the proxy's dispatcher
// should route new connections straight to raw passthrough and skip
// all parser matching and recording.
//
// The switch is tripped by any of:
//   - env var KEPLOY_DISABLE_PARSING set to "1"/"true"/"yes" at process start
//   - SIGUSR1 delivered to the process (Unix only)
//   - Trip() called explicitly (for admin HTTP endpoints)
//
// Reset() clears the flag (useful for tests and for admin endpoints
// that want to re-enable parsing without a restart).
//
// The default KillSwitch is [DefaultKillSwitch]. Tests may construct
// their own via [New].
type KillSwitch struct {
	tripped    atomic.Bool
	signalOnce sync.Once
}

// DefaultKillSwitch is the process-wide instance consulted by the
// proxy dispatcher. It reads the env var on package init and
// installs a SIGUSR1 handler (Unix only).
var DefaultKillSwitch = NewFromEnv()

// New constructs a fresh KillSwitch in the not-tripped state. It
// does NOT read env vars or install signal handlers — use
// [NewFromEnv] for that.
func New() *KillSwitch {
	return &KillSwitch{}
}

// NewFromEnv constructs a KillSwitch, reads KEPLOY_DISABLE_PARSING
// and trips if truthy, and installs a SIGUSR1 handler on Unix.
//
// If installing the signal handler fails for any reason (the
// Unix-only path calls into os/signal which should not fail, but we
// stay defensive), the error is written to stderr and the switch is
// returned in whatever state the env var produced. Callers are not
// expected to treat startup-time signal wiring as fatal.
func NewFromEnv() *KillSwitch {
	ks := New()
	if isTruthy(os.Getenv(envDisableParsing)) {
		ks.Trip()
	}
	installSignalHandler(ks)
	return ks
}

// Enabled reports whether parsing is currently disabled.
func (k *KillSwitch) Enabled() bool {
	return k.tripped.Load()
}

// Trip sets the switch to "parsing disabled".
func (k *KillSwitch) Trip() {
	k.tripped.Store(true)
}

// Reset clears the switch.
func (k *KillSwitch) Reset() {
	k.tripped.Store(false)
}

// isTruthy reports whether s is a recognised truthy value for
// KEPLOY_DISABLE_PARSING. Comparison is case-insensitive.
func isTruthy(s string) bool {
	s = strings.TrimSpace(s)
	if s == "" {
		return false
	}
	for _, t := range truthyValues {
		if strings.EqualFold(s, t) {
			return true
		}
	}
	return false
}

var truthyValues = []string{"1", "true", "yes"}

// logSignalHandlerFailure writes a diagnostic line to stderr without
// panicking. Shared between the unix and windows implementations so
// the message format stays consistent.
func logSignalHandlerFailure(err error) {
	if err == nil {
		return
	}
	fmt.Fprintf(os.Stderr, "%s failed to install KillSwitch signal handler: %v\n", Emoji, err)
}
