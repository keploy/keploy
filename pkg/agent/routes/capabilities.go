package routes

import (
	"net/http"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// ingressSteerer is the agent service, where its hooks steer the
// application's ports (the linux hooks' IngressSteers).
type ingressSteerer interface {
	IngressSteers() bool
}

// capabilities is what this agent says it does in /agent/health's
// models.AgentCapabilitiesHeader: models.CapabilityIngressFailOpen while its
// hooks steer the application's ports. None, and no header, otherwise.
func (a *Agent) capabilities() []string {
	var caps []string
	if s, ok := a.svc.(ingressSteerer); ok && s.IngressSteers() {
		caps = append(caps, models.CapabilityIngressFailOpen)
	}
	return caps
}

// setCapabilities sets models.AgentCapabilitiesHeader on w, when this agent
// has any capability to say.
func (a *Agent) setCapabilities(w http.ResponseWriter) {
	if caps := a.capabilities(); len(caps) > 0 {
		w.Header().Set(models.AgentCapabilitiesHeader, strings.Join(caps, ","))
	}
}
