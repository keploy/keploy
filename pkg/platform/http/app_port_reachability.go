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
	"unicode"
	"unicode/utf8"

	"github.com/docker/docker/api/types/container"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

var _ pkg.AppPortReachability = (*AgentClient)(nil)

// appPortCheckTimeout bounds one UnreachableAppPort answer: a container inspect
// and up to two questions to the agent. It is asked from a recording's insert
// loop, a replay's readiness gate and a refused test, and a slow or hung docker
// daemon must hold none of them up for longer. A var only so tests can shorten
// it.
var appPortCheckTimeout = 5 * time.Second

// UnreachableAppPort implements pkg.AppPortReachability for Docker mode.
//
// There the app runs in the keploy agent container's network namespace, and
// that container publishes the ports the user's docker command (or compose
// service) publishes: the host reaches the app through those and nothing else.
// A test sent from the host to host:port goes wherever the publish of host
// port port on an address host reaches leads, and nowhere if there is none. It
// is for the app's server on appPort, the port it was recorded on. Replay
// sends each test to that port number, unless test.port (--port), a protocol
// port or replaceWith sends it to another host port, which a compose file
// publishing "18080:8080" needs.
//
// What can be told from the publishes and from where the agent, which shares
// the app's namespace, sees the app listen:
//
//   - Host port port is not published on an address host reaches: nothing
//     will ever answer there, however long replay waits.
//   - It leads to appPort: it reaches the app, unless the app listens there
//     only on 127.0.0.1 (or ::1). docker forwards a published port to the
//     container's own address, which such a socket never sees.
//   - It leads to another container port: the app may have moved there since
//     the recording (recorded natively on 3000, run in a container on 8080 and
//     replayed with --port 8080 behind -p 8080:8080), so that is where it is
//     reached when it listens there. It is wrong when the app listens there
//     only on loopback, or when the agent sees nothing listening there and the
//     app listening on appPort.
//
// A host port that leads where nothing listens, while the app listens on
// neither appPort nor there, is not called unreachable: apps open their
// servers one after another, so the app may yet listen there, and nothing
// seen now tells "not yet" from "never". A wrong "unreachable" turns off the
// readiness gate's wait and the re-send of a refused test; a missed one costs
// the gate its ceiling, which it says when it gives up.
//
// appPort 0 is a test that does not say which port it was recorded on; the
// port it is sent to stands in for it. That is the port it was recorded on
// unless test.port (--port), a protocol port or replaceWith sends it
// elsewhere, which such a test gives no way to tell.
//
// It answers "" outside Docker mode, for a host that is not this machine's
// loopback (a test.host pointing elsewhere is routed however the operator set
// it up), whenever the container cannot be inspected, and wherever the agent
// cannot tell what decides it: "cannot tell" must never read as "unreachable".
func (a *AgentClient) UnreachableAppPort(ctx context.Context, host string, port, appPort uint16) string {
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
			zap.String("container", a.conf.KeployContainer), zap.Uint16("port", port), zap.Uint16("appPort", appPort), zap.Error(err))
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
	pub, ok := appPortPublish(inspect, host, port, appPort, own)
	if !ok {
		return ""
	}
	listening := a.appListening(ctx, pub.listenPort())
	var atTarget appListening
	if pub.leadsTo != "" && (listening.state == listenNothing || listening.state == listenLoopbackOnly) {
		// Nothing, or only loopback, where host port port leads: whether the
		// app listens on the port the tests are for says whether the publish
		// is what is wrong.
		atTarget = a.appListening(ctx, pub.targetPort())
	}
	return unreachableAppPortReason(pub, cmdType, listening, atTarget)
}

