package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

type setEndSvc struct {
	agent.Service // nil: any other call panics loudly
	setEnd        *bool
}

func (s setEndSvc) GetMockErrors(ctx context.Context) ([]models.UnmatchedCall, error) {
	*s.setEnd = models.SetEndRead(ctx)
	return nil, nil
}

// Only ?setend=1 reads the misses as a set's end: no query, or another value,
// is a test's end read, as before.
func TestGetMockErrors_SetEndQueryMarksTheRead(t *testing.T) {
	for target, want := range map[string]bool{
		"/agent/mockerrors":                  false,
		"/agent/mockerrors?setend=0":         false,
		"/agent/mockerrors?setend=1":         true,
		"/agent/mockerrors?other=1&setend=1": true,
	} {
		var setEnd bool
		a := &Agent{logger: zap.NewNop(), svc: setEndSvc{setEnd: &setEnd}}
		rec := httptest.NewRecorder()
		a.GetMockErrors(rec, httptest.NewRequest(http.MethodGet, target, nil))
		if rec.Code != http.StatusOK || setEnd != want {
			t.Fatalf("%s: status %d, set's end read %v, want %v", target, rec.Code, setEnd, want)
		}
	}
}
