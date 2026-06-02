package proxy

import (
	"fmt"
	"net"
	"runtime"
	"strconv"
	"time"

	"github.com/miekg/dns"
	"go.uber.org/zap"
)

// defaultResolvConfPath is the production path captureDNSUpstream
// reads on non-Windows hosts. Kept as a typed constant so the
// Windows-short-circuit check below compares against the actual
// production literal rather than re-spelling it.
const defaultResolvConfPath = "/etc/resolv.conf"

// resolvConfPath is the file the forwarder snapshots at startup to
// discover real upstream resolvers. A package-level var so tests can
// swap in a temp file without hitting host DNS.
var resolvConfPath = defaultResolvConfPath

// captureDNSUpstream snapshots the cluster resolver list from
// /etc/resolv.conf into p.dnsUpstreamServers / p.dnsUpstreamPort. Must
// be called exactly once, at proxy startup, BEFORE p.DNSPort is bound
// as the app container's resolver — see the call site in StartProxy
// for the ordering rationale.
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
// Windows has no /etc/resolv.conf and this feature — forward-on-miss
// to cluster resolvers — is only meaningful in Linux sidecar
// environments where the k8s injector has rewritten resolv.conf. On
// Windows with the default path we short-circuit to a clean no-op
// so we neither burn a stat on a path that can never exist nor log
// a misleading "file not found" at every agent startup.
// hasDNSUpstream() then returns false and the proxy's DNS handler
// falls back to the legacy synthetic response, matching pre-feature
// behavior exactly.
//
// The `resolvConfPath == defaultResolvConfPath` guard means tests
// that swap in a temp resolv.conf (via the resolvConfPath package
// var) still exercise the full parsing/filter path on every GOOS,
// so `go test ./...` on Windows stays green.
func (p *Proxy) captureDNSUpstream() {
	if runtime.GOOS == "windows" && resolvConfPath == defaultResolvConfPath {
		return
	}
	config, err := dns.ClientConfigFromFile(resolvConfPath)
	if err != nil {
		p.logger.Info("could not read resolv.conf for upstream forwarding; DNS mock misses will fall back to synthetic responses",
			zap.String("path", resolvConfPath),
			zap.Error(err))
		return
	}
	if config == nil {
		return
	}

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
		ip := net.ParseIP(s)
		if ip == nil {
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
	p.dnsUpstreamPort = nsPort
	p.logger.Debug("captured upstream DNS resolvers for forward-on-miss",
		zap.Strings("servers", p.dnsUpstreamServers),
		zap.String("port", p.dnsUpstreamPort))
}

// hasDNSUpstream reports whether the forwarder has any real upstream
// resolver to try. When false, forwardDNSUpstream is a no-op and
// callers fall back to the legacy synthetic response.
func (p *Proxy) hasDNSUpstream() bool {
	return p != nil && len(p.dnsUpstreamServers) > 0
}

// forwardDNSUpstream performs a single DNS Exchange against each
// snapshotted upstream server in turn, returning the first Successful
// response. On timeout or transport error we advance to the next
// server; if all fail we return an error so the caller can fall
// through to the legacy default response.
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
	for _, server := range p.dnsUpstreamServers {
		remaining := time.Until(deadline)
		if remaining <= 0 {
			lastErr = fmt.Errorf("dns forward deadline exceeded before trying %s", server)
			break
		}
		addr := net.JoinHostPort(server, p.dnsUpstreamPort)

		c := &dns.Client{Net: "udp", Timeout: remaining}
		resp, _, err := c.Exchange(m, addr)
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
			tcpResp, _, terr := tc.Exchange(m, addr)
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
		return resp, nil
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
