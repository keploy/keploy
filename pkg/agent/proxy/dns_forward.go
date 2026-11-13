package proxy

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"os"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/netnames"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// defaultResolvConfPath is the production path captureDNSUpstream
// reads on non-Windows hosts. Kept as a typed constant so upstreamConfig's
// Windows check compares against the actual production literal rather
// than re-spelling it.
const defaultResolvConfPath = "/etc/resolv.conf"

// dnsGOOS is the OS whose resolver configuration captureDNSUpstream reads
// (upstreamConfig): runtime.GOOS, or the one a row reads on another.
var dnsGOOS = runtime.GOOS

// resolvConfPath is the file the forwarder snapshots at startup to
// discover real upstream resolvers. A package-level var so tests can
// swap in a temp file without hitting host DNS.
var resolvConfPath = defaultResolvConfPath

// captureDNSUpstream snapshots the machine's resolver list
// (upstreamConfig: /etc/resolv.conf, or the DNS servers of the adapters on
// Windows) into p.dnsUpstreamServers / p.dnsUpstreamPort. Must be called
// exactly once, at proxy startup, BEFORE p.DNSPort is bound as the app
// container's resolver — see the call site in StartProxy for the ordering
// rationale.
//
// A loopback nameserver is filtered out ONLY when the effective
// resolver port in use matches the proxy's own DNS listen port
// (p.DNSPort) — that combination is the unambiguous self-forward
// case: the k8s injector rewrote resolv.conf to a loopback address
// and redirected traffic to our listener, so forwarding there would
// loop and hang the app's query for dnsForwardTimeout on every miss.
//
// Note: resolv.conf `nameserver` lines cannot carry a port; the port
// comes from an `options` directive (or defaults to 53). A loopback
// resolver listening on ANY non-proxy port — e.g. systemd-resolved
// (typically listens on 53 at a loopback address) or a local dnsmasq
// instance bound to a non-default port — is a legitimate real
// resolver and MUST be kept. Dropping loopback resolvers
// unconditionally would leave dnsUpstreamServers empty in
// environments where resolv.conf is entirely loopback, silently
// disabling forward-on-miss.
//
// A nil / empty result is acceptable. The forwarder handles that by
// returning "no upstream configured" and letting the caller fall back
// to the legacy synthetic response.
//
// On Windows a recording asks these servers what a name it records
// resolves to, as it does on Linux: a name only the machine's own resolvers
// answer (a VPN's or a LAN's internal name, a private address's PTR) is
// recorded with the answer the application gets, not a public resolver's.
// Each server's adapter is kept with it, so a name one adapter's server
// denies is asked on the next adapter, as Windows asks it (forwardDNS).
// The search list stays empty there: the shim asks keploy for the name the
// application passed, before Windows appends a suffix to it.
func (p *Proxy) captureDNSUpstream() {
	config, adapter, source, err := upstreamConfig(dnsGOOS)
	if err != nil {
		p.logger.Info("could not read the machine's DNS resolvers for upstream forwarding; a DNS query no mock answers gets a synthetic response",
			zap.String("source", source),
			zap.Error(err))
		return
	}
	if config == nil {
		return
	}

	// Keep the search list: it is what makes a redundant expansion
	// recognisable without a round trip. Captured even when every
	// nameserver is filtered out below, since the two are independent.
	p.dnsSearch = append([]string(nil), config.Search...)

	// resolv.conf has no port syntax per RFC — config.Port is populated
	// from an "options" line or defaults to "53". We compare by string
	// to avoid parsing surprises (leading zeros, etc.).
	nsPort := config.Port
	if nsPort == "" {
		nsPort = "53"
	}
	selfPort := strconv.FormatUint(uint64(p.DNSPort), 10)

	filtered := make([]string, 0, len(config.Servers))
	for _, s := range config.Servers {
		// netip, not net.ParseIP: a link-local IPv6 server carries the
		// zone it is reached in (fe80::1%12), which net.ParseIP refuses.
		// netip's IsLoopback takes an IPv4-mapped 127.0.0.1 as loopback,
		// as net.IP's does.
		ip, err := netip.ParseAddr(s)
		if err != nil {
			continue
		}
		// Only drop loopback when it points at our own listener.
		// Anything else (systemd-resolved, dnsmasq, etc.) is a real
		// resolver we can legitimately forward to.
		if ip.IsLoopback() && nsPort == selfPort {
			p.logger.Debug("dropping self-referential loopback upstream resolver from forwarder list",
				zap.String("server", s),
				zap.String("port", nsPort),
				zap.String("proxyDNSPort", selfPort))
			continue
		}
		filtered = append(filtered, s)
	}
	p.dnsUpstreamServers = filtered
	p.dnsUpstreamAdapter = adapter
	p.dnsUpstreamPort = nsPort
	inCluster := agentInCluster(p.IsDocker, os.Getenv)
	p.dnsInNetworkServers = inNetworkResolvers(filtered, p.IsDocker, inCluster, localNetworks())
	p.dnsClusterDomain = ""
	if inCluster {
		p.dnsClusterDomain = clusterDomain(p.dnsSearch)
	}
	p.logger.Debug("captured upstream DNS resolvers for forward-on-miss",
		zap.Strings("servers", p.dnsUpstreamServers),
		zap.Strings("in_network", p.dnsInNetworkServers),
		zap.String("cluster_domain", p.dnsClusterDomain),
		zap.String("port", p.dnsUpstreamPort),
		zap.Strings("search", p.dnsSearch),
		zap.String("source", source))
	p.logReplayDNSForwarding()
}

