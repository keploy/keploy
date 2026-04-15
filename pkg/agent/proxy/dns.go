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
		utils.LogError(p.logger, err, "failed to start tcp dns server", zap.String("addr", server.Addr))
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
		utils.LogError(p.logger, err, "failed to start udp dns server", zap.String("addr", server.Addr))
		return err
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

func dnsRcodeString(rcode int) string {
	if rcodeName, ok := dns.RcodeToString[rcode]; ok {
		return rcodeName
	}
	return strconv.Itoa(rcode)
}

func dnsQuestionFields(question dns.Question) []zap.Field {
	return []zap.Field{
		zap.String("query", question.Name),
		zap.String("qtype", dnsTypeString(question.Qtype)),
		zap.Uint16("qclass", question.Qclass),
	}
}

func dnsMessageFields(prefix string, msg *dns.Msg) []zap.Field {
	if msg == nil {
		return []zap.Field{zap.Bool(prefix+"_nil", true)}
	}

	fields := []zap.Field{
		zap.Int(prefix+"_rcode", msg.Rcode),
		zap.String(prefix+"_rcode_name", dnsRcodeString(msg.Rcode)),
		zap.Bool(prefix+"_authoritative", msg.Authoritative),
		zap.Bool(prefix+"_recursion_available", msg.RecursionAvailable),
		zap.Bool(prefix+"_truncated", msg.Truncated),
		zap.Int(prefix+"_answer_count", len(msg.Answer)),
		zap.Int(prefix+"_ns_count", len(msg.Ns)),
		zap.Int(prefix+"_extra_count", len(msg.Extra)),
	}

	if len(msg.Answer) > 0 {
		fields = append(fields, zap.Strings(prefix+"_answers", encodeDNSRRs(msg.Answer)))
	}
	if len(msg.Ns) > 0 {
		fields = append(fields, zap.Strings(prefix+"_ns", encodeDNSRRs(msg.Ns)))
	}
	if len(msg.Extra) > 0 {
		fields = append(fields, zap.Strings(prefix+"_extra", encodeDNSRRs(msg.Extra)))
	}

	return fields
}

