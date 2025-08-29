//go:build linux

package hooks

import (
	"context"
	"fmt"

	"github.com/cilium/ebpf"
	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/hooks/structs"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
)

//TODO: rename this file.

// Get Used by proxy
func (h *Hooks) Get(_ context.Context, srcPort uint16) (*core.NetworkAddress, error) {
	d, err := h.GetDestinationInfo(srcPort)
	if err != nil {
		return nil, err
	}

	// Use the current app ID with proper synchronization
	h.m.Lock()
	currentAppID := h.appID
	h.m.Unlock()

	s, ok := h.sess.Get(currentAppID)
	if !ok {
		return nil, fmt.Errorf("session not found")
	}

	return &core.NetworkAddress{
		AppID:    s.ID,
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
	destInfo := structs.DestInfo{}
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
	h.logger.Debug("successfully removed entry from redirect proxy map", zap.Uint16("(Key)/SourcePort", srcPort))
	return nil
}

func (h *Hooks) SendClientInfo(id uint64, appInfo structs.ClientInfo) error {
	err := h.clientRegistrationMap.Update(id, appInfo, ebpf.UpdateAny)
	if err != nil {
		utils.LogError(h.logger, err, "failed to send the app info to the ebpf program")
		return err
	}
	return nil
}

func (h *Hooks) SendAgentInfo(agentInfo structs.AgentInfo) error {
	key := 0
	err := h.agentRegistartionMap.Update(uint32(key), agentInfo, ebpf.UpdateAny)
	if err != nil {
		utils.LogError(h.logger, err, "failed to send the agent info to the ebpf program")
		return err
	}
	return nil
}

func (h *Hooks) SendE2EInfo(pid uint32) error {
	key := 0
	err := h.e2eAppRegistrationMap.Update(uint64(key), pid, ebpf.UpdateAny)
	if err != nil {
		utils.LogError(h.logger, err, "failed to send the E2E info to the ebpf program")
		return err
	}
	return nil
}

func (h *Hooks) SendDockerAppInfo(appID uint64, dockerAppInfo structs.DockerAppInfo) error {
	h.m.Lock()
	defer h.m.Unlock()

	// Use the provided app ID or the current app ID, don't generate a random one
	dockerAppID := appID
	if dockerAppID == 0 {
		dockerAppID = h.appID
	}

	if h.appID != 0 {
		err := h.dockerAppRegistrationMap.Delete(h.appID)
		if err != nil {
			utils.LogError(h.logger, err, "failed to remove entry from dockerAppRegistrationMap", zap.Uint64("(Key)/AppID", h.appID))
			return err
		}
	}

	// Don't override the app ID with a random number - use the real app ID
	err := h.dockerAppRegistrationMap.Update(dockerAppID, dockerAppInfo, ebpf.UpdateAny)
	if err != nil {
		utils.LogError(h.logger, err, "failed to send the dockerAppInfo info to the ebpf program", zap.Uint64("appID", dockerAppID))
		return err
	}

	// Update the app ID only if we received a valid one
	if appID != 0 {
		h.appID = appID
	}

	return nil
}
