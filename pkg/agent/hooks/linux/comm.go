//go:build linux

package linux

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/ringbuf"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

//TODO: rename this file.

// Get Used by proxy
func (h *Hooks) Get(_ context.Context, srcPort uint16) (*agent.NetworkAddress, error) {
	d, err := h.GetDestinationInfo(srcPort)
	if err != nil {
		return nil, err
	}

	return &agent.NetworkAddress{
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

func (h *Hooks) SendClientInfo(clientInfo structs.ClientInfo) error {
	err := h.clientRegistrationMap.Update(uint64(0), clientInfo, ebpf.UpdateAny)
	if err != nil {
		utils.LogError(h.logger, err, "failed to send the client info to the ebpf program")
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

func (h *Hooks) WatchBindEvents(ctx context.Context) (<-chan models.IngressEvent, error) {
	rb, err := ringbuf.NewReader(h.BindEvents) // Assuming h.BindEvents is the eBPF map
	if err != nil {
		return nil, err // Return error if we can't create the reader
	}

	eventChan := make(chan models.IngressEvent, 100) // A buffered channel can be useful

	go func() {
		defer rb.Close()
		defer close(eventChan)

		for {
			// Read raw data from the ring buffer
			rec, err := rb.Read()
			if err != nil {
				// If the reader was closed, it's a clean shutdown.
				if errors.Is(err, ringbuf.ErrClosed) {
					return
				}
				continue
			}

			var e models.IngressEvent
			if err := binary.Read(bytes.NewReader(rec.RawSample), binary.LittleEndian, &e); err != nil {
				utils.LogError(h.logger, err, "failed to decode ingress event")
				continue
			}
			h.logger.Debug("Intercepted application bind event")
			select {
			case <-ctx.Done(): // Context was cancelled, so we shut down.
				return
			case eventChan <- e: // Send the decoded event to the channel.
			}
		}
	}()
	return eventChan, nil
}