// isLoopbackHost reports whether a dial to host stays on this machine.
func isLoopbackHost(host string) bool {
	if strings.EqualFold(host, "localhost") {
		return true
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	return ip != nil && (ip.IsLoopback() || ip.IsUnspecified())
}

// portPublish is how the agent container publishes the host port the tests
// are sent to, and the app's port they are for.
type portPublish struct {
	host    string // the loopback host the tests are sent to
	port    string // the host port they are sent to
	appPort string // the app's port they were recorded on; "" when not known

	reaches   bool     // host port port leads to target() on an address host reaches
	leadsTo   string   // else, the container port it leads to there, if it leads anywhere
	leadsIP   string   // the host address of that publish as -p writes it ("127.0.0.1:"), "" for every address
	notHere   []string // its publishes on addresses host does not reach, as -p writes them
	notHereOn []string // the addresses of those
	published []string // every tcp publish of the user's, as -p writes it
	elsewhere []string // other host ports target() is published on, where host reaches them
}

// target is the app's port the tests have to reach: the one they were recorded
// on, or, when that is not known, the port they are sent to.
func (p portPublish) target() string {
	if p.appPort != "" {
		return p.appPort
	}
	return p.port
}

func (p portPublish) targetPort() uint16 { return portNumber(p.target()) }

// listenPort is the port inside the container whose sockets decide whether the
// host reaches the app: the one host port port leads to, if it leads anywhere,
// else the one the tests have to reach.
func (p portPublish) listenPort() uint16 {
	if p.leadsTo != "" {
		return portNumber(p.leadsTo)
	}
	return p.targetPort()
}

func portNumber(s string) uint16 {
	n, _ := strconv.ParseUint(s, 10, 16)
	return uint16(n)
}

// appPortPublish reads the agent container's published ports for tests sent
// to host:port for the app's port appPort (0: not known). ok is false when
// the question does not apply: a stopped container refuses everything, and
// under host networking the app's ports are the host's. keployPorts are
// container ports that are keploy's, not the app's.
func appPortPublish(inspect container.InspectResponse, host string, port, appPort uint16, keployPorts map[string]bool) (portPublish, bool) {
	pub := portPublish{host: host, port: strconv.Itoa(int(port))}
	if appPort != 0 {
		pub.appPort = strconv.Itoa(int(appPort))
	}
	if inspect.ContainerJSONBase == nil || inspect.HostConfig == nil || inspect.NetworkSettings == nil {
		return pub, false
	}
	if inspect.State == nil || !inspect.State.Running {
		return pub, false
	}
	if inspect.HostConfig.NetworkMode.IsHost() {
		return pub, false
	}
	type lead struct {
		port int
		ip   string
	}
	var leads []lead
	target := pub.target()
	for containerPort, bindings := range inspect.NetworkSettings.Ports {
		cport := containerPort.Port()
		if containerPort.Proto() != "tcp" || keployPorts[cport] {
			continue
		}
		for _, b := range bindings {
			if b.HostPort == "" {
				continue
			}
			ip := hostIPPrefix(b.HostIP)
			spec := ip + b.HostPort + ":" + cport
			pub.published = append(pub.published, spec)
			reached := publishReaches(b.HostIP, host)
			switch {
			case b.HostPort != pub.port:
				if cport == target && reached {
					pub.elsewhere = append(pub.elsewhere, b.HostPort)
				}
			case !reached:
				pub.notHere = append(pub.notHere, spec)
				pub.notHereOn = append(pub.notHereOn, b.HostIP)
			case cport == target:
				pub.reaches = true
			default:
				n, _ := strconv.Atoi(cport)
				leads = append(leads, lead{port: n, ip: ip})
			}
		}
	}
	pub.published = uniqueSorted(pub.published)
	pub.elsewhere = uniqueSorted(pub.elsewhere)
	pub.notHere = uniqueSorted(pub.notHere)
	pub.notHereOn = uniqueSorted(pub.notHereOn)
	if !pub.reaches && len(leads) > 0 {
		// One host port can be published on several host addresses, to
		// several container ports, and ports map in random order: the lowest
		// container port is taken, so every ask answers alike. One of them
		// leading to target is enough to reach it (above): a connection to
		// localhost may go to either loopback address.
		sort.Slice(leads, func(i, j int) bool {
			if leads[i].port != leads[j].port {
				return leads[i].port < leads[j].port
			}
			return leads[i].ip < leads[j].ip
		})
		pub.leadsTo, pub.leadsIP = strconv.Itoa(leads[0].port), leads[0].ip
	}
	return pub, true
}

// hostIPPrefix is how -p writes a publish's host address before its host port:
// nothing for every address, "127.0.0.1:" or "[::1]:" for one.
func hostIPPrefix(hostIP string) string {
	ip, err := netip.ParseAddr(hostIP)
	switch {
	case hostIP == "" || (err == nil && ip.IsUnspecified()):
		return ""
	case err == nil && ip.Is6() && !ip.Is4In6():
		return "[" + hostIP + "]:"
	}
	return hostIP + ":"
}

// publishReaches reports whether a publish on the host address hostIP takes a
// connection to host, a loopback host. Only a publish on one address is known
// not to: one on every address may or may not take both families, and
// localhost (or a dial to the unspecified address) may be either loopback
// address.
func publishReaches(hostIP, host string) bool {
	ip, err := netip.ParseAddr(hostIP)
	if hostIP == "" || err != nil || ip.IsUnspecified() {
		return true
	}
	ip = ip.Unmap()
	if !ip.IsLoopback() {
		return false
	}
	dial, err := netip.ParseAddr(strings.Trim(host, "[]"))
	if err != nil || dial.IsUnspecified() {
		return true
	}
	return dial.Unmap() == ip
}

// listenState is what the agent says about the app's sockets on a port.
type listenState int

const (
	// listenUnknown is an agent that cannot tell: none, an older one, one that
	// failed, or an answer it cannot read.
	listenUnknown listenState = iota
	// listenNothing is nothing listening there.
	listenNothing
	// listenReachable is the app listening there on an address a published
	// port reaches: a wildcard, or the container's own address.
	listenReachable
	// listenLoopbackOnly is the app listening there only on loopback.
	listenLoopbackOnly
)

// appListening is where the app's sockets listen on one of its ports, as far
// as the agent can tell. The zero value is "cannot tell".
type appListening struct {
	state    listenState
	loopback []string // every address it listens on, for listenLoopbackOnly
}

// listens reports whether the app is seen listening there at all.
func (l appListening) listens() bool {
	return l.state == listenReachable || l.state == listenLoopbackOnly
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
		a.logger.Debug("could not read the agent's answer to where the app listens", zap.Uint16("port", port), zap.Error(err))
		return appListening{}
	}
	return classifyAppListening(resp.Addrs)
}

