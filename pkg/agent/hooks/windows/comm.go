//go:build windows && amd64

package windows

import (
	"context"
	"errors"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
)

type WinDest struct {
	IPVersion uint32
	DestIP4   uint32
	DestIP6   [4]uint32
	DestPort  uint32
	KernelPid uint32
}

func (h *Hooks) Get(_ context.Context, srcPort uint16) (*agent.NetworkAddress, error) {
	d, ok := GetDestination(uint32(srcPort))
	if !ok {
		return nil, errors.New("destination not found")
	}

	return &agent.NetworkAddress{
		Version:  d.IPVersion,
		IPv4Addr: d.DestIP4,
		IPv6Addr: d.DestIP6,
		Port:     d.DestPort,
	}, nil
}

func (h *Hooks) Delete(_ context.Context, srcPort uint16) error {
	return h.CleanProxyEntry(srcPort)
}

func (h *Hooks) CleanProxyEntry(srcPort uint16) error {
	return nil
}

func (h *Hooks) SendAgentInfo(agentInfo structs.AgentInfo) error {
	return nil
}