package docker

import (
	"net"
	"testing"

	"go.keploy.io/server/v3/config"
)

// TestOnlyALiteralNonLoopbackIPIsTreatedAsPinned guards the one signal that
// switches the whole stack from loopback+published-ports to container
// addressing. A false positive points the replay at an agent that is not there.
func TestOnlyALiteralNonLoopbackIPIsTreatedAsPinned(t *testing.T) {
	for _, tc := range []struct {
		uri  string
		want string
	}{
		{"", ""},                                        // the laptop default
		{"http://localhost:16789/agent", ""},            // loopback is NOT a pin
		{"http://127.0.0.1:16789/agent", ""},            // ditto, literal
		{"http://keploy-agent:16789/agent", ""},         // a name is not an address
		{"not a url at all %%", ""},                     // garbage
		{"http://172.31.253.10/agent", "172.31.253.10"}, // pinned, no port
		{"http://172.31.253.10:16789/agent", "172.31.253.10"},

		// OUTSIDE InternalSubnet is not pinnable, however literal.
		//
		// The address is written into the generated compose file as
		// `ipv4_address` on the keploy network, and Docker refuses one the
		// network has no subnet for: "user specified IP address is supported
		// only when connecting to networks with user configured subnets". So
		// accepting these failed compose-up instead of falling back to the
		// published-port path the caller was asking for.
		{"http://10.0.0.5:16789/agent", ""},
		{"http://0.0.0.0:16789/agent", ""},
		{"http://192.168.1.20:16789/agent", ""},
		{"http://172.31.254.10:16789/agent", ""}, // the neighbouring /24
	} {
		c := &config.Config{}
		c.Agent.AgentURI = tc.uri
		if got := PresetAgentIP(c); got != tc.want {
			t.Errorf("PresetAgentIP(%q) = %q, want %q", tc.uri, got, tc.want)
		}
	}
	if got := PresetAgentIP(nil); got != "" {
		t.Errorf("PresetAgentIP(nil) = %q", got)
	}
}

// TestThePinnedAddressIsInsideTheDeclaredSubnet keeps the two constants in
// step: the generator pins the agent to AgentInternalIP, and composegen
// declares InternalSubnet. An address outside its own subnet fails at
// compose-up, after the Pod is already running.
func TestThePinnedAddressIsInsideTheDeclaredSubnet(t *testing.T) {
	_, netw, err := net.ParseCIDR(InternalSubnet)
	if err != nil {
		t.Fatalf("InternalSubnet %q does not parse: %v", InternalSubnet, err)
	}
	ip := net.ParseIP(AgentInternalIP)
	if ip == nil {
		t.Fatalf("AgentInternalIP %q does not parse", AgentInternalIP)
	}
	if !netw.Contains(ip) {
		t.Errorf("AgentInternalIP %s is outside InternalSubnet %s", AgentInternalIP, InternalSubnet)
	}
}