// agentInCluster is the test routes/auth.go's inClusterAgent makes: a
// container (--is-docker) the kubelet started, which is the only kind that
// carries KUBERNETES_SERVICE_HOST. A native agent on a machine that also has
// it set (a CI runner pod, a shell inside a pod) is not in-cluster: its
// resolv.conf is the machine's, so nothing there is inside the replay.
func agentInCluster(isDocker bool, getenv func(string) string) bool {
	return isDocker && getenv("KUBERNETES_SERVICE_HOST") != ""
}

// dockerEmbeddedDNS is the resolver docker writes into the resolv.conf of a
// container on a user-defined network: keploy's own keploy-network, and every
// compose network. It answers the network's container names itself.
var dockerEmbeddedDNS = net.IPv4(127, 0, 0, 11)

// inNetworkResolvers picks, from the resolv.conf servers, the ones inside the
// replay's own network: the only resolvers a replay's DNS mock miss is
// forwarded to unless test.allowEgress names models.AllowEgressDNS.
//
//   - An in-cluster agent (a sidecar or a replay pod): every server. The pod's
//     resolv.conf is the cluster's DNS, which answers the cluster's own
//     services (mysql.svc.cluster.local) that are never recorded as mocks.
//   - An agent in a container: docker's embedded resolver, and a resolver
//     on one of the container's own networks (podman's aardvark-dns answers
//     at the network's gateway). A server docker copied from the host's
//     resolv.conf, as it does on the default bridge, is the host's.
//   - A native agent: none. Its resolv.conf is the host's, so a miss is
//     answered by keploy alone.
func inNetworkResolvers(servers []string, isDocker, inCluster bool, localNets []*net.IPNet) []string {
	if inCluster {
		return append([]string(nil), servers...)
	}
	if !isDocker {
		return nil
	}
	var out []string
	for _, s := range servers {
		ip := net.ParseIP(s)
		if ip == nil {
			continue
		}
		if ip.Equal(dockerEmbeddedDNS) || onNetwork(ip, localNets) {
			out = append(out, s)
		}
	}
	return out
}

// localNetworks lists the networks of this network namespace's own
// non-loopback addresses (networksOf). An error leaves the list empty, which
// only narrows what a miss may be forwarded to.
func localNetworks() []*net.IPNet {
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		return nil
	}
	return networksOf(addrs)
}