// classifyAppListening says whether the app listens on an address a published
// port can reach: anything but loopback (a wildcard, or the container's own
// address). No address is nothing listening. An address it cannot read makes
// the answer unknown, unless another one already shows the app reachable.
func classifyAppListening(addrs []string) appListening {
	if len(addrs) == 0 {
		return appListening{state: listenNothing}
	}
	var loopback []string
	unreadable := false
	for _, s := range addrs {
		ip, err := netip.ParseAddr(s)
		switch {
		case err != nil:
			unreadable = true
		case !ip.Unmap().IsLoopback():
			return appListening{state: listenReachable}
		default:
			loopback = append(loopback, ip.Unmap().String())
		}
	}
	if unreadable {
		return appListening{}
	}
	return appListening{state: listenLoopbackOnly, loopback: uniqueSorted(loopback)}
}

// maxListedPublishes caps the publishes a reason lists: `-p 8000-9000:8000-9000`
// is a thousand of them, and the reason is one log line.
const maxListedPublishes = 10

// publishFix words changes to the app's publishes for how it was started.
type publishFix utils.CmdType

// command is what publishes the app's ports.
func (f publishFix) command() string {
	switch utils.CmdType(f) {
	case utils.DockerCompose:
		return "the docker compose service"
	case utils.FromContainer:
		return "the container"
	}
	return "the docker command"
}

// spec writes publish s as the user writes it.
func (f publishFix) spec(s string) string {
	if utils.CmdType(f) == utils.DockerCompose {
		return `"` + s + `"`
	}
	return "-p " + s
}

