package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	expirable "github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/agent"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

func (p *Proxy) startTCPDNSServer(_ context.Context) error {
	addr := fmt.Sprintf(":%v", p.DNSPort)

	handler := p
	server := &dns.Server{
		Addr:      addr,
		Net:       "tcp",
		Handler:   handler,
		ReusePort: true,
	}

	p.TCPDNSServer = server

	p.logger.Info(fmt.Sprintf("starting TCP DNS server at addr %v", server.Addr))
	err := server.ListenAndServe()
	if err != nil {
		enhanced := utils.SuggestProxyStartError(err, p.DNSPort)
		utils.LogError(p.logger, enhanced, "failed to start tcp dns server", zap.String("addr", server.Addr))
		return enhanced
	}
	return nil
}

func (p *Proxy) startUDPDNSServer(_ context.Context) error {
	addr := fmt.Sprintf(":%v", p.DNSPort)

	handler := p
	server := &dns.Server{
		Addr:      addr,
		Net:       "udp",
		Handler:   handler,
		ReusePort: true,
	}

	p.UDPDNSServer = server

	p.logger.Info(fmt.Sprintf("starting UDP DNS server at addr %v", server.Addr))
	err := server.ListenAndServe()
	if err != nil {
		enhanced := utils.SuggestProxyStartError(err, p.DNSPort)
		utils.LogError(p.logger, enhanced, "failed to start udp dns server", zap.String("addr", server.Addr))
		return enhanced
	}
	return nil
}

type dnsCacheEntry struct {
	*dns.Msg

	// true if the response came from upstream resolution or a recorded mock.
	// false if we synthesized defaults (fallback).
	FromUpstream bool
}

const (
	// dnsCacheMaxSize is the maximum number of entries in the DNS cache.
	dnsCacheMaxSize = 256
	// dnsCacheTTL is the default time-to-live for cached DNS entries.
	// Entries expire automatically after this duration.
	dnsCacheTTL = 30 * time.Second

	// recordedDNSMocksMaxSize is the maximum number of entries in the DNS deduplication tracker.
	recordedDNSMocksMaxSize = 1024
	// recordedDNSMocksTTL is the TTL for recorded DNS mock entries.
	// Recording sessions typically don't last longer than this.
	recordedDNSMocksTTL = 30 * time.Minute
)

// newDNSCache creates a new thread-safe, size-bounded, TTL-expiring DNS cache.
func newDNSCache() *expirable.LRU[string, dnsCacheEntry] {
	return expirable.NewLRU[string, dnsCacheEntry](dnsCacheMaxSize, nil, dnsCacheTTL)
}

// shouldCacheDNSResponse reports whether a DNS resolution result is
// eligible for the per-proxy dnsCache.
//
// Only real upstream responses are cached (FromUpstream == true), in
// both record and test modes. Replayed-mock responses and
// synthetic/fallback responses are intentionally NOT cached: mock
// responses already come from an in-memory store and adding another
// layer only complicates invalidation, and caching a fallback would
// pin it for 30 s after transient upstream failures.
//
// Record-mode caching is safe against duplicate DNS-mock emission
// because a cache hit short-circuits before resolveUncachedDNSResponse
// is called — which is the only site that invokes recordDNSMock. As a
// second line of defense recordDNSMock has its own (name, qtype)
// dedupe tracker (p.recordedDNSMocks), so even if the cache were
// bypassed the mock stream would still see each query at most once.
// A 30 s TTL is short enough to pick up DNS record changes during a
// long recording session yet long enough to amortise repeated
// resolutions from client libraries that open one TCP connection per
// request.
func shouldCacheDNSResponse(mode models.Mode, resp dnsCacheEntry) bool {
	if !resp.FromUpstream {
		return false
	}
	return mode == models.MODE_TEST || mode == models.MODE_RECORD
}

// newRecordedDNSMocksCache creates a bounded, TTL-expiring cache for DNS mock deduplication.
func newRecordedDNSMocksCache() *expirable.LRU[string, bool] {
	return expirable.NewLRU[string, bool](recordedDNSMocksMaxSize, nil, recordedDNSMocksTTL)
}

