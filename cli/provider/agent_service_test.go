package provider

import (
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/proxy"
	incoming "go.keploy.io/server/v3/pkg/agent/proxy/incoming"
	"go.keploy.io/server/v3/pkg/service/agent"
	"go.uber.org/zap"
)

// The agent's ingress gives each client test's request it forwards to the
// agent's proxy, whose learn step reads it as a place of the test's calls
// and whose replay binds the test's variables at it: GetAgent makes the
// proxy the ingress's request taker. Left unwired, both do nothing, and
// nothing says so.
func TestTheAgentsIngressGivesItsRequestsToItsProxy(t *testing.T) {
	got, err := GetAgent(t.Context(), "agent", &config.Config{}, zap.NewNop())
	if err != nil {
		t.Fatal(err)
	}
	a, ok := got.(*agent.Agent)
	if !ok {
		t.Fatalf("GetAgent gave %T", got)
	}
	ip, ok := a.IncomingProxy.(*incoming.IngressProxyManager)
	if !ok {
		t.Fatalf("the agent's ingress is %T", a.IncomingProxy)
	}
	p, ok := a.Proxy.(*proxy.Proxy)
	if !ok || p == nil {
		t.Fatalf("the agent's proxy is %T", a.Proxy)
	}
	if taker, ok := ip.RequestTaker().(*proxy.Proxy); !ok || taker != p {
		t.Fatalf("the ingress gives its requests to %T %p; want the agent's proxy %p", ip.RequestTaker(), ip.RequestTaker(), p)
	}
}