// networksOf keeps the networks of addrs that a resolver inside the replay's
// own network can sit on. Loopback is not one: a host-network agent's
// resolv.conf naming 127.0.0.53 is the host's systemd-resolved, which
// recurses to the host's upstreams. Link-local is not one either.
func networksOf(addrs []net.Addr) []*net.IPNet {
	var nets []*net.IPNet
	for _, a := range addrs {
		n, ok := a.(*net.IPNet)
		if !ok || n.IP.IsLoopback() || n.IP.IsLinkLocalUnicast() {
			continue
		}
		nets = append(nets, n)
	}
	return nets
}

func onNetwork(ip net.IP, nets []*net.IPNet) bool {
	for _, n := range nets {
		if n.Contains(ip) {
			return true
		}
	}
	return false
}

// clusterDomain reads the cluster's DNS domain from a pod's search list, whose
// entries the kubelet writes as <namespace>.svc.<domain>, svc.<domain> and
// <domain>. Empty when no entry names it (a pod whose dnsPolicy is not
// ClusterFirst): no name is then known to be the cluster's own.
func clusterDomain(search []string) string {
	for _, s := range search {
		s = strings.ToLower(strings.Trim(s, "."))
		if d, ok := strings.CutPrefix(s, "svc."); ok && d != "" {
			return d
		}
		if i := strings.Index(s, ".svc."); i >= 0 && i+len(".svc.") < len(s) {
			return s[i+len(".svc."):]
		}
	}
	return ""
}

// networkOwnsName reports whether name is one the replay's own network
// answers itself, the only kind a DNS mock miss is forwarded to an in-network
// resolver for. Docker's embedded resolver (127.0.0.11) and cluster DNS answer
// their own names and recurse to the host's or public resolvers for every
// other one, single-label names included, so forwarding any other name would
// still send it off the machine.
//
//   - In a cluster: a name under the cluster's domain (clusterDomain), which
//     cluster DNS answers itself: every service and pod name, and every
//     search expansion under it (mysql.ns.svc.cluster.local). A bare name is
//     not one: CoreDNS forwards it upstream. The pod's resolver asks the
//     search expansions first (ndots:5), so a service in the pod's own
//     namespace is still found.
//   - In a container: a name docker's resolver holds for the networks the
//     agent's container is on, as the keploy CLI lists them (netnames): a
//     container's name, its compose service name and network aliases, its
//     short ID, each also qualified by the network's name. A miss for a name
//     the list may hold but does not (netnames.Store.MayOwn) asks the CLI once
//     for a fresh list and waits for it, or for the CLI to say it could not
//     list the names or to go away: never on a clock, so a loaded machine
//     whose CLI lists slowly still finds a container started since.
//
// Every other name is answered as a native replay answers it: record it, or
// name "dns" in test.allowEgress.
func (p *Proxy) networkOwnsName(name string) bool {
	q := netnames.Canon(name)
	if q == "" {
		return false
	}
	if d := p.dnsClusterDomain; d != "" {
		return q == d || strings.HasSuffix(q, "."+d)
	}
	if !p.IsDocker {
		return false
	}
	names := p.networkNames()
	if names.Owns(q) {
		return true
	}
	if !names.MayOwn(q) {
		return false
	}
	names.Refresh(context.Background())
	return names.Owns(q)
}

// networkNames is the Store of the names the replay's docker networks answer:
// the agent process's own, which its routes fill, unless one was set.
func (p *Proxy) networkNames() *netnames.Store {
	if p.netNames != nil {
		return p.netNames
	}
	return netnames.Default()
}

// replayMissServers are the resolvers a replay's DNS mock miss for name, of
// type qtype, is forwarded to: the in-network resolvers, and only for a name
// the replay's own network owns (networkOwnsName) and a type that network's
// resolver answers for it itself (inNetworkAnswersItself). Real egress beyond
// the replay's own network is opened only by a named knob: test.allowEgress
// "dns", --global-passthrough, or mocking turned off, which asks for real
// traffic by definition.
func (p *Proxy) replayMissServers(mockingEnabled bool, name string, qtype uint16) []string {
	if !mockingEnabled || p.GlobalPassthrough || p.allowEgressDNS {
		return p.dnsUpstreamServers
	}
	return p.inNetworkServers(name, qtype)
}

