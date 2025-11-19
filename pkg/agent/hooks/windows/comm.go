//go:build windows

package windows

import (
	"context"
	"errors"
	"io"
	"strconv"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/hooks/conn"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/agent/hooks/windows/ipc/grpc"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"google.golang.org/protobuf/proto"
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

func (h *Hooks) SendClientInfo(appInfo structs.ClientInfo) error {

	conf := &grpc.InterceptConf{
		Default: true,
		Actions: []string{strconv.Itoa(int(appInfo.ClientNSPID))},
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

// func (h *Hooks) SendDockerAppInfo(_ uint64, dockerAppInfo structs.DockerAppInfo) error {
// 	return nil
// }

func (h *Hooks) GetEvents(ctx context.Context) error {
	for {
		buf, err := util.ReadBytes(ctx, h.logger, h.conn)
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

			// dest := WinDest{
			// 	IPVersion: addr.Version,
			// 	DestIP4:   addr.Host,
			// 	DestPort:  addr.Port,
			// }
			h.dstMap.Store(addr.SrcPort, WinDest{})
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
			// Chunk the incoming message into EventBodyMaxSize-sized events so
			// we don't lose data when the incoming proto msg is larger than the
			// fixed-size buffer used by the Tracker/conn structs.
			fullMsg := msg.SocketDataEvent.GetMsg()
			total := len(fullMsg)
			// iterate over fullMsg in chunks of conn.EventBodyMaxSize
			for offset := 0; offset < total; {
				end := offset + int(conn.EventBodyMaxSize)
				if end > total {
					end = total
				}
				var msgArray [conn.EventBodyMaxSize]byte
				chunk := fullMsg[offset:end]
				copy(msgArray[:], chunk)

				event := conn.SocketDataEvent{
					TimestampNano:      msg.SocketDataEvent.GetTimeStampNano(),
					ConnID:             conn.ID{TGID: msg.SocketDataEvent.GetPid()},
					EntryTimestampNano: msg.SocketDataEvent.GetEntryTimeStampNano(),
					Direction:          direction,
					// MsgSize here represents the size of this chunk
					MsgSize:              uint64(len(chunk)),
					Pos:                  uint64(offset),
					Msg:                  msgArray,
					ValidateReadBytes:    msg.SocketDataEvent.GetValidateReadBytes(),
					ValidateWrittenBytes: msg.SocketDataEvent.GetValidateWrittenBytes(),
				}
				h.dataEventChan <- event

				offset = end
			}
		default:
			h.logger.Error("received unknown message type")
		}
	}
}

func (h *Hooks) RegisterClient(ctx context.Context, opts config.Agent, rules []models.BypassRule) error {
	h.logger.Info("Registering the client Info with keploy")
	// Register the client and start processing

	clientInfo := structs.ClientInfo{}

	switch opts.Mode {
	case models.MODE_RECORD:
		clientInfo.Mode = uint32(1)
	case models.MODE_TEST:
		clientInfo.Mode = uint32(2)
	default:
		clientInfo.Mode = uint32(0)
	}

	ports := agent.GetPortToSendToKernel(ctx, rules)
	for i := 0; i < 10; i++ {
		if len(ports) <= i {
			clientInfo.PassThroughPorts[i] = -1
			continue
		}
		clientInfo.PassThroughPorts[i] = int32(ports[i])
	}
	clientInfo.ClientNSPID = opts.ClientNSPID
	return h.SendClientInfo(clientInfo)
}
