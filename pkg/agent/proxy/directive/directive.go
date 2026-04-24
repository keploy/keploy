package directive

import (
	"crypto/tls"
	"errors"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// Kind identifies the directive's operation.
type Kind uint8

const (
	// KindUpgradeTLS requests that the relay pause byte forwarding
	// in both directions and perform TLS upgrades on the real
	// sockets. Payload: [*UpgradeTLSParams]. Ack carries the
	// boundary timestamps and success/error.
	KindUpgradeTLS Kind = iota + 1

	// KindPauseDir asks the relay to stop feeding chunks of Dir
	// into the parser's FakeConn without dropping the real
	// connection. Used rarely — most pause needs are implicit via
	// TLS upgrade.
	KindPauseDir

	// KindResumeDir resumes a previously paused direction.
	KindResumeDir

	// KindAbortMock tells the supervisor the parser is giving up
	// on its current mock. The supervisor discards partial state
	// and leaves the relay running so user traffic continues.
	// Parsers use this when they hit a state they cannot continue
	// decoding but the connection itself is healthy.
	KindAbortMock

	// KindFinalizeMock signals the supervisor that a mock is
	// complete and may be handed off to storage. Used by parsers
	// that batch multi-frame mocks and want an explicit commit
	// point rather than relying on EmitMock-per-frame.
	KindFinalizeMock
)

// String returns a short label for logging.
func (k Kind) String() string {
	switch k {
	case KindUpgradeTLS:
		return "upgrade-tls"
	case KindPauseDir:
		return "pause"
	case KindResumeDir:
		return "resume"
	case KindAbortMock:
		return "abort-mock"
	case KindFinalizeMock:
		return "finalize-mock"
	default:
		return "unknown"
	}
}

// UpgradeTLSParams carries the TLS configuration for a
// [KindUpgradeTLS] directive.
//
// The relay processes the two configs in order: DestTLSConfig first
// (keploy acts as TLS client to the real destination), then
// ClientTLSConfig (keploy acts as TLS server to the real client,
// presenting the MITM cert). If either side's handshake fails, the
// ack's OK is false and the parser is expected to accept that the
// rest of the connection becomes passthrough.
//
// A nil config for either side means "do not upgrade that side" —
// useful if the parser knows one peer is already on TLS or does
// not require symmetric upgrade.
type UpgradeTLSParams struct {
	DestTLSConfig   *tls.Config
	ClientTLSConfig *tls.Config
}

// Directive is a control message from a parser to the supervisor /
// relay. Exactly one of the payload fields is populated, determined
// by Kind. Unpopulated payloads are ignored.
type Directive struct {
	Kind   Kind
	TLS    *UpgradeTLSParams
	Dir    fakeconn.Direction // used by KindPauseDir / KindResumeDir
	Reason string             // free-form; written to telemetry and logs
}

// Ack is the supervisor → parser response to a Directive.
//
// OK is true if the directive was carried out successfully. For
// KindUpgradeTLS, BoundaryReadAt and BoundaryWrittenAt are
// relay-observed upgrade-boundary timestamps: BoundaryReadAt is
// captured just BEFORE the handshakes begin (so it bounds the last
// instant any cleartext Read could have returned), and
// BoundaryWrittenAt is captured AFTER both handshakes succeed (so
// it bounds the first instant any TLS Write can land). Parsers
// MAY record these in prelude mocks for boundary analysis but
// must not treat them as the exact microsecond of transition on
// either socket — the TCP stack has no hook that would let the
// relay observe that instant, and the handshake itself is a
// multi-roundtrip sequence that takes real time. What the ack
// guarantees is: "everything after this ack on
// ClientStream/DestStream is plaintext from the upgraded session."
//
// On failure (OK=false), Err is populated with the root cause.
// The directive package itself implies no particular follow-on
// action: the relay returns this Ack, and higher layers decide
// whether to mark the mock incomplete, propagate the error, retry,
// continue forwarding, or do something else. Any supervisor,
// dispatcher, fallthrough, cancellation, or subsequent stream
// closure/read behavior is implementation-specific and must not be
// inferred from OK=false alone.
type Ack struct {
	Kind              Kind
	OK                bool
	Err               error
	BoundaryReadAt    time.Time
	BoundaryWrittenAt time.Time
}

// ErrDirectiveClosed is returned from helpers that try to send on a
// directive channel after the supervisor has been torn down. Parsers
// seeing this should return without emitting further mocks.
var ErrDirectiveClosed = errors.New("directive: channel closed")

// Helpers to construct common directives. Keep call sites short in
// parsers and centralize any future validation.

// UpgradeTLS returns a [KindUpgradeTLS] directive. Either config may
// be nil to skip that side.
func UpgradeTLS(dest, client *tls.Config, reason string) Directive {
	return Directive{
		Kind:   KindUpgradeTLS,
		TLS:    &UpgradeTLSParams{DestTLSConfig: dest, ClientTLSConfig: client},
		Reason: reason,
	}
}

// AbortMock returns a [KindAbortMock] directive.
func AbortMock(reason string) Directive {
	return Directive{Kind: KindAbortMock, Reason: reason}
}

// FinalizeMock returns a [KindFinalizeMock] directive.
func FinalizeMock(reason string) Directive {
	return Directive{Kind: KindFinalizeMock, Reason: reason}
}

// Pause returns a [KindPauseDir] directive for the given direction.
func Pause(d fakeconn.Direction, reason string) Directive {
	return Directive{Kind: KindPauseDir, Dir: d, Reason: reason}
}

// Resume returns a [KindResumeDir] directive for the given direction.
func Resume(d fakeconn.Direction, reason string) Directive {
	return Directive{Kind: KindResumeDir, Dir: d, Reason: reason}
}
