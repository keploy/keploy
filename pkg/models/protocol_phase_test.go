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
