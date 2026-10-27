package models

import (
	"errors"
	"fmt"
	"net"
	"strconv"
	"time"
)

// A ConnectionFailure mock records that the application's attempt to open a
// connection to a dependency failed in a way the application could see: the
// connect was refused, the destination was unreachable, nothing answered, or
// the connection was closed before the application got any data back. Replay
// is meant to make the same attempt fail the same way, at the same step, in
// the same test.
//
// This file defines the format and its rules only. Nothing records the kind
// yet and nothing replays it: a keploy that reads one decodes it, validates it
// and keeps it out of everything that would treat it like an ordinary mock
// (see WindowBound and ExcludedFromDependencyAssertion), so a recording
// behaves as if the document were not there.
//
// The format is closed. A field, a phase or an outcome this keploy does not
// know, or a field whose value is not of the format's type, means the
// document was written by a newer keploy (or edited by hand): the reader
// skips it with an ERROR that says to upgrade, and a rewrite of the mock file
// copies it through unchanged.
//
// On disk (kind: ConnectionFailure) the spec is flat:
//
//	spec:
//	    metadata:
//	        type: mocks
//	    address: 127.0.0.1:5432
//	    host: db.example.com
//	    phase: connect
//	    outcome: refused
//	    cause: 'dial tcp 127.0.0.1:5432: connect: connection refused'
//	    reqTimestampMock: 2026-10-08T10:21:46.511876Z
//	    resTimestampMock: 2026-10-08T10:21:46.512001Z

// ConnFailurePhase is the step at which the connection failed, as the
// application saw it.
type ConnFailurePhase string

const (
	// ConnFailurePhaseConnect: the connect itself failed. The application
	// never had a connection.
	ConnFailurePhaseConnect ConnFailurePhase = "connect"
	// ConnFailurePhaseAccepted: the connection was established, then closed by
	// the other end before the application sent anything.
	ConnFailurePhaseAccepted ConnFailurePhase = "accepted"
	// ConnFailurePhaseTLS: the application sent its TLS ClientHello and the
	// connection was closed before the handshake completed.
	ConnFailurePhaseTLS ConnFailurePhase = "tls"
	// ConnFailurePhaseRequest: the application sent its request (over TLS,
	// after the handshake completed) and the connection was closed before any
	// byte of a response came back.
	ConnFailurePhaseRequest ConnFailurePhase = "request"
)

// ConnFailureOutcome is how the connection failed, as the application saw it.
type ConnFailureOutcome string

const (
	// ConnFailureRefused: the connect was refused (ECONNREFUSED).
	ConnFailureRefused ConnFailureOutcome = "refused"
	// ConnFailureHostUnreachable: no route to the host (EHOSTUNREACH).
	ConnFailureHostUnreachable ConnFailureOutcome = "host-unreachable"
	// ConnFailureNetUnreachable: the network is unreachable (ENETUNREACH).
	ConnFailureNetUnreachable ConnFailureOutcome = "net-unreachable"
	// ConnFailureTimeout: nothing answered before the application, or the
	// kernel, gave up on the connect.
	ConnFailureTimeout ConnFailureOutcome = "timeout"
	// ConnFailureClosed: the other end closed the connection normally (a FIN),
	// so the application read the end of the stream (EOF). A reset (an RST,
	// which the application sees as ECONNRESET) is not "closed": a later
	// keploy that records resets writes them as an outcome of their own, which
	// this keploy then skips as "uses outcome ..., upgrade keploy".
	ConnFailureClosed ConnFailureOutcome = "closed"
)

// connFailureOutcomesByPhase is the format: the outcomes each phase can have.
// A value missing here was written by a newer keploy (or by hand), and this
// keploy cannot replay it faithfully, so Validate rejects it.
var connFailureOutcomesByPhase = map[ConnFailurePhase][]ConnFailureOutcome{
	ConnFailurePhaseConnect:  {ConnFailureRefused, ConnFailureHostUnreachable, ConnFailureNetUnreachable, ConnFailureTimeout},
	ConnFailurePhaseAccepted: {ConnFailureClosed},
	ConnFailurePhaseTLS:      {ConnFailureClosed},
	ConnFailurePhaseRequest:  {ConnFailureClosed},
}

