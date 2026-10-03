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