// inNetworkServers are the resolvers inside the replay's own network that a
// question for name, of type qtype, may be put to without leaving the machine:
// the in-network resolvers for a name that network owns (networkOwnsName) and
// a type its resolver answers for that name itself (inNetworkAnswersItself),
// none otherwise. They are part of the replay's environment, not real egress,
// so a replay asks them whatever the egress knobs say: for a DNS mock miss
// (replayMissServers), and for a question whose recorded answer is negative
// (replayDNSAnswer), as the name may exist in the replay's network now.
func (p *Proxy) inNetworkServers(name string, qtype uint16) []string {
	if !p.inNetworkAnswersItself(qtype) || !p.networkOwnsName(name) {
		return nil
	}
	return p.dnsInNetworkServers
}

// inNetworkAnswersItself reports whether the replay network's resolver
// answers a query of type qtype for a name the network owns without asking
// the host's or public resolvers.
//
//   - Cluster DNS is authoritative for the cluster's domain: it answers every
//     type for a name under it, a name it has no record of included.
//   - Docker's embedded resolver (127.0.0.11) answers only A and AAAA (the
//     container's address, or no answer for a family it has none of) and MX
//     (no answer: RFC 5321's implicit MX) for a name it holds. It recurses
//     every other type (CNAME, TXT, SRV, PTR, NS, SOA, CAA, ANY) to the host's
//     or public resolvers, even for a name it holds (measured on docker 29).
//
// A query of another type for an owned name is answered as a native replay
// answers it.
func (p *Proxy) inNetworkAnswersItself(qtype uint16) bool {
	if p.dnsClusterDomain != "" {
		return true
	}
	switch qtype {
	case dns.TypeA, dns.TypeAAAA, dns.TypeMX:
		return true
	}
	return false
}

// logReplayDNSForwarding says once, at a replay agent's start, which resolvers
// a DNS mock miss is forwarded to and which it is not, and how to open the
// rest. Mocking turned off is a per-session choice the agent does not know yet
// at its start, so only the agent-wide knobs are named here.
func (p *Proxy) logReplayDNSForwarding() {
	if p.agentMode != models.MODE_TEST || len(p.dnsUpstreamServers) == 0 {
		return
	}
	knob := ""
	switch {
	case p.allowEgressDNS:
		knob = "test.allowEgress (" + models.AllowEgressDNS + ")"
	case p.GlobalPassthrough:
		knob = "--global-passthrough"
	}
	if knob != "" {
		agent.LogRealEgress(p.logger, knob, "a DNS query no mock answers is forwarded to the machine's resolvers",
			zap.Strings("resolvers", p.dnsUpstreamServers))
		return
	}
	var notAsked []string
	for _, s := range p.dnsUpstreamServers {
		if !slices.Contains(p.dnsInNetworkServers, s) {
			notAsked = append(notAsked, s)
		}
	}
	p.logger.Info("in this replay, a DNS query no mock answers is forwarded only to a resolver inside the replay's own network, and only for a name that network owns (one docker's resolver holds for the replay's networks, as the keploy CLI lists them, or one under the cluster domain); keploy answers every other name itself, and the host's and public resolvers are not asked",
		zap.Strings("in_network_resolvers", p.dnsInNetworkServers),
		zap.String("cluster_domain", p.dnsClusterDomain),
		zap.Strings("not_asked", notAsked),
		zap.String("opt_in", "test.allowEgress: ["+models.AllowEgressDNS+"] (or --allow-egress "+models.AllowEgressDNS+")"))
}

// hasDNSUpstream reports whether the forwarder has any real upstream
// resolver to try. When false, forwardDNSUpstream is a no-op and
// callers fall back to the legacy synthetic response.
func (p *Proxy) hasDNSUpstream() bool {
	return p != nil && len(p.dnsUpstreamServers) > 0
}