// ConnFailureSpec is the body of a ConnectionFailure mock (MockSpec.ConnFailure).
type ConnFailureSpec struct {
	// Address is the destination the application connected to, "ip:port" or
	// "[ipv6]:port", as keploy saw the attempt.
	Address string `json:"address" yaml:"address" bson:"address"`
	// Host is the name the application resolved to Address through keploy's
	// DNS, or "" when it did not (a literal IP, /etc/hosts).
	Host string `json:"host,omitempty" yaml:"host,omitempty" bson:"host,omitempty"`
	// Phase is the step at which the connection failed.
	Phase ConnFailurePhase `json:"phase" yaml:"phase" bson:"phase"`
	// Outcome is how it failed.
	Outcome ConnFailureOutcome `json:"outcome" yaml:"outcome" bson:"outcome"`
	// Cause is what the destination did, as error text, for people reading the
	// mock. Nothing matches on it.
	Cause string `json:"cause,omitempty" yaml:"cause,omitempty" bson:"cause,omitempty"`
}

// ConnFailureSchema is the on-disk spec of a ConnectionFailure mock, for the
// YAML and JSON mock files: ConnFailureSpec's fields, flat, between the
// metadata and the timestamps every mock carries.
type ConnFailureSchema struct {
	Metadata         map[string]string `json:"metadata,omitempty" yaml:"metadata,omitempty"`
	ConnFailureSpec  `yaml:",inline"`
	ReqTimestampMock time.Time `json:"reqTimestampMock,omitempty" yaml:"reqTimestampMock,omitempty"`
	ResTimestampMock time.Time `json:"resTimestampMock,omitempty" yaml:"resTimestampMock,omitempty"`
}

// connFailureFields is every field of ConnFailureSchema, as the YAML and JSON
// mock files spell it. The format is closed: a field missing here was added
// by a newer keploy for a replay this keploy does not have, so a document
// that has one is refused (CheckConnFailureFields) rather than read without
// it, which would replay it without what the field says. Pinned to the struct
// by TestConnFailureFieldsAreTheSchemas.
var connFailureFields = map[string]bool{
	"metadata":         true,
	"address":          true,
	"host":             true,
	"phase":            true,
	"outcome":          true,
	"cause":            true,
	"reqTimestampMock": true,
	"resTimestampMock": true,
}

// CheckConnFailureFields reports, as "uses field <f>, which this keploy does
// not support; upgrade keploy", the first of names (the field names of a
// connection failure's on-disk spec, exactly as the document spells them)
// that the format does not have. It returns nil when the format has them all.
func CheckConnFailureFields(names []string) error {
	for _, n := range names {
		if !connFailureFields[n] {
			return unsupported(fmt.Sprintf("field %q", n))
		}
	}
	return nil
}

// unsupported is the error for a value a newer keploy may write and this one
// cannot replay: it says so and says what to do about it.
func unsupported(what string) error {
	return fmt.Errorf("the connection failure uses %s, which this keploy does not support; upgrade keploy", what)
}

