package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"
	"time"

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
		// DisableBackground: true,
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

// For DNS caching
var cache = struct {
	sync.RWMutex
	m map[string][]dns.RR
}{m: make(map[string][]dns.RR)}

func generateCacheKey(name string, qtype uint16) string {
	// For MongoDB SRV queries, include "mongodb" in the cache key to differentiate from other SRV queries
	if strings.HasPrefix(name, "_mongodb._tcp.") {
		return fmt.Sprintf("mongodb-%s-%s", name, dns.TypeToString[qtype])
	}
	return fmt.Sprintf("%s-%s", name, dns.TypeToString[qtype])
}

func (p *Proxy) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {
	p.logger.Debug("", zap.String("Source socket info", w.RemoteAddr().String()))
	msg := new(dns.Msg)
	msg.SetReply(r)
	msg.Authoritative = true

	session, hasSession := p.sessions.Get(uint64(0))
	mode := models.GetMode()
	mockingEnabled := true
	if hasSession && session != nil {
		mode = session.Mode
		mockingEnabled = session.Mocking
	}

	p.logger.Debug("Got some Dns queries")
	for _, question := range r.Question {
		p.logger.Debug("", zap.Int("Record Type", int(question.Qtype)), zap.String("Received Query", question.Name))

		key := generateCacheKey(question.Name, question.Qtype)
		reqTimestamp := time.Now().UTC()

		// Clear cache for MongoDB SRV queries to ensure fresh resolution
		if strings.HasPrefix(question.Name, "_mongodb._tcp.") {
			cache.Lock()
			delete(cache.m, key)
			cache.Unlock()
		}

		cache.RLock()
		answers, found := cache.m[key]
		cache.RUnlock()

		if !found {
			answers = p.resolveUncachedDNSAnswers(question, mode, mockingEnabled, reqTimestamp, session)

			cache.Lock()
			cache.m[key] = answers
			cache.Unlock()
			p.logger.Debug(fmt.Sprintf("Answers[after uncached resolution for query:%v]:\n%v\n", question.Qtype, answers))
		}

		p.logger.Debug(fmt.Sprintf("Answers[before appending to msg]:\n%v\n", answers))
		msg.Answer = append(msg.Answer, answers...)
		p.logger.Debug(fmt.Sprintf("Answers[After appending to msg]:\n%v\n", msg.Answer))
	}

	// p.logger.Debug(fmt.Sprintf("dns msg sending back:\n%v\n", msg))
	p.logger.Debug(fmt.Sprintf("dns msg RCODE sending back:\n%v\n", msg.Rcode))
	p.logger.Debug("Writing dns info back to the client...")
	err := w.WriteMsg(msg)
	if err != nil {
		utils.LogError(p.logger, err, "failed to write dns info back to the client")
	}
}

func (p *Proxy) resolveUncachedDNSAnswers(question dns.Question, mode models.Mode, mockingEnabled bool, reqTime time.Time, session *agent.Session) []dns.RR {
	var answers []dns.RR

	switch mode {
	case models.MODE_TEST:
		// In test mode, only use recorded DNS mocks.
		// If a matching mock has an intentionally empty answer set (e.g. TXT),
		// return it as-is and do not synthesize defaults.
		mocked := false
		if mockingEnabled {
			answers, mocked = p.getMockedDNSAnswers(question)
		}
		if mocked {
			return answers
		}

	case models.MODE_RECORD:
		answers = p.recordDNSMock(question, reqTime, session)
	}

	if len(answers) > 0 {
		return answers
	}
	return p.defaultDNSAnswers(question)
}

