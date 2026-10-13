package routes

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// paramsSvc takes mock filters and counts the times it forgot the bound
// variables, and when (before the filter, or after).
type paramsSvc struct {
	agent.Service // nil: any other call panics loudly
	events        *[]string
}

func (s paramsSvc) UpdateMockParams(context.Context, models.MockFilterParams) error {
	*s.events = append(*s.events, "filter")
	return nil
}

func (s paramsSvc) ResetVariables() { *s.events = append(*s.events, "reset") }

// Under the proven switch every mock filter a client sends starts a test: the
// variables bound so far are forgotten as the filter applies. With the
// switch off nothing is forgotten. The scope begins once the new pool is in.
func TestAMockFilterStartsATestsVariables(t *testing.T) {
	for _, tc := range []struct {
		proven string
		want   string
	}{
		{"1", "filter reset"},
		{"0", "filter"},
	} {
		t.Run("KEPLOY_PROVEN="+tc.proven, func(t *testing.T) {
			t.Setenv(provenswitch.Env, tc.proven)
			var events []string
			a := &Agent{logger: zap.NewNop(), svc: paramsSvc{events: &events}}
			rec := httptest.NewRecorder()
			a.UpdateMockParams(rec, httptest.NewRequest(http.MethodPost, "/agent/updatemockparams", strings.NewReader(`{"filterParams":{}}`)))
			if rec.Code != http.StatusOK || strings.Join(events, " ") != tc.want {
				t.Fatalf("status %d, events %v; want 200 and %q", rec.Code, events, tc.want)
			}
		})
	}
}
