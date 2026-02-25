package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
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
	// Entries expire automatically after this duration.
	dnsCacheTTL = 30 * time.Second
)

// newDNSCache creates a new thread-safe, size-bounded, TTL-expiring DNS cache.
func newDNSCache() *expirable.LRU[string, dnsCacheEntry] {
	return expirable.NewLRU[string, dnsCacheEntry](dnsCacheMaxSize, nil, dnsCacheTTL)
}

func generateCacheKey(name string, qtype uint16) string {
	return fmt.Sprintf("%s-%s", dns.Fqdn(name), dns.TypeToString[qtype])
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

	session, hasSession := p.sessions.Get(uint64(0))
	mode := models.GetMode()
	mockingEnabled := true
	if hasSession && session != nil {
		mode = session.Mode
		mockingEnabled = session.Mocking
	}

	p.logger.Debug("Got some Dns queries")

	flagsSet := false

	for _, question := range r.Question {
		p.logger.Debug("", zap.Int("Record Type", int(question.Qtype)), zap.String("Received Query", question.Name))

		key := generateCacheKey(question.Name, question.Qtype)
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
				zap.String("qtype", dns.TypeToString[question.Qtype]),
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
	mgrIface, ok := p.MockManagers.Load(uint64(0))
	if !ok {
		return dnsCacheEntry{}, false
	}
	mgr, ok := mgrIface.(*MockManager)
	if !ok || mgr == nil {
		return dnsCacheEntry{}, false
	}

	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
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
	// Avoid copying structs that embed locks; construct old/new mock keys explicitly.
	id, isFiltered, sortOrder := matchedMock.TestModeInfo.ID, matchedMock.TestModeInfo.IsFiltered, matchedMock.TestModeInfo.SortOrder

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

	return mgr.UpdateUnFilteredMock(originalMatchedMock, updatedMock)
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

	resTime := time.Now().UTC()
	mock := &models.Mock{
		Version: models.GetVersion(),
		Name:    "mocks",
		Kind:    models.DNS,
		Spec: models.MockSpec{
			Metadata: map[string]string{
				"name":  "DNS",
				"qtype": dns.TypeToString[question.Qtype],
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
