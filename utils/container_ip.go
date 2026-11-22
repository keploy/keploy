package utils

import (
	"fmt"
	"net"
)

// containerIface is what containerIP reads of one network interface: its
// flags, and its addresses (read only for an up interface that is not the
// loopback).
type containerIface struct {
	flags net.Flags
	addrs []net.Addr
}

// GetContainerIP is the address the applications an agent started with
// --is-docker serves send their connections to, read from within the agent's
// container: its first IPv4 address, as keploy has always picked it, and on a
// network that gives the container none (a single-stack IPv6 Kubernetes Pod),
// its first IPv6 address that names it beyond its link. Its hooks, as they
// load, have the kernel send those connections to this address.
//
// Go does not show an IPv6 address's flags (tentative, deprecated,
// temporary). Any address local to the container serves, as the proxy
// listens on all of them; it only has to stay, and a Pod's address, which
// its network plugin sets once and checks for duplicates before the Pod
// starts, does.
func GetContainerIP() (net.IP, error) {
	interfaces, err := net.Interfaces()
	if err != nil {
		return nil, err
	}
	ifaces := make([]containerIface, 0, len(interfaces))
	for _, i := range interfaces {
		c := containerIface{flags: i.Flags}
		if usableIface(c.flags) {
			// An interface whose addresses cannot be read has none to give.
			c.addrs, _ = i.Addrs()
		}
		ifaces = append(ifaces, c)
	}
	return containerIP(ifaces)
}

// usableIface says an interface may carry the container's address: it is up
// and is not the loopback.
func usableIface(flags net.Flags) bool {
	return flags&net.FlagUp != 0 && flags&net.FlagLoopback == 0
}

// containerIP picks GetContainerIP's address from the container's interfaces,
// in their order: the first IPv4 address of an up interface that is not the
// loopback; with none, the first such IPv6 address that is global unicast,
// unique local (fc00::/7) included. A link-local IPv6 address (fe80::/10)
// is never picked: a connection the kernel is told to send there names no
// interface, so it could not be made. A container with neither has nowhere
// its applications reach it, and the error says so.
func containerIP(ifaces []containerIface) (net.IP, error) {
	var ipv6 net.IP
	for _, i := range ifaces {
		if !usableIface(i.flags) {
			continue
		}
		for _, addr := range i.addrs {
			ipnet, ok := addr.(*net.IPNet)
			if !ok || ipnet.IP.IsLoopback() {
				continue
			}
			if ip4 := ipnet.IP.To4(); ip4 != nil {
				return ip4, nil
			}
			if ipv6 == nil && ipnet.IP.IsGlobalUnicast() {
				ipv6 = ipnet.IP
			}
		}
	}
	if ipv6 != nil {
		return ipv6, nil
	}

	// Tagged, so the agent that cannot start without it says so in its exit
	// status: a container with no address but loopback's and link-local ones
	// (docker --network none, a network that gives it no routable address) is
	// the environment to change, not a privilege to grant. Only an agent
	// started with --is-docker asks (its hooks, as they load). A native one
	// is reached over loopback and needs no address of its own.
	return nil, fmt.Errorf("%w: could not find a non-loopback IP for the container: it has no IPv4 address but loopback's, and no IPv6 address but loopback and link-local (fe80::/10) ones, which a redirected connection cannot be sent to", ErrEnvironmentUnsupported)
}
