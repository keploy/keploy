package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/netip"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/docker/docker/api/types/container"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

var _ pkg.AppPortReachability = (*AgentClient)(nil)

// appPortCheckTimeout bounds one UnreachableAppPort answer: a container inspect
// and one question to the agent. It is asked from a recording's insert loop, a
// replay's readiness gate and a refused test, and a slow or hung docker daemon
// must hold none of them up for longer. A var only so tests can shorten it.
var appPortCheckTimeout = 5 * time.Second

// UnreachableAppPort implements pkg.AppPortReachability for Docker mode.
//
// There the app runs in the keploy agent container's network namespace, and
// that container publishes the ports the user's docker command (or compose
// service) publishes: the host reaches the app through those and nothing else.
// Replay sends each test from the host to the port it was recorded on, so a
// loopback address whose port the agent container does not publish as that
// same port can never reach the app, however long replay waits. Nor can one
// whose port is published but where the app listens only on 127.0.0.1 (or
// ::1) inside the container: docker forwards a published port to the
// container's own address, which such a socket never sees. The agent, which
// shares the app's namespace, says where the app listens.
//
// It answers "" outside Docker mode, for a host that is not this machine's
// loopback (a test.host pointing elsewhere is routed however the operator set
// it up), and whenever the container cannot be inspected: "cannot tell" must
// never read as "unreachable".
func (a *AgentClient) UnreachableAppPort(ctx context.Context, host string, port uint16) string {
	if a.conf == nil || a.dockerClient == nil || a.conf.KeployContainer == "" {
		return ""
	}
	cmdType := utils.CmdType(a.conf.CommandType)
	if !utils.IsDockerCmd(cmdType) || !isLoopbackHost(host) {
		return ""
	}
	ctx, cancel := context.WithTimeout(ctx, appPortCheckTimeout)
	defer cancel()
	inspect, err := a.dockerClient.ContainerInspect(ctx, a.conf.KeployContainer)
	if err != nil {
		a.logger.Debug("could not inspect the keploy agent container to tell whether the app's port is published",
			zap.String("container", a.conf.KeployContainer), zap.Uint16("port", port), zap.Error(err))
		return ""
	}
	// The agent container also publishes keploy's own ports on loopback; the
	// user never asked for those, so they are not listed as theirs.
	own := map[string]bool{}
	for _, p := range []uint32{a.conf.Agent.AgentPort, a.conf.Agent.ProxyPort, a.conf.ProxyPort, a.conf.Agent.DnsPort, a.conf.DNSPort} {
		if p != 0 {
			own[strconv.FormatUint(uint64(p), 10)] = true
		}
	}
	pub, ok := appPortPublish(inspect, port, own)
	if !ok {
		return ""
	}
	return unreachableAppPortReason(pub, port, cmdType, a.appListening(ctx, port))
}

// isLoopbackHost reports whether a dial to host stays on this machine.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// portPublish is how the agent container publishes the app's port.
type portPublish struct {
	asItself   bool     // host port N leads to the app's port N
	published  []string // every tcp publish of the user's, as host:container
	elsewhere  []string // host ports the app's port is published on instead
	forwardsTo string   // the app port host port N leads to, when not N
}

// appPortPublish reads the agent container's published ports for the app's
// port. ok is false when the question does not apply: a stopped container
// refuses everything, and under host networking the app's ports are the host's.
// keployPorts are container ports that are keploy's, not the app's.
func appPortPublish(inspect container.InspectResponse, port uint16, keployPorts map[string]bool) (portPublish, bool) {
	var pub portPublish
	if inspect.ContainerJSONBase == nil || inspect.HostConfig == nil || inspect.NetworkSettings == nil {
		return pub, false
	}
	if inspect.State == nil || !inspect.State.Running {
		return pub, false
	}
	if inspect.HostConfig.NetworkMode.IsHost() {
		return pub, false
	}
	want := strconv.Itoa(int(port))
	for containerPort, bindings := range inspect.NetworkSettings.Ports {
		if containerPort.Proto() != "tcp" || keployPorts[containerPort.Port()] {
			continue
		}
		for _, b := range bindings {
			if b.HostPort == "" {
				continue
			}
			pub.published = append(pub.published, b.HostPort+":"+containerPort.Port())
			switch {
			case b.HostPort == want && containerPort.Port() == want:
				pub.asItself = true
			case b.HostPort == want:
				pub.forwardsTo = containerPort.Port()
			case containerPort.Port() == want:
				pub.elsewhere = append(pub.elsewhere, b.HostPort)
			}
		}
	}
	pub.published = uniqueSorted(pub.published)
	pub.elsewhere = uniqueSorted(pub.elsewhere)
	return pub, true
}

// appListening is where the app's sockets listen on its port, as far as the
// agent can tell.
type appListening struct {
	known        bool     // false: nothing listens there yet, or the agent cannot tell
	loopbackOnly []string // every address it listens on, when all are loopback
}