func generateCacheKey(name string, qtype uint16) string {
	return fmt.Sprintf("%s-%s", dns.Fqdn(name), dns.TypeToString[qtype])
}

func dnsTypeString(qtype uint16) string {
	if typeName, ok := dns.TypeToString[qtype]; ok {
		return typeName
	}
	return strconv.Itoa(int(qtype))
}

func dnsQuestionFields(question dns.Question) []zap.Field {
	return []zap.Field{
		zap.String("query", question.Name),
		zap.String("qtype", dnsTypeString(question.Qtype)),
		zap.Uint16("qclass", question.Qclass),
	}
}

func mergeRcode(cur, next int) int {
	// keep success unless we see a failure
	if next == dns.RcodeSuccess {
		return cur
	}
	if cur == dns.RcodeSuccess {
		return next
	}

	// severity preference (helpful for rare multi-question queries)
	if cur == dns.RcodeServerFailure || next == dns.RcodeServerFailure {
		return dns.RcodeServerFailure
	}
	if cur == dns.RcodeRefused || next == dns.RcodeRefused {
		return dns.RcodeRefused
	}
	if cur == dns.RcodeNameError || next == dns.RcodeNameError {
		return dns.RcodeNameError
	}
	return cur
}

func (p *Proxy) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	msg := new(dns.Msg)
	msg.SetReply(r)

	session := p.getSession()
	mode := models.GetMode()
	mockingEnabled := true
	if session != nil {
		mode = session.Mode
		mockingEnabled = session.Mocking
	}

	flagsSet := false

	for _, question := range r.Question {
		key := generateCacheKey(question.Name, question.Qtype)
		reqTimestamp := time.Now().UTC()

		resp, found := p.dnsCache.Get(key)
		if !found {
			// No per-cache-miss log: the dnsCache TTL is 30s, so any
			// unique query (including NXDOMAIN-heavy search-domain
			// expansion that we never record as a mock) cycles through
			// here every 30s and would fill the log on long sessions.
			// Diagnostic value is captured downstream — by upstream
			// failure logs in record mode, by mock-not-found SendError
			// in test mode, and by the deduped "Recording new DNS mock".
			resp = p.resolveUncachedDNSResponse(question, mode, mockingEnabled, reqTimestamp, session)

			if shouldCacheDNSResponse(mode, resp) {
				cached := dnsCacheEntry{Msg: resp.Msg.Copy(), FromUpstream: resp.FromUpstream}
				p.dnsCache.Add(key, cached)
			}
		}

		// Apply flags from the first response (DNS messages normally contain a single question)
		if !flagsSet {
			msg.Authoritative = resp.Authoritative
			msg.RecursionAvailable = resp.RecursionAvailable
			msg.Truncated = resp.Truncated
			flagsSet = true
		}

		// Merge rcode across questions (rare, but safe)
		msg.Rcode = mergeRcode(msg.Rcode, resp.Rcode)

		msg.Answer = append(msg.Answer, resp.Answer...)
		msg.Ns = append(msg.Ns, resp.Ns...)
		msg.Extra = append(msg.Extra, resp.Extra...)
	}

	err := w.WriteMsg(msg)
	if err != nil {
		utils.LogError(p.logger, err, "failed to write dns info back to the client")
	}
}

