package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// beginSvc can open a capture window; continueSvc can continue one too.
type beginSvc struct {
	agent.Service // nil: any other call panics loudly
	opened        *[]string
}

func (s beginSvc) BeginTestErrorCapture(context.Context) error {
	*s.opened = append(*s.opened, "begin")
	return nil
}

type continueSvc struct{ beginSvc }

func (s continueSvc) ContinueTestErrorCapture(context.Context) error {
	*s.opened = append(*s.opened, "continue")
	return nil
}

// POST /test-capture/begin opens a test's window; with ?carry=1 it continues
// the capture, carrying in what was missed since the previous test's window
// closed. A service that cannot continue opens the window as begin does.
func TestBeginTestErrorCaptureContinuesOnCarry(t *testing.T) {
	for _, tc := range []struct {
		name, query string
		canContinue bool
		want        string
	}{
		{"begin", "", true, "begin"},
		{"carry", "?carry=1", true, "continue"},
		{"carry to a service that cannot continue", "?carry=1", false, "begin"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var opened []string
			var svc agent.Service = beginSvc{opened: &opened}
			if tc.canContinue {
				svc = continueSvc{beginSvc{opened: &opened}}
			}
			a := &Agent{logger: zap.NewNop(), svc: svc}
			rec := httptest.NewRecorder()
			a.BeginTestErrorCapture(rec, httptest.NewRequest(http.MethodPost, "/agent/test-capture/begin"+tc.query, nil))
			if rec.Code != http.StatusOK || len(opened) != 1 || opened[0] != tc.want {
				t.Fatalf("status %d, opened %v; want 200 and [%s]", rec.Code, opened, tc.want)
			}
		})
	}
}
