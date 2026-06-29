package generic

import (
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.uber.org/zap"
)

// TestGenericRecordOutgoingErrorPaths covers the two early-return paths
// in (*Generic).recordLegacy: a nil session and a session whose Ingress
// has not been initialized (the V2-only / not-yet-wired case). Both
// should surface a wrapped error prefixed with "generic:" rather than
// nil-deref panicking.
func TestGenericRecordOutgoingErrorPaths(t *testing.T) {
	g := &Generic{logger: zap.NewNop()}

	tests := []struct {
		name    string
		session *integrations.RecordSession
		wants   []string
	}{
		{
			name:    "nil session",
			session: nil,
			wants:   []string{"generic:", "nil"},
		},
		{
			name: "nil ingress",
			session: &integrations.RecordSession{
				Logger: zap.NewNop(),
			},
			wants: []string{"generic:", "connection not initialized"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := g.RecordOutgoing(context.Background(), tc.session)
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
