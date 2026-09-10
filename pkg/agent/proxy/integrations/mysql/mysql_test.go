package mysql

import (
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.uber.org/zap"
)

// TestMySQLRecordOutgoingErrorPaths covers (*MySQL).recordLegacy returning
// a wrapped error on a nil session and a session whose Ingress was not
// initialized, instead of nil-deref panicking. Both messages must carry
// the "mysql:" prefix so dispatcher / log search can attribute the
// error to the MySQL parser.
func TestMySQLRecordOutgoingErrorPaths(t *testing.T) {
	m := &MySQL{logger: zap.NewNop()}

	tests := []struct {
		name    string
		session *integrations.RecordSession
		wants   []string
	}{
		{
			name:    "nil session",
			session: nil,
			wants:   []string{"mysql:", "nil"},
		},
		{
			name: "nil ingress",
			session: &integrations.RecordSession{
				Logger: zap.NewNop(),
			},
			wants: []string{"mysql:", "connection not initialized"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := m.RecordOutgoing(context.Background(), tc.session)
			if err == nil {
				t.Fatalf("expected error, got nil")
			}
			msg := err.Error()
			for _, want := range tc.wants {
				if !strings.Contains(msg, want) {
					t.Errorf("error %q does not contain %q", msg, want)
				}
			}
		})
	}
}