func (p *Proxy) resolveUncachedDNSResponse(question dns.Question, mode models.Mode, mockingEnabled bool, reqTime time.Time, session *agent.Session) dnsCacheEntry {
	switch mode {
	case models.MODE_TEST:
		// Use recorded DNS mocks only.
		if mockingEnabled {
			if resp, mocked := p.getMockedDNSResponse(question); mocked {
				return resp
			}
		}
		// Mock miss (or mocking disabled). Before falling back to a
		// synthetic/NXDOMAIN response, try forwarding to the cluster's
		// real resolver (typically CoreDNS). This is the fix for the
		// sap-demo / mysql.svc.cluster.local case: cluster-internal
		// hostnames are not — and should not be — recorded as mocks, so
		// a miss in replay mode is the expected shape for in-cluster
		// DB / cache / queue connections. Faking them with the proxy's
		// 127.0.0.1 (the legacy default) steers the app at the wrong
		// IP and crashes the DB driver.
		//
		// When mocking is explicitly disabled the operator's intent is
		// "use real traffic", so forwarding is even more clearly the
		// right behaviour than returning a synthetic proxy IP.
		//
		// If the forward succeeds we return the real answer as
		// FromUpstream so the outer cache layer retains it for the
		// usual 30 s — subsequent queries don't hit the network.
		// If the forward fails we fall through to the existing
		// "mock not found" path: same error, same log line, same
		// NXDOMAIN / synthetic fallback. Forwarding is strictly
		// additive.
		if fwdResp, fwdErr := p.forwardDNSUpstream(question); fwdErr == nil && fwdResp != nil {
			p.logger.Debug("DNS mock miss resolved via upstream forward",
				zap.String("query", question.Name),
				zap.String("qtype", dns.TypeToString[question.Qtype]),
				zap.Int("rcode", fwdResp.Rcode),
				zap.Int("answers", len(fwdResp.Answer)))
			return dnsCacheEntry{Msg: fwdResp, FromUpstream: true}
		} else if fwdErr != nil {
			p.logger.Debug("DNS mock miss + upstream forward failed; falling back to synthetic response",
				zap.String("query", question.Name),
				zap.String("qtype", dns.TypeToString[question.Qtype]),
				zap.Error(fwdErr))
		}
		if mockingEnabled {
			// Send mock not found error if we couldn't match any DNS
			// mock and upstream forwarding also failed.
			p.logger.Debug("mock miss",
				zap.String("protocol", "DNS"),
				zap.String("query", question.Name),
				zap.String("qtype", dns.TypeToString[question.Qtype]),
				zap.String("hint", "DNS mocks may be missing. Re-record to capture DNS queries."),
			)
			proxyErr := models.ParserError{
				ParserErrorType: models.ErrMockNotFound,
				Err:             fmt.Errorf("DNS mock not found for query: %s (%s)", question.Name, dns.TypeToString[question.Qtype]),
				MismatchReport: &models.MockMismatchReport{
					Protocol:      "DNS",
					ActualSummary: fmt.Sprintf("%s %s", dns.TypeToString[question.Qtype], question.Name),
					NextSteps:     "DNS mocks may be missing. Re-record to capture DNS queries.",
				},
			}
			p.SendError(proxyErr)
		}
		return p.defaultDNSResponse(question)

	case models.MODE_RECORD:
		resp, err := p.recordDNSMock(question, reqTime, session)
		if err != nil {
			utils.LogError(p.logger, err, "DNS resolution failed in record mode",
				dnsQuestionFields(question)...,
			)
			return dnsCacheEntry{
				Msg: &dns.Msg{
					MsgHdr: dns.MsgHdr{Rcode: dns.RcodeServerFailure},
				},
				FromUpstream: false,
			}
		}
		return resp
	default:
		// any other mode -> best-effort defaults
		return p.defaultDNSResponse(question)
	}
}

