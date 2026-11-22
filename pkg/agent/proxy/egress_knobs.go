package proxy

import (
	"slices"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// egressKnob is one setting that lets a replay reach the real network, and
// what it opens. It is logged with agent.LogRealEgress.
type egressKnob struct {
	knob string
	dest string
}

// allDestinations is what a knob that relays every connection opens.
const allDestinations = "every destination; each connection goes to its real destination and nothing is served from mocks"

// agentEgressKnobs names what the agent's own flags open in a replay, before
// and during every session: handleConnection and serveWithoutSession dial the
// real destination under global passthrough.
func agentEgressKnobs(agentMode models.Mode, globalPassthrough bool) []egressKnob {
	if agentMode != models.MODE_TEST || !globalPassthrough {
		return nil
	}
	return []egressKnob{{knob: "--global-passthrough", dest: allDestinations}}
}

// sessionEgressKnobs names what a replay session's options open, in the order
// handleConnection decides them: an opportunistic TLS intercept and mocking
// off relay every connection, and an onMiss of passthrough or record sends an
// HTTP call no mock answers to its real destination. mock.real (SetRealDeps),
// the DNS forward (logReplayDNSForwarding) and the kernel's pass-through
// ports (the hooks' RegisterClient) name their own.
func sessionEgressKnobs(opts models.OutgoingOptions) []egressKnob {
	var ks []egressKnob
	if opts.OpportunisticTLSIntercept {
		ks = append(ks, egressKnob{knob: "opportunisticTLSIntercept", dest: "every destination; each connection is relayed to its real destination, its TLS intercepted"})
	}
	if !opts.Mocking {
		ks = append(ks, egressKnob{knob: "test.mocking: false (--mocking=false)", dest: allDestinations})
	}
	if opts.OnMiss.PassesThroughOnMiss() {
		ks = append(ks, egressKnob{knob: "onMiss " + string(opts.OnMiss) + " (--on-miss " + string(opts.OnMiss) + ")", dest: "an HTTP call no mock answers goes to its real destination"})
	}
	return ks
}

// logAgentEgressKnobs logs, at the proxy's start, what the agent's own flags
// open in a replay.
func (p *Proxy) logAgentEgressKnobs() {
	for _, k := range agentEgressKnobs(p.agentMode, p.GlobalPassthrough) {
		agent.LogRealEgress(p.logger, k.knob, k.dest)
	}
}

// logSessionEgressKnobs logs what a replay session's options open, when they
// differ from what the last session's opened: keploy test starts a session
// per test set, all with the same options, and a line per set says nothing
// new.
func (p *Proxy) logSessionEgressKnobs(opts models.OutgoingOptions) {
	ks := sessionEgressKnobs(opts)
	p.egressKnobsMu.Lock()
	same := slices.Equal(ks, p.lastSessionEgress)
	p.lastSessionEgress = ks
	p.egressKnobsMu.Unlock()
	if same {
		return
	}
	for _, k := range ks {
		agent.LogRealEgress(p.logger, k.knob, k.dest)
	}
}
