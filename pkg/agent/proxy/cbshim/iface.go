// Package cbshim defines the OSS-side interface for the
// SCRAM-SHA-256-PLUS channel-binding shim. The actual eBPF
// implementation lives in the enterprise repository under
// enterprise/pkg/agent/proxy/cbshim/ and registers itself via
// RegisterFactory at init() time.
//
// The proxy package consumes cbshim only via the CBShim interface
// declared here, so the OSS build has no eBPF dependency, no BPF
// artifacts to embed, and no cbshim implementation. When the cbshim
// implementation is not registered (i.e. on an OSS-only build, or in
// an enterprise build with the feature disabled), the proxy operates
// exactly as it did before cbshim existed: SCRAM-SHA-256-PLUS clients
// connecting through keploy's TLS MITM will fail with
// "SCRAM channel binding check failed", which is the documented
// limitation for non-enterprise builds.
package cbshim

import (
	"context"
	"crypto/x509"

	"go.uber.org/zap"
)

// CBShim is the subset of the channel-binding shim's behaviour the OSS
// proxy actually consumes. Keep this surface minimal — the concrete
// implementation in enterprise has additional methods (Counters,
// AttachToProcess, RegisterLibpqRanges, etc.) but the OSS proxy only
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
	// Subsequent late-loaded libraries are handled by the BPF-side
	// discovery hook (see enterprise impl's StartProcEventConsumer).
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

// Factory constructs a CBShim. Enterprise registers one via
// RegisterFactory in an init() function; OSS builds leave it nil and
// NewFromFactory returns (nil, nil).
type Factory func(logger *zap.Logger) (CBShim, error)

var registeredFactory Factory

// RegisterFactory installs the concrete cbshim constructor. Intended
// to be called from init() in the enterprise cbshim package. Calling
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
func NewFromFactory(logger *zap.Logger) (CBShim, error) {
	if registeredFactory == nil {
		return nil, nil
	}
	return registeredFactory(logger)
}
