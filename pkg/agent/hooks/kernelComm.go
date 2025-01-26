//go:build linux

package hooks

import (
	"context"

	"github.com/cilium/ebpf"
	"go.keploy.io/server/v2/pkg/agent"
	"go.keploy.io/server/v2/pkg/agent/hooks/structs"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

// Get Used by proxy
func (h *Hooks) Get(_ context.Context, srcPort uint16) (*agent.NetworkAddress, error) {
	d, err := h.GetDestinationInfo(srcPort)
	if err != nil {
		return nil, err
	}
	// TODO : need to implement eBPF code to differentiate between different apps
	// s, ok := h.sess.Get(d.ClientID)
	// if !ok {
	// 	return nil, fmt.Errorf("session not found")
	// }

	return &agent.NetworkAddress{
		ClientID: d.ClientID,
		Version:  d.IPVersion,
		IPv4Addr: d.DestIP4,
		IPv6Addr: d.DestIP6,
		Port:     d.DestPort,
	}, nil
}

// GetDestinationInfo retrieves destination information associated with a source port.
func (h *Hooks) GetDestinationInfo(srcPort uint16) (*structs.DestInfo, error) {
	h.m.Lock()
	defer h.m.Unlock()
	destInfo := structs.DestInfo{
		ClientID: 0,
	}
	if err := h.redirectProxyMap.Lookup(srcPort, &destInfo); err != nil {
		return nil, err
	}
	return &destInfo, nil
}

func (h *Hooks) Delete(_ context.Context, srcPort uint16) error {
	return h.CleanProxyEntry(srcPort)
}

func (h *Hooks) CleanProxyEntry(srcPort uint16) error {
	h.m.Lock()
	defer h.m.Unlock()
	err := h.redirectProxyMap.Delete(srcPort)
	if err != nil {
		utils.LogError(h.logger, err, "failed to remove entry from redirect proxy map")
		return err
	}
	h.logger.Debug("successfully removed entry from redirect proxy map", zap.Any("(Key)/SourcePort", srcPort))
	return nil
}

func (h *Hooks) SendClientInfo(id uint64, clientInfo structs.ClientInfo) error {
	h.m.Lock()
	defer h.m.Unlock()
	err := h.clientRegistrationMap.Update(id, clientInfo, ebpf.UpdateAny)
	if err != nil {
		utils.LogError(h.logger, err, "failed to send the app info to the ebpf program")
		return err
	}
	return nil
}

func (h *Hooks) DeleteClientInfo(id uint64) error {
	h.m.Lock()
	defer h.m.Unlock()
	err := h.clientRegistrationMap.Delete(id)
	if err != nil {
		utils.LogError(h.logger, err, "failed to send the app info to the ebpf program")
		return err
	}
	h.logger.Info("successfully removed the client info from the ebpf program with clientId", zap.Any("clientId", id))
	return nil
}

// SendProxyInfo sends the network information to the kernel
func (h *Hooks) SendProxyInfo(id uint64, proxInfo structs.ProxyInfo) error {
	h.m.Lock()
	defer h.m.Unlock()
	err := h.proxyInfoMap.Update(id, proxInfo, ebpf.UpdateAny)
	if err != nil {
		utils.LogError(h.logger, err, "failed to send the proxy info to the ebpf program")
		return err
	}
	return nil
}

// func (h *Hooks) SendTestBenchInfo(key models.ModeKey, tb structs.TestBenchInfo) error {
// 	fmt.Println("Test bench info", tb)
// 	if key == models.RecordKey {
// 		h.logger.Info("Sending test bench info for record mode")
// 	} else {
// 		h.logger.Info("Sending test bench info for test mode")
// 	}

// 	err := h.tbenchFilterPid.Update(uint32(0), tb, ebpf.UpdateAny)
// 	if err != nil {
// 		utils.LogError(h.logger, err, "failed to send the test bench info to the ebpf program")
// 		return err
// 	}
// 	return nil
// }

func (h *Hooks) SendAgentInfo(agentInfo structs.AgentInfo) error {
	key := 0
	err := h.agentRegistartionMap.Update(uint32(key), agentInfo, ebpf.UpdateAny)
	if err != nil {
		utils.LogError(h.logger, err, "failed to send the agent info to the ebpf program")
		return err
	}
	return nil
}
