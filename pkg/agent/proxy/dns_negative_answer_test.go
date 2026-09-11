package proxy

import (
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// rcodeUpstream starts a local DNS server that answers every query with the
// given rcode and no records, standing in for a cluster resolver's verdict on a
// name. It returns the address in the two parts the Proxy fields want, plus a
// stop func.
//
// It delegates to startFakeUpstream rather than binding its own server so the
// two helpers cannot drift, and so this one inherits the readiness poll:
// dns.Server.Shutdown returns "server not started" if it beats ActivateAndServe,
// leaving the shutdown channel unclosed and the join inside startFakeUpstream
// blocking until the test binary panics.
func rcodeUpstream(t *testing.T, rcode int) (host, port string, stop func()) {
	t.Helper()
	addr, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetRcode(r, rcode)
		_ = w.WriteMsg(m)
	})
	h, p, err := net.SplitHostPort(addr)
	if err != nil {
		stop()
		t.Fatalf("split %q: %v", addr, err)
	}
	return h, p, stop
}

// collectMockMissReports drains p.errChannel and returns the DNS mock-miss
// reports it held, checking the SHAPE of each one on the way through.
//
// The gate under test decides WHETHER a report is emitted, but a report emitted
// with an empty or malformed payload is just as broken as a missing one, and a
// bare `case <-p.errChannel` cannot tell those apart -- it passes on any value
// of any type. Asserting ErrMockNotFound + a non-nil MismatchReport here means
// the negative assertions below stay honest if the reporting path is reshaped.
//
// The non-blocking drain cannot race the producer: SendError delivers on the
// caller's goroutine, and resolveUncachedDNSResponse calls it synchronously
// before returning, so every report is already buffered by the time these
// helpers run. StartErrorDrain is never started against these hand-built Proxy
// values, so this is the only reader.
func collectMockMissReports(t *testing.T, p *Proxy) []models.MockMismatchReport {
	t.Helper()
	var reports []models.MockMismatchReport
	for {
		select {
		case err := <-p.errChannel:
			parserErr, ok := err.(models.ParserError)
			if !ok {
				t.Fatalf("unexpected error type on errChannel: %T (%v)", err, err)
			}
			if parserErr.ParserErrorType != models.ErrMockNotFound {
				t.Fatalf("unexpected parser error type on errChannel: %q", parserErr.ParserErrorType)
			}
			if parserErr.MismatchReport == nil {
				t.Fatalf("ErrMockNotFound carried no MismatchReport: %v", parserErr.Err)
			}
			reports = append(reports, *parserErr.MismatchReport)
		default:
			return reports
		}
	}
}

// assertReportedOnce pins that the miss was reported EXACTLY once and that the
// report identifies the query. "Exactly" matters: a duplicate double-counts one
// miss in the replay summary. `why` carries the caller's reason into the failure
// output -- without it a regression prints only "got 0 reports", which says what
// happened but not why it is wrong.
//
// The qtype is asserted alongside the name because a name alone is not
// actionable: glibc fires A and AAAA for every hostname, so "which record type
// was missing" is precisely what the user needs to know.
func assertReportedOnce(t *testing.T, p *Proxy, q dns.Question, why string) {
	t.Helper()
	reports := collectMockMissReports(t, p)
	if len(reports) != 1 {
		t.Fatalf("%s; expected exactly 1 DNS mock-miss report, got %d: %+v", why, len(reports), reports)
	}
	if reports[0].Protocol != "DNS" {
		t.Errorf("report protocol = %q, want \"DNS\"", reports[0].Protocol)
	}
	if !strings.Contains(reports[0].ActualSummary, q.Name) {
		t.Errorf("report ActualSummary = %q; want it to name the query %q", reports[0].ActualSummary, q.Name)
	}
	// Anchored, not Contains: "A" is a substring of "AAAA", so a Contains check
	// passes when the report names the wrong record type -- exactly the A/AAAA
	// confusion this assertion exists to catch.
	if qt := dns.TypeToString[q.Qtype]; !strings.HasPrefix(reports[0].ActualSummary, qt+" ") {
		t.Errorf("report ActualSummary = %q; want it to start with the qtype %q", reports[0].ActualSummary, qt)
	}
}

// assertNotReported pins that nothing was reported, and prints what WAS
// reported when the gate regresses.
func assertNotReported(t *testing.T, p *Proxy, why string) {
	t.Helper()
	if reports := collectMockMissReports(t, p); len(reports) != 0 {
		t.Fatalf("%s; got %d report(s): %+v", why, len(reports), reports)
	}
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

	assertNotReported(t, p, "upstream authoritatively said this name does not exist, "+
		"so there is no mock to be missing and it must not be reported as a mock miss")
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

	assertReportedOnce(t, p, q, "upstream was unreachable, so the miss is unexplained "+
		"and MUST still be reported")
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

	assertReportedOnce(t, p, q, "no upstream to consult, so the miss is unexplained "+
		"and MUST still be reported")
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

			assertReportedOnce(t, p, q, fmt.Sprintf("%s means the resolver failed, "+
				"not that the name is absent; the miss MUST still be reported", tc.name))
		})
	}
}

// NODATA -- NOERROR with zero answer records -- is a VALID negative answer
// ("the name exists, but has no record of this type"), not a failure and not an
// absence. It is relayed to the app verbatim rather than steered, so it leaves
// resolveUncachedDNSResponse well before the reporting block.
//
// That early return is exactly why this needs pinning: the no-report behaviour
// here is a consequence of statement ORDER, not of the rcode gate, so it is
// invisible to the gate's own tests and would be silently lost by anything that
// moved reporting earlier. A dual-stack app resolving a v4-only service emits
// an AAAA query per lookup, so a regression would report a mock miss on every
// one of them.
func TestDNSMockMiss_UpstreamNODATA_IsRelayedAndNotReported(t *testing.T) {
	host, port, stop := rcodeUpstream(t, dns.RcodeSuccess) // NOERROR, no answers
	defer stop()

	p := &Proxy{
		logger:             zap.NewNop(),
		errChannel:         make(chan error, 4),
		dnsUpstreamServers: []string{host},
		dnsUpstreamPort:    port,
		dnsForwardTimeout:  2 * time.Second,
	}

	q := dns.Question{Name: "postgres.checkr.svc.cluster.local.", Qtype: dns.TypeAAAA, Qclass: dns.ClassINET}
	got := p.resolveUncachedDNSResponse(q, models.MODE_TEST, true, time.Now(), nil)

	// FromUpstream is the assertion that actually separates a relay from a
	// synthetic answer, so it goes first. Rcode and answer count cannot: for an
	// AAAA query defaultDNSResponse ALSO returns NOERROR with zero answers,
	// because EnableIPv6Redirect is unset here and the AAAA arm returns an empty
	// answer rather than synthesising one (dns.go, case dns.TypeAAAA). Checking
	// rcode and answer count alone would therefore pass on the steer.
	if !got.FromUpstream {
		t.Fatalf("expected the upstream NODATA relayed as-is; got a synthetic answer instead: %+v", got.Msg)
	}
	if got.Msg == nil || got.Msg.Rcode != dns.RcodeSuccess || len(got.Msg.Answer) != 0 {
		t.Fatalf("expected an empty NOERROR, got %+v", got.Msg)
	}

	assertNotReported(t, p, "NODATA is a valid answer that the app receives verbatim, "+
		"so there is no mock miss to report")
}
