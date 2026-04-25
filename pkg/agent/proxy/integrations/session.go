package integrations

import (
	"net"

	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// RecordSession bundles all resources a parser needs during record
// mode. It replaces the previous pattern of passing individual
// net.Conn parameters plus smuggling errgroup, connection IDs, and
// other values through context.
//
// Connections are SafeConn wrappers where Close() and SetDeadline()
// are no-ops. The proxy retains the real connections and manages
// their lifecycle.
type RecordSession struct {
	// Ingress is the client-side connection (SafeConn).
	// Read: client requests. Write: server responses back to client.
	Ingress net.Conn

	// Egress is the destination-side connection (SafeConn).
	// Read: server responses. Write: client requests forwarded to server.
	Egress net.Conn

	// Mocks channel for sending recorded mock objects.
	Mocks chan<- *models.Mock

	// ErrGroup for managing parser goroutines. Previously passed via
	// context.Value(models.ErrGroupKey). Currently also injected into
	// the parser context via context.WithValue in proxy.go so that
	// ReadFromPeer and other utilities can retrieve it. Retained here
	// as part of the planned migration to pass it explicitly through
	// RecordSession rather than via context.
	ErrGroup *errgroup.Group

	// TLSUpgrader provides controlled mid-stream TLS upgrade for
	// parsers that need it (PostgreSQL SSLRequest, MySQL CLIENT_SSL).
	// nil for parsers that don't do mid-stream TLS.
	TLSUpgrader models.TLSUpgrader

	// Logger is pre-configured with connection-scoped fields
	// (client conn ID, dest conn ID, addresses).
	Logger *zap.Logger

	// ClientConnID identifies the client connection. Previously
	// passed via context.Value(models.ClientConnectionIDKey).
	ClientConnID string

	// DestConnID identifies the destination connection. Previously
	// passed via context.Value(models.DestConnectionIDKey).
	DestConnID string

	// Opts contains protocol-specific options (bypass rules,
	// passwords, TLS keys, noise config, etc.).
	Opts models.OutgoingOptions

	// OnMockRecorded runs against each newly created mock before it is stored.
	// It lets a wrapper parser annotate a mock produced by a shared parser —
	// for example, an enterprise parser that reuses HTTP recording can add
	// protocol-specific metadata without teaching the OSS HTTP parser about
	// that protocol.
	OnMockRecorded PostRecordHook

	// V2 exposes the new FakeConn-based session (see
	// pkg/agent/proxy/supervisor.Session) when the connection is being
	// served through the supervisor + relay architecture. A migrated
	// parser checks for a non-nil V2 and reads from V2.ClientStream /
	// V2.DestStream and sends directives via V2.Directives rather than
	// touching the legacy Ingress / Egress / TLSUpgrader fields.
	// Un-migrated parsers may ignore V2 entirely and continue using
	// the legacy fields; the dispatcher only routes through supervisor
	// for parsers that implement [IntegrationsV2].
	V2 *supervisor.Session
}

// PostRecordHook is invoked after a shared parser produces a mock and before
// the mock is handed off for storage. Wrapper parsers that layer on top of a
// shared parser (for example the Enterprise SQS parser which delegates
// recording to the OSS HTTP parser) use this hook to annotate or reshape the
// mock without teaching the shared parser about downstream protocols.
//
// Call RecordSession.AddPostRecordHook rather than assigning to
// OnMockRecorded directly — the helper preserves any hook already installed
// by an outer parser, which is the usual chaining contract.
type PostRecordHook func(*models.Mock)

// AddPostRecordHook adds h to the front of the session's post-record chain
// so h runs before any previously-installed hook. The previously-installed
// hook (if any) then observes the mock already annotated by h and can layer
// its own annotations on top without clobbering them.
//
// Calling with a nil hook, or on a nil *RecordSession, is a no-op. Making
// the nil-receiver case safe lets defensive call sites drop their own nil
// guard before invoking AddPostRecordHook.
//
// This helper exists to turn the "capture prev, call prev after" pattern
// into one call. Direct assignment to OnMockRecorded is still legal for
// cases that deliberately replace the chain (e.g. tests), but parser code
// should use the helper so future third-party parsers composing with the
// same shared recorder do not have to reimplement the chain contract and
// risk silently dropping prior annotations.
func (s *RecordSession) AddPostRecordHook(h PostRecordHook) {
	if s == nil || h == nil {
		return
	}
	prev := s.OnMockRecorded
	if prev == nil {
		s.OnMockRecorded = h
		return
	}
	s.OnMockRecorded = func(m *models.Mock) {
		h(m)
		prev(m)
	}
}
