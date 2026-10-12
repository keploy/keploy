//go:build linux

package linux

import (
	"context"
	"encoding/binary"
	"os"
	"sync"
	"testing"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/rlimit"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// The kernel side of the bypass list is a fixed-size array; RegisterClient
// copies at most this many ports into it and drops the rest.
const kernelPortCapacity = len(structs.ClientInfo{}.PassThroughPorts)

const testAgentPort = 16790

// newRegisterClientHooks builds a Hooks wired to a REAL bpf hash map standing in
// for client_info, so RegisterClient runs unmodified all the way through
// SendClientInfo and we can read back exactly what the kernel would see.
func newRegisterClientHooks(t *testing.T) (*Hooks, *ebpf.Map, *observer.ObservedLogs) {
	t.Helper()
	// Asserting on the array the kernel actually receives needs a real BPF map,
	// which needs CAP_BPF. Without it these tests can only skip -- and a skip
	// here reports the package green while the thing under test never ran,
	// which is precisely the failure this commit is about. So a lane that DOES
	// have the capability sets KEPLOY_TEST_BPF_REQUIRED and turns the skip
	// fatal. Keyed on its own var rather than on CI, for the same reason
	// KEPLOY_TEST_MYSQL_REQUIRED is: CI is set on every fork and every lane,
	// including ones that were never offered the capability.
	required := os.Getenv("KEPLOY_TEST_BPF_REQUIRED") != ""
	unavailable := func(err error) {
		t.Helper()
		if required {
			t.Fatalf("KEPLOY_TEST_BPF_REQUIRED is set but this lane cannot create a bpf map: %v", err)
		}
		t.Skipf("no CAP_BPF, cannot assert on the real kernel array: %v", err)
	}

	if err := rlimit.RemoveMemlock(); err != nil {
		unavailable(err)
	}
	m, err := ebpf.NewMap(&ebpf.MapSpec{
		Name:       "client_info_t",
		Type:       ebpf.Hash,
		KeySize:    8,
		ValueSize:  uint32(binary.Size(structs.ClientInfo{})),
		MaxEntries: 1,
	})
	if err != nil {
		unavailable(err)
	}
	t.Cleanup(func() { _ = m.Close() })

	core, logs := observer.New(zapcore.WarnLevel)
	return &Hooks{
		logger:                zap.New(core),
		objectsMutex:          sync.RWMutex{},
		m:                     sync.Mutex{},
		clientRegistrationMap: m,
	}, m, logs
}

func portRules(ports ...uint) []models.BypassRule {
	rules := make([]models.BypassRule, 0, len(ports))
	for _, p := range ports {
		rules = append(rules, models.BypassRule{Port: p})
	}
	return rules
}

func readBack(t *testing.T, m *ebpf.Map) structs.ClientInfo {
	t.Helper()
	var got structs.ClientInfo
	if err := m.Lookup(uint64(0), &got); err != nil {
		t.Fatalf("lookup client_info: %v", err)
	}
	return got
}

func hasPort(a [10]int32, p int32) bool {
	for _, v := range a {
		if v == p {
			return true
		}
	}
	return false
}

// THE BUG. The agent's own control port is the one bypass the wrapped test
// runner itself depends on: in mock mode the runner calls /agent/scope/* at
// localhost:<AgentPort>, and without a kernel bypass that call is redirected
// into the proxy instead of reaching the agent. Appended LAST, it is the first
// thing the fixed-size array truncates away when a user supplies a full set of
// --pass-through-ports.
func TestRegisterClient_AgentPortSurvivesAFullUserPortList(t *testing.T) {
	h, m, _ := newRegisterClientHooks(t)

	userPorts := make([]uint, 0, kernelPortCapacity)
	for i := 0; i < kernelPortCapacity; i++ {
		userPorts = append(userPorts, uint(7001+i))
	}

	opts := config.Agent{SetupOptions: models.SetupOptions{
		Mode: models.MODE_RECORD, MockMode: true, AgentPort: testAgentPort,
	}}
	if err := h.RegisterClient(context.Background(), opts, portRules(userPorts...)); err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}

	got := readBack(t, m)
	if !hasPort(got.PassThroughPorts, testAgentPort) {
		t.Fatalf("agent control port %d was truncated out of the kernel bypass list; mock mode would fail outright.\n  got: %v", testAgentPort, got.PassThroughPorts)
	}
	if got.PassThroughPorts[0] != testAgentPort {
		t.Fatalf("agent control port should claim slot 0 ahead of user ports, got slot value %d\n  got: %v", got.PassThroughPorts[0], got.PassThroughPorts)
	}
}

// Truncation itself is legitimate; silently dropping part of an explicit flag
// is not.
func TestRegisterClient_WarnsWhenUserPortsAreTruncated(t *testing.T) {
	h, _, logs := newRegisterClientHooks(t)

	userPorts := make([]uint, 0, kernelPortCapacity+3)
	for i := 0; i < kernelPortCapacity+3; i++ {
		userPorts = append(userPorts, uint(7001+i))
	}

	opts := config.Agent{SetupOptions: models.SetupOptions{
		Mode: models.MODE_RECORD, MockMode: true, AgentPort: testAgentPort,
	}}
	if err := h.RegisterClient(context.Background(), opts, portRules(userPorts...)); err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}

	if logs.FilterMessageSnippet("kernel bypass list can hold").Len() == 0 {
		t.Fatalf("no warning logged for %d ports into a %d-slot array; the drop is silent.\n  logs: %v",
			len(userPorts)+1, kernelPortCapacity, logs.All())
	}
}

// CONTROL: under the capacity everything fits, so both orderings would pass.
// This one must be green before AND after the fix, or the tests above are
// measuring the wrong thing.
func TestRegisterClient_ShortListKeepsEveryPort(t *testing.T) {
	h, m, _ := newRegisterClientHooks(t)

	opts := config.Agent{SetupOptions: models.SetupOptions{
		Mode: models.MODE_RECORD, MockMode: true, AgentPort: testAgentPort,
	}}
	if err := h.RegisterClient(context.Background(), opts, portRules(7001, 7002, 7003)); err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}

	got := readBack(t, m)
	for _, want := range []int32{testAgentPort, 7001, 7002, 7003} {
		if !hasPort(got.PassThroughPorts, want) {
			t.Fatalf("port %d missing: %v", want, got.PassThroughPorts)
		}
	}
	for i := 4; i < kernelPortCapacity; i++ {
		if got.PassThroughPorts[i] != -1 {
			t.Fatalf("unused slot %d should be -1, got %d: %v", i, got.PassThroughPorts[i], got.PassThroughPorts)
		}
	}
}

// CONTROL: outside mock mode there is no agent control port to reserve, and the
// user's ports must still start at slot 0.
func TestRegisterClient_NonMockModeReservesNothing(t *testing.T) {
	h, m, _ := newRegisterClientHooks(t)

	opts := config.Agent{SetupOptions: models.SetupOptions{
		Mode: models.MODE_RECORD, MockMode: false, AgentPort: testAgentPort,
	}}
	if err := h.RegisterClient(context.Background(), opts, portRules(7001, 7002)); err != nil {
		t.Fatalf("RegisterClient: %v", err)
	}

	got := readBack(t, m)
	if hasPort(got.PassThroughPorts, testAgentPort) {
		t.Fatalf("agent port must not be bypassed outside mock mode: %v", got.PassThroughPorts)
	}
	if got.PassThroughPorts[0] != 7001 || got.PassThroughPorts[1] != 7002 {
		t.Fatalf("user ports should start at slot 0: %v", got.PassThroughPorts)
	}
}
