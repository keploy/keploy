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
// implementation AND set config.Record.ChannelBindingShim to true
// (record.channelBindingShim in keploy.yml — nested YAML, i.e.
// `record:\n  channelBindingShim: true`); the agent subprocess
// receives the same value via the hidden --channel-binding-shim flag
// the orchestrator forwards on argv.
package cbshim

import (
	"context"
	"crypto/x509"
	"errors"
	"fmt"

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

// Factory constructs a CBShim. Registered at init() time by each
// implementation package; the package supports multiple registered
// factories so distinct shim backends (e.g. eBPF uprobe + LD_PRELOAD)
// can coexist in a single binary and each handle the cases the other
// can't — eBPF works regardless of how libcrypto was loaded but needs
// CAP_BPF + kernel support; LD_PRELOAD works without kernel support
// but only against binaries that consult ld.so. With both registered,
// RegisterMITM/RegisterReal etc. fan out to every impl via the
// composite returned by NewFromFactory.
type Factory func(logger *zap.Logger) (CBShim, error)

var registeredFactories []Factory

// RegisterFactory installs a concrete cbshim constructor. Called from
// init() in each implementation package; subsequent calls APPEND
// (they do not overwrite, in contrast to the original single-factory
// behaviour this replaces) so that blank-importing multiple impl
// packages from a binary's main wires all of them.
//
// Not safe for concurrent registration, but init() ordering makes
// that a non-issue in practice.
func RegisterFactory(f Factory) {
	if f == nil {
		return
	}
	registeredFactories = append(registeredFactories, f)
}

// NewFromFactory invokes every registered factory and returns either
// the single impl (when one factory is registered) or a composite
// CBShim that fans out every call to all sub-impls (when more than
// one is registered). Returns (nil, nil) when no factory is registered
// — that signals "no cbshim available" to the caller, which must
// keep working without it.
//
// Contract per factory: each REGISTERED factory must return either
// (impl, nil) on success or (nil, err) on construction failure.
// (nil, nil) from a registered factory is treated as an error so
// callers don't observe an ambiguous "no factory registered" state
// that's actually "factory ran but produced nothing". A construction
// error from any one factory aborts the whole NewFromFactory call —
// the OSS proxy.New log branch that points users at
// OSS-vs-enterprise stays accurate.
func NewFromFactory(logger *zap.Logger) (CBShim, error) {
	if len(registeredFactories) == 0 {
		return nil, nil
	}
	if len(registeredFactories) == 1 {
		// Preserve the original single-factory return path verbatim —
		// no composite wrapper, callers that pre-date multi-factory
		// observe the same concrete type / behaviour as before.
		cb, err := registeredFactories[0](logger)
		if err == nil && cb == nil {
			return nil, errors.New("cbshim: registered factory returned (nil, nil); a registered factory must produce a non-nil implementation or a non-nil error")
		}
		return cb, err
	}
	impls := make([]CBShim, 0, len(registeredFactories))
	for i, f := range registeredFactories {
		cb, err := f(logger)
		if err != nil {
			return nil, fmt.Errorf("cbshim: factory %d construction failed: %w", i, err)
		}
		if cb == nil {
			return nil, fmt.Errorf("cbshim: factory %d returned (nil, nil); each registered factory must produce a non-nil implementation or a non-nil error", i)
		}
		impls = append(impls, cb)
	}
	return &composite{impls: impls}, nil
}

// composite fans every CBShim method out to a slice of underlying
// impls. Used by NewFromFactory when more than one factory is
// registered. Each impl maintains its own internal state + actuator
// (eBPF impl publishes into a BPF map; LD_PRELOAD impl publishes
// into a file/shm read by its .so), so calling RegisterMITM /
// RegisterReal on all of them is the correct semantics — whichever
// path the target process exposes is the one that takes effect.
//
// AttachToProcessTree and StartProcEventConsumer also fan out, but
// the LD_PRELOAD impl typically no-ops them (it doesn't attach
// uprobes; it ships a .so the loader picks up). Each impl is
// responsible for being a no-op when its mechanism doesn't apply.
//
// Close is best-effort: every impl's Close is invoked even when an
// earlier one returns an error, so a failing impl can't strand
// another impl's resources. The first error is returned; subsequent
// errors are dropped (they'd be redundant with the first in
// practice, since Close failures share a root cause — process
// shutting down, etc.).
type composite struct {
	impls []CBShim
}

func (c *composite) RegisterMITM(connID string, mitmDER []byte) {
	for _, i := range c.impls {
		i.RegisterMITM(connID, mitmDER)
	}
}

func (c *composite) RegisterReal(connID string, realDER []byte, sigAlgo x509.SignatureAlgorithm) {
	for _, i := range c.impls {
		i.RegisterReal(connID, realDER, sigAlgo)
	}
}

func (c *composite) CleanupConnection(connID string) {
	for _, i := range c.impls {
		i.CleanupConnection(connID)
	}
}

func (c *composite) AttachToProcessTree(rootPID int) error {
	// Fan out; collect errors. An impl that doesn't attach to
	// processes (LD_PRELOAD) should return nil — it's not a failure
	// to have nothing to attach.
	var firstErr error
	for i, impl := range c.impls {
		if err := impl.AttachToProcessTree(rootPID); err != nil {
			if firstErr == nil {
				firstErr = fmt.Errorf("cbshim: impl %d AttachToProcessTree: %w", i, err)
			}
		}
	}
	return firstErr
}

func (c *composite) StartProcEventConsumer(ctx context.Context) {
	for _, i := range c.impls {
		i.StartProcEventConsumer(ctx)
	}
}

func (c *composite) Close() error {
	var firstErr error
	for i, impl := range c.impls {
		if err := impl.Close(); err != nil && firstErr == nil {
			firstErr = fmt.Errorf("cbshim: impl %d Close: %w", i, err)
		}
	}
	return firstErr
}
