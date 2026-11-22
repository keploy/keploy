//go:build linux

package linux

import (
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// The kernel's pass-through list holds 10 ports. Before, RegisterClient put
// the user's port-only bypassRules first and keploy's own ports after them,
// then kept the first 10 and dropped the rest without a word. So with 10
// port-only bypassRules, `keploy mock` lost the kernel bypass of its own
// scope API: the test runner's /agent/scope calls went into the proxy. Now
// keploy's own ports come first, and a run whose ports do not fit is refused
// at start, naming them.

func portRules(ports ...uint) []models.BypassRule {
	rules := make([]models.BypassRule, len(ports))
	for i, p := range ports {
		rules[i] = models.BypassRule{Port: p}
	}
	return rules
}

func mockAgent(agentPort uint32) config.Agent {
	var a config.Agent
	a.Mode = models.MODE_TEST
	a.MockMode = true
	a.AgentPort = agentPort
	return a
}

func listOf(ports ...int32) [kernelPassThroughSlots]int32 {
	var l [kernelPassThroughSlots]int32
	for i := range l {
		l[i] = -1
	}
	copy(l[:], ports)
	return l
}

func withExtraPorts(t *testing.T, ports ...uint) {
	t.Helper()
	prev := agent.ExtraPassThroughPortsHook
	agent.ExtraPassThroughPortsHook = func() []uint { return ports }
	t.Cleanup(func() { agent.ExtraPassThroughPortsHook = prev })
}

func TestKernelPassThroughListHoldsTen(t *testing.T) {
	if kernelPassThroughSlots != 10 {
		t.Fatalf("the kernel's list holds %d ports here; keploy/ebpf's PASS_THROUGH_ARRAY_SIZE is 10", kernelPassThroughSlots)
	}
}

// Mock mode, 9 port-only bypassRules: the agent's control port and all 9 fit,
// the control port first.
func TestMockModeNineBypassRulesKeepTheScopePort(t *testing.T) {
	ci, err := clientInfoFor(context.Background(), mockAgent(16789), portRules(9001, 9002, 9003, 9004, 9005, 9006, 9007, 9008, 9009))
	if err != nil {
		t.Fatalf("9 bypassRules and the control port are 10 ports and fit: %v", err)
	}
	want := listOf(16789, 9001, 9002, 9003, 9004, 9005, 9006, 9007, 9008, 9009)
	if ci.PassThroughPorts != want {
		t.Fatalf("pass-through list = %v, want %v", ci.PassThroughPorts, want)
	}
}

// Mock mode, 10 port-only bypassRules: 11 ports. The start is refused, and the
// error names the bypassRule that does not fit, never the control port.
func TestMockModeTenBypassRulesRefusedNamingTheRuleNotTheScopePort(t *testing.T) {
	_, err := clientInfoFor(context.Background(), mockAgent(16789), portRules(9001, 9002, 9003, 9004, 9005, 9006, 9007, 9008, 9009, 9010))
	if err == nil {
		t.Fatal("10 bypassRules and the control port are 11 ports, one more than the kernel holds, and were accepted")
	}
	msg := err.Error()
	if !strings.Contains(msg, "instead: 9010. Keep the bypassRules to at most 9 distinct ports") {
		t.Fatalf("the error must name bypassRules port 9010, and only it, as not fitting: %q", msg)
	}
	if !strings.Contains(msg, "keploy's own (16789) take 1") {
		t.Fatalf("the error must say keploy's own control port takes a slot: %q", msg)
	}
}

// keploy record with 10 port-only bypassRules (no port of keploy's own): all
// fit. An 11th is refused by name.
func TestRecordTenBypassRulesFitAnEleventhIsRefused(t *testing.T) {
	var rec config.Agent
	rec.Mode = models.MODE_RECORD
	ten := []uint{9001, 9002, 9003, 9004, 9005, 9006, 9007, 9008, 9009, 9010}
	ci, err := clientInfoFor(context.Background(), rec, portRules(ten...))
	if err != nil {
		t.Fatalf("10 bypassRules fit: %v", err)
	}
	if ci.Mode != 1 || ci.PassThroughPorts != listOf(9001, 9002, 9003, 9004, 9005, 9006, 9007, 9008, 9009, 9010) {
		t.Fatalf("client info = %+v", ci)
	}
	_, err = clientInfoFor(context.Background(), rec, portRules(append(ten, 9094)...))
	if err == nil || !strings.Contains(err.Error(), "instead: 9094. Keep the bypassRules to at most 10 distinct ports") {
		t.Fatalf("an 11th bypassRule port must be refused by name, got %v", err)
	}
}

// The ports a provider adds (agent.ExtraPassThroughPortsHook) come before the
// user's, so they are never the ones that do not fit.
func TestExtraPassThroughPortsComeFirst(t *testing.T) {
	withExtraPorts(t, 7001, 7002)
	ci, err := clientInfoFor(context.Background(), mockAgent(16789), portRules(9001))
	if err != nil {
		t.Fatal(err)
	}
	if want := listOf(7001, 7002, 16789, 9001); ci.PassThroughPorts != want {
		t.Fatalf("pass-through list = %v, want %v", ci.PassThroughPorts, want)
	}
	_, err = clientInfoFor(context.Background(), mockAgent(16789), portRules(9001, 9002, 9003, 9004, 9005, 9006, 9007, 9008, 9009))
	if err == nil {
		t.Fatal("2 provider ports, the control port and 9 bypassRules are 12 ports and were accepted")
	}
	if msg := err.Error(); !strings.Contains(msg, "instead: 9008, 9009. Keep the bypassRules to at most 7 distinct ports") {
		t.Fatalf("the error must name the last two bypassRules, not keploy's ports: %q", msg)
	}
}

// A port listed twice, or a bypassRule on a port keploy already passes
// through, takes one slot.
func TestPassThroughListCountsEachPortOnce(t *testing.T) {
	rules := portRules(16789, 9001, 9001, 9002, 9003, 9004, 9005, 9006, 9007, 9008, 9009)
	ci, err := clientInfoFor(context.Background(), mockAgent(16789), rules)
	if err != nil {
		t.Fatalf("10 distinct kernel ports fit: %v", err)
	}
	if want := listOf(16789, 9001, 9002, 9003, 9004, 9005, 9006, 9007, 9008, 9009); ci.PassThroughPorts != want {
		t.Fatalf("pass-through list = %v, want %v", ci.PassThroughPorts, want)
	}
}

// keploy's own ports alone over the limit: refused, naming them.
func TestOwnPortsOverTheLimitAreRefused(t *testing.T) {
	withExtraPorts(t, 7001, 7002, 7003, 7004, 7005, 7006, 7007, 7008, 7009, 7010)
	_, err := clientInfoFor(context.Background(), mockAgent(16789), portRules(9001))
	if err == nil || !strings.Contains(err.Error(), "(7001, 7002, 7003, 7004, 7005, 7006, 7007, 7008, 7009, 7010, 16789), which leaves no room for bypassRules. Keploy's own 16789 and the bypassRules (or --pass-through-ports) ports 9001 cannot be passed through") {
		t.Fatalf("11 ports of keploy's own must be refused by name, got %v", err)
	}
}

// A pass-through port past 65535 is refused by name. Before, it was cast to
// int32: 4294967295 became -1, where the kernel stops reading the list, so
// every port after it was dropped too.
func TestPassThroughPortOutOfRangeIsRefused(t *testing.T) {
	var rec config.Agent
	rec.Mode = models.MODE_RECORD
	_, err := clientInfoFor(context.Background(), rec, portRules(9001, 4294967295, 9002, 4294967295))
	if err == nil || !strings.Contains(err.Error(), "the pass-through ports 4294967295 are not ports (1-65535)") {
		t.Fatalf("port 4294967295 must be refused by name, got %v", err)
	}
}
