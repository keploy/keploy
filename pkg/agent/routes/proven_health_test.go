package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// /agent/health reports the agent's proven switch, so the CLI can refuse an
// agent whose switch is not its own.
func TestHealthReportsTheProvenSwitch(t *testing.T) {
	for _, tc := range []struct{ env, want string }{{"0", "off"}, {"1", "on"}} {
		t.Setenv(provenswitch.Env, tc.env)
		rec := httptest.NewRecorder()
		(&Agent{logger: zap.NewNop()}).Health(rec, httptest.NewRequest(http.MethodGet, "/agent/health", nil))
		if rec.Code != http.StatusOK {
			t.Fatalf("health answered %d", rec.Code)
		}
		if got := rec.Header().Get(provenswitch.HealthHeader); got != tc.want {
			t.Fatalf("%s=%q: %s = %q, want %q", provenswitch.Env, tc.env, provenswitch.HealthHeader, got, tc.want)
		}
	}
}