// add publishes s, on a host port that has no publish.
func (f publishFix) add(s string) string {
	switch utils.CmdType(f) {
	case utils.DockerCompose:
		return fmt.Sprintf(`add "%s" to the app service's ports`, s)
	case utils.FromContainer:
		// keploy re-publishes what the container was created with.
		return "re-create the container with -p " + s
	}
	return fmt.Sprintf("add -p %s to the docker command", s)
}

// change replaces publish from with to. A host port takes one publish on each
// host address (docker refuses a second: "port is already allocated"), so a
// host port that has one is fixed by changing it, never by adding another.
func (f publishFix) change(from, to string) string {
	switch utils.CmdType(f) {
	case utils.DockerCompose:
		return fmt.Sprintf(`change "%s" to "%s" in the app service's ports`, from, to)
	case utils.FromContainer:
		return fmt.Sprintf("re-create the container with -p %s in place of -p %s", to, from)
	}
	return fmt.Sprintf("change -p %s to -p %s in the docker command", from, to)
}

// replace replaces every publish in from with to, a publish on every address.
// None of from may be left: docker does not take a publish of a host port on
// every address next to one of the same host port on a single address.
func (f publishFix) replace(from []string, to string) string {
	if len(from) == 1 {
		return f.change(from[0], to)
	}
	specs := make([]string, len(from))
	for i, s := range from {
		specs[i] = f.spec(s)
	}
	all := strings.Join(specs[:len(specs)-1], ", ") + " and " + specs[len(specs)-1]
	switch utils.CmdType(f) {
	case utils.DockerCompose:
		return fmt.Sprintf(`replace %s with "%s" in the app service's ports`, all, to)
	case utils.FromContainer:
		return fmt.Sprintf("re-create the container with -p %s in place of %s", to, all)
	}
	return fmt.Sprintf("replace %s with -p %s in the docker command", all, to)
}

// reason words why the tests of pub cannot reach the app, and what would let
// them.
type reason struct {
	pub    portPublish
	fix    publishFix
	port   string // the host port the tests are sent to
	target string // pub.target()
	known  bool   // target is the app's port the tests were recorded on, not port standing in for it
}

// sends says where keploy sends the tests.
func (r reason) sends() string {
	switch {
	case !r.known:
		return fmt.Sprintf("keploy sends these tests from the host to port %s", r.port)
	case r.port == r.target:
		return fmt.Sprintf("keploy sends each test from the host to port %s, the port it was recorded on", r.port)
	}
	return fmt.Sprintf("keploy sends these tests from the host to port %s, for the app's port %s they were recorded on", r.port, r.target)
}

// targetName names the port the tests have to reach.
func (r reason) targetName() string {
	if r.known {
		return "the app's port " + r.target
	}
	return "the container's port " + r.target
}

// theTarget names the port the tests have to reach, and why.
func (r reason) theTarget() string {
	if r.known {
		return fmt.Sprintf("the app's port %s these tests were recorded on", r.target)
	}
	return r.targetName()
}

// redirected is said of a port map for tests sent to another port than the
// one they were recorded on. A setting sends them there: test.port (--port),
// a protocol port (test.grpcPort, test.ssePort) or a replaceWith rule, and
// which one is not known here. A port map keyed on the recorded port works in
// place of it, not alongside it.
func (r reason) redirected() string {
	if r.known && r.port != r.target {
		return fmt.Sprintf(" in place of the setting that sends them to port %s now", r.port)
	}
	return ""
}

// sendThere offers the host ports the tests' port is published on, for a
// replay that can send these tests there: a port map does that for them alone,
// where --port would send every test of every set there.
func (r reason) sendThere() string {
	if len(r.pub.elsewhere) == 0 {
		return ""
	}
	there := "host port " + r.pub.elsewhere[0]
	if len(r.pub.elsewhere) > 1 {
		there = "one of host ports " + strings.Join(r.pub.elsewhere, ", ")
	}
	return fmt.Sprintf(", or send these tests to %s with a port map in keploy.yml (test.replaceWith.global.port: {%s: %s})%s",
		there, r.target, r.pub.elsewhere[0], r.redirected())
}

