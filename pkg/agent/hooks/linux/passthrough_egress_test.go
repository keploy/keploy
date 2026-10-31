//go:build linux

package linux

import (
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A replay names each port the kernel leaves alone as real egress when the
// client registers, with the knob that put it there; a recording does not,
// and a rule that names a host is the proxy's, not the kernel's.
func TestAReplayNamesTheKernelsPassThroughPortsAsRealEgress(t *testing.T) {
	rules := []models.BypassRule{{Port: 7002}, {Host: "api.example.com", Port: 443}}
	for _, mode := range []models.Mode{models.MODE_TEST, models.MODE_RECORD} {
		var opts config.Agent
		opts.Mode = mode
		got, err := clientInfoFor(context.Background(), opts, rules)
		if err != nil {
			t.Fatal(err)
		}
		if got.PassThroughPorts[0] != 7002 || got.PassThroughPorts[1] != -1 {
			t.Fatalf("%s: the kernel got pass-through ports %v, want [7002 -1 ...]", mode, got.PassThroughPorts)
		}

		core, logs := observer.New(zapcore.InfoLevel)
		logKernelPassThroughEgress(context.Background(), zap.New(core), opts, rules)
		var lines []observer.LoggedEntry
		for _, e := range logs.All() {
			if strings.HasPrefix(e.Message, agent.RealEgressPrefix) {
				lines = append(lines, e)
			}
		}
		if mode == models.MODE_RECORD {
			if len(lines) != 0 {
				t.Fatalf("a recording named real egress: %v", lines)
			}
			continue
		}
		if len(lines) != 1 || !strings.HasPrefix(lines[0].Message, agent.RealEgressPrefix+"bypassRules naming only a port: any host") {
			t.Fatalf("a replay logged %v, want one line naming the port-only bypass rule", lines)
		}
		if ports, ok := lines[0].ContextMap()["ports"].([]interface{}); !ok || len(ports) != 1 || ports[0] != uint(7002) {
			t.Fatalf("the line names ports %#v, want [7002]", lines[0].ContextMap()["ports"])
		}
	}
}

// keploy mock's own agent port is named as its own knob, before the user's.
func TestAMockReplayNamesItsAgentPortAsItsOwnKnob(t *testing.T) {
	opts := config.Agent{Mode: models.MODE_TEST, MockMode: true, AgentPort: 16789}
	core, logs := observer.New(zapcore.InfoLevel)
	logKernelPassThroughEgress(context.Background(), zap.New(core), opts, []models.BypassRule{{Port: 7002}})
	var knobs []string
	for _, e := range logs.All() {
		if rest, ok := strings.CutPrefix(e.Message, agent.RealEgressPrefix); ok {
			knobs = append(knobs, rest[:strings.Index(rest, ":")])
		}
	}
	if len(knobs) != 2 || knobs[0] != "keploy mock's agent API port" || knobs[1] != "bypassRules naming only a port" {
		t.Fatalf("knobs named %q, want keploy mock's agent API port, then the port-only bypass rule", knobs)
	}
}
