// Package cbshim defines the interface for the SCRAM-SHA-256-PLUS
// channel-binding shim. The implementation registers itself at init()
// time via RegisterFactory — this package carries only the interface
// and the factory hook so the proxy can depend on cbshim without
// pulling in any eBPF code or BPF artifacts.
//
// When no implementation is registered, NewFromFactory returns
// (nil, nil) and the proxy operates exactly as it did before cbshim
// existed: SCRAM-SHA-256-PLUS clients connecting through keploy's TLS
// MITM will fail with "SCRAM channel binding check failed". Users who
// need PLUS support must run a build that registers a CBShim
// implementation AND set config.ChannelBindingShim to true
// (top-level `channelBindingShim: true` in keploy.yml — the flag
// governs both record AND test/replay symmetrically). The agent
// subprocess receives the same value via the hidden
// --channel-binding-shim flag the orchestrator forwards on argv.
package cbshim

import (
	"context"
	"crypto/x509"
	"errors"

	"go.uber.org/zap"
)

// CBShim is the subset of the channel-binding shim's behaviour the
// proxy actually consumes. Keep this surface minimal — a concrete
// implementation may carry additional methods (Counters,
// AttachToProcess, RegisterLibpqRanges, etc.), but the proxy only
// needs the cert-rendezvous + lifecycle pair below.
//
// All methods MUST be safe to call on a freshly-constructed value;
// the proxy never calls Close before the factory finishes.
type CBShim interface {
	// RegisterMITM accepts the MITM-cert DER for connID. The
	// implementation pairs it with the matching real-cert DER from
	// RegisterReal whichever arrives later and publishes the hash
	// pair into the BPF map that drives X509_digest substitution.
	RegisterMITM(connID string, mitmDER []byte)

	// RegisterReal accepts the upstream real-cert DER + signature
	// algorithm for connID. Counterpart to RegisterMITM; either side
	// may arrive first.
	RegisterReal(connID string, realDER []byte, sigAlgo x509.SignatureAlgorithm)

	// CleanupConnection releases any half-arrived (mitm OR real)
	// rendezvous state for connID. Called when a connection ends
	// before the second half arrives.
	CleanupConnection(connID string)

	// AttachToProcessTree performs a one-shot scan of rootPID and its
	// descendants for libcrypto/libpq mappings and attaches uprobes.
	// Subsequent late-loaded libraries are handled by the
	// implementation's own BPF-side discovery hook (see
	// StartProcEventConsumer below).
	AttachToProcessTree(rootPID int) error

	// StartProcEventConsumer spawns the ringbuf consumer goroutine
	// that listens for library-mmap events from the BPF program and
	// attaches uprobes on demand. Idempotent — subsequent calls after
	// the first are no-ops.
	StartProcEventConsumer(ctx context.Context)

	// Close releases BPF resources and detaches uprobes. After Close
	// the value MUST NOT be used.
	Close() error
}

// Factory constructs a CBShim. Registered once at init() time by the
// implementation package; nil otherwise, in which case NewFromFactory
// returns (nil, nil).
type Factory func(logger *zap.Logger) (CBShim, error)

var registeredFactory Factory

// RegisterFactory installs the concrete cbshim constructor. Intended
// to be called from init() in the implementation package. Calling
// twice silently overwrites — the last registration wins. Not safe
// for concurrent registration, but init() ordering makes that a
// non-issue in practice.
func RegisterFactory(f Factory) {
	registeredFactory = f
}

// NewFromFactory invokes the registered factory if one was installed.
// Returns (nil, nil) when no factory is registered — that signals
// "no cbshim available" to the caller, which must keep working
// without it.
//
// Contract: a REGISTERED factory must return either (impl, nil) on
// success or (nil, err) on construction failure. (nil, nil) from a
// registered factory is treated as an error here, so callers don't
// observe an ambiguous "no factory registered" state that's actually
// "factory ran but produced nothing", and the proxy.New log branch
// that points users at OSS-vs-enterprise stays accurate.
func NewFromFactory(logger *zap.Logger) (CBShim, error) {
	if registeredFactory == nil {
		return nil, nil
	}
	cb, err := registeredFactory(logger)
	if err == nil && cb == nil {
		return nil, errors.New("cbshim: registered factory returned (nil, nil); a registered factory must produce a non-nil implementation or a non-nil error")
	}
	return cb, err
}