// keepOrSendThere is the way to leave publish from as it is: send these tests
// to a host port the tests' port is published on, or publish it on one.
func (r reason) keepOrSendThere(from string) string {
	if there := r.sendThere(); there != "" {
		return there
	}
	return fmt.Sprintf(", or, to keep %s, publish %s on another host port too and send these tests there with a port map in keploy.yml (test.replaceWith.global.port)%s",
		r.fix.spec(from), r.targetName(), r.redirected())
}

// unreachableAppPortReason says why the host cannot reach the app's port
// through the host port the tests are sent to, and what would let it, or ""
// when it can (a refusal there is then the app not listening yet) or it cannot
// tell. listening is where the app listens on pub.listenPort(); atTarget is
// where it listens on pub.targetPort(), asked only when host port port leads
// to another container port, where nothing or only loopback listens. cmdType
// words the fix for how the app was started.
func unreachableAppPortReason(pub portPublish, cmdType utils.CmdType, listening, atTarget appListening) string {
	r := reason{pub: pub, fix: publishFix(cmdType), port: pub.port, target: pub.target(), known: pub.appPort != ""}
	switch {
	case pub.reaches:
		return r.reaching(listening)
	case pub.leadsTo != "":
		return r.leadingElsewhere(listening, atTarget)
	}
	return r.notPublished(listening)
}

// reaching is the reason for host port port leading to the tests' port, where
// the app listens as l says.
func (r reason) reaching(l appListening) string {
	if l.state != listenLoopbackOnly {
		return ""
	}
	// Publishing cannot help: docker forwards a published port to the
	// container's own address, and a socket on loopback never sees it.
	loopback := strings.Join(l.loopback, " and ")
	where := fmt.Sprintf("the app listens on port %s only on %s inside the container", r.target, loopback)
	if r.port != r.target {
		where = fmt.Sprintf("host port %s is published to the app's port %s, where the app listens only on %s inside the container", r.port, r.target, loopback)
	}
	return fmt.Sprintf("%s, which no published port reaches, and %s. keploy cannot replay these tests from the host while the app listens only there: the app would have to listen on 0.0.0.0:%s",
		where, r.sends(), r.target)
}

// leadingElsewhere is the reason for host port port leading to another
// container port, where the app listens as there says, while it listens on the
// tests' port as atTarget says.
func (r reason) leadingElsewhere(there, atTarget appListening) string {
	to := r.pub.leadsTo
	from := r.pub.leadsIP + r.port + ":" + to
	change := r.fix.change(from, r.pub.leadsIP+r.port+":"+r.target)
	switch there.state {
	case listenNothing:
		if !atTarget.listens() {
			return "" // the app may be starting, and may listen there yet
		}
		s := fmt.Sprintf("host port %s is published to the container's port %s, where nothing listens, instead of %s, where the app listens, and %s. %s",
			r.port, to, r.theTarget(), r.sends(), capFirst(change))
		if atTarget.state == listenLoopbackOnly {
			s += fmt.Sprintf(", with the app listening on 0.0.0.0:%s (it listens there only on %s, which no published port reaches)",
				r.target, strings.Join(atTarget.loopback, " and "))
		}
		return s + r.keepOrSendThere(from)
	case listenLoopbackOnly:
		loopback := strings.Join(there.loopback, " and ")
		if atTarget.state == listenReachable {
			// Listening there on 0.0.0.0 would send the tests to a server
			// they were not recorded on.
			return fmt.Sprintf("host port %s is published to the container's port %s, where the app listens only on %s, which no published port reaches, instead of %s, where it listens on an address one does, and %s. %s%s",
				r.port, to, loopback, r.theTarget(), r.sends(), capFirst(change), r.keepOrSendThere(from))
		}
		s := fmt.Sprintf("host port %s is published to the container's port %s, where the app listens only on %s, which no published port reaches, and %s. keploy cannot replay these tests from the host through it: the app would have to listen on 0.0.0.0:%s",
			r.port, to, loopback, r.sends(), to)
		if r.known || atTarget.listens() {
			// A test that does not say its port has nothing else to go by
			// unless the app is seen on the port it stands in for.
			s += fmt.Sprintf(", or host port %s be published to %s instead (%s), with the app listening on 0.0.0.0:%s",
				r.port, r.theTarget(), change, r.target)
			if atTarget.state == listenLoopbackOnly {
				s += fmt.Sprintf(" (it listens there only on %s)", strings.Join(atTarget.loopback, " and "))
			}
		}
		return s
	}
	// Listening there: the app may have moved there since the recording.
	// Or the agent cannot tell.
	return ""
}

