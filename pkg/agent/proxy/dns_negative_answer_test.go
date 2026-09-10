package proxy

import (
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// nxdomainUpstream starts a local DNS server that answers NXDOMAIN for every
// query, standing in for a cluster resolver being asked about a name that does
// not exist. Returns its address parts and a stop func.
func rcodeUpstream(t *testing.T, rcode int) (host, port string, stop func()) {
	t.Helper()
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetRcode(r, rcode)
		_ = w.WriteMsg(m)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	h, p, err := net.SplitHostPort(pc.LocalAddr().String())
	if err != nil {
		t.Fatalf("split: %v", err)
	}
	return h, p, func() { _ = srv.Shutdown() }
}

// A Kubernetes resolver walks its search list (ndots:5 by default), so a name
// like appdb.ns.svc.cluster.local is first tried as
// appdb.ns.svc.cluster.local.ns.svc.cluster.local. Those probes NXDOMAIN by
// design, and the record side deliberately does not store non-Success rcodes,
// so there is no mock for them and never can be.
//
// Reporting that absence as a mock miss fails the test case over a name that
// legitimately does not exist, and tells the user to "re-record to capture DNS
// queries" — advice that can never work. Observed in enterprise pipelines 8649
// (main) and 8653 as:
//
//	Mock mismatch: [DNS] AAAA appdb.java-mysql.svc.cluster.local.java-mysql.svc.cluster.local.
func TestDNSMockMiss_UpstreamNXDOMAIN_IsNotReportedAsAMockMiss(t *testing.T) {
	host, port, stop := rcodeUpstream(t, dns.RcodeNameError)
	defer stop()

	p := &Proxy{
		logger:             zap.NewNop(),
		errChannel:         make(chan error, 4),
		dnsUpstreamServers: []string{host},
		dnsUpstreamPort:    port,
		dnsForwardTimeout:  2 * time.Second,
		// Without this the synthesised A record carries a nil address, and an
		// assertion on Rcode alone would pass on a response no app could use.
		IP4: "127.0.0.1",
	}

	q := dns.Question{
		Name:   "appdb.java-mysql.svc.cluster.local.java-mysql.svc.cluster.local.",
		Qtype:  dns.TypeA,
		Qclass: dns.ClassINET,
	}

	got := p.resolveUncachedDNSResponse(q, models.MODE_TEST, true, time.Now(), nil)

	// The app must still get a USABLE answer — steering it at the proxy rather
	// than relaying NXDOMAIN is deliberate (issue #2006).
	if got.Msg == nil || got.Msg.Rcode != dns.RcodeSuccess {
		t.Fatalf("expected a synthetic NOERROR response so the app proceeds, got %+v", got.Msg)
	}
	if len(got.Msg.Answer) == 0 {
		t.Fatalf("synthetic response carried no answer; the app would have nothing to dial")
	}
	a, ok := got.Msg.Answer[0].(*dns.A)
	if !ok || a.A == nil || a.A.String() != "127.0.0.1" {
		t.Fatalf("expected the proxy steer 127.0.0.1 in the A record, got %v", got.Msg.Answer[0])
	}

	select {
	case err := <-p.errChannel:
		t.Fatalf("upstream authoritatively said this name does not exist, so there is no "+
			"mock to be missing; it must not be reported as a mock miss. got: %v", err)
	default:
	}
}

// The narrowing above must not swallow REAL gaps. When upstream cannot be
// reached we have no evidence either way about the name, so an unmatched query
// is still a mock miss worth failing on — otherwise a cluster with a broken
// resolver would silently "pass" every replay.
func TestDNSMockMiss_UpstreamUnreachable_IsStillReported(t *testing.T) {
	// Port 1 on loopback: nothing listens, so the forward fails rather than
	// returning an authoritative negative answer.
	p := &Proxy{
		logger:             zap.NewNop(),
		errChannel:         make(chan error, 4),
		dnsUpstreamServers: []string{"127.0.0.1"},
		dnsUpstreamPort:    "1",
		dnsForwardTimeout:  200 * time.Millisecond,
	}

	q := dns.Question{Name: "some-service.default.svc.cluster.local.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	_ = p.resolveUncachedDNSResponse(q, models.MODE_TEST, true, time.Now(), nil)

	select {
	case <-p.errChannel:
		// expected: we could not confirm the name is absent, so report it.
	default:
		t.Fatal("upstream was unreachable, so the miss is unexplained and MUST still be reported")
	}
}

// With no upstream configured at all there is likewise no evidence, so a miss
// stays a miss.
func TestDNSMockMiss_NoUpstreamConfigured_IsStillReported(t *testing.T) {
	p := &Proxy{
		logger:            zap.NewNop(),
		errChannel:        make(chan error, 4),
		dnsForwardTimeout: 200 * time.Millisecond,
	}

	q := dns.Question{Name: "some-service.default.svc.cluster.local.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	_ = p.resolveUncachedDNSResponse(q, models.MODE_TEST, true, time.Now(), nil)

	select {
	case <-p.errChannel:
		// expected
	default:
		t.Fatal("no upstream to consult, so the miss is unexplained and MUST still be reported")
	}
}

// SERVFAIL/REFUSED are NOT "this name does not exist" — they are "the resolver
// could not or would not answer". That is the same no-evidence state as an
// unreachable upstream, so the miss must still be reported. Suppressing them
// would let an overloaded CoreDNS, a DNSSEC validation failure, or a resolver
// refusing our queries turn every replay silently green.
func TestDNSMockMiss_UpstreamSERVFAILOrREFUSED_IsStillReported(t *testing.T) {
	for _, tc := range []struct {
		name  string
		rcode int
	}{
		{"SERVFAIL", dns.RcodeServerFailure},
		{"REFUSED", dns.RcodeRefused},
	} {
		t.Run(tc.name, func(t *testing.T) {
			host, port, stop := rcodeUpstream(t, tc.rcode)
			defer stop()

			p := &Proxy{
				logger:             zap.NewNop(),
				errChannel:         make(chan error, 4),
				dnsUpstreamServers: []string{host},
				dnsUpstreamPort:    port,
				dnsForwardTimeout:  2 * time.Second,
			}
			q := dns.Question{Name: "some-service.default.svc.cluster.local.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
			_ = p.resolveUncachedDNSResponse(q, models.MODE_TEST, true, time.Now(), nil)

			select {
			case <-p.errChannel:
				// expected: the resolver failed, so we learned nothing about the name.
			default:
				t.Fatalf("%s means the resolver failed, not that the name is absent; "+
					"the miss MUST still be reported", tc.name)
			}
		})
	}
}
