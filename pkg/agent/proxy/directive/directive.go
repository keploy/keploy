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
// presenting the MITM cert). A handshake failure is reported via the
// ack (OK=false, Err populated); how the caller handles that — mark
// the mock incomplete, fall through to raw passthrough, retry,
// propagate the error — is a higher-layer policy choice that the
// directive package itself does not dictate. See the Ack doc below
// for the general contract.
//
// A nil config for either side means "do not upgrade that side" —
// useful if the parser knows one peer is already on TLS or does
// not require symmetric upgrade.
//
// PreambleReadFromDest, PreambleForwardToSrc, and ProceedOnPreamble
// support synchronous protocol-preamble exchanges that race against
// the relay's pause barrier. The Postgres SSL choreography is the
// canonical case: client→SSLRequest, server→one of {'S','N'}, client
// →TLS-ClientHello (immediately after seeing 'S'). The legacy parser-
// owned path read the SSLResponse byte off the real dest socket and
// did the TLS handshake atomically; the V2 relay forwarder reads
// continuously from the real client, so by the time the parser sees
// 'S' on its FakeConn the client may already have written TLS bytes
// that the C2D forwarder forwards as plaintext to the server. That
// in turn corrupts the upstream wire — the server processes part of
// the client's ClientHello and then keploy's tls.Client.Handshake
// sends a second ClientHello, which the server rejects ("bad key
// share" / "tls: illegal parameter"). The race is fundamental to
// "forwarder reads autonomously" + "parser drives upgrade decisions"
// without a synchronous handshake op in the directive contract.
//
// When PreambleReadFromDest > 0, the relay (under the pause barrier)
// reads exactly that many bytes from the real dest socket directly
// (bypassing the C2D/D2C forwarders' tee), forwards them to the real
// src socket if PreambleForwardToSrc is true, and then decides:
//
//   - If ProceedOnPreamble is empty, always proceed with the TLS
//     handshakes specified by DestTLSConfig / ClientTLSConfig.
//   - If ProceedOnPreamble is non-empty and the read bytes match it
//     byte-for-byte, proceed; otherwise return OK=true with no
//     handshake performed (Ack.PreamblePayload populated, Ack.TLS
//     Upgraded=false). The caller distinguishes "preamble matched
//     and TLS is up" from "preamble did not match and TLS was
//     skipped" via Ack.TLSUpgraded.
//
// The Ack also reports Ack.PreamblePayload so the parser can record
// what the server actually said in its mock. Errors at any stage
// (Read on dest, Write on src, mismatched length) surface via OK=false
// just like a handshake failure.
type UpgradeTLSParams struct {
	DestTLSConfig   *tls.Config
	ClientTLSConfig *tls.Config

	// PreambleReadFromDest, when > 0, instructs the relay to read
	// exactly that many bytes from the real destination socket
	// before any TLS handshake. Reads use the live socket directly
	// and bypass the parser's FakeConns; the bytes are returned to
	// the caller via Ack.PreamblePayload regardless of forwarding.
	PreambleReadFromDest int

	// PreambleForwardToSrc, when true, asks the relay to write the
	// preamble bytes back to the real source socket before the TLS
	// handshakes begin. Used by the Postgres parser to deliver the
	// SSLResponse byte ('S' / 'N') to the real client without ever
	// letting the C2D forwarder pick up the client's reply (TLS
	// ClientHello after 'S') as cleartext.
	PreambleForwardToSrc bool

	// ProceedOnPreamble, when non-empty, gates the TLS handshakes on
	// an exact byte-for-byte match against the read preamble. A
	// mismatch is not an error — the relay returns OK=true with
	// TLSUpgraded=false so the parser can adapt (e.g. Postgres 'N'
	// means the server declined SSL and the rest of the session
	// stays cleartext).
	ProceedOnPreamble []byte
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

	// PreamblePayload carries the bytes the relay read from the real
	// destination socket when [UpgradeTLSParams.PreambleReadFromDest]
	// was > 0. Populated whether or not the TLS handshake actually
	// ran; an empty slice means no preamble read was requested.
	PreamblePayload []byte

	// TLSUpgraded reports whether the relay actually performed a TLS
	// handshake. True when both (or only the requested) sides
	// completed; false when the preamble gate
	// [UpgradeTLSParams.ProceedOnPreamble] short-circuited (Postgres
	// 'N' decline) — the directive still acks OK=true in that case
	// because the caller's contract was satisfied; the parser uses
	// PreamblePayload to interpret the outcome.
	TLSUpgraded bool
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
