//go:build linux

package linux

import (
	"encoding/binary"
	"testing"

	"github.com/cilium/ebpf"
	"go.uber.org/zap"

	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
)

// The proxy moved off a taken port (proxy.SetPortMover): the hooks that
// loaded redirect to the port they were given until they are told the new
// one, and are told by sending the kernel the agent info Load sent again,
// with only the proxy's and its DNS server's ports changed. Hooks that sent
// none (not loaded, or Load's send failed) say so and send nothing. As root: the agent info is a
// kernel map's.
func TestTheLoadedHooksAreToldTheProxysMovedPorts(t *testing.T) {
	m, err := ebpf.NewMap(&ebpf.MapSpec{Type: ebpf.Array, KeySize: 4, ValueSize: uint32(binary.Size(structs.AgentInfo{})), MaxEntries: 1})
	if err != nil {
		t.Skipf("no kernel map for the agent info (needs root): %v", err)
	}
	defer func() { _ = m.Close() }()
	h := &Hooks{logger: zap.NewNop(), agentRegistrationMap: m}
	if err := h.SetProxyPorts(40001, 40002); err == nil {
		t.Fatal("hooks that sent no agent info took the proxy's ports")
	}
	var got structs.AgentInfo
	if err := m.Lookup(uint32(0), &got); err != nil {
		t.Fatal(err)
	}
	if got != (structs.AgentInfo{}) {
		t.Fatalf("hooks that sent no agent info sent the kernel %+v", got)
	}

	// A Load whose send failed sent the kernel nothing: no hooks to tell.
	wrong, err := ebpf.NewMap(&ebpf.MapSpec{Type: ebpf.Array, KeySize: 4, ValueSize: uint32(binary.Size(structs.AgentInfo{})) + 8, MaxEntries: 1})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = wrong.Close() }()
	h.agentRegistrationMap = wrong
	if err := h.SendAgentInfo(structs.AgentInfo{DNSPort: 26789}); err == nil {
		t.Fatal("a send to a map of another value size succeeded")
	}
	h.agentRegistrationMap = m
	if err := h.SetProxyPorts(40001, 40002); err == nil {
		t.Fatal("hooks whose Load failed to send took the proxy's ports")
	}

	loaded := structs.AgentInfo{KeployAgentNsPid: 7, DNSPort: 26789, IsDocker: 1, Flags: 0x5, RecordingStartTime: 99,
		Proxy: structs.ProxyInfo{IP4: 0x7f000001, IP6: [4]uint32{0, 0, 0, 1}, Port: 16789}}
	if err := h.SendAgentInfo(loaded); err != nil {
		t.Fatal(err)
	}
	if err := h.SetProxyPorts(40001, 40002); err != nil {
		t.Fatalf("the loaded hooks: %v", err)
	}
	want := loaded
	want.Proxy.Port, want.DNSPort = 40001, 40002
	if err := m.Lookup(uint32(0), &got); err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("the kernel holds %+v, want %+v: the moved ports, the rest as Load sent it", got, want)
	}
}
