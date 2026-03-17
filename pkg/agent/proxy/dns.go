package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"sort"
	"strings"
	"time"

	expirable "github.com/hashicorp/golang-lru/v2/expirable"
	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg"
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
	dnsCacheTTL = 30 * time.Second

	// recordedDNSMocksMaxSize is the maximum number of entries in the DNS deduplication tracker.
	recordedDNSMocksMaxSize = 1024
	// recordedDNSMocksTTL is the TTL for recorded DNS mock entries.
	recordedDNSMocksTTL = 30 * time.Minute
)

type dnsDedupeStrategy string

const (
	// Exact question + normalized negative response (ignore TTL / SOA serial churn).
	dnsDedupeNXDOMAIN dnsDedupeStrategy = "rcode_nxdomain"

	// Exact question + normalized stable success sections.
	dnsDedupeStableSuccess dnsDedupeStrategy = "rcode_success_stable"

	// Exact question + normalized answer-set-preserving success sections.
	// Used for public/load-balanced A answers where different answer sets are meaningful.
	dnsDedupeRotatingSuccess dnsDedupeStrategy = "rcode_success_rotating"

	// Catch-all for other rcodes.
	dnsDedupeOther dnsDedupeStrategy = "rcode_other"
)

// newDNSCache creates a new thread-safe, size-bounded, TTL-expiring DNS cache.
func newDNSCache() *expirable.LRU[string, dnsCacheEntry] {
	return expirable.NewLRU[string, dnsCacheEntry](dnsCacheMaxSize, nil, dnsCacheTTL)
}

// newRecordedDNSMocksCache creates a bounded, TTL-expiring cache for DNS mock deduplication.
func newRecordedDNSMocksCache() *expirable.LRU[string, bool] {
	return expirable.NewLRU[string, bool](recordedDNSMocksMaxSize, nil, recordedDNSMocksTTL)
}

func generateCacheKey(name string, qtype, qclass uint16) string {
	return fmt.Sprintf("%s-%d-%d", canonicalDNSName(name), qtype, qclass)
}

func canonicalDNSName(name string) string {
	return strings.ToLower(dns.Fqdn(name))
}

