package routes

import (
	"testing"

	"go.keploy.io/server/v3/pkg/service/agent"
)

// keploy's own agent service takes the recording's worker-mock rules the
// /outgoing route hands it: the route asks for them through an optional
// interface (backgroundRuleSetter), so a service that lost the method would
// compile and silently apply no rule.
var _ backgroundRuleSetter = (*agent.Agent)(nil)

func TestTheAgentServiceTakesTheRecordingsRules(t *testing.T) {
	var svc agent.Service = &agent.Agent{}
	if _, ok := svc.(backgroundRuleSetter); !ok {
		t.Fatal("keploy's agent service does not take the recording's worker-mock rules")
	}
}
