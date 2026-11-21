package docker

import (
	"fmt"
	"strconv"

	"gopkg.in/yaml.v3"
)

// MoveComposeAgentPort moves keploy's agent, in compose (a document
// ModifyComposeForAgent wrote it into), from port from to port to: its
// publish on the host's loopback (127.0.0.1:from:from), the port its server
// listens on (its command's --port from), and, in mock mode, the address an
// application's tests mark their bounds at (KEPLOY_MOCK_AGENT
// http://localhost:from in a service's environment). A document in which the
// agent's service does not publish from or is not told --port from, exactly
// once each, is left as it was, and its error says so: keploy does not start
// an agent whose port it could not move whole.
func MoveComposeAgentPort(compose *Compose, from, to uint32) error {
	if compose == nil {
		return fmt.Errorf("there is no compose document to move keploy's agent in")
	}
	var agent *yaml.Node
	for _, e := range effectiveServiceEntries(&compose.Services) {
		if aliasTarget(e.key).Value == "keploy-agent" {
			agent = aliasTarget(e.value)
			break
		}
	}
	if agent == nil || agent.Kind != yaml.MappingNode {
		return fmt.Errorf("the compose document has no keploy-agent service to move off port %d", from)
	}

	fromS, toS := strconv.FormatUint(uint64(from), 10), strconv.FormatUint(uint64(to), 10)
	publish := fmt.Sprintf("127.0.0.1:%s:%s", fromS, fromS)
	var publishes, ports []**yaml.Node
	for i := 0; i+1 < len(agent.Content); i += 2 {
		value := aliasTarget(agent.Content[i+1])
		if value.Kind != yaml.SequenceNode {
			continue
		}
		switch agent.Content[i].Value {
		case "ports":
			for j := range value.Content {
				if aliasTarget(value.Content[j]).Value == publish {
					publishes = append(publishes, &value.Content[j])
				}
			}
		case "command":
			for j := 0; j+1 < len(value.Content); j++ {
				if aliasTarget(value.Content[j]).Value == "--port" && aliasTarget(value.Content[j+1]).Value == fromS {
					ports = append(ports, &value.Content[j+1])
				}
			}
		}
	}
	if len(publishes) != 1 || len(ports) != 1 {
		return fmt.Errorf("keploy-agent's service publishes %s %d times and is told --port %d %d times, not once each, so keploy cannot move it off port %d",
			publish, len(publishes), from, len(ports), from)
	}
	movedScalar(publishes[0], fmt.Sprintf("127.0.0.1:%s:%s", toS, toS))
	movedScalar(ports[0], toS)

	mockFrom, mockTo := "http://localhost:"+fromS, "http://localhost:"+toS
	for _, e := range effectiveServiceEntries(&compose.Services) {
		service := aliasTarget(e.value)
		if service == nil || service.Kind != yaml.MappingNode {
			continue
		}
		for i := 0; i+1 < len(service.Content); i += 2 {
			if service.Content[i].Value != "environment" {
				continue
			}
			env := aliasTarget(service.Content[i+1])
			switch env.Kind {
			case yaml.SequenceNode:
				for j := range env.Content {
					if aliasTarget(env.Content[j]).Value == "KEPLOY_MOCK_AGENT="+mockFrom {
						movedScalar(&env.Content[j], "KEPLOY_MOCK_AGENT="+mockTo)
					}
				}
			case yaml.MappingNode:
				for j := 0; j+1 < len(env.Content); j += 2 {
					if aliasTarget(env.Content[j]).Value == "KEPLOY_MOCK_AGENT" && aliasTarget(env.Content[j+1]).Value == mockFrom {
						movedScalar(&env.Content[j+1], mockTo)
					}
				}
			}
		}
	}
	return nil
}

// movedScalar puts value in the scalar at slot, written as the one there was
// (its tag, its style, its comments): a new node, so an anchor the old one
// was, or an alias led to, is not changed for whatever else names it.
func movedScalar(slot **yaml.Node, value string) {
	n := *aliasTarget(*slot)
	n.Value, n.Anchor = value, ""
	*slot = &n
}
