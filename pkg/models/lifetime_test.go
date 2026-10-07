package models

import (
	"testing"
)

// brokerKind stands in for an out-of-tree kind (the enterprise Pulsar parser
// registers its own); the hook is keyed by Kind, so any value works.
const brokerKind Kind = "TestBroker"

func brokerMock(tag, command string) *Mock {
	md := map[string]string{"commandType": command}
	if tag != "" {
		md["type"] = tag
	}
	return &Mock{Kind: brokerKind, Spec: MockSpec{Metadata: md}}
}

func isHandshake(m *Mock) bool {
	c := m.Spec.Metadata["commandType"]
	return c == "PRODUCER" || c == "SUBSCRIBE"
}

// TestRegisterSessionReusable pins the Part A hook: a registered Kind+metadata
// combination recorded as "mocks" is derived session-tier, while everything the
// hook does not name keeps the classification DeriveLifetime gave it before.
func TestRegisterSessionReusable(t *testing.T) {
	t.Cleanup(sessionReusableHooks.reset)
	sessionReusableHooks.reset()
	RegisterSessionReusable(brokerKind, isHandshake)
	RegisterSessionReusable(brokerKind, nil) // ignored, must not panic at derive time

	for _, tc := range []struct {
		name string
		mock *Mock
		want Lifetime
	}{
		{"mocks-tagged PRODUCER is session", brokerMock("mocks", "PRODUCER"), LifetimeSession},
		{"untagged SUBSCRIBE is session", brokerMock("", "SUBSCRIBE"), LifetimeSession},
		{"mocks-tagged SEND stays per-test", brokerMock("mocks", "SEND"), LifetimePerTest},
		{"mocks-tagged CLOSE_PRODUCER stays per-test", brokerMock("mocks", "CLOSE_PRODUCER"), LifetimePerTest},
		{"config-tagged SEND is session as before", brokerMock("config", "SEND"), LifetimeSession},
		{
			"connection-tagged PRODUCER keeps connection scope",
			func() *Mock {
				m := brokerMock("connection", "PRODUCER")
				m.Spec.Metadata["connID"] = "c1"
				return m
			}(),
			LifetimeConnection,
		},
		{
			"another kind with the same metadata is untouched",
			&Mock{Kind: "OtherBroker", Spec: MockSpec{Metadata: map[string]string{"type": "mocks", "commandType": "PRODUCER"}}},
			LifetimePerTest,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.mock.DeriveLifetime()
			if got := tc.mock.TestModeInfo.Lifetime; got != tc.want {
				t.Fatalf("Lifetime = %v, want %v", got, tc.want)
			}
			if !tc.mock.TestModeInfo.LifetimeDerived {
				t.Fatal("LifetimeDerived not set")
			}
		})
	}
}

// TestRegisterSessionReusableDoesNotCountAsLegacyFallback: an untagged mock the
// hook promotes is classified by the hook, not by the pre-tag kind fallback, so
// the operator-facing legacy counter must not move.
func TestRegisterSessionReusableDoesNotCountAsLegacyFallback(t *testing.T) {
	t.Cleanup(sessionReusableHooks.reset)
	sessionReusableHooks.reset()
	RegisterSessionReusable(brokerKind, isHandshake)

	before := LegacyKindFallbackFires()
	m := brokerMock("", "PRODUCER")
	m.DeriveLifetime()
	if m.TestModeInfo.Lifetime != LifetimeSession {
		t.Fatalf("Lifetime = %v, want session", m.TestModeInfo.Lifetime)
	}
	if after := LegacyKindFallbackFires(); after != before {
		t.Fatalf("legacy kind fallback counter moved %d -> %d", before, after)
	}
}