func (p *Proxy) defaultDNSResponse(question dns.Question) dnsCacheEntry {
	// Default synthesized responses should be NOERROR.
	resp := dnsCacheEntry{
		Msg: &dns.Msg{
			MsgHdr: dns.MsgHdr{
				Rcode:              dns.RcodeSuccess,
				Authoritative:      true,
				RecursionAvailable: true,
			},
		},
		FromUpstream: false,
	}

	switch question.Qtype {
	case dns.TypeA:
		p.logger.Debug("using synthesized A fallback for DNS query",
			append(dnsQuestionFields(question), zap.String("proxy_ip4", p.IP4))...,
		)
		resp.Answer = []dns.RR{&dns.A{
			Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 3600},
			A:   net.ParseIP(p.IP4),
		}}
		return resp

	case dns.TypeAAAA:
		// When EnableIPv6Redirect is set, the BPF cgroup program rewrites
		// v6 destinations (including ::1) to the v4-mapped proxy address,
		// so it is safe — and in fact required on modern Linux distros
		// where glibc prefers ::1 over 127.0.0.1 — to answer AAAA with
		// the proxy's v6 address. Mirror the TypeA path symmetrically.
		//
		// With the flag disabled we preserve the legacy behaviour of
		// returning an empty AAAA answer: synthesising ::1 in a v4-only
		// environment (no v6 redirect) would point clients at an
		// unreachable destination.
		if p.EnableIPv6Redirect {
			proxyIP6 := net.ParseIP(p.IP6)
			if proxyIP6 == nil {
				// Fallback to v4-mapped ::ffff:<IP4> if p.IP6 is not a
				// valid address (defensive — p.IP6 is hard-coded to ::1
				// in New() today but could be overridden in docker mode).
				if v4 := net.ParseIP(p.IP4); v4 != nil {
					proxyIP6 = v4.To16()
				}
			}
			if proxyIP6 != nil {
				resp.Answer = []dns.RR{&dns.AAAA{
					Hdr:  dns.RR_Header{Name: question.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 3600},
					AAAA: proxyIP6,
				}}
				return resp
			}
			// fall through to empty AAAA response if we could not build an answer
		}
		return resp

	case dns.TypeSRV:
		// Special handling for MongoDB SRV queries
		if strings.HasPrefix(question.Name, "_mongodb._tcp.") {
			baseDomain := strings.TrimPrefix(question.Name, "_mongodb._tcp.")
			resp.Answer = []dns.RR{&dns.SRV{
				Hdr:      dns.RR_Header{Name: dns.Fqdn(question.Name), Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: 3600},
				Priority: 0,
				Weight:   0,
				Port:     27017,
				Target:   dns.Fqdn("mongodb." + baseDomain),
			}}
			return resp
		}
		p.logger.Debug("sending default SRV record response")
		resp.Answer = []dns.RR{&dns.SRV{
			Hdr:      dns.RR_Header{Name: question.Name, Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: 3600},
			Priority: 0,
			Weight:   0,
			Port:     8080,
			Target:   dns.Fqdn("keploy.proxy"),
		}}
		return resp

	case dns.TypeTXT:
		// Always return no TXT records (empty answer). Avoid bogus TXT payloads.
		p.logger.Debug("skipping TXT answer (configured to always return empty TXT)")
		return resp

	case dns.TypeMX:
		p.logger.Debug("sending default MX record response")
		resp.Answer = []dns.RR{&dns.MX{
			Hdr:        dns.RR_Header{Name: dns.Fqdn(question.Name), Rrtype: dns.TypeMX, Class: dns.ClassINET, Ttl: 3600},
			Preference: 10,
			Mx:         dns.Fqdn("mail." + question.Name),
		}}
		return resp

	default:
		p.logger.Debug("Ignoring unsupported DNS query type", zap.Int("query type", int(question.Qtype)))
		return resp
	}
}

func (p *Proxy) getMockedDNSResponse(question dns.Question) (dnsCacheEntry, bool) {
	mgr := p.getMockManager()
	if mgr == nil {
		return dnsCacheEntry{}, false
	}

	// DNS mocks are stateless config mocks — no need to consume or
	// bump SortOrder. Just find and return the matching response.
	filteredMocks, unfilteredMocks := mgr.GetStatelessMocks(models.DNS, question.Name)
	if len(filteredMocks) == 0 && len(unfilteredMocks) == 0 {
		return dnsCacheEntry{}, false
	}

	if _, resp := findDNSMock(filteredMocks, question, p.logger); resp.Msg != nil {
		return resp, true
	}

	if _, resp := findDNSMock(unfilteredMocks, question, p.logger); resp.Msg != nil {
		return resp, true
	}

	return dnsCacheEntry{}, false
}

func findDNSMock(mocks []*models.Mock, question dns.Question, logger *zap.Logger) (*models.Mock, dnsCacheEntry) {
	for _, mock := range mocks {
		if mock == nil || mock.Spec.DNSReq == nil {
			continue
		}
		if !dnsRequestMatches(mock.Spec.DNSReq, question) {
			continue
		}

		resp := dnsCacheEntry{
			Msg: &dns.Msg{
				MsgHdr: dns.MsgHdr{
					Rcode:              dns.RcodeSuccess,
					Authoritative:      true,
					RecursionAvailable: true,
				},
			},
			FromUpstream: true,
		}

		if mock.Spec.DNSResp != nil {
			resp.Rcode = mock.Spec.DNSResp.Rcode
			resp.Authoritative = mock.Spec.DNSResp.Authoritative
			resp.RecursionAvailable = mock.Spec.DNSResp.RecursionAvailable
			resp.Truncated = mock.Spec.DNSResp.Truncated

			resp.Answer = decodeDNSRRs(logger, mock.Spec.DNSResp.Answers)
			resp.Ns = decodeDNSRRs(logger, mock.Spec.DNSResp.Ns)
			resp.Extra = decodeDNSRRs(logger, mock.Spec.DNSResp.Extra)
		}

		return mock, resp
	}
	return nil, dnsCacheEntry{}
}

