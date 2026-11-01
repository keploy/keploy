//go:build linux

package linux

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"net/netip"

	"github.com/cilium/ebpf"
	"github.com/cilium/ebpf/ringbuf"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

//TODO: rename this file.

// redirectKey is redirect_proxy_map's key (struct redirect_key_t): the
// application's end of a connection, its address (IPv4 as v4-mapped, word by
// word in network order) and port.
type redirectKey struct {
	IP   [4]uint32
	Port uint32
}

func redirectKeyFor(app netip.AddrPort) redirectKey {
	a := app.Addr()
	if a.Is4() {
		a = netip.AddrFrom16(a.As16()) // v4-mapped
	}
	b := a.As16()
	var k redirectKey
	for i := range k.IP {
		// Each word as the kernel stores it: the address's bytes in order.
		k.IP[i] = binary.NativeEndian.Uint32(b[4*i:])
	}
	k.Port = uint32(app.Port())
	return k
}

// GetForConn is the original destination of the connection whose application
// end is app (agent.ConnDestInfo), used by the proxy.
func (h *Hooks) GetForConn(_ context.Context, app netip.AddrPort) (*agent.NetworkAddress, error) {
	h.m.Lock()
	defer h.m.Unlock()
	d := structs.DestInfo{}
	if err := h.redirectProxyMap.Lookup(redirectKeyFor(app), &d); err != nil {
		return nil, err
	}
	return &agent.NetworkAddress{
		Version:    d.IPVersion,
		IPv4Addr:   d.DestIP4,
		IPv6Addr:   d.DestIP6,
		Port:       d.DestPort,
		KernelPid:  d.KernelPid,
		AgentNsPid: d.AgentNsPid,
	}, nil
}

// DeleteForConn drops the redirect of the connection whose application end
// is app, once the proxy has it.
func (h *Hooks) DeleteForConn(_ context.Context, app netip.AddrPort) error {
	h.m.Lock()
	defer h.m.Unlock()
	if err := h.redirectProxyMap.Delete(redirectKeyFor(app)); err != nil {
		if errors.Is(err, ebpf.ErrKeyNotExist) {
			// Gone already (the LRU evicted it, or its connection's close
			// dropped it): the delete's work is done, and the proxy routes
			// the connection by what it read, or by its pairing.
			h.logger.Debug("the redirect was already gone", zap.String("application end", app.String()))
			return nil
		}
		utils.LogError(h.logger, err, "failed to remove entry from redirect proxy map")
		return err
	}
	h.logger.Debug("successfully removed entry from redirect proxy map", zap.String("application end", app.String()))
	return nil
}

// errRedirectByPort is Get's and Delete's answer on Linux: a redirect is
// keyed by the application's address and port (GetForConn), which a source
// port alone does not name.
var errRedirectByPort = errors.New("the Linux hooks name a connection's destination by the application's address and port (GetForConn), not its source port alone")

// Get implements agent.DestInfo; the proxy uses GetForConn.
func (h *Hooks) Get(_ context.Context, _ uint16) (*agent.NetworkAddress, error) {
	return nil, errRedirectByPort
}

// Delete implements agent.DestInfo; the proxy uses DeleteForConn.
func (h *Hooks) Delete(_ context.Context, _ uint16) error {
	return errRedirectByPort
}

var _ agent.ConnDestInfo = (*Hooks)(nil)

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
	err := h.agentRegistrationMap.Update(uint32(key), agentInfo, ebpf.UpdateAny)
	if err != nil {
		utils.LogError(h.logger, err, "failed to send the agent info to the ebpf program")
		return err
	}
	return nil
}

func (h *Hooks) WatchBindEvents(ctx context.Context) (<-chan models.IngressEvent, error) {
	// Acquire read lock to safely access h.BindEvents while it might be closed by unLoad()
	h.objectsMutex.RLock()
	if h.BindEvents == nil {
		h.objectsMutex.RUnlock()
		return nil, errors.New("BindEvents map is nil")
	}
	rb, err := ringbuf.NewReader(h.BindEvents) // Assuming h.BindEvents is the eBPF map
	h.objectsMutex.RUnlock()
	if err != nil {
		return nil, err // Return error if we can't create the reader
	}

	eventChan := make(chan models.IngressEvent, 100) // A buffered channel can be useful

	go func() {
		defer close(eventChan)

		// Close the ringbuf reader when context is done to unblock rb.Read()
		go func() {
			<-ctx.Done()
			rb.Close()
		}()

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
