//go:build linux

package linux

import (
	"fmt"
	"strings"

	"go.keploy.io/server/v3/pkg/agent/hooks/structs"
)

// kernelPassThroughSlots is how many ports the kernel's pass-through list
// holds: keploy_client_info_t.pass_through_ports in keploy/ebpf, which is
// PASS_THROUGH_ARRAY_SIZE long, mirrored by structs.ClientInfo. A connect to a
// port in the list is left alone; any other is redirected into keploy's proxy.
const kernelPassThroughSlots = len(structs.ClientInfo{}.PassThroughPorts)

// kernelPassThroughList lays out the kernel's pass-through list. Keploy's own
// ports come first, so they always get a slot: a connect to one of them that
// is redirected into the proxy breaks keploy itself (in mock mode, the test
// runner's calls to the agent's scope API). The user's ports follow, each port
// once. The agent gets those as --pass-through-ports: the ports of the
// port-only bypassRules (config.GetByPassPorts). A rule with a host or a path
// is applied in the proxy, never here, where it would pass every call to its
// port through. The slots after the last port hold -1, where the kernel stops
// reading.
//
// When the ports do not all fit, or one is not a port, it returns an error that
// names them, and no list: a port the kernel does not hold is recorded or
// mocked like any other, so leaving it out would break the rule without a word.
func kernelPassThroughList(own, user []uint) ([kernelPassThroughSlots]int32, error) {
	var list [kernelPassThroughSlots]int32
	seen := make(map[uint]bool, len(own)+len(user))
	ports := make([]uint, 0, len(own)+len(user))
	var bad []uint
	add := func(p uint) {
		switch {
		case p == 0 || seen[p]:
		case p > 65535:
			seen[p] = true
			bad = append(bad, p)
		default:
			seen[p] = true
			ports = append(ports, p)
		}
	}
	for _, p := range own {
		add(p)
	}
	nOwn := len(ports)
	for _, p := range user {
		add(p)
	}
	if len(bad) > 0 {
		return list, fmt.Errorf("the pass-through ports %s are not ports (1-65535)", joinPorts(bad))
	}
	if len(ports) > kernelPassThroughSlots {
		ownPorts, userPorts := ports[:nOwn], ports[nOwn:]
		if nOwn >= kernelPassThroughSlots {
			return list, fmt.Errorf("keploy can pass through at most %d ports in the kernel, and keploy's own ports are %d (%s), which leaves no room for bypassRules. Keploy's own %s and the bypassRules (or --pass-through-ports) ports %s cannot be passed through",
				kernelPassThroughSlots, nOwn, joinPorts(ownPorts), joinPorts(ownPorts[kernelPassThroughSlots:]), joinPorts(userPorts))
		}
		room := kernelPassThroughSlots - nOwn
		ownNote := ""
		if nOwn > 0 {
			ownNote = fmt.Sprintf(" keploy's own (%s) take %d, which leaves %d for bypassRules.", joinPorts(ownPorts), nOwn, room)
		}
		return list, fmt.Errorf("keploy can pass through at most %d ports in the kernel, and this run asks for %d.%s These bypassRules (or --pass-through-ports) ports do not fit, so their calls would be recorded or mocked instead: %s. Keep the bypassRules to at most %d distinct ports",
			kernelPassThroughSlots, len(ports), ownNote, joinPorts(userPorts[room:]), room)
	}
	for i := range list {
		list[i] = -1
	}
	for i, p := range ports {
		list[i] = int32(p)
	}
	return list, nil
}

func joinPorts(ports []uint) string {
	s := make([]string, len(ports))
	for i, p := range ports {
		s[i] = fmt.Sprint(p)
	}
	return strings.Join(s, ", ")
}