func resolverConfigFields(config *dns.ClientConfig, question dns.Question) []zap.Field {
	if config == nil {
		return nil
	}

	return []zap.Field{
		zap.Strings("resolver_servers", config.Servers),
		zap.String("resolver_port", config.Port),
		zap.Strings("resolver_search", config.Search),
		zap.Int("resolver_ndots", config.Ndots),
		zap.Int("resolver_timeout_seconds", config.Timeout),
		zap.Int("resolver_attempts", config.Attempts),
		zap.Strings("resolver_name_list", config.NameList(question.Name)),
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

	p.logger.Debug("received dns request",
		zap.String("source", w.RemoteAddr().String()),
		zap.String("mode", string(mode)),
		zap.Bool("mocking_enabled", mockingEnabled),
		zap.Int("question_count", len(r.Question)),
	)

	flagsSet := false

	for _, question := range r.Question {
		key := generateCacheKey(question.Name, question.Qtype)
		reqTimestamp := time.Now().UTC()

		resp, found := p.dnsCache.Get(key)
		logFields := append([]zap.Field{
			zap.String("mode", string(mode)),
			zap.Bool("cache_hit", found),
			zap.String("cache_key", key),
		}, dnsQuestionFields(question)...)
		if !found {
			p.logger.Debug("dns cache miss", logFields...)
			resp = p.resolveUncachedDNSResponse(question, mode, mockingEnabled, reqTimestamp, session)

			// Only cache real mock responses in test mode.
			// Never cache synthetic/fallback responses, and never cache in record mode.
			if mode == models.MODE_TEST && resp.FromUpstream {
				cached := dnsCacheEntry{Msg: resp.Msg.Copy(), FromUpstream: resp.FromUpstream}
				p.dnsCache.Add(key, cached)
			}
		} else {
			p.logger.Debug("dns cache hit", logFields...)
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

		responseFields := append([]zap.Field{
			zap.Bool("from_upstream", resp.FromUpstream),
		}, dnsQuestionFields(question)...)
		responseFields = append(responseFields, dnsMessageFields("resolved", resp.Msg)...)
		p.logger.Debug("resolved dns question", responseFields...)
	}

	p.logger.Debug("sending dns response to client", dnsMessageFields("outgoing", msg)...)

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
			// Send mock not found error if we couldn't match any DNS mock.
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
		p.logger.Debug("resolving dns question in record mode", dnsQuestionFields(question)...)
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
		// Do not synthesize AAAA fallback (::1/proxy IPv6). Returning synthetic IPv6 can make
		// clients prefer an unreachable ::1 destination in IPv4-only environments.
		p.logger.Debug("no synthesized AAAA fallback; returning empty AAAA response", dnsQuestionFields(question)...)
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
		p.logger.Warn("Ignoring unsupported DNS query type", zap.Int("query type", int(question.Qtype)))
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
	config, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	var servers []string
	port := "53"

	if err == nil {
		servers = config.Servers
		port = config.Port
		fields := append(dnsQuestionFields(question), resolverConfigFields(config, question)...)
		if resolvConfBytes, readErr := os.ReadFile("/etc/resolv.conf"); readErr == nil {
			fields = append(fields, zap.String("resolv_conf", strings.TrimSpace(string(resolvConfBytes))))
		} else {
			fields = append(fields, zap.NamedError("resolv_conf_read_error", readErr))
		}
		p.logger.Debug("loaded resolver configuration for record-mode DNS", fields...)
	} else {
		// Fallback to public DNS if resolv.conf fails
		servers = []string{"8.8.8.8", "1.1.1.1"}
		p.logger.Debug("failed to parse /etc/resolv.conf for record-mode DNS; using fallback resolvers",
			append(dnsQuestionFields(question),
				zap.Error(err),
				zap.Strings("resolver_servers", servers),
				zap.String("resolver_port", port),
			)...,
		)
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

		c.Net = "udp"
		p.logger.Debug("sending DNS query upstream",
			append(dnsQuestionFields(question),
				zap.String("upstream_addr", addr),
				zap.String("transport", c.Net),
			)...,
		)
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

		p.logger.Debug("received upstream DNS response",
			append(append(dnsQuestionFields(question),
				zap.String("upstream_addr", addr),
				zap.String("transport", c.Net),
			), dnsMessageFields("upstream", resp)...)...,
		)

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

			p.logger.Debug("received upstream DNS response after TCP retry",
				append(append(dnsQuestionFields(question),
					zap.String("upstream_addr", addr),
					zap.String("transport", c.Net),
				), dnsMessageFields("upstream", resp)...)...,
			)
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
	if in.Rcode != dns.RcodeSuccess {
		fields := append(dnsQuestionFields(question), dnsMessageFields("upstream", in)...)
		p.logger.Debug("skipping DNS mock recording due to non-zero rcode", fields...)
		return resp, nil
	}

	// ========== DNS MOCK DEDUPLICATION ==========
	// Generate a unique key based on the DNS query (ignoring response IPs).
	// If we've already recorded a mock for this query, skip recording.
	dedupeKey := generateDNSDedupeKey(question)
	if _, alreadyRecorded := p.recordedDNSMocks.Get(dedupeKey); alreadyRecorded {
		p.logger.Debug("Skipping duplicate DNS mock",
			append(dnsQuestionFields(question), zap.String("dedupe_key", dedupeKey))...,
		)
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

	p.logger.Debug("Recording new DNS mock",
		append(append(dnsQuestionFields(question), zap.String("dedupe_key", dedupeKey)), dnsMessageFields("upstream", in)...)...,
	)

	if session.Synchronous {
		if mgr := syncMock.Get(); mgr != nil {
			mgr.SetOutputChannel(session.MC)
			mgr.AddMock(mock)
			return resp, nil
		}
	}
	session.MC <- mock
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