// notPublished is the reason for host port port leading nowhere host reaches,
// with the app listening on the tests' port as l says.
func (r reason) notPublished(l appListening) string {
	publish := r.port + ":" + r.target
	add := r.fix.add(publish)
	if len(r.pub.notHere) > 0 {
		add = r.fix.replace(r.pub.notHere, publish)
	}
	if l.state == listenLoopbackOnly {
		// Publishing alone cannot help: docker forwards a published port to
		// the container's own address, and a socket on loopback never sees it.
		need := fmt.Sprintf(" and have host port %s published to %s (%s)", r.port, r.targetName(), add)
		if r.port == r.target {
			need = fmt.Sprintf(" and have port %s published as itself (%s)", r.target, add)
		}
		return fmt.Sprintf("the app listens on port %s only on %s inside the container, which no published port reaches, and %s. keploy cannot replay these tests from the host while the app listens only there: the app would have to listen on 0.0.0.0:%s%s%s",
			r.target, strings.Join(l.loopback, " and "), r.sends(), r.target, need, r.sendThere())
	}
	// "it" is the app's port the error names first; else host port port is.
	it := r.known && r.port == r.target
	var s string
	switch {
	case len(r.pub.notHere) > 0:
		specs := make([]string, len(r.pub.notHere))
		for i, spec := range r.pub.notHere {
			specs[i] = r.fix.spec(spec)
		}
		s = fmt.Sprintf("host port %s is published only on %s (%s), which keploy's connections to %s do not reach, and %s. Publish it on every address: %s%s",
			r.port, strings.Join(r.pub.notHereOn, " and "), listCapped(specs, maxListedPublishes), r.pub.host, r.sends(), add, r.sendThere())
	case len(r.pub.elsewhere) > 0 && it:
		s = fmt.Sprintf("it is published on host port %s instead, and %s. Publish it on the same port (%s)%s",
			strings.Join(r.pub.elsewhere, ", "), r.sends(), add, r.sendThere())
	case len(r.pub.elsewhere) > 0:
		s = fmt.Sprintf("host port %s is not published, while %s is published on host port %s, and %s. Publish host port %s to it (%s)%s",
			r.port, r.targetName(), strings.Join(r.pub.elsewhere, ", "), r.sends(), r.port, add, r.sendThere())
	default:
		subject := "host port " + r.port + " is not published"
		if it {
			subject = "it is not published on the host"
		}
		publishes := "no ports"
		if len(r.pub.published) > 0 {
			publishes = listCapped(r.pub.published, maxListedPublishes)
		}
		s = fmt.Sprintf("%s (%s publishes %s), and %s. Publish it: %s", subject, r.fix.command(), publishes, r.sends(), add)
	}
	if l.state != listenReachable {
		// Not seen listening, so publishing may not be all it takes.
		s += fmt.Sprintf(", with the app listening on 0.0.0.0:%s (a published port does not reach a socket on 127.0.0.1)", r.target)
	}
	return s
}

// capFirst is s with its first letter upper-case, to start a sentence.
func capFirst(s string) string {
	r, n := utf8.DecodeRuneInString(s)
	return string(unicode.ToUpper(r)) + s[n:]
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
