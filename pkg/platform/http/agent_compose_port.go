package http

import (
	"context"
	"fmt"
	"net"
	"net/url"
	"os"
	"strconv"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// mockAgentAt is the address keploy hands a mock run's tests to mark their
// bounds at (KEPLOY_MOCK_AGENT): the agent's port on the host's loopback.
func mockAgentAt(port uint32) string { return fmt.Sprintf("http://localhost:%d", port) }

// composeAgentPortRefused is what the application client asks when a
// `docker compose up` that starts keploy's agent failed (App.SetOnAgentPortRefused),
// with what the up said (said). Where it refused to publish the agent's port
// on the host's loopback, 127.0.0.1:P, because another socket took P after
// Setup drew it (in words of takenPortWordings), the agent moves to another
// port, drawn as Setup draws it: keploy addresses it there (addressAgent)
// and hands a mock run's tests that address (KEPLOY_MOCK_AGENT), and the
// port is returned, for the application client to move the agent's service
// to and run the up again. The up failed before compose started the app,
// which runs in the agent's network namespace, so no test has reached the
// agent yet. 0 and no error where the up said nothing of 127.0.0.1:P: its
// failure is another one. An up that named 127.0.0.1:P in words keploy does
// not know for a taken port fails closed: its error shows the line, and the
// agent is not moved.
func (a *AgentClient) composeAgentPortRefused(ctx context.Context, said string) (uint32, error) {
	from := a.conf.Agent.AgentPort
	line, known := publishRefusalLine(said, "127.0.0.1:"+strconv.FormatUint(uint64(from), 10))
	switch {
	case known:
	case line != "":
		return 0, fmt.Errorf("docker compose said %q of the keploy agent's port %d, in words keploy does not know for a taken port, so it does not start the agent again", line, from)
	default:
		return 0, nil
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	taken := utils.PortTaken(from)
	staticPorts := a.composeStaticPortsSnapshot()
	keep := make([]uint32, 0, 3+len(staticPorts))
	keep = append(keep, a.conf.Agent.ProxyPort, a.conf.Agent.DnsPort, from)
	keep = append(keep, staticPorts...)
	to, err := agentPortFor(true, keep...)
	if err != nil {
		return 0, fmt.Errorf("docker compose could not publish the keploy agent's port: %s, saying %q, and keploy found no other port for it: %w", taken, line, err)
	}
	uri, err := agentURIOn(a.conf.AgentURI(), to)
	if err != nil {
		return 0, err
	}
	a.logger.Warn("docker compose could not publish the keploy agent's port on the host, since another socket took it after keploy picked it; moving the agent to another port, before the app starts",
		zap.String("taken", taken), zap.String("composeSaid", line), zap.Uint32("from", from), zap.Uint32("to", to))
	a.conf.Agent.AgentPort = to
	a.moveAgentAddress(uri)
	if os.Getenv("KEPLOY_MOCK_AGENT") == mockAgentAt(from) {
		if err := os.Setenv("KEPLOY_MOCK_AGENT", mockAgentAt(to)); err != nil {
			return 0, fmt.Errorf("failed to hand the tests the keploy agent's new address: %w", err)
		}
	}
	return to, nil
}

// moveAgentAddress moves keploy's address for this compose agent to uri,
// carrying every address-keyed registry populated before compose up (the
// agent session, containerAt / agentContainerAt, and instancesAt) before
// publishing the new address so concurrent readers never see an orphaned
// entry, then drops the old keys and any pushers tied to the old address.
func (a *AgentClient) moveAgentAddress(uri string) {
	oldURI := a.conf.AgentURI()
	if oldURI == uri {
		return
	}
	oldHost, newHost := "", ""
	if oldURI != "" {
		oldHost = agentHost(oldURI)
	}
	if uri != "" {
		newHost = agentHost(uri)
	}
	movedSession, _ := stageMoveSession(oldURI, uri)
	movedContainer, oldContainerAt := a.stageMoveAgentContainer(uri)
	movedInstances, _ := stageMoveInstances(oldHost, newHost)

	a.addressAgent(uri)

	finishMoveSession(oldURI, movedSession)
	finishMoveAgentContainer(oldContainerAt, movedContainer)
	finishMoveInstances(oldHost, movedInstances)
	if oldURI != "" {
		forgetContainers(oldURI)
		forgetNetworkNames(oldURI)
	}
}

func (a *AgentClient) composeStaticPortsSnapshot() []uint32 {
	a.mu.Lock()
	defer a.mu.Unlock()
	if len(a.composeStaticPorts) == 0 {
		return nil
	}
	return append([]uint32(nil), a.composeStaticPorts...)
}

// agentURIOn is uri, the agent's address, on port: the same scheme, host and
// path.
func agentURIOn(uri string, port uint32) (string, error) {
	u, err := url.Parse(uri)
	if err != nil || u.Host == "" {
		return "", fmt.Errorf("keploy cannot move the agent's address %q to port %d: it is not a URL with a host", pkg.ShownAgentURI(uri), port)
	}
	u.Host = net.JoinHostPort(u.Hostname(), strconv.FormatUint(uint64(port), 10))
	return u.String(), nil
}
