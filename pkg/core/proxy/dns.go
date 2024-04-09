package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"strings"
	"sync"

	"github.com/miekg/dns"
	"go.keploy.io/server/v2/pkg/models"
	"go.keploy.io/server/v2/utils"
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
		utils.LogError(p.logger, err, "failed to start tcp dns server", zap.Any("addr", server.Addr))
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
		utils.LogError(p.logger, err, "failed to start udp dns server", zap.Any("addr", server.Addr))
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
	return fmt.Sprintf("%s-%s", name, dns.TypeToString[qtype])
}

func (p *Proxy) ServeDNS(w dns.ResponseWriter, r *dns.Msg) {

	p.logger.Debug("", zap.Any("Source socket info", w.RemoteAddr().String()))
	msg := new(dns.Msg)
	msg.SetReply(r)
	msg.Authoritative = true
	p.logger.Debug("Got some Dns queries")
	for _, question := range r.Question {
		p.logger.Debug("", zap.Any("Record Type", question.Qtype), zap.Any("Received Query", question.Name))

		key := generateCacheKey(question.Name, question.Qtype)

		// Check if the answer is cached
		cache.RLock()
		answers, found := cache.m[key]
		cache.RUnlock()

		if !found {
			// If not found in cache, resolve the DNS query only in case of record mode
			//TODO: Add support for passThrough here using the src<->dst mapping
			if models.GetMode() == models.MODE_RECORD {
				answers = resolveDNSQuery(p.logger, question.Name)
			}

			if len(answers) == 0 {
				// If the resolution failed, return a default A record with Proxy IP
				if question.Qtype == dns.TypeA {
					answers = []dns.RR{&dns.A{
						Hdr: dns.RR_Header{Name: question.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 3600},
						A:   net.ParseIP(p.IP4),
					}}
					p.logger.Debug("failed to resolve dns query hence sending proxy ip4", zap.Any("proxy Ip", p.IP4))
				} else if question.Qtype == dns.TypeAAAA {
					answers = []dns.RR{&dns.AAAA{
						Hdr:  dns.RR_Header{Name: question.Name, Rrtype: dns.TypeAAAA, Class: dns.ClassINET, Ttl: 3600},
						AAAA: net.ParseIP(p.IP6),
					}}
					p.logger.Debug("failed to resolve dns query hence sending proxy ip6", zap.Any("proxy Ip", p.IP6))

				}

				p.logger.Debug(fmt.Sprintf("Answers[when resolution failed for query:%v]:\n%v\n", question.Qtype, answers))
			}

			// Cache the answer
			cache.Lock()
			cache.m[key] = answers
			cache.Unlock()
			p.logger.Debug(fmt.Sprintf("Answers[after caching it]:\n%v\n", answers))
		}

		p.logger.Debug(fmt.Sprintf("Answers[before appending to msg]:\n%v\n", answers))
		msg.Answer = append(msg.Answer, answers...)
		p.logger.Debug(fmt.Sprintf("Answers[After appending to msg]:\n%v\n", msg.Answer))
	}

	p.logger.Debug(fmt.Sprintf("dns msg sending back:\n%v\n", msg))
	p.logger.Debug(fmt.Sprintf("dns msg RCODE sending back:\n%v\n", msg.Rcode))
	p.logger.Debug("Writing dns info back to the client...")
	err := w.WriteMsg(msg)
	if err != nil {
		utils.LogError(p.logger, err, "failed to write dns info back to the client")
	}
}

// TODO: passThrough the dns queries rather than resolving them.
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
