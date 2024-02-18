package proxy

import (
	"context"
	"fmt"
	"github.com/miekg/dns"
	"go.keploy.io/server/v2/pkg/core/proxy/util"
	"go.keploy.io/server/v2/pkg/models"
	"go.uber.org/zap"
	"net"
	"strings"
	"sync"
)

func (ps *Proxy) startTcpDnsServer() {
	addr := fmt.Sprintf(":%v", ps.DnsPort)

	handler := ps
	server := &dns.Server{
		Addr:      addr,
		Net:       "tcp",
		Handler:   handler,
		ReusePort: true,
	}

	ps.TcpDnsServer = server

	ps.logger.Info(fmt.Sprintf("starting TCP DNS server at addr %v", server.Addr))
	err := server.ListenAndServe()
	if err != nil {
		ps.logger.Error("failed to start tcp dns server", zap.Any("addr", server.Addr), zap.Error(err))
	}
}

func (ps *Proxy) startUdpDnsServer() {

	addr := fmt.Sprintf(":%v", ps.DnsPort)

	handler := ps
	server := &dns.Server{
		Addr:      addr,
		Net:       "udp",
		Handler:   handler,
		ReusePort: true,
		// DisableBackground: true,
	}

	ps.UdpDnsServer = server

	ps.logger.Info(fmt.Sprintf("starting UDP DNS server at addr %v", server.Addr))
	err := server.ListenAndServe()
	if err != nil {
		ps.logger.Error("failed to start dns server", zap.Any("addr", server.Addr), zap.Error(err))
	}
}

// For DNS caching
var cache = struct {
	sync.RWMutex
	m map[string][]dns.RR
}{m: make(map[string][]dns.RR)}

func generateCacheKey(name string, qtype uint16) string {
	return fmt.Sprintf("%s-%s", name, dns.TypeToString[qtype])
}

func (ps *Proxy) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {

	ps.logger.Debug("", zap.Any("Source socket info", w.RemoteAddr().String()))
	msg := new(dns.Msg)
	msg.SetReply(r)
	msg.Authoritative = true
	ps.logger.Debug("Got some Dns queries")
	for _, question := range r.Question {
		ps.logger.Debug("", zap.Any("Record Type", question.Qtype), zap.Any("Received Query", question.Name))

		key := generateCacheKey(question.Name, question.Qtype)

		// Check if the answer is cached
		cache.RLock()
		answers, found := cache.m[key]
		cache.RUnlock()

		if !found {
			// If not found in cache, resolve the DNS query only in case of record mode
			if models.GetMode() == models.MODE_RECORD {
				answers = resolveDNSQuery(ps.logger, question.Name)
			}

			if answers == nil || len(answers) == 0 {
				// If the resolution failed, return a default A record with Proxy IP
				if question.Qtype == dns.TypeA {
					answers = []dns.RR{&dns.A{
						Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 3600},
						A:   net.ParseIP(util.ToIP4AddressStr(ps.IP4)),
					}}
					ps.logger.Debug("failed to resolve dns query hence sending proxy ip4", zap.Any("proxy Ip", util.ToIP4AddressStr(ps.IP4)))
				} else if question.Qtype == dns.TypeAAAA {
					if ps.dockerAppCmd {
						ps.logger.Debug("failed to resolve dns query (in docker case) hence sending empty record")
					} else {
						answers = []dns.RR{&dns.AAAA{
							Hdr:  dns.RR_Header{Name: question.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 3600},
							AAAA: net.ParseIP(util.ToIPv6AddressStr(ps.IP6)),
						}}
						ps.logger.Debug("failed to resolve dns query hence sending proxy ip6", zap.Any("proxy Ip", util.ToIPv6AddressStr(ps.IP6)))
					}
				}

				ps.logger.Debug(fmt.Sprintf("Answers[when resolution failed for query:%v]:\n%v\n", question.Qtype, answers))
			}

			// Cache the answer
			cache.Lock()
			cache.m[key] = answers
			cache.Unlock()
			ps.logger.Debug(fmt.Sprintf("Answers[after caching it]:\n%v\n", answers))
		}

		ps.logger.Debug(fmt.Sprintf("Answers[before appending to msg]:\n%v\n", answers))
		msg.Answer = append(msg.Answer, answers...)
		ps.logger.Debug(fmt.Sprintf("Answers[After appending to msg]:\n%v\n", msg.Answer))
	}

	ps.logger.Debug(fmt.Sprintf("dns msg sending back:\n%v\n", msg))
	ps.logger.Debug(fmt.Sprintf("dns msg RCODE sending back:\n%v\n", msg.Rcode))
	ps.logger.Debug("Writing dns info back to the client...")
	err := w.WriteMsg(msg)
	if err != nil {
		ps.logger.Error("failed to write dns info back to the client", zap.Error(err))
	}
}

func resolveDNSQuery(logger *zap.Logger, domain string) []dns.RR {
	// Remove the last dot from the domain name if it exists
	domain = strings.TrimSuffix(domain, ".")

	// Use the default system resolver
	resolver := net.DefaultResolver

	// Perform the lookup with the context
	ips, err := resolver.LookupIPAddr(context.Background(), domain)
	if err != nil {
		logger.Debug(fmt.Sprintf("failed to resolve the dns query for:%v", domain), zap.Error(err))
		return nil
	}

	// Convert the resolved IPs to dns.RR
	var answers []dns.RR
	for _, ip := range ips {
		if ipv4 := ip.IP.To4(); ipv4 != nil {
			answers = append(answers, &dns.A{
				Hdr: dns.RR_Header{Name: dns.Fqdn(domain), Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 3600},
				A:   ipv4,
			})
		} else {
			answers = append(answers, &dns.AAAA{
				Hdr:  dns.RR_Header{Name: dns.Fqdn(domain), Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 3600},
				AAAA: ip.IP,
			})
		}
	}

	if len(answers) > 0 {
		logger.Debug("net.LookupIP resolved the ip address...")
	}

	return answers
}

func (ps *Proxy) stopDnsServer(ctx context.Context) {
	// stop udp dns server & tcp dns server
	if ps.UdpDnsServer != nil {
		err := ps.UdpDnsServer.Shutdown()
		if err != nil {
			ps.logger.Error("failed to stop udp dns server", zap.Error(err))
		}
		ps.logger.Info("Udp Dns server stopped")
	}

	// stop tcp dns server & tcp dns server
	if ps.TcpDnsServer != nil {
		err := ps.TcpDnsServer.Shutdown()
		if err != nil {
			ps.logger.Error("failed to stop tcp dns server", zap.Error(err))
		}
		ps.logger.Info("Tcp Dns server stopped")
	}
}