// Validate reports whether this keploy supports every value in s. An unknown
// phase or outcome, or a pair this keploy does not know (both written by a
// newer keploy), is reported as "uses <value>, which this keploy does not
// support; upgrade keploy". A missing or malformed field is reported as
// malformed.
//
// The phase and the outcome are checked first: what the address must look
// like depends on them (a later phase may have no address, or a name or a
// socket path for one), so a document with a phase or outcome this keploy does
// not know gets the upgrade advice, not a "malformed address".
func (s *ConnFailureSpec) Validate() error {
	if s == nil {
		return errors.New("the connection failure has no details")
	}
	if s.Phase == "" {
		return errors.New("the connection failure has no phase")
	}
	if s.Outcome == "" {
		return errors.New("the connection failure has no outcome")
	}
	outcomes, ok := connFailureOutcomesByPhase[s.Phase]
	if !ok {
		return unsupported(fmt.Sprintf("phase %q", s.Phase))
	}
	if !knownConnFailureOutcome(s.Outcome) {
		return unsupported(fmt.Sprintf("outcome %q", s.Outcome))
	}
	if !containsOutcome(outcomes, s.Outcome) {
		return unsupported(fmt.Sprintf("outcome %q in phase %q", s.Outcome, s.Phase))
	}
	if s.Address == "" {
		return errors.New("the connection failure has no address")
	}
	_, port, err := net.SplitHostPort(s.Address)
	if err != nil {
		return fmt.Errorf("the connection failure's address %q is not host:port: %w", s.Address, err)
	}
	if p, err := strconv.ParseUint(port, 10, 16); err != nil || p == 0 {
		return fmt.Errorf("the connection failure's address %q has no valid port", s.Address)
	}
	return nil
}

// ValidateConnFailure checks a ConnectionFailure mock: its spec (see
// ConnFailureSpec.Validate) and its two timestamps. A connection failure is
// only ever replayed inside the test window its request time falls in, so a
// mock without both times, or with the response before the request, cannot
// be placed in any test and is rejected rather than left to the filters
// (which serve an untimed per-test mock to every test).
func (m *Mock) ValidateConnFailure() error {
	if m == nil {
		return errors.New("no mock")
	}
	if err := m.Spec.ConnFailure.Validate(); err != nil {
		return err
	}
	req, res := m.Spec.ReqTimestampMock, m.Spec.ResTimestampMock
	if req.IsZero() || res.IsZero() {
		return errors.New("the connection failure has no reqTimestampMock or resTimestampMock, so it cannot be placed in the test that saw it")
	}
	if res.Before(req) {
		return fmt.Errorf("the connection failure's resTimestampMock %s is before its reqTimestampMock %s", res.Format(time.RFC3339Nano), req.Format(time.RFC3339Nano))
	}
	return nil
}

func knownConnFailureOutcome(o ConnFailureOutcome) bool {
	for _, outcomes := range connFailureOutcomesByPhase {
		if containsOutcome(outcomes, o) {
			return true
		}
	}
	return false
}

func containsOutcome(outcomes []ConnFailureOutcome, o ConnFailureOutcome) bool {
	for _, x := range outcomes {
		if x == o {
			return true
		}
	}
	return false
}

// WindowBound reports whether mocks of kind belong to the one test window
// their request time falls in (or to app boot, before the first test) and
// must never be served anywhere else. A connection failure recorded in test 3
// must not fail a connect in test 4, so such a mock is always per-test
// (DeriveLifetime), whatever its metadata says, and is never promoted to the
// session pool for being out of window (lax mode), carried over to a later
// window (RegisterCarryOver), or moved to the session pool because a mutator
// re-tagged it (the replay's rebalance).
func WindowBound(kind Kind) bool {
	return kind == ConnectionFailure
}

// AgentBound is mocks less those this keploy never sends to the agent: the
// connection failures, which it does not replay. An agent that predates the
// kind reads one as a mock with no kind and no metadata.destAddr, whose unknown
// port turns loopback refusal off for the whole test set (see
// ConnectionFailure). Every sender, and every count of what the agent holds,
// goes through it, so they all agree. It returns a new slice when it drops
// any, leaving the caller's as it was; a nil entry is kept, as before.
func AgentBound(mocks []*Mock) []*Mock {
	n := 0
	for _, m := range mocks {
		if m != nil && m.Kind == ConnectionFailure {
			n++
		}
	}
	if n == 0 {
		return mocks
	}
	out := make([]*Mock, 0, len(mocks)-n)
	for _, m := range mocks {
		if m == nil || m.Kind != ConnectionFailure {
			out = append(out, m)
		}
	}
	return out
}