func (p *Proxy) defaultDNSAnswers(question dns.Question) []dns.RR {
	switch question.Qtype {
	case dns.TypeA:
		p.logger.Debug("failed to resolve dns query hence sending proxy ip4", zap.String("proxy Ip", p.IP4))
		return []dns.RR{&dns.A{
			Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 3600},
			A:   net.ParseIP(p.IP4),
		}}

	case dns.TypeAAAA:
		// Do not synthesize AAAA fallback (::1/proxy IPv6). For environments that are IPv4-only
		// (for example many docker bridge networks), returning a synthetic IPv6 answer can make
		// clients prefer an unreachable ::1 destination.
		p.logger.Debug("no AAAA answer resolved; returning empty AAAA response")
		return nil

	case dns.TypeSRV:
		// Special handling for MongoDB SRV queries
		if strings.HasPrefix(question.Name, "_mongodb._tcp.") {
			baseDomain := strings.TrimPrefix(question.Name, "_mongodb._tcp.")
			return []dns.RR{&dns.SRV{
				Hdr:      dns.RR_Header{Name: dns.Fqdn(question.Name), Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: 3600},
				Priority: 0,
				Weight:   0,
				Port:     27017,
				Target:   dns.Fqdn("mongodb." + baseDomain),
			}}
		}
		p.logger.Debug("sending default SRV record response")
		return []dns.RR{&dns.SRV{
			Hdr:      dns.RR_Header{Name: question.Name, Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: 3600},
			Priority: 0,
			Weight:   0,
			Port:     8080,
			Target:   dns.Fqdn("keploy.proxy"),
		}}

	case dns.TypeTXT:
		// Always return no TXT records (empty answer). This avoids sending bogus
		// TXT payloads that clients (e.g. mongodb+srv) might try to parse.
		p.logger.Debug("skipping TXT answer (configured to always return empty TXT)")
		return nil

	case dns.TypeMX:
		p.logger.Debug("sending default MX record response")
		return []dns.RR{&dns.MX{
			Hdr:        dns.RR_Header{Name: dns.Fqdn(question.Name), Rrtype: dns.TypeMX, Class: dns.ClassINET, Ttl: 3600},
			Preference: 10,
			Mx:         dns.Fqdn("mail." + question.Name),
		}}

	default:
		p.logger.Warn("Ignoring unsupported DNS query type", zap.Int("query type", int(question.Qtype)))
		return nil
	}
}

