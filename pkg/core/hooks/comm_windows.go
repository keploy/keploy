//go:build windows

package hooks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/hooks/ipc/grpc"
	"go.keploy.io/server/v2/pkg/core/hooks/structs"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/utils"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
)

//TODO: rename this file.

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
	info, ok := h.dstMap[uint32(srcPort)]
	if !ok {
		h.logger.Error("failed to get the dest info")
		return nil, errors.New("failed to get dst info")
	}
	dstInfo := structs.DestInfo{
		DestPort: info.Port,
	}
	return &dstInfo, nil
}

func (h *Hooks) Delete(_ context.Context, srcPort uint16) error {
	return h.CleanProxyEntry(srcPort)
}

func (h *Hooks) CleanProxyEntry(srcPort uint16) error {
	return nil
}

func (h *Hooks) SendClientInfo(id uint64, appInfo structs.ClientInfo) error {

	conf := &grpc.InterceptConf{
		Default: true,
		Actions: []string{strconv.Itoa(int(appInfo.KeployClientNsPid))},
	}

	interceptConf := &grpc.FromProxy_InterceptConf{
		InterceptConf: conf,
	}

	fromProxy := &grpc.FromProxy{
		Message: interceptConf,
	}

	data, err := proto.Marshal(fromProxy)
	if err != nil {
		h.logger.Info("Failed to marshal response:", zap.Error(err))
	}

	_, err = h.conn.Write(data)
	if err != nil {
		h.logger.Error("failed to send intercept config info", zap.Error(err))
		return err
	}

	return nil
}

func (h *Hooks) SendAgentInfo(agentInfo structs.AgentInfo) error {
	return nil
}

func (h *Hooks) SendDockerAppInfo(_ uint64, dockerAppInfo structs.DockerAppInfo) error {
	return nil
}

func (h *Hooks) ReadDestInfo(ctx context.Context) error {
	for {
		buf, err := util.ReadBytes(ctx, h.logger, h.conn)
		if err != nil {
			if err == io.EOF {
				h.logger.Debug("recieved request buffer is empty from redirector")
				return nil
			}
			utils.LogError(h.logger, err, "failed to read request from redirector")
			return err
		}
		var packet grpc.Address
		err = proto.Unmarshal(buf, &packet)
		if err != nil {
			utils.LogError(h.logger, err, "failed to decode message from redirector")
			return err
		}
		h.dstMap[packet.Port] = packet
	}
}