// TestUnregisteredKindsDeriveAsBefore: with no registration at all, the same
// mocks derive per-test, which is today's behaviour for an out-of-tree kind.
func TestUnregisteredKindsDeriveAsBefore(t *testing.T) {
	t.Cleanup(sessionReusableHooks.reset)
	sessionReusableHooks.reset()
	for _, cmd := range []string{"PRODUCER", "SUBSCRIBE", "SEND"} {
		m := brokerMock("mocks", cmd)
		m.DeriveLifetime()
		if m.TestModeInfo.Lifetime != LifetimePerTest {
			t.Fatalf("%s: Lifetime = %v, want per-test", cmd, m.TestModeInfo.Lifetime)
		}
	}
}

// TestDeriveConsumeMode pins the ConsumeMode classification DeriveLifetime
// assigns alongside Lifetime. Data-plane mocks promoted to session by the
// kind-fallback (rules #4 untagged, #5 lax non-canonical tag) must get
// ConsumeCursorSaturate so repeated identical requests advance through the
// recorded responses (closing the 1,1,1 stateful false pass), while true
// session/config/connection mocks, and the order-nondeterministic DNS kind,
// must stay ConsumeReuse.
func TestDeriveConsumeMode(t *testing.T) {
	t.Cleanup(sessionReusableHooks.reset)
	sessionReusableHooks.reset()
	RegisterSessionReusable(brokerKind, isHandshake)

	mk := func(kind Kind, tag string) *Mock {
		md := map[string]string{}
		if tag != "" {
			md["type"] = tag
		}
		if tag == "connection" {
			md["connID"] = "c1"
		}
		return &Mock{Kind: kind, Spec: MockSpec{Metadata: md}}
	}

	// Rule #5 (lax promotion of a non-canonical tag) only fires when the lax
	// kind-fallback is enabled, which is the default; honour whatever this
	// process was started with so the test is deterministic under either.
	laxCursor := ConsumeCursorSaturate
	if laxKindFallbackDisabled() {
		laxCursor = ConsumeReuse
	}

	for _, tc := range []struct {
		name string
		mock *Mock
		want ConsumeMode
	}{
		// Rule #4 — untagged data-plane kinds → cursor (fires regardless of lax).
		{"untagged HTTP", mk(HTTP, ""), ConsumeCursorSaturate},
		{"untagged HTTP2", mk(HTTP2, ""), ConsumeCursorSaturate},
		{"untagged Generic", mk(GENERIC, ""), ConsumeCursorSaturate},
		{"untagged Postgres", mk(Postgres, ""), ConsumeCursorSaturate},
		{"untagged PostgresV2", mk(PostgresV2, ""), ConsumeCursorSaturate},
		// Rule #5 — lax non-canonical tag on a data-plane kind.
		{"mocks-tagged HTTP follows lax mode", mk(HTTP, "mocks"), laxCursor},
		{"HTTP_CLIENT-tagged Generic follows lax mode", mk(GENERIC, "HTTP_CLIENT"), laxCursor},
		// DNS is excluded from cursoring (non-deterministic resolution order).
		{"untagged DNS stays reuse", mk(DNS, ""), ConsumeReuse},
		{"mocks-tagged DNS stays reuse", mk(DNS, "mocks"), ConsumeReuse},
		// True session mocks keep reuse: config (#2), connection (#3), and the
		// registered session-reusable hook (#1a).
		{"config-tagged HTTP stays reuse", mk(HTTP, "config"), ConsumeReuse},
		{"connection-tagged HTTP stays reuse", mk(HTTP, "connection"), ConsumeReuse},
		{"registered-reusable broker stays reuse", brokerMock("mocks", "PRODUCER"), ConsumeReuse},
		// A kind outside the implicit-session list is per-test and reuse.
		{"unlisted kind stays reuse", mk(Kind("UnlistedKind"), "mocks"), ConsumeReuse},
	} {
		t.Run(tc.name, func(t *testing.T) {
			tc.mock.DeriveLifetime()
			if got := tc.mock.TestModeInfo.Consume; got != tc.want {
				t.Fatalf("Consume = %v, want %v (Lifetime=%v)", got, tc.want, tc.mock.TestModeInfo.Lifetime)
			}
		})
	}
}