// forwardDNSUpstream performs a single DNS Exchange against each
// snapshotted upstream server in turn, returning the first non-nil
// response regardless of rcode (NOERROR, NXDOMAIN, SERVFAIL, …) — the
// caller discriminates on Rcode/Answer count (a valid answer vs NODATA
// vs a genuinely negative rcode). On Windows a negative answer from one
// adapter's server passes the query to the next adapter (forwardDNS),
// as Windows passes it. On timeout or transport error we
// advance to the next server; if all fail we return an error so the
// caller can fall through to the legacy default response.
//
// Transport rules:
//   - Start on UDP (cheapest).
//   - If a UDP response is truncated, retry on TCP against the same
//     server to pull the full answer.
//   - All exchanges share a per-call deadline of p.dnsForwardTimeout.
//
// No caching is done here — the outer ServeDNS handler already caches
// "FromUpstream" responses via p.dnsCache, and recording-mode mocks are
// emitted by the caller. Keeping this function stateless makes it
// trivially safe to call concurrently from the UDP and TCP DNS servers.
func (p *Proxy) forwardDNSUpstream(question dns.Question) (*dns.Msg, error) {
	if !p.hasDNSUpstream() {
		return nil, fmt.Errorf("no upstream DNS resolver configured")
	}
	return p.forwardDNS(question, p.dnsUpstreamServers)
}

// dnsExchanger asks one DNS server, as (*dns.Client).Exchange does.
type dnsExchanger func(c *dns.Client, m *dns.Msg, addr string) (*dns.Msg, time.Duration, error)

// exchange asks addr through p.dnsExchange, or the client itself.
func (p *Proxy) exchange(c *dns.Client, m *dns.Msg, addr string) (*dns.Msg, time.Duration, error) {
	if p.dnsExchange != nil {
		return p.dnsExchange(c, m, addr)
	}
	return c.Exchange(m, addr)
}

// isNegativeAnswer reports whether resp says the name does not exist
// (NXDOMAIN) or has no record of the type asked (NODATA, RFC 2308): an
// answer Windows takes from one adapter's server as that adapter's, while it
// still asks the other adapters.
func isNegativeAnswer(resp *dns.Msg) bool {
	return resp.Rcode == dns.RcodeNameError || (resp.Rcode == dns.RcodeSuccess && len(resp.Answer) == 0)
}