func (p *Proxy) getMockedDNSAnswers(question dns.Question) ([]dns.RR, bool) {
	mgrIface, ok := p.MockManagers.Load(uint64(0))
	if !ok {
		return nil, false
	}
	mgr, ok := mgrIface.(*MockManager)
	if !ok || mgr == nil {
		return nil, false
	}

	const maxRetries = 5
	for attempt := 0; attempt < maxRetries; attempt++ {
		mocks, err := mgr.GetUnFilteredMocksByKind(models.DNS)
		if err != nil {
			utils.LogError(p.logger, err, "failed to get dns mocks")
			return nil, false
		}
		if len(mocks) == 0 {
			return nil, false
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

		if matchedMock, answers := findDNSMock(filteredMocks, question, p.logger); matchedMock != nil {
			if p.updateDNSMock(mgr, matchedMock) {
				return answers, true
			}
			p.logger.Debug("DNS mock update failed (filtered), retrying",
				zap.String("mockName", matchedMock.Name),
				zap.Int("attempt", attempt+1),
				zap.Int("maxRetries", maxRetries),
			)
			// On final retry, return the answers anyway to avoid blocking the DNS response
			if attempt == maxRetries-1 {
				p.logger.Warn("DNS mock update exhausted retries, returning matched answers to avoid DNS timeout",
					zap.String("mockName", matchedMock.Name),
					zap.String("query", question.Name),
				)
				return answers, true
			}
			continue
		}

		if matchedMock, answers := findDNSMock(unfilteredMocks, question, p.logger); matchedMock != nil {
			if p.updateDNSMock(mgr, matchedMock) {
				return answers, true
			}
			p.logger.Debug("DNS mock update failed (unfiltered), retrying",
				zap.String("mockName", matchedMock.Name),
				zap.Int("attempt", attempt+1),
				zap.Int("maxRetries", maxRetries),
			)
			// On final retry, return the answers anyway to avoid blocking the DNS response
			if attempt == maxRetries-1 {
				p.logger.Warn("DNS mock update exhausted retries, returning matched answers to avoid DNS timeout",
					zap.String("mockName", matchedMock.Name),
					zap.String("query", question.Name),
				)
				return answers, true
			}
			continue
		}

		return nil, false
	}

	return nil, false
}

func findDNSMock(mocks []*models.Mock, question dns.Question, logger *zap.Logger) (*models.Mock, []dns.RR) {
	for _, mock := range mocks {
		if mock == nil || mock.Spec.DNSReq == nil {
			continue
		}
		if !dnsRequestMatches(mock.Spec.DNSReq, question) {
			continue
		}
		var answers []dns.RR
		if mock.Spec.DNSResp != nil {
			answers = decodeDNSAnswers(logger, mock.Spec.DNSResp.Answers)
		}
		return mock, answers
	}
	return nil, nil
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

func decodeDNSAnswers(logger *zap.Logger, answers []string) []dns.RR {
	if len(answers) == 0 {
		return nil
	}
	decoded := make([]dns.RR, 0, len(answers))
	for _, raw := range answers {
		rr, err := dns.NewRR(raw)
		if err != nil {
			if logger != nil {
				logger.Debug("failed to parse dns answer", zap.String("answer", raw), zap.Error(err))
			}
			continue
		}
		decoded = append(decoded, rr)
	}
	return decoded
}

func encodeDNSAnswers(answers []dns.RR) []string {
	if len(answers) == 0 {
		return nil
	}
	encoded := make([]string, 0, len(answers))
	for _, rr := range answers {
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

func (p *Proxy) recordDNSMock(question dns.Question, reqTime time.Time, session *agent.Session) []dns.RR {
	// TODO: Add support for passThrough here using the src<->dst mapping.
	domain := strings.TrimSuffix(question.Name, ".")
	resolver := net.DefaultResolver
	ctx := context.Background()
	var answers []dns.RR

	// Resolve DNS first, then record the resolved/defaulted answer set.
	switch question.Qtype {
	case dns.TypeSRV:
		if strings.HasPrefix(domain, "_mongodb._tcp.") {
			baseDomain := strings.TrimPrefix(domain, "_mongodb._tcp.")
			_, addrs, err := resolver.LookupSRV(ctx, "mongodb", "tcp", baseDomain)
			if err == nil && len(addrs) > 0 {
				for _, addr := range addrs {
					answers = append(answers, &dns.SRV{
						Hdr:      dns.RR_Header{Name: dns.Fqdn(domain), Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: 3600},
						Priority: addr.Priority,
						Weight:   addr.Weight,
						Port:     addr.Port,
						Target:   dns.Fqdn(addr.Target),
					})
				}
				if len(answers) > 0 {
					p.logger.Debug("resolved the dns records successfully")
				}
			} else {
				// If resolution fails, return a default SRV record
				answers = []dns.RR{&dns.SRV{
					Hdr:      dns.RR_Header{Name: dns.Fqdn(domain), Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: 3600},
					Priority: 0,
					Weight:   0,
					Port:     27017, // Default MongoDB port
					Target:   dns.Fqdn("mongodb." + baseDomain),
				}}
			}
		}

	case dns.TypeTXT:
		// For TXT records, try to resolve them directly
		txtRecords, err := resolver.LookupTXT(ctx, domain)
		if err == nil && len(txtRecords) > 0 {
			for _, txt := range txtRecords {
				answers = append(answers, &dns.TXT{
					Hdr: dns.RR_Header{Name: dns.Fqdn(domain), Rrtype: dns.TypeTXT, Class: dns.ClassINET, Ttl: 3600},
					Txt: []string{txt},
				})
			}
			if len(answers) > 0 {
				p.logger.Debug("resolved the dns records successfully")
			}
		}

	case dns.TypeMX:
		// For MX records, try to resolve them directly
		mxRecords, err := resolver.LookupMX(ctx, domain)
		if err == nil && len(mxRecords) > 0 {
			for _, mx := range mxRecords {
				answers = append(answers, &dns.MX{
					Hdr:        dns.RR_Header{Name: dns.Fqdn(domain), Rrtype: dns.TypeMX, Class: dns.ClassINET, Ttl: 3600},
					Preference: mx.Pref,
					Mx:         dns.Fqdn(mx.Host),
				})
			}
			if len(answers) > 0 {
				p.logger.Debug("resolved the dns records successfully")
			}
		}

	case dns.TypeA, dns.TypeAAAA:
		// For A/AAAA records
		ips, err := resolver.LookupIPAddr(ctx, domain)
		if err != nil {
			p.logger.Debug(fmt.Sprintf("failed to resolve the dns query for:%v", domain), zap.Error(err))
		} else {
			for _, ip := range ips {
				if ipv4 := ip.IP.To4(); ipv4 != nil {
					// Only add A record if TypeA was requested
					if question.Qtype == dns.TypeA {
						answers = append(answers, &dns.A{
							Hdr: dns.RR_Header{Name: dns.Fqdn(domain), Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 3600},
							A:   ipv4,
						})
					}
				} else {
					// Only add AAAA record if TypeAAAA was requested
					if question.Qtype == dns.TypeAAAA {
						answers = append(answers, &dns.AAAA{
							Hdr:  dns.RR_Header{Name: dns.Fqdn(domain), Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 3600},
							AAAA: ip.IP,
						})
					}
				}
			}
			if len(answers) > 0 {
				p.logger.Debug("resolved the dns records successfully")
			}
		}

	default:
		p.logger.Debug("unsupported DNS query type for resolution", zap.Int("query type", int(question.Qtype)))
	}

	if len(answers) == 0 {
		answers = p.defaultDNSAnswers(question)
	}

	if session == nil || session.MC == nil {
		return answers
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
				Answers: encodeDNSAnswers(answers),
			},
			ReqTimestampMock: reqTime,
			ResTimestampMock: resTime,
		},
	}

	if session.Synchronous {
		if mgr := syncMock.Get(); mgr != nil {
			mgr.SetOutputChannel(session.MC)
			mgr.AddMock(mock)
			return answers
		}
	}
	session.MC <- mock
	return answers
}

func clearDNSCache() {
	cache.Lock()
	cache.m = make(map[string][]dns.RR)
	cache.Unlock()
}

func (p *Proxy) stopDNSServers(_ context.Context) error {
	// stop tcp dns server
	if err := p.stopTCPDNSServer(); err != nil {
		return err
	}
	// stop udp dns server
	err := p.stopUDPDNSServer()
	return err
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

	// Check if the nsswitch.conf present for the system
	if _, err := os.Stat(nsSwitchConfig); err == nil {
		// Read the current nsswitch.conf
		data, err := os.ReadFile(nsSwitchConfig)
		if err != nil {
			utils.LogError(p.logger, err, "failed to read the nsswitch.conf file from system")
			return errors.New("failed to setup the nsswitch.conf file to redirect the DNS queries to proxy")
		}
		// copy the data of the nsswitch.conf file in order to reset it back to the original state in the end
		p.nsswitchData = data

		// Replace the hosts field value if it exists
		lines := strings.Split(string(data), "\n")
		for i, line := range lines {
			if strings.HasPrefix(line, "hosts:") {
				lines[i] = "hosts: files dns"
			}
		}

		data = []byte(strings.Join(lines, "\n"))

		// Write the modified nsswitch.conf back to the file
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

	// Write the original data back to the nsswitch.conf file
	err := writeNsswitchConfig(p.logger, nsSwitchConfig, data, nsSwitchPerm)
	if err != nil {
		return errors.New("failed to reset the nsswitch.conf back to the original state")
	}

	p.logger.Debug("Successfully reset the nsswitch config of linux")
	return nil
}
