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

// GetDestinationInfo retrieves destination information associated with a source port.
// func (h *Hooks) GetDestinationInfo(srcPort uint16) (*structs.DestInfo, error) {
// 	newPort := uint32(srcPort)
// 	info, ok := h.dstMap.Load(newPort)
// 	if !ok {
// 		h.logger.Error("failed to get the dest info")
// 		return nil, errors.New("failed to get dst info")
// 	}
// 	new, ok := info.(WinDest)
// 	if !ok {
// 		return nil, errors.New("internal server error")
// 	}
// 	var host uint32
// 	var v6host [4]uint32
// 	var ipVersion uint32
// 	host, err := util.IP4StrToUint32(new.Host)
// 	if err != nil {
// 		v6host, err = util.IP6StrToUint32(new.Host)
// 		if err != nil {
// 			h.logger.Error("failed to convert IP", zap.Error(err))
// 			return nil, err
// 		}
// 		ipVersion = 6
// 	} else {
// 		ipVersion = 4
// 	}
// 	dstInfo := structs.DestInfo{
// 		DestIP4:   host,
// 		DestPort:  new.Port,
// 		DestIP6:   v6host,
// 		IPVersion: ipVersion,
// 	}
// 	return &dstInfo, nil
// }

func (h *Hooks) Delete(_ context.Context, srcPort uint16) error {
	return h.CleanProxyEntry(srcPort)
}

func (h *Hooks) CleanProxyEntry(srcPort uint16) error {
	return nil
}

func (h *Hooks) SendAgentInfo(agentInfo structs.AgentInfo) error {
	return nil
}

// func (h *Hooks) SendDockerAppInfo(_ uint64, dockerAppInfo structs.DockerAppInfo) error {
// 	return nil
// }