func dnsRequestMatches(recorded *models.DNSReq, question dns.Question) bool {
	if recorded == nil {
		return false
	}
	if recorded.Qtype != 0 && recorded.Qtype != question.Qtype {
		return false
	}
	if recorded.Qclass != 0 && recorded.Qclass != question.Qclass {
		return false
	}
	return strings.EqualFold(dns.Fqdn(recorded.Name), dns.Fqdn(question.Name))
}

func decodeDNSRRs(logger *zap.Logger, rrs []string) []dns.RR {
	if len(rrs) == 0 {
		return nil
	}
	decoded := make([]dns.RR, 0, len(rrs))
	for _, raw := range rrs {
		rr, err := dns.NewRR(raw)
		if err != nil {
			if logger != nil {
				logger.Debug("failed to parse dns rr", zap.String("rr", raw), zap.Error(err))
			}
			continue
		}
		decoded = append(decoded, rr)
	}
	return decoded
}

func encodeDNSRRs(rrs []dns.RR) []string {
	if len(rrs) == 0 {
		return nil
	}
	encoded := make([]string, 0, len(rrs))
	for _, rr := range rrs {
		if rr == nil {
			continue
		}
		encoded = append(encoded, rr.String())
	}
	return encoded
}

// generateDNSDedupeKey creates a unique key for DNS mock deduplication.
// The key is based solely on the DNS question (name + type + class).
// Response data (IP addresses, etc.) is intentionally excluded because services
// like AWS SQS return different IPs on every query via round-robin / load balancing,
// which would defeat deduplication and produce thousands of duplicate mocks.
// For test replay, a single recorded response is sufficient.
func generateDNSDedupeKey(question dns.Question) string {
	return fmt.Sprintf("%s:%d:%d",
		strings.ToLower(dns.Fqdn(question.Name)),
		question.Qtype,
		question.Qclass,
	)
}

