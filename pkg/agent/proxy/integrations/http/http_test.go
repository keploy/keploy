package http

import (
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.uber.org/zap"
)

// TestHTTPRecordOutgoingErrorPaths covers (*HTTP).recordLegacy returning
// a wrapped error on the two early failure modes — nil session and
// missing Ingress — instead of nil-deref panicking. Both messages must
// carry the "http:" prefix so log search can attribute the error to
// the HTTP parser.
func TestHTTPRecordOutgoingErrorPaths(t *testing.T) {
	h := &HTTP{Logger: zap.NewNop()}

	tests := []struct {
		name    string
		session *integrations.RecordSession
		wants   []string
	}{
		{
			name:    "nil session",
			session: nil,
			wants:   []string{"http:", "nil"},
		},
		{
			name: "nil ingress",
			session: &integrations.RecordSession{
				Logger: zap.NewNop(),
			},
			wants: []string{"http:", "connection not initialized"},
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			err := h.RecordOutgoing(context.Background(), tc.session)
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
