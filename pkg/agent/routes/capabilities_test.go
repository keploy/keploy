package routes

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/service/agent"
)

// steeringService is an agent service whose hooks steer the app's ports, or
// not.
type steeringService struct {
	agent.Service
	steers bool
}

func (s steeringService) IngressSteers() bool { return s.steers }

// /agent/health says ingress-fail-open while the agent's hooks steer the
// application's ports (its death leaves them to the application, so k8s-proxy
// re-arms a restarted agent at once), and says nothing where they move the
// app's binds, nor for a service that cannot tell.
func TestHealthSaysTheAgentFailsOpenOnlyWhileItSteers(t *testing.T) {
	for _, tc := range []struct {
		name string
		svc  agent.Service
		want string
	}{
		{"steering", steeringService{steers: true}, models.CapabilityIngressFailOpen},
		{"moving the binds", steeringService{steers: false}, ""},
		{"a service that cannot tell", nil, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := httptest.NewRecorder()
			(&Agent{logger: zap.NewNop(), svc: tc.svc}).Health(rec, httptest.NewRequest(http.MethodGet, "/agent/health", nil))
			if rec.Code != http.StatusOK {
				t.Fatalf("health answered %d", rec.Code)
			}
			if got := rec.Header().Get(models.AgentCapabilitiesHeader); got != tc.want {
				t.Fatalf("%s = %q, want %q", models.AgentCapabilitiesHeader, got, tc.want)
			}
		})
	}
}