func (p *Proxy) recordDNSMock(question dns.Question, reqTime time.Time, session *agent.Session) (dnsCacheEntry, error) {
	// Prefer the upstream resolver list captured once at proxy
	// startup (see captureDNSUpstream). This is critical in sidecar
	// deployments where the app-container resolv.conf has been
	// rewritten so the `nameserver` entry is 127.0.0.1 and DNS
	// traffic is redirected to the agent's port 26789 via iptables/
	// eBPF (resolv.conf entries themselves are IP-only and carry no
	// port). Re-reading the file here would either (a) discover the
	// loopback entry and forward queries back at ourselves, or
	// (b) discover nothing at all. Capturing once at startup pins
	// the REAL cluster resolvers.
	//
	// The public-DNS fallback (8.8.8.8 / 1.1.1.1) is retained for the
	// rare local-dev case where the proxy runs outside a cluster AND
	// resolv.conf is entirely missing — the forwarder treats this as
	// "no upstream" and we would otherwise be stuck.
	var servers []string
	port := "53"
	if p.hasDNSUpstream() {
		servers = p.dnsUpstreamServers
		port = p.dnsUpstreamPort
	} else {
		// captureDNSUpstream already logs once at startup if it cannot
		// snapshot a real resolver, so we don't re-log per-query here
		// (that path runs on every cache miss and would burst the log).
		servers = []string{"8.8.8.8", "1.1.1.1"}
	}

	c := new(dns.Client)
	c.Timeout = 5 * time.Second

	m := new(dns.Msg)
	m.SetQuestion(question.Name, question.Qtype)
	m.RecursionDesired = true

	var (
		in         *dns.Msg
		resolveErr error
	)

	for _, server := range servers {
		addr := net.JoinHostPort(server, port)

		// Per-server-attempt logs fire on every cache miss (worst case, multiple
		// servers per miss) so we keep only the diagnostic ones — silent success
		// path, log the rare failure / truncation / TCP-retry transitions.
		c.Net = "udp"
		resp, _, err := c.Exchange(m, addr)
		if err != nil {
			resolveErr = err
			p.logger.Debug("upstream DNS exchange failed",
				append(dnsQuestionFields(question),
					zap.String("upstream_addr", addr),
					zap.String("transport", c.Net),
					zap.Error(err),
				)...,
			)
			continue
		}

		if resp != nil && resp.Truncated {
			p.logger.Debug("upstream DNS response truncated; retrying over TCP",
				append(dnsQuestionFields(question), zap.String("upstream_addr", addr))...,
			)
			c.Net = "tcp"
			resp, _, err = c.Exchange(m, addr)
			if err != nil {
				resolveErr = err
				p.logger.Debug("upstream DNS TCP retry failed",
					append(dnsQuestionFields(question),
						zap.String("upstream_addr", addr),
						zap.String("transport", c.Net),
						zap.Error(err),
					)...,
				)
				continue
			}
		}

		if resp != nil {
			in = resp
			resolveErr = nil
			break
		}
	}

	if resolveErr != nil {
		return dnsCacheEntry{}, fmt.Errorf("failed to resolve DNS query for %s: %w", question.Name, resolveErr)
	}
	if in == nil {
		return dnsCacheEntry{}, fmt.Errorf("nil DNS response for %s", question.Name)
	}

	resp := dnsCacheEntry{
		Msg:          in,
		FromUpstream: true,
	}

	// If no session, we still return resolved response but skip recording.
	if session == nil || session.MC == nil {
		return resp, nil
	}

	// Do not record failed DNS responses (e.g., NXDOMAIN, SERVFAIL, REFUSED) in mocks.yaml.
	// These are often noise from search domain expansion or external transient issues.
	// Silent on this path: negatives are not deduped via recordedDNSMocks (we only
	// add successful responses there), so they re-enter recordDNSMock every 30s
	// once the dnsCache TTL elapses. A debug log here would fill the agent log
	// for the full lifetime of a recording session against a chatty client.
	if in.Rcode != dns.RcodeSuccess {
		return resp, nil
	}

	// ========== DNS MOCK DEDUPLICATION ==========
	// Generate a unique key based on the DNS query (ignoring response IPs).
	// If we've already recorded a mock for this query, skip recording.
	// Silent on the dedup-hit path: this fires on every cache miss after
	// the first record (every 30s once the dnsCache TTL elapses) for the
	// duration of the dedup tracker TTL — a debug log here would burst
	// just like the negative-rcode path above.
	dedupeKey := generateDNSDedupeKey(question)
	if _, alreadyRecorded := p.recordedDNSMocks.Get(dedupeKey); alreadyRecorded {
		return resp, nil
	}
	// Mark as recorded
	p.recordedDNSMocks.Add(dedupeKey, true)
	// ============================================

	resTime := time.Now().UTC()
	mock := &models.Mock{
		Version: models.GetVersion(),
		Name:    "mocks",
		Kind:    models.DNS,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"name":  "DNS",
				"qtype": dns.TypeToString[question.Qtype],
				"type":  "config",
			},
			DNSReq: &models.DNSReq{
				Name:   dns.Fqdn(question.Name),
				Qtype:  question.Qtype,
				Qclass: question.Qclass,
			},
			DNSResp: &models.DNSResp{
				Rcode:              in.Rcode,
				Authoritative:      in.Authoritative,
				RecursionAvailable: in.RecursionAvailable,
				Truncated:          in.Truncated,
				Answers:            encodeDNSRRs(in.Answer),
				Ns:                 encodeDNSRRs(in.Ns),
				Extra:              encodeDNSRRs(in.Extra),
			},
			ReqTimestampMock: reqTime,
			ResTimestampMock: resTime,
		},
	}

	// One log per UNIQUE (name, qtype) — bounded by recordedDNSMocksMaxSize
	// (1024) so this is naturally rate-limited. Keep counts only; full
	// answer/ns/extra string dumps would balloon log size for chatty clients.
	p.logger.Debug("Recording new DNS mock",
		append(dnsQuestionFields(question),
			zap.String("dedupe_key", dedupeKey),
			zap.Int("rcode", in.Rcode),
			zap.Int("answer_count", len(in.Answer)),
			zap.Int("ns_count", len(in.Ns)),
			zap.Int("extra_count", len(in.Extra)),
		)...,
	)

	// DNS is a separate key-value map, not a streaming parser; each
	// (name, qtype) unique query is captured exactly once via the
	// p.recordedDNSMocks dedupe above, independent of whether the
	// first app request has been seen. Route every DNS mock through
	// SyncMockManager so its outChanMu also guards the send against
	// a concurrent CloseOutChan during proxy shutdown — a bare
	// `session.MC <- mock` here would race the close.
	//
	// SendConfigMock is the bypass-buffering path: unlike AddMock
	// it does not observe firstReqSeen, matching the original async
	// behavior of always forwarding immediately. The Synchronous
	// path keeps AddMock so its session-level ordering (startup
	// buffer then flush) stays intact.
	//
	// SetOutputChannel is safe to call on every DNS mock because it
	// is now idempotent: a same-pointer re-bind (this hot path) is
	// a no-op, and a post-Close same-pointer call deliberately does
	// NOT reset outChanClosed — see SetOutputChannel's doc. DNS
	// needs to be able to bind the channel the first time a query
	// arrives, which may precede the first TCP connection that
	// otherwise triggers proxy.buildRecordSession's SetOutputChannel.
	if mgr := syncMock.Get(); mgr != nil {
		mgr.SetOutputChannel(session.MC)
		if session.Synchronous {
			mgr.AddMock(mock)
		} else {
			mgr.SendConfigMock(mock)
		}
	}
	return resp, nil
}

