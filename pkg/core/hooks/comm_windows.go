//go:build windows

package hooks

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strconv"

	"go.keploy.io/server/v2/pkg/core"
	"go.keploy.io/server/v2/pkg/core/hooks/conn"
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
	newPort := uint32(srcPort)
	info, ok := h.dstMap.Load(newPort)
	if !ok {
		h.logger.Error("failed to get the dest info")
		return nil, errors.New("failed to get dst info")
	}
	new, ok := info.(WinDest)
	if !ok {
		return nil, errors.New("internal server error")
	}
	var host uint32
	var v6host [4]uint32
	var ipVersion uint32
	host, err := util.IP4StrToUint32(new.Host)
	if err != nil {
		v6host, err = util.IP6StrToUint32(new.Host)
		if err != nil {
			h.logger.Error("failed to convert IP", zap.Error(err))
			return nil, err
		}
		ipVersion = 6
	} else {
		ipVersion = 4
	}
	dstInfo := structs.DestInfo{
		DestIP4:   host,
		DestPort:  new.Port,
		DestIP6:   v6host,
		IPVersion: ipVersion,
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

	data, err := proto.Marshal(conf)
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

func (h *Hooks) GetEvents(ctx context.Context) error {
	for {
		buf, err := util.ReadBytes(ctx, h.logger, h.conn)
		fmt.Println("recieved few bytes")
		if err != nil {
			if err == io.EOF {
				h.logger.Error("recieved request buffer is empty from redirector")
				return nil
			}
			utils.LogError(h.logger, err, "failed to read request from redirector")
			return err
		}
		var message grpc.Message
		err = proto.Unmarshal(buf, &message)
		if err != nil {
			utils.LogError(h.logger, err, "Failed to decode message from redirector")
			return err
		}

		// Process the message based on its type
		switch msg := message.Message.(type) {
		case *grpc.Message_Flow:
			dst := msg.Flow.GetTcp()
			addr := dst.GetRemoteAddress()
			// h.logger.Info("port ", zap.Any("port", addr.GetPort()))
			// h.logger.Info("host ", zap.Any("addr", addr.GetHost()))
			// h.logger.Info("version ", zap.Any("version", addr.GetVersion()))
			// h.logger.Info("srcport", zap.Any("src", addr.GetSrcPort()))

			dest := WinDest{
				Host:    addr.Host,
				Port:    addr.Port,
				Version: addr.Version,
			}
			h.dstMap.Store(addr.SrcPort, dest)
		case *grpc.Message_SocketOpenEvent:
			event := conn.SocketOpenEvent{
				TimestampNano: msg.SocketOpenEvent.GetTimeStampNano(),
				ConnID:        conn.ID{TGID: msg.SocketOpenEvent.GetPid()},
			}
			h.openEventChan <- event
		case *grpc.Message_SocketCloseEvent:
			event := conn.SocketCloseEvent{
				TimestampNano: msg.SocketCloseEvent.GetTimeStampNano(),
				ConnID:        conn.ID{TGID: msg.SocketCloseEvent.GetPid()},
			}
			h.closeEventChan <- event
		case *grpc.Message_SocketDataEvent:
			direction := conn.EgressTraffic
			if msg.SocketDataEvent.GetDirection() {
				direction = conn.IngressTraffic
			}
			var msgArray [16384]byte
			copy(msgArray[:], msg.SocketDataEvent.GetMsg())
			event := conn.SocketDataEvent{
				TimestampNano:        msg.SocketDataEvent.GetTimeStampNano(),
				ConnID:               conn.ID{TGID: msg.SocketDataEvent.GetPid()},
				EntryTimestampNano:   msg.SocketDataEvent.GetEntryTimeStampNano(),
				Direction:            direction,
				MsgSize:              msg.SocketDataEvent.GetMsgSize(),
				Msg:                  msgArray,
				ValidateReadBytes:    msg.SocketDataEvent.GetValidateReadBytes(),
				ValidateWrittenBytes: msg.SocketDataEvent.GetValidateWrittenBytes(),
			}
			h.dataEventChan <- event
		default:
			h.logger.Error("received unknown message type")
		}
	}
}