// forwardDNS is forwardDNSUpstream against the given subset of the upstream
// servers (replayMissServers).
//
// On Windows the servers are those of the machine's adapters
// (p.dnsUpstreamAdapter), and Windows asks every adapter a name until one
// answers it: a negative answer (isNegativeAnswer) from one adapter's server
// rules that adapter out, its other servers are not asked, and the next
// adapter's servers are. A name only a VPN's or a LAN's own resolver knows
// is recorded with the answer the application gets, not the NXDOMAIN of the
// adapter Windows prefers. When every adapter answers negatively, the
// preferred adapter's answer is the answer. A server on no adapter (all of
// resolv.conf's) ends the search with whatever it answers, as Linux's
// resolver does.
func (p *Proxy) forwardDNS(question dns.Question, servers []string) (*dns.Msg, error) {
	if len(servers) == 0 {
		return nil, fmt.Errorf("no upstream DNS resolver this replay may forward to")
	}

	// Only forward types we know how to relay sensibly. Everything
	// listed here is pass-through for both the query and the answer —
	// the miekg/dns library serializes whatever RRs the upstream
	// returns. A / AAAA / SRV / PTR are the task's mandatory set;
	// TXT / MX / CNAME / NS / SOA / CAA / ANY ride along because
	// they're free once we're already forwarding and excluding them
	// would break otherwise-working apps (e.g. SMTP clients chasing
	// MX, SPF validators reading TXT). Unrecognised QTypes we leave
	// to the caller's existing default response path so we don't
	// accidentally start answering DNSSEC queries we can't sign.
	if !isForwardableQType(question.Qtype) {
		if qtypeName, ok := dns.TypeToString[question.Qtype]; ok && qtypeName != "" {
			return nil, fmt.Errorf("qtype %s not forwarded", qtypeName)
		}
		return nil, fmt.Errorf("qtype %d not forwarded", question.Qtype)
	}

	// dns.Client.Timeout is the per-exchange wall-clock cap. We also
	// derive a loop-wide deadline so the worst case across all
	// servers is still bounded by dnsForwardTimeout, not
	// N*dnsForwardTimeout. This matters when all upstreams are
	// black-holed: without the outer deadline, three servers would
	// each burn 2 s before we fall back.
	deadline := time.Now().Add(p.dnsForwardTimeout)

	m := new(dns.Msg)
	m.SetQuestion(dns.Fqdn(question.Name), question.Qtype)
	m.Question[0].Qclass = nonZeroQclass(question.Qclass)
	m.RecursionDesired = true

	var lastErr error
	// negative is the first negative answer of an adapter, kept while
	// another adapter may still answer; ruledOut are the adapters that
	// answered negatively.
	var negative *dns.Msg
	ruledOut := map[int]bool{}
	for _, server := range servers {
		adapter, onAdapter := p.dnsUpstreamAdapter[server]
		if onAdapter && ruledOut[adapter] {
			continue
		}
		remaining := time.Until(deadline)
		if remaining <= 0 {
			lastErr = fmt.Errorf("dns forward deadline exceeded before trying %s", server)
			break
		}
		addr := net.JoinHostPort(server, p.dnsUpstreamPort)

		c := &dns.Client{Net: "udp", Timeout: remaining}
		resp, _, err := p.exchange(c, m, addr)
		if err != nil {
			p.logger.Debug("upstream DNS forward failed (udp); trying next server",
				zap.String("server", addr),
				zap.String("question", question.Name),
				zap.Error(err))
			lastErr = err
			continue
		}

		if resp != nil && resp.Truncated {
			// Upstream said "answer too big for UDP" — retry on TCP
			// against the same server using whatever is left of the
			// loop-wide deadline.
			tcpRemaining := time.Until(deadline)
			if tcpRemaining <= 0 {
				lastErr = fmt.Errorf("dns forward deadline exceeded before TCP retry to %s", server)
				break
			}
			tc := &dns.Client{Net: "tcp", Timeout: tcpRemaining}
			tcpResp, _, terr := p.exchange(tc, m, addr)
			if terr != nil {
				p.logger.Debug("upstream DNS forward failed (tcp); trying next server",
					zap.String("server", addr),
					zap.String("question", question.Name),
					zap.Error(terr))
				lastErr = terr
				continue
			}
			resp = tcpResp
		}

		if resp == nil {
			continue
		}
		if onAdapter && isNegativeAnswer(resp) {
			p.logger.Debug("upstream DNS server answered negatively; asking the other adapters' servers",
				zap.String("server", addr),
				zap.String("question", question.Name),
				zap.String("rcode", dns.RcodeToString[resp.Rcode]))
			ruledOut[adapter] = true
			if negative == nil {
				negative = resp
			}
			continue
		}
		return resp, nil
	}

	if negative != nil {
		return negative, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no upstream DNS server returned a response for %s", question.Name)
	}
	return nil, lastErr
}

// isForwardableQType is the allowlist of DNS query types the
// forwarder will relay. See forwardDNSUpstream for the rationale.
func isForwardableQType(qtype uint16) bool {
	switch qtype {
	case dns.TypeA,
		dns.TypeAAAA,
		dns.TypeSRV,
		dns.TypePTR,
		dns.TypeCNAME,
		dns.TypeTXT,
		dns.TypeMX,
		dns.TypeNS,
		dns.TypeSOA,
		dns.TypeCAA,
		dns.TypeANY:
		return true
	}
	return false
}

// nonZeroQclass substitutes dns.ClassINET when the caller did not set
// a query class. The miekg/dns server layer passes Qclass==0 when the
// incoming question did not include one (rare but RFC-allowed for
// some crafted packets); forwarding Qclass=0 upstream is a spec
// violation and CoreDNS answers with FORMERR.
func nonZeroQclass(qclass uint16) uint16 {
	if qclass == 0 {
		return dns.ClassINET
	}
	return qclass
}
