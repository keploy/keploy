package models

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models/mysql"
)

func mysqlCommand(typ string) *Mock {
	return &Mock{Kind: MySQL, Spec: MockSpec{
		Metadata:      map[string]string{"type": "mocks"},
		MySQLRequests: []mysql.Request{{PacketBundle: mysql.PacketBundle{Header: &mysql.PacketInfo{Type: typ}}}},
	}}
}

func TestIsProtocolPhase(t *testing.T) {
	ping, query := mysqlCommand("COM_PING"), mysqlCommand("COM_QUERY")
	for _, tc := range []struct {
		name string
		mock *Mock
		want bool
	}{
		{"config-tagged handshake", &Mock{Kind: Postgres, Spec: MockSpec{Metadata: map[string]string{"type": "config"}}}, true},
		{"connection-tagged prepare", &Mock{Kind: Postgres, Spec: MockSpec{Metadata: map[string]string{"type": "connection", "connID": "c1"}}}, true},
		{"DNS", &Mock{Kind: DNS}, true},
		{"MySQL COM_PING tagged mocks", ping, true},
		{"MySQL COM_QUERY tagged mocks", query, false},
		// DeriveLifetime's kind fallback calls these session-tier; they are
		// still the calls a test recorded.
		{"untagged HTTP", &Mock{Kind: HTTP}, false},
		{"HTTP_CLIENT-tagged HTTP", &Mock{Kind: HTTP, Spec: MockSpec{Metadata: map[string]string{"type": "HTTP_CLIENT"}}}, false},
		{"untagged Generic", &Mock{Kind: GENERIC}, false},
		{"connection failure, even tagged config", &Mock{Kind: ConnectionFailure, Spec: MockSpec{Metadata: map[string]string{"type": "config"}}}, false},
		{"nil", nil, false},
		// The recorder's statement decides, for the kinds that make one.
		{"MySQL SET stated session, tagged mocks", stated(mysqlCommand("COM_QUERY"), MetadataLifetimeSession), true},
		{"MySQL CLOSE stated connection, tagged mocks", stated(withConn(mysqlCommand("COM_STMT_CLOSE")), MetadataLifetimeConnection), true},
		{"MySQL query stated per-test", stated(mysqlCommand("COM_QUERY"), MetadataLifetimePerTest), false},
		{"MySQL COM_PING stated session", stated(mysqlCommand("COM_PING"), MetadataLifetimeSession), true},
		{"HTTP stated per-test", stated(&Mock{Kind: HTTP, Spec: MockSpec{Metadata: map[string]string{"type": "HTTP_CLIENT"}}}, MetadataLifetimePerTest), false},
		{"HTTP recordOne stated session", stated(&Mock{Kind: HTTP, Spec: MockSpec{Metadata: map[string]string{"type": "config"}}}, MetadataLifetimeSession), true},
		// A post-record hook tagged it config over its recorder's stated
		// per-test (enterprise's SQS config calls): reusable, so no test must
		// make it.
		{"HTTP stated per-test, then tagged config", stated(&Mock{Kind: HTTP, Spec: MockSpec{Metadata: map[string]string{"type": "config"}}}, MetadataLifetimePerTest), true},
		{"Generic stated per-test, tagged config", stated(&Mock{Kind: GENERIC, Spec: MockSpec{Metadata: map[string]string{"type": "config"}}}, MetadataLifetimePerTest), false},
		{"Postgres v3 query with its own lifetime key", stated(&Mock{Kind: PostgresV2, Spec: MockSpec{Metadata: map[string]string{"type": "mocks"}}}, "session"), false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.mock.IsProtocolPhase(); got != tc.want {
				t.Fatalf("IsProtocolPhase() = %v, want %v", got, tc.want)
			}
		})
	}
}

// The session tier and protocol phase are different questions: the fixture
// the kind fallback promotes to session is still data-plane.
func TestIsProtocolPhaseIsNotTheSessionTier(t *testing.T) {
	m := &Mock{Kind: HTTP}
	m.DeriveLifetime()
	if m.TestModeInfo.Lifetime != LifetimeSession {
		t.Fatalf("precondition: an untagged HTTP mock is session-tier by DeriveLifetime's kind fallback, got %s", m.TestModeInfo.Lifetime)
	}
	if m.IsProtocolPhase() {
		t.Fatal("an untagged HTTP mock is a call its test made; the session tier must not make it protocol-phase")
	}
}

// stated states m's lifetime as a recorder does (MetadataLifetimeKey).
func stated(m *Mock, lifetime string) *Mock {
	m.Spec.Metadata[MetadataLifetimeKey] = lifetime
	return m
}

func withConn(m *Mock) *Mock {
	m.Spec.Metadata["connID"] = "c1"
	return m
}
