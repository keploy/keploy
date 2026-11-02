//go:build linux

package linux

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"go.uber.org/zap"
)

// netSysctlDir holds the IPv4 sysctls of the network namespace of the
// process that reads them: the agent's, the one the app binds in (the
// ingress hold attaches its lookup there too, ingressport.go).
const netSysctlDir = "/proc/sys/net/ipv4"

// bindMovePorts are the ports keploy/ebpf's bind hooks move an app's bind
// to when they pick the port themselves, as find_free_port picks them
// (headers/k_helpers.h): outside the namespace's ephemeral range, above it
// when it ends below 65535, else below it, from the higher of 1024 and
// net.ipv4.ip_unprivileged_port_start up. They do so when a post_bind hook
// did not attach, so the kernel cannot pick the port for them
// (structs.FlagKernelPortAlloc unset). When no port lies there, or each
// port they try there is listened on, they leave the bind on the port the
// app asked for, and keploy's ingress never hears of it: that port's
// incoming calls are not recorded.
type bindMovePorts struct {
	lo, hi int // net.ipv4.ip_local_port_range, both included
	unpriv int // net.ipv4.ip_unprivileged_port_start
	// first..last, both included, are the ports a bind moves to; both 0
	// when there is none.
	first, last int
}

// readBindMovePorts reads the two sysctls from dir (netSysctlDir) and makes
// find_free_port's choice from them.
func readBindMovePorts(dir string) (bindMovePorts, error) {
	var p bindMovePorts
	r, err := readSysctlPorts(dir, "ip_local_port_range", 2)
	if err != nil {
		return p, err
	}
	p.lo, p.hi = r[0], r[1]
	u, err := readSysctlPorts(dir, "ip_unprivileged_port_start", 1)
	if err != nil {
		return p, err
	}
	p.unpriv = u[0]
	from := 1024
	if p.unpriv > from {
		from = p.unpriv
	}
	if p.hi < 65535 {
		p.first, p.last = p.hi+1, 65535
	} else if p.lo > from {
		p.first, p.last = from, p.lo-1
	}
	return p, nil
}

// readSysctlPorts reads the sysctl name in dir: n ports, each 0 to 65535.
func readSysctlPorts(dir, name string, n int) ([]int, error) {
	b, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return nil, fmt.Errorf("read net.ipv4.%s: %w", name, err)
	}
	f := strings.Fields(string(b))
	if len(f) != n {
		return nil, fmt.Errorf("net.ipv4.%s is %q: want %d port(s)", name, strings.TrimSpace(string(b)), n)
	}
	ports := make([]int, n)
	for i, s := range f {
		v, err := strconv.Atoi(s)
		if err != nil || v < 0 || v > 65535 {
			return nil, fmt.Errorf("net.ipv4.%s is %q: %q is no port", name, strings.TrimSpace(string(b)), s)
		}
		ports[i] = v
	}
	return ports, nil
}

// warnBindFallback says, once, where the app's binds go when the post_bind
// hook named hook did not attach (attachErr): the ports the bind hooks move
// them to, read from the sysctls in dir, and that a bind which finds each
// port it tries there listened on is left on the port the app names and not
// recorded; or, when there are none or a sysctl cannot be read, that the
// binds are left on the ports the app names and those ports' incoming
// traffic is not recorded.
func warnBindFallback(logger *zap.Logger, hook string, attachErr error, dir string) {
	head := fmt.Sprintf("the %s cgroup hook did not attach, so keploy picks the port each of the app's binds moves to itself", hook)
	p, err := readBindMovePorts(dir)
	switch {
	case err != nil:
		logger.Warn(fmt.Sprintf("%s, and it cannot say whether any port is left to move one to (%v): take the app's ports' incoming traffic "+
			"as not recorded, since a bind with no port to move to is left on the port the app names", head, err), zap.Error(attachErr))
	case p.first == 0:
		logger.Warn(fmt.Sprintf("%s, and no port is left outside net.ipv4.ip_local_port_range %d-%d from %d up "+
			"(the higher of 1024 and net.ipv4.ip_unprivileged_port_start %d): each bind is left on the port the app names, "+
			"and that port's incoming traffic is not recorded", head, p.lo, p.hi, max(1024, p.unpriv), p.unpriv), zap.Error(attachErr))
	default:
		logger.Warn(fmt.Sprintf("%s: a port in %d-%d, outside net.ipv4.ip_local_port_range %d-%d, that no socket listens on "+
			"(rare: a port there that another socket bound without listening fails the app's bind with 'address already in use'). "+
			"A bind that finds each port it tries there listened on is left on the port the app names, "+
			"and that port's incoming traffic is not recorded",
			head, p.first, p.last, p.lo, p.hi), zap.Error(attachErr))
	}
}