func (p *Proxy) stopDNSServers(_ context.Context) error {
	if err := p.stopTCPDNSServer(); err != nil {
		return err
	}
	return p.stopUDPDNSServer()
}

func (p *Proxy) stopTCPDNSServer() error {
	if p.TCPDNSServer != nil {
		err := p.TCPDNSServer.Shutdown()
		if err != nil {
			utils.LogError(p.logger, err, "failed to stop tcp dns server")
			return err
		}
		p.logger.Info("Tcp Dns server stopped successfully")
	}
	return nil
}

func (p *Proxy) stopUDPDNSServer() error {
	if p.UDPDNSServer != nil {
		err := p.UDPDNSServer.Shutdown()
		if err != nil {
			utils.LogError(p.logger, err, "failed to stop udp dns server")
			return err
		}
		p.logger.Info("Udp Dns server stopped successfully")
	}
	return nil
}

const (
	nsSwitchConfig = "/etc/nsswitch.conf"
	nsSwitchPerm   = 0644
)

// setting up the dns routing for the linux system
func (p *Proxy) setupNsswitchConfig() error {
	if _, err := os.Stat(nsSwitchConfig); err == nil {
		data, err := os.ReadFile(nsSwitchConfig)
		if err != nil {
			utils.LogError(p.logger, err, "failed to read the nsswitch.conf file from system")
			return errors.New("failed to setup the nsswitch.conf file to redirect the DNS queries to proxy")
		}

		p.nsswitchData = data

		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if strings.HasPrefix(line, "hosts:") {
				lines[i] = "hosts: files dns"
			}
		}

		data = []byte(strings.Join(lines, "\n"))

		err = writeNsswitchConfig(p.logger, nsSwitchConfig, data, nsSwitchPerm)
		if err != nil {
			return errors.New("failed to setup the nsswitch.conf file to redirect the DNS queries to proxy")
		}

		p.logger.Debug("Successfully written to nsswitch config of linux")
	}
	return nil
}

// resetNsSwitchConfig resets the hosts config of nsswitch of the system
func (p *Proxy) resetNsSwitchConfig() error {
	data := p.nsswitchData

	err := writeNsswitchConfig(p.logger, nsSwitchConfig, data, nsSwitchPerm)
	if err != nil {
		return errors.New("failed to reset the nsswitch.conf back to the original state")
	}

	p.logger.Debug("Successfully reset the nsswitch config of linux")
	return nil
}
