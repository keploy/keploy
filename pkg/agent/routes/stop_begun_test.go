package routes

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/service/agent"
)

// stopBegunSvc notes the recording's stop, or fails to.
type stopBegunSvc struct {
	agent.Service // nil: any other call panics loudly
	noted         *bool
	err           error
}

func (s stopBegunSvc) NoteStopBegun(context.Context) error {
	*s.noted = true
	return s.err
}

// The agent's /stop-begun notes the recording's stop and answers 200; an agent
// whose service cannot note it, or fails to, says so with an error status, so
// keploy mock record's client warns rather than taking it as noted.
func TestHandleStopBegunNotesTheStopOrSaysItCannot(t *testing.T) {
	for _, tc := range []struct {
		name string
		svc  agent.Service
		want int
	}{
		{"noted", stopBegunSvc{noted: new(bool)}, http.StatusOK},
		{"the service fails", stopBegunSvc{noted: new(bool), err: errors.New("no proxy")}, http.StatusInternalServerError},
		{"the service cannot note it", gracefulSvc{drains: new(bool)}, http.StatusNotImplemented},
	} {
		t.Run(tc.name, func(t *testing.T) {
			a := &Agent{logger: zap.NewNop(), svc: tc.svc}
			rec := httptest.NewRecorder()
			a.HandleStopBegun(rec, httptest.NewRequest(http.MethodPost, "/agent/stop-begun", nil))
			if rec.Code != tc.want {
				t.Fatalf("status %d, want %d", rec.Code, tc.want)
			}
			if s, ok := tc.svc.(stopBegunSvc); ok && !*s.noted {
				t.Fatal("the service was not asked to note the stop")
			}
		})
	}
}
