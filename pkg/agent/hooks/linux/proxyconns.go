package linux

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"net/netip"

	"github.com/cilium/ebpf"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
)

// ProxyConnsEstablished counts the connections the kernel has completed to
// the proxy's listener at the addresses the connect hooks redirect to
// (proxy_conns_established, counted by k_sockops at the proxy side's
// handshake). It only ever grows: a reload of the object adds the count it
// had to the new one's.
func (h *Hooks) ProxyConnsEstablished(_ context.Context) (uint64, error) {
	h.objectsMutex.RLock()
	defer h.objectsMutex.RUnlock()
	n, err := readProxyConns(h.objects.ProxyConnsEstablished)
	if err != nil {
		return 0, err
	}
	return h.proxyConnsBase + n, nil
}

func readProxyConns(m *ebpf.Map) (uint64, error) {
	if m == nil {
		return 0, errors.New("the hooks are not loaded: proxy_conns_established is not open")
	}
	var n uint64
	if err := m.Lookup(uint32(0), &n); err != nil {
		return 0, fmt.Errorf("read proxy_conns_established: %w", err)
	}
	return n, nil
}

// keepProxyConnsAcrossUnload adds the count of the object about to be closed
// to the base the next object's count is added to. Called by unLoad with
// objectsMutex held, before the object is closed.
func (h *Hooks) keepProxyConnsAcrossUnload() {
	if n, err := readProxyConns(h.objects.ProxyConnsEstablished); err == nil {
		h.proxyConnsBase += n
	}
}

// ProxyConnTargets are the local addresses proxy_conns_established counts
// connections at: where the connect hooks send a redirected connection, the
// proxy's IPv4 and IPv6 address at its port. Empty until Load has resolved
// them.
func (h *Hooks) ProxyConnTargets() []netip.AddrPort {
	h.objectsMutex.RLock()
	info, ok := h.proxyInfo, h.proxyInfoSet
	h.objectsMutex.RUnlock()
	if !ok {
		return nil
	}
	return proxyConnTargets(info)
}

// proxyConnTargets turns the proxy_info the kernel was given into addresses:
// IP4 is one host-order word, IP6 four, each the address's bytes in order.
func proxyConnTargets(info structs.ProxyInfo) []netip.AddrPort {
	port := uint16(info.Port)
	if port == 0 {
		return nil
	}
	var out []netip.AddrPort
	if info.IP4 != 0 {
		var b [4]byte
		binary.BigEndian.PutUint32(b[:], info.IP4)
		out = append(out, netip.AddrPortFrom(netip.AddrFrom4(b), port))
	}
	if info.IP6 != [4]uint32{} {
		var b [16]byte
		for i, w := range info.IP6 {
			binary.BigEndian.PutUint32(b[4*i:], w)
		}
		out = append(out, netip.AddrPortFrom(netip.AddrFrom16(b), port))
	}
	return out
}

// Compile-time proof that the Linux hooks count the connections completed to
// the proxy: were a method renamed, the agent's type assertion would stop
// matching and every begin and end would read "unknown".
var _ agent.ProxyConnsReporter = (*Hooks)(nil)
