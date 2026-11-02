package proxy

import (
	"errors"
	"fmt"
	"syscall"

	"go.keploy.io/server/v3/utils"
)

// SetPortMover lets the proxy move its listener, or its DNS server's, off a
// port another socket holds when it binds it, and has it call move with the
// ports the proxy and its DNS server then listen on, at each move, before
// StartProxy returns: the agent tells its hooks there
// (agent.ProxyPortsSetter). Unset, a taken port fails the start, naming what
// holds it.
//
// The CLI that starts an agent checks that the ports it hands it are free
// (utils.EnsureAvailablePorts), but cannot keep them free until the agent
// binds them: two runs started together both found the default ports free,
// and the second agent failed to bind them. The bind is the only check that
// cannot be overtaken.
func (p *Proxy) SetPortMover(move func(proxyPort, dnsPort uint32) error) {
	p.portMover = move
}

// bindOrMove binds the server's sockets on *port with bind. Where another
// socket holds that port (EADDRINUSE), it says which port and what holds it
// (utils.PortTaken). Where the port may move (SetPortMover), it binds the
// first free port instead (utils.BindFreePort), keeps it in *port, says so,
// and tells the port mover where the proxy and its DNS server then listen,
// before the server serves: the hooks redirect to the ports they loaded
// with, and are told before the agent says it is ready. A mover that refuses
// fails the start, with the new port's sockets let go (release). Where the
// port may not move, the error says what was taken.
func (p *Proxy) bindOrMove(server string, port *uint32, bind func(port uint32) error, release func()) error {
	err := bind(*port)
	if err == nil {
		return nil
	}
	if !errors.Is(err, syscall.EADDRINUSE) {
		return err
	}
	taken := fmt.Sprintf("the %s's %s", server, utils.PortTaken(*port))
	if p.portMover == nil {
		return fmt.Errorf("%s: %w", taken, err)
	}
	moved, err := utils.BindFreePort(bind)
	if err != nil {
		return fmt.Errorf("%s, and no other port could be bound: %w", taken, err)
	}
	*port = moved
	p.logger.Warn(fmt.Sprintf("%s; the %s listens on port %d instead", taken, server, moved))
	if err := p.portMover(p.Port, p.DNSPort); err != nil {
		release()
		return fmt.Errorf("failed to tell the hooks the proxy's ports (proxy %d, DNS %d): %w", p.Port, p.DNSPort, err)
	}
	return nil
}
