package proxy

import (
	"net"
	"net/netip"
	"sync"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.uber.org/zap"
)

// A replay answers the application's DNS from the recording, so a name it
// looks up resolves to an address the name had at record. A call keploy
// relays to the real server at replay instead of mocking it (the Redis skip,
// redis_skip.go) must reach the server where it is now: a container
// recreated between the record and the replay may have another address, and
// the old one may be another container's. So keploy keeps, for each address
// its DNS answered from a mock, the name it was the answer for, and such a
// relay dials the name's address now when the recorded one is no longer
// among the name's addresses (liveAddr).

// dnsAnswered is the name each address keploy's DNS answered from a
// recorded mock was the answer for. The latest answer keploy handed out that
// gave an address decides, from its cache or not: one from a mock names it,
// one the resolver gave for real (a name with no mock, or a mock.real name)
// leaves it unnamed, so that a connection to it is not taken for one to the
// mocked name. The zero value is ready to use.
type dnsAnswered struct {
	mu    sync.Mutex
	names map[netip.Addr]string
}

// answerAddrs is the A and AAAA addresses of msg's answer.
func answerAddrs(msg *dns.Msg) []netip.Addr {
	if msg == nil {
		return nil
	}
	var out []netip.Addr
	for _, rr := range msg.Answer {
		var ip net.IP
		switch r := rr.(type) {
		case *dns.A:
			ip = r.A
		case *dns.AAAA:
			ip = r.AAAA
		default:
			continue
		}
		if addr, ok := netip.AddrFromSlice(ip); ok {
			out = append(out, addr.Unmap())
		}
	}
	return out
}

// answered keeps what resp, keploy's answer to a question for name, says of
// its addresses: a mock's names each of them name's, any other (the
// resolver's, for real) leaves each its own.
func (d *dnsAnswered) answered(name string, resp dnsCacheEntry) {
	addrs := answerAddrs(resp.Msg)
	if len(addrs) == 0 {
		return
	}
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, a := range addrs {
		if !resp.Mocked {
			delete(d.names, a)
			continue
		}
		if d.names == nil {
			d.names = make(map[netip.Addr]string)
		}
		d.names[a] = dns.Fqdn(name)
	}
}

// name is the name addr was last answered for from a mock, if it was.
func (d *dnsAnswered) name(addr netip.Addr) (string, bool) {
	d.mu.Lock()
	defer d.mu.Unlock()
	n, ok := d.names[addr.Unmap()]
	return n, ok
}

// liveAddr is liveTarget's address.
func (p *Proxy) liveAddr(dstAddr string) string {
	addr, _ := p.liveTarget(dstAddr)
	return addr
}

// liveTarget is where a replay reaches dstAddr for real, and the name that
// takes it there. When keploy's DNS answered the application's lookup of a
// name with dstAddr's address from a recorded mock, and the name no longer
// has that address (as the resolver keploy forwards to answers now), it is
// the name's first address of the same family, at dstAddr's port, and the
// name, fully qualified (with its trailing dot). Otherwise it is dstAddr and
// "": an address the name still has is dialled as the application dialled
// it (one node of a name with many is not another), and so is one whose
// name keploy cannot resolve now, nor any address no mock gave.
//
// It asks the resolver once for each connection it is asked about, bounded
// as every forward is (dnsForwardTimeout).
func (p *Proxy) liveTarget(dstAddr string) (string, string) {
	ap, err := containerkey.ParseAddrPort(dstAddr)
	if err != nil {
		return dstAddr, ""
	}
	ip := ap.Addr()
	name, ok := p.dnsAnswered.name(ip)
	if !ok {
		return dstAddr, ""
	}
	qtype := dns.TypeA
	if ip.Is6() {
		qtype = dns.TypeAAAA
	}
	msg, err := p.forwardDNSUpstream(dns.Question{Name: name, Qtype: qtype, Qclass: dns.ClassINET})
	now := answerAddrs(msg)
	if err != nil || msg == nil || msg.Rcode != dns.RcodeSuccess || len(now) == 0 {
		p.logger.Debug("keploy could not resolve, now, the name a recorded DNS answer gave this address; dialling the recorded address",
			zap.String("name", name), zap.String("recorded address", dstAddr), zap.Error(err))
		return dstAddr, ""
	}
	for _, a := range now {
		if a == ip {
			return dstAddr, ""
		}
	}
	for _, a := range now {
		if a.Is4() == ip.Is4() {
			live := netip.AddrPortFrom(a, ap.Port()).String()
			p.logger.Debug("dialling the name's address now: the address the recording's DNS answer gave is no longer the name's",
				zap.String("name", name), zap.String("recorded address", dstAddr), zap.String("address", live))
			return live, name
		}
	}
	return dstAddr, ""
}