func qtypeToString(qtype uint16) string {
	if s, ok := dns.TypeToString[qtype]; ok && s != "" {
		return s
	}
	return fmt.Sprintf("TYPE%d", qtype)
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
	p.logger.Debug("", zap.String("Source socket info", w.RemoteAddr().String()))

	msg := new(dns.Msg)
	msg.SetReply(r)

	session := p.getSession()
	mode := models.GetMode()
	mockingEnabled := true
	if session != nil {
		mode = session.Mode
		mockingEnabled = session.Mocking
	}

	p.logger.Debug("Got some Dns queries")

	flagsSet := false

	for _, question := range r.Question {
		p.logger.Debug("", zap.Int("Record Type", int(question.Qtype)), zap.String("Received Query", question.Name))

		key := generateCacheKey(question.Name, question.Qtype, question.Qclass)
		reqTimestamp := time.Now().UTC()

		resp, found := p.dnsCache.Get(key)
		if !found {
			resp = p.resolveUncachedDNSResponse(question, mode, mockingEnabled, reqTimestamp, session)

			// Only cache real mock responses in test mode.
			// Never cache synthetic/fallback responses, and never cache in record mode.
			if mode == models.MODE_TEST && resp.FromUpstream {
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

	p.logger.Debug(fmt.Sprintf("dns msg RCODE sending back:\n%v\n", msg.Rcode))
	p.logger.Debug("Writing dns info back to the client...")

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
		return p.defaultDNSResponse(question)

	case models.MODE_RECORD:
		resp, err := p.recordDNSMock(question, reqTime, session)
		if err != nil {
			utils.LogError(p.logger, err, "DNS resolution failed in record mode",
				zap.String("query", question.Name),
				zap.String("qtype", qtypeToString(question.Qtype)),
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
		p.logger.Debug("failed to resolve dns query hence sending proxy ip4", zap.String("proxy Ip", p.IP4))
		resp.Answer = []dns.RR{&dns.A{
			Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 3600},
			A:   net.ParseIP(p.IP4),
		}}
		return resp

	case dns.TypeAAAA:
		// Do not synthesize AAAA fallback (::1/proxy IPv6). Returning synthetic IPv6 can make
		// clients prefer an unreachable ::1 destination in IPv4-only environments.
		p.logger.Debug("no AAAA answer resolved; returning empty AAAA response")
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

	const maxRetries = 3
	for attempt := 0; attempt < maxRetries; attempt++ {
		if attempt > 0 {
			time.Sleep(time.Duration(attempt*5) * time.Millisecond)
		}
		mocks, err := mgr.GetUnFilteredMocksByKind(models.DNS)
		if err != nil {
			utils.LogError(p.logger, err, "failed to get dns mocks")
			return dnsCacheEntry{}, false
		}
		if len(mocks) == 0 {
			return dnsCacheEntry{}, false
		}

		var filteredMocks []*models.Mock
		var unfilteredMocks []*models.Mock

		for _, mock := range mocks {
			if mock == nil {
				continue
			}
			if mock.TestModeInfo.IsFiltered {
				filteredMocks = append(filteredMocks, mock)
			} else {
				unfilteredMocks = append(unfilteredMocks, mock)
			}
		}

		if matchedMock, resp := findDNSMock(filteredMocks, question, p.logger); matchedMock != nil {
			if p.updateDNSMock(mgr, matchedMock) {
				return resp, true
			}
			p.logger.Debug("DNS mock update failed (filtered), retrying",
				zap.String("mockName", matchedMock.Name),
				zap.Int("attempt", attempt+1),
				zap.Int("maxRetries", maxRetries),
			)
			if attempt == maxRetries-1 {
				p.logger.Warn("DNS mock update exhausted retries, returning matched response to avoid DNS timeout",
					zap.String("mockName", matchedMock.Name),
					zap.String("query", question.Name),
					zap.Int("attempts", maxRetries),
				)
				return resp, true
			}
			continue
		}

		if matchedMock, resp := findDNSMock(unfilteredMocks, question, p.logger); matchedMock != nil {
			if p.updateDNSMock(mgr, matchedMock) {
				return resp, true
			}
			p.logger.Debug("DNS mock update failed (unfiltered), retrying",
				zap.String("mockName", matchedMock.Name),
				zap.Int("attempt", attempt+1),
				zap.Int("maxRetries", maxRetries),
			)
			if attempt == maxRetries-1 {
				p.logger.Warn("DNS mock update exhausted retries, returning matched response to avoid DNS timeout",
					zap.String("mockName", matchedMock.Name),
					zap.String("query", question.Name),
					zap.Int("attempts", maxRetries),
				)
				return resp, true
			}
			continue
		}

		return dnsCacheEntry{}, false
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

func (p *Proxy) updateDNSMock(mgr *MockManager, matchedMock *models.Mock) bool {
	if mgr == nil || matchedMock == nil {
		return false
	}

	id, isFiltered, sortOrder := matchedMock.TestModeInfo.ID, matchedMock.TestModeInfo.IsFiltered, matchedMock.TestModeInfo.SortOrder

	p.logger.Debug("Attempting DNS mock update",
		zap.String("mockName", matchedMock.Name),
		zap.Int("id", id),
		zap.Bool("isFiltered", isFiltered),
		zap.Int64("sortOrder", sortOrder),
	)

	originalMatchedMock := &models.Mock{
		Name: matchedMock.Name,
		Kind: matchedMock.Kind,
		TestModeInfo: models.TestModeInfo{
			ID:         id,
			IsFiltered: isFiltered,
			SortOrder:  sortOrder,
		},
	}

	updatedMock := &models.Mock{
		Version:      matchedMock.Version,
		Name:         matchedMock.Name,
		Kind:         matchedMock.Kind,
		Spec:         matchedMock.Spec,
		ConnectionID: matchedMock.ConnectionID,
		TestModeInfo: models.TestModeInfo{
			ID:         id,
			IsFiltered: false,
			SortOrder:  pkg.GetNextSortNum(),
		},
	}

	result := mgr.UpdateUnFilteredMock(originalMatchedMock, updatedMock)
	if !result {
		p.logger.Debug("DNS mock update failed - mock not found with expected key",
			zap.String("mockName", matchedMock.Name),
			zap.Int("expectedID", id),
			zap.Int64("expectedSortOrder", sortOrder),
		)
	}
	return result
}

// classifyDNSDedupeStrategy decides how aggressively a DNS response may be deduplicated.
// Important: we still keep the exact qname/qtype/qclass in the key to preserve replay correctness.
func classifyDNSDedupeStrategy(question dns.Question, resp *dns.Msg) dnsDedupeStrategy {
	if resp == nil {
		return dnsDedupeOther
	}

	switch resp.Rcode {
	case dns.RcodeNameError:
		return dnsDedupeNXDOMAIN
	case dns.RcodeSuccess:
		if isLikelyRotatingPublicAnswer(question, resp) {
			return dnsDedupeRotatingSuccess
		}
		return dnsDedupeStableSuccess
	default:
		return dnsDedupeOther
	}
}

func isLikelyRotatingPublicAnswer(question dns.Question, resp *dns.Msg) bool {
	if resp == nil {
		return false
	}
	if question.Qtype != dns.TypeA {
		return false
	}
	if len(resp.Answer) == 0 {
		return false
	}
	if isLikelyInternalDNSName(question.Name) {
		return false
	}

	addressAnswers := 0
	for _, rr := range resp.Answer {
		switch rr.(type) {
		case *dns.A, *dns.AAAA:
			addressAnswers++
		}
	}

	return addressAnswers > 0
}

func isLikelyInternalDNSName(name string) bool {
	n := strings.ToLower(dns.Fqdn(name))
	return strings.HasSuffix(n, ".svc.cluster.local.") ||
		strings.HasSuffix(n, ".cluster.local.") ||
		strings.HasSuffix(n, ".local.")
}

// generateDNSDedupeKey creates a semantic DNS dedupe key.
//
// Key properties:
//   - Exact DNS question is always preserved (name + qtype + qclass).
//   - TTL is ignored everywhere.
//   - SOA serial is ignored to avoid duplicate NXDOMAIN/NODATA mocks caused by authority churn.
//   - For NOERROR answers, distinct answer sets are preserved.
//   - OPT records are ignored in dedupe because they can contain transport/request noise.
func generateDNSDedupeKey(question dns.Question, resp *dns.Msg) (string, dnsDedupeStrategy) {
	strategy := classifyDNSDedupeStrategy(question, resp)

	base := []string{
		"v2",
		string(strategy),
		canonicalDNSName(question.Name),
		fmt.Sprintf("%d", question.Qtype),
		fmt.Sprintf("%d", question.Qclass),
		fmt.Sprintf("%d", resp.Rcode),
	}

	switch strategy {
	case dnsDedupeNXDOMAIN:
		// Strong dedupe on negative responses. Usually Answers are empty and the churn is in authority SOA/NS.
		base = append(base,
			"ans=",
			"ns="+normalizeRRSetForDedupe(resp.Ns, false),
			"extra="+normalizeRRSetForDedupe(resp.Extra, false),
		)
	case dnsDedupeStableSuccess, dnsDedupeRotatingSuccess, dnsDedupeOther:
		base = append(base,
			"ans="+normalizeRRSetForDedupe(resp.Answer, false),
			"ns="+normalizeRRSetForDedupe(resp.Ns, false),
			"extra="+normalizeRRSetForDedupe(resp.Extra, false),
		)
	}

	return strings.Join(base, "|"), strategy
}

func normalizeRRSetForDedupe(rrs []dns.RR, includeOPT bool) string {
	if len(rrs) == 0 {
		return ""
	}

	out := make([]string, 0, len(rrs))
	for _, rr := range rrs {
		if rr == nil {
			continue
		}
		s := normalizeRRForDedupe(rr, includeOPT)
		if s == "" {
			continue
		}
		out = append(out, s)
	}

	if len(out) == 0 {
		return ""
	}

	sort.Strings(out)
	return strings.Join(out, "|")
}

func normalizeRRForDedupe(rr dns.RR, includeOPT bool) string {
	switch r := rr.(type) {
	case *dns.A:
		return fmt.Sprintf("%s|A|%s", canonicalDNSName(r.Hdr.Name), r.A.String())

	case *dns.AAAA:
		return fmt.Sprintf("%s|AAAA|%s", canonicalDNSName(r.Hdr.Name), r.AAAA.String())

	case *dns.CNAME:
		return fmt.Sprintf("%s|CNAME|%s", canonicalDNSName(r.Hdr.Name), canonicalDNSName(r.Target))

	case *dns.NS:
		return fmt.Sprintf("%s|NS|%s", canonicalDNSName(r.Hdr.Name), canonicalDNSName(r.Ns))

	case *dns.PTR:
		return fmt.Sprintf("%s|PTR|%s", canonicalDNSName(r.Hdr.Name), canonicalDNSName(r.Ptr))

	case *dns.MX:
		return fmt.Sprintf("%s|MX|%d|%s", canonicalDNSName(r.Hdr.Name), r.Preference, canonicalDNSName(r.Mx))

	case *dns.SRV:
		return fmt.Sprintf("%s|SRV|%d|%d|%d|%s",
			canonicalDNSName(r.Hdr.Name),
			r.Priority,
			r.Weight,
			r.Port,
			canonicalDNSName(r.Target),
		)

	case *dns.TXT:
		txt := append([]string(nil), r.Txt...)
		sort.Strings(txt)
		return fmt.Sprintf("%s|TXT|%s", canonicalDNSName(r.Hdr.Name), strings.Join(txt, "\x1f"))

	case *dns.SOA:
		// Ignore SOA serial because that is the main negative-response churn source.
		return fmt.Sprintf("%s|SOA|%s|%s|%d|%d|%d|%d",
			canonicalDNSName(r.Hdr.Name),
			canonicalDNSName(r.Ns),
			canonicalDNSName(r.Mbox),
			r.Refresh,
			r.Retry,
			r.Expire,
			r.Minttl,
		)

	case *dns.OPT:
		// EDNS OPT often carries transport/request noise and should not create mock duplication.
		if !includeOPT {
			return ""
		}
		optCodes := make([]string, 0, len(r.Option))
		for _, opt := range r.Option {
			if opt == nil {
				continue
			}
			optCodes = append(optCodes, fmt.Sprintf("%d", opt.Option()))
		}
		sort.Strings(optCodes)
		return fmt.Sprintf("%s|OPT|udp=%d|do=%t|opts=%s",
			canonicalDNSName(r.Hdr.Name),
			r.UDPSize(),
			r.Do(),
			strings.Join(optCodes, ","),
		)

	default:
		// Generic fallback: strip TTL/class header noise as much as possible.
		parts := strings.Fields(rr.String())
		if len(parts) <= 4 {
			return rr.String()
		}
		name := canonicalDNSName(parts[0])
		rrType := parts[3]
		data := strings.Join(parts[4:], " ")
		return fmt.Sprintf("%s|%s|%s", name, rrType, data)
	}
}

func (p *Proxy) recordDNSMock(question dns.Question, reqTime time.Time, session *agent.Session) (dnsCacheEntry, error) {
	config, err := dns.ClientConfigFromFile("/etc/resolv.conf")
	var servers []string
	port := "53"

	if err == nil {
		servers = config.Servers
		port = config.Port
	} else {
		// Fallback to public DNS if resolv.conf fails
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

		c.Net = "udp"
		resp, _, err := c.Exchange(m, addr)
		if err != nil {
			resolveErr = err
			continue
		}

		if resp != nil && resp.Truncated {
			c.Net = "tcp"
			resp, _, err = c.Exchange(m, addr)
			if err != nil {
				resolveErr = err
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

	// ===================== DNS MOCK DEDUPLICATION =====================
	// We dedupe semantically per exact DNS question, with normalization based on rcode family:
	//   - NXDOMAIN: ignore TTL and SOA serial churn
	//   - Stable NOERROR: ignore TTL churn
	//   - Rotating/public NOERROR: keep different answer sets distinct
	dedupeKey, strategy := generateDNSDedupeKey(question, in)
	if _, alreadyRecorded := p.recordedDNSMocks.Get(dedupeKey); alreadyRecorded {
		p.logger.Debug("Skipping duplicate DNS mock",
			zap.String("query", dns.Fqdn(question.Name)),
			zap.String("qtype", qtypeToString(question.Qtype)),
			zap.Int("rcode", in.Rcode),
			zap.String("strategy", string(strategy)),
		)
		return resp, nil
	}
	p.recordedDNSMocks.Add(dedupeKey, true)
	// =================================================================

	resTime := time.Now().UTC()
	mock := &models.Mock{
		Version: models.GetVersion(),
		Name:    "mocks",
		Kind:    models.DNS,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"name":                 "DNS",
				"qtype":                qtypeToString(question.Qtype),
				"type":                 "config",
				"dns_dedupe_strategy":  string(strategy),
				"dns_recorded_rcode":   fmt.Sprintf("%d", in.Rcode),
				"dns_recorded_qname":   dns.Fqdn(question.Name),
				"dns_recorded_qclass":  fmt.Sprintf("%d", question.Qclass),
				"dns_recorded_qtypeid": fmt.Sprintf("%d", question.Qtype),
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
		zap.String("query", dns.Fqdn(question.Name)),
		zap.String("qtype", qtypeToString(question.Qtype)),
		zap.Int("rcode", in.Rcode),
		zap.String("strategy", string(strategy)),
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
