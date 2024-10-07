package hooks

import (
	"context"
	"fmt"

	"math/rand"

	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/hooks/structs"
	"go.uber.org/zap"
)

// Get Used by proxy
func (h *Hooks) Get(_ context.Context, srcPort uint16) (*core.NetworkAddress, error) {
	d, err := h.GetDestinationInfo(srcPort)
	if err != nil {
		return nil, err
	}
	// TODO : need to implement eBPF code to differentiate between different apps
	s, ok := h.sess.Get(0)
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
	return &destInfo, nil
}

func (h *Hooks) Delete(_ context.Context, srcPort uint16) error {
	return h.CleanProxyEntry(srcPort)
}

func (h *Hooks) CleanProxyEntry(srcPort uint16) error {
	h.m.Lock()
	defer h.m.Unlock()
	h.logger.Debug("successfully removed entry from redirect proxy map", zap.Any("(Key)/SourcePort", srcPort))
	return nil
}

func (h *Hooks) SendClientInfo(id uint64, appInfo structs.ClientInfo) error {
	return nil
}

func (h *Hooks) SendAgentInfo(agentInfo structs.AgentInfo) error {
	return nil
}

func (h *Hooks) SendDockerAppInfo(_ uint64, dockerAppInfo structs.DockerAppInfo) error {
	r := rand.New(rand.NewSource(rand.Int63()))
	randomNum := r.Uint64()
	h.appID = randomNum
	return nil
}