// appListening asks the agent, which shares the app's network namespace, where
// the app listens on port.
func (a *AgentClient) appListening(ctx context.Context, port uint16) appListening {
	if a.conf.Agent.AgentURI == "" {
		return appListening{}
	}
	url := fmt.Sprintf("%s/app/listen-addrs?port=%d", a.conf.Agent.AgentURI, port)
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return appListening{}
	}
	res, err := a.client.Do(req)
	if err != nil {
		a.logger.Debug("could not ask the agent where the app listens", zap.Uint16("port", port), zap.Error(err))
		return appListening{}
	}
	defer func() { _ = res.Body.Close() }()
	body, err := readAgentBody(res)
	if err != nil {
		return appListening{}
	}
	if res.StatusCode != http.StatusOK {
		// An agent without the route (older, or another build) answers 404
		// or 501: that is "cannot tell", as is its own failure.
		a.logger.Debug("the agent cannot say where the app listens",
			zap.Uint16("port", port), zap.Error(agentRespErr("app listen addrs", res, body)))
		return appListening{}
	}
	var resp models.AppListenAddrs
	if err := json.Unmarshal(body, &resp); err != nil {
		return appListening{}
	}
	return classifyAppListening(resp.Addrs)
}

// classifyAppListening says whether any listening address is one a published
// port can reach: anything but loopback (a wildcard, or the container's own
// address). An address it cannot read makes the answer unknown.
func classifyAppListening(addrs []string) appListening {
	if len(addrs) == 0 {
		return appListening{}
	}
	var loopback []string
	for _, s := range addrs {
		ip, err := netip.ParseAddr(s)
		if err != nil {
			return appListening{}
		}
		if !ip.Unmap().IsLoopback() {
			return appListening{known: true}
		}
		loopback = append(loopback, ip.Unmap().String())
	}
	return appListening{known: true, loopbackOnly: uniqueSorted(loopback)}
}

// maxListedPublishes caps the publishes a reason lists: `-p 8000-9000:8000-9000`
// is a thousand of them, and the reason is one log line.
const maxListedPublishes = 10

// unreachableAppPortReason says why the host cannot reach the app at the same
// port number, and what would let it, or "" when it can (a refusal there is
// then the app not listening yet) or it cannot tell. cmdType words the fix for
// how the app was started.
func unreachableAppPortReason(pub portPublish, port uint16, cmdType utils.CmdType, listening appListening) string {
	want := strconv.Itoa(int(port))
	command, fix := "the docker command", fmt.Sprintf("add -p %s:%s to the docker command", want, want)
	switch cmdType {
	case utils.DockerCompose:
		command, fix = "the docker compose service", fmt.Sprintf(`add "%s:%s" to the app service's ports`, want, want)
	case utils.FromContainer:
		// keploy re-publishes what the container was created with.
		command, fix = "the container", fmt.Sprintf("re-create the container with -p %s:%s", want, want)
	}
	sends := fmt.Sprintf("keploy sends each test from the host to port %s, the port it was recorded on", want)

	if len(listening.loopbackOnly) > 0 {
		// Publishing alone cannot help: docker forwards a published port to
		// the container's own address, and a socket on loopback never sees it.
		need := fmt.Sprintf("listen on 0.0.0.0:%s", want)
		if !pub.asItself {
			need += fmt.Sprintf(" and have port %s published as itself (%s)", want, fix)
		}
		return fmt.Sprintf("the app listens on port %s only on %s inside the container, which no published port reaches, and %s. keploy cannot replay these tests from the host while the app listens only there: the app would have to %s",
			want, strings.Join(listening.loopbackOnly, " and "), sends, need)
	}
	if pub.asItself {
		return ""
	}
	var reason string
	switch {
	case pub.forwardsTo != "":
		reason = fmt.Sprintf("host port %s is published to the app's port %s instead, and %s. Publish %s as itself: map host port %s to the app's port %s",
			want, pub.forwardsTo, sends, want, want, want)
	case len(pub.elsewhere) > 0:
		reason = fmt.Sprintf("it is published on host port %s instead, and %s. Publish it on the same port: %s",
			strings.Join(pub.elsewhere, ", "), sends, fix)
	case len(pub.published) > 0:
		reason = fmt.Sprintf("it is not published on the host (%s publishes %s), and %s. Publish it: %s",
			command, listCapped(pub.published, maxListedPublishes), sends, fix)
	default:
		reason = fmt.Sprintf("it is not published on the host (%s publishes no ports), and %s. Publish it: %s",
			command, sends, fix)
	}
	if !listening.known {
		// Not seen listening, so publishing may not be all it takes.
		reason += fmt.Sprintf(", with the app listening on 0.0.0.0:%s (a published port does not reach a socket on 127.0.0.1)", want)
	}
	return reason
}

// listCapped joins s with ", ", listing at most max of it.
func listCapped(s []string, max int) string {
	if len(s) <= max {
		return strings.Join(s, ", ")
	}
	return fmt.Sprintf("%s and %d more", strings.Join(s[:max], ", "), len(s)-max)
}

func uniqueSorted(s []string) []string {
	sort.Strings(s)
	out := s[:0]
	for i, v := range s {
		if i == 0 || v != s[i-1] {
			out = append(out, v)
		}
	}
	return out
}
