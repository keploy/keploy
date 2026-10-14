//go:build linux

package linux

import (
	"context"
	"errors"
	"fmt"
	"syscall"

	"github.com/cilium/ebpf"
	"golang.org/x/sys/unix"

	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
)

// MarkProxyListener marks the proxy's listening socket in proxy_listener_mark,
// keyed by its fd: the kernel copies the mark into every socket it creates for
// a connection to it, and k_sockops counts each such connection at the proxy
// side's handshake (proxy_conns_established). The proxy calls it before
// listen(), so every connection the listener can ever accept is counted.
//
// The socket must be plain TCP. An MPTCP listener's fd is the MPTCP socket;
// the kernel creates its connections' sockets from a TCP socket of its own (the
// first subflow), which the mark never reaches, so nothing would be counted.
func (h *Hooks) MarkProxyListener(rc syscall.RawConn) error {
	h.objectsMutex.RLock()
	defer h.objectsMutex.RUnlock()
	m := h.objects.ProxyListenerMark
	if m == nil {
		return errors.New("the hooks are not loaded: proxy_listener_mark is not open")
	}
	var markErr error
	if err := rc.Control(func(fd uintptr) {
		markErr = markListenerFD(m, int(fd))
	}); err != nil {
		return fmt.Errorf("reach the proxy's listening socket: %w", err)
	}
	if markErr == nil {
		h.proxyMarked.Store(true)
	}
	return markErr
}

func markListenerFD(m *ebpf.Map, fd int) error {
	proto, err := unix.GetsockoptInt(fd, unix.SOL_SOCKET, unix.SO_PROTOCOL)
	if err != nil {
		return fmt.Errorf("read the proxy's listening socket's protocol: %w", err)
	}
	if proto != unix.IPPROTO_TCP {
		return fmt.Errorf("the proxy's listening socket is protocol %d, not plain TCP (%d): the kernel never copies a mark into an MPTCP listener's connections", proto, unix.IPPROTO_TCP)
	}
	if err := m.Update(int32(fd), uint64(0), ebpf.UpdateAny); err != nil {
		return fmt.Errorf("mark the proxy's listening socket in proxy_listener_mark: %w", err)
	}
	return nil
}

// ProxyConnSeq is the sequence number k_sockops wrote into the accepted
// connection rc's socket as it counted it (proxy_listener_mark's value): the
// kernel counts a connection before accept() can return it, so an accepted
// connection of the marked listener always has one. None, or 0, is an error.
func (h *Hooks) ProxyConnSeq(rc syscall.RawConn) (uint64, error) {
	h.objectsMutex.RLock()
	defer h.objectsMutex.RUnlock()
	m := h.objects.ProxyListenerMark
	if m == nil {
		return 0, errors.New("the hooks are not loaded: proxy_listener_mark is not open")
	}
	var seq uint64
	var lerr error
	if err := rc.Control(func(fd uintptr) {
		lerr = m.Lookup(int32(fd), &seq)
	}); err != nil {
		return 0, fmt.Errorf("reach the accepted connection's socket: %w", err)
	}
	if errors.Is(lerr, ebpf.ErrKeyNotExist) || (lerr == nil && seq == 0) {
		return 0, errors.New("an accepted connection of the proxy's marked listener has no sequence number: the kernel did not count it")
	}
	if lerr != nil {
		return 0, fmt.Errorf("read the accepted connection's sequence number: %w", lerr)
	}
	return seq, nil
}

// proxyConnsValue is proxy_conns_established's value: its bpf_spin_lock, and
// the count.
type proxyConnsValue struct {
	Lock uint32
	_    uint32
	N    uint64
}

// ProxyConnsEstablished counts the connections the kernel has completed to the
// proxy's marked listener (proxy_conns_established, counted by k_sockops at
// the proxy side's handshake), the highest sequence number given. It only ever
// grows, for the life of the loaded object, which is the agent's: the hooks
// are loaded once. Until a listener has been marked it is an error: a 0 then
// would be no count at all.
func (h *Hooks) ProxyConnsEstablished(_ context.Context) (uint64, error) {
	if !h.proxyMarked.Load() {
		return 0, fmt.Errorf("%w: no listener of the proxy has been marked for the kernel's count", models.ErrProxyConnsUnsupported)
	}
	h.objectsMutex.RLock()
	defer h.objectsMutex.RUnlock()
	m := h.objects.ProxyConnsEstablished
	if m == nil {
		return 0, errors.New("the hooks are not loaded: proxy_conns_established is not open")
	}
	var v proxyConnsValue
	if err := m.LookupWithFlags(uint32(0), &v, ebpf.LookupLock); err != nil {
		return 0, fmt.Errorf("read proxy_conns_established: %w", err)
	}
	return v.N, nil
}

// Compile-time proof that the Linux hooks count the connections completed to
// the proxy: were a method renamed, the agent's type assertion would stop
// matching and every begin and end would read "unknown".
var _ agent.ProxyConnsReporter = (*Hooks)(nil)
