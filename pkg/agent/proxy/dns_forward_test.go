package proxy

import (
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// startFakeUpstream spins up a miekg/dns UDP server on a
// kernel-assigned port and returns its host:port plus a shutdown func.
// The handler is supplied by the caller so each test can encode its
// own answer / delay / error behaviour.
func startFakeUpstream(t *testing.T, handler dns.HandlerFunc) (string, func()) {
	t.Helper()

	// Bind on an ephemeral UDP port. We capture the actual assigned
	// port via net.ListenPacket so we can drive dns.Server with a
	// pre-bound PacketConn; this is more reliable across CI runners
	// than relying on dns.Server to discover its own port after
	// ListenAndServe.
	pc, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}

	mux := dns.NewServeMux()
	mux.HandleFunc(".", handler)
	srv := &dns.Server{
		PacketConn: pc,
		Handler:    mux,
	}
	done := make(chan struct{})
	go func() {
		_ = srv.ActivateAndServe()
		close(done)
	}()

	// Small startup wait: ActivateAndServe returns only once the
	// listener is actually ready, but its goroutine completes
	// asynchronously. Without this, the very first Exchange can race
	// the server's internal run loop on slow CI boxes. We poll
	// instead of sleeping a fixed duration to keep the happy path
	// fast.
	addr := pc.LocalAddr().String()
	deadline := time.Now().Add(500 * time.Millisecond)
	for time.Now().Before(deadline) {
		c := &dns.Client{Net: "udp", Timeout: 50 * time.Millisecond}
		probe := new(dns.Msg)
		probe.SetQuestion("probe.test.", dns.TypeA)
		if _, _, err := c.Exchange(probe, addr); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}

	return addr, func() {
		_ = srv.Shutdown()
		<-done
	}
}

// newProxyWithUpstream builds the minimum Proxy for forwarder tests
// and points its upstream list at the supplied addr.
func newProxyWithUpstream(t *testing.T, addr string, timeout time.Duration) *Proxy {
	t.Helper()
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		t.Fatalf("split upstream addr: %v", err)
	}
	return &Proxy{
		logger:             zap.NewNop(),
		IP4:                "127.0.0.1",
		IP6:                "::1",
		EnableIPv6Redirect: false,
		dnsUpstreamServers: []string{host},
		dnsUpstreamPort:    port,
		dnsForwardTimeout:  timeout,
		dnsCache:           newDNSCache(),
		recordedDNSMocks:   newRecordedDNSMocksCache(),
	}
}

// TestForwardDNSUpstream_Success validates the happy path: the
// forwarder hits the fake upstream, receives an answer, and returns
// it verbatim.
func TestForwardDNSUpstream_Success(t *testing.T) {
	addr, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Authoritative = true
		if len(r.Question) > 0 && r.Question[0].Qtype == dns.TypeA {
			m.Answer = append(m.Answer, &dns.A{
				Hdr: dns.RR_Header{
					Name:   r.Question[0].Name,
					Rrtype: dns.TypeA,
					Class:  dns.ClassINET,
					Ttl:    60,
				},
				A: net.ParseIP("10.0.0.7"),
			})
		}
		_ = w.WriteMsg(m)
	})
	defer stop()

	p := newProxyWithUpstream(t, addr, 2*time.Second)
	q := dns.Question{Name: "mysql.sap-demo.svc.cluster.local.", Qtype: dns.TypeA, Qclass: dns.ClassINET}

	resp, err := p.forwardDNSUpstream(q)
	if err != nil {
		t.Fatalf("forwardDNSUpstream: %v", err)
	}
	if resp == nil {
		t.Fatalf("resp is nil")
	}
	if resp.Rcode != dns.RcodeSuccess {
		t.Errorf("Rcode = %d, want %d", resp.Rcode, dns.RcodeSuccess)
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("Answer len = %d, want 1", len(resp.Answer))
	}
	a, ok := resp.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("answer not *dns.A: %T", resp.Answer[0])
	}
	if !a.A.Equal(net.ParseIP("10.0.0.7")) {
		t.Errorf("A = %v, want 10.0.0.7", a.A)
	}
}

// TestForwardDNSUpstream_Timeout confirms the forwarder does not hang
// forever when the upstream is black-holed. With dnsForwardTimeout
// set to 100 ms the call must return within ~200 ms (generous for CI
// jitter) with a clear error the caller can fall back on.
func TestForwardDNSUpstream_Timeout(t *testing.T) {
	// Stall handler: never responds fast enough. The forwarder's
	// per-exchange timeout is what has to cut this short. The stall
	// duration only needs to be longer than the forwarder's timeout
	// plus a margin — keep it tight so the test exits promptly.
	addr, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		// Swallow probe queries from startFakeUpstream so the server
		// looks alive enough for the setup phase to succeed, then
		// stall on everything else.
		if len(r.Question) > 0 && strings.HasPrefix(r.Question[0].Name, "probe.test") {
			m := new(dns.Msg)
			m.SetReply(r)
			_ = w.WriteMsg(m)
			return
		}
		// Block briefly. Long enough that the 100 ms forwarder
		// deadline fires, short enough to not stretch the test.
		time.Sleep(400 * time.Millisecond)
	})
	defer stop()

	p := newProxyWithUpstream(t, addr, 100*time.Millisecond)
	q := dns.Question{Name: "does-not-exist.example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}

	start := time.Now()
	resp, err := p.forwardDNSUpstream(q)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatalf("expected timeout error, got resp=%v", resp)
	}
	// Give the system some slack — 500ms is a comfortable ceiling that
	// still catches regression to a multi-second wait.
	if elapsed > 500*time.Millisecond {
		t.Errorf("forwardDNSUpstream took %v; expected <500ms with 100ms timeout", elapsed)
	}
}

// TestServeDNS_LocalMockHit_NoForward asserts case #1 of the task's
// test matrix: when a DNS query matches a recorded mock we return the
// mocked answer and do NOT forward upstream. The fake upstream's
// handler panics on unexpected traffic — if the forwarder is invoked
// the test fails loudly.
func TestServeDNS_LocalMockHit_NoForward(t *testing.T) {
	addr, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		// Allow only the setup probe (see startFakeUpstream). Any
		// real traffic here is a bug — the local mock path must
		// short-circuit the forwarder.
		if len(r.Question) > 0 && strings.HasPrefix(r.Question[0].Name, "probe.test") {
			m := new(dns.Msg)
			m.SetReply(r)
			_ = w.WriteMsg(m)
			return
		}
		t.Errorf("upstream received query for %q; local-mock path should have answered without forwarding", r.Question[0].Name)
		m := new(dns.Msg)
		m.SetReply(r)
		m.Rcode = dns.RcodeServerFailure
		_ = w.WriteMsg(m)
	})
	defer stop()

	p := newProxyWithUpstream(t, addr, 2*time.Second)

	// Seed a MockManager with a single DNS mock for example.com. ->
	// 42.42.42.42. The exact structure mirrors a recorded mock so
	// getMockedDNSResponse accepts it. We feed the mock through
	// SetFilteredMocks — that path populates statelessFiltered,
	// which GetStatelessMocks keys on for DNS lookups.
	mgr := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mgr.Close)
	mgr.SetFilteredMocks([]*models.Mock{{
		Version: models.GetVersion(),
		Name:    "dns-mock-test",
		Kind:    models.DNS,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": "config"},
			DNSReq: &models.DNSReq{
				Name:   "example.com.",
				Qtype:  dns.TypeA,
				Qclass: dns.ClassINET,
			},
			DNSResp: &models.DNSResp{
				Rcode:              dns.RcodeSuccess,
				Authoritative:      true,
				RecursionAvailable: true,
				Answers:            []string{"example.com. 60 IN A 42.42.42.42"},
			},
		},
	}})
	p.mockManager = mgr

	q := dns.Question{Name: "example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	resp, mocked := p.getMockedDNSResponse(q)
	if !mocked {
		t.Fatalf("expected local mock hit, got miss")
	}
	if len(resp.Answer) != 1 {
		t.Fatalf("expected 1 answer from mock, got %d", len(resp.Answer))
	}
	a, ok := resp.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("answer not *dns.A: %T", resp.Answer[0])
	}
	if !a.A.Equal(net.ParseIP("42.42.42.42")) {
		t.Errorf("A = %v, want 42.42.42.42", a.A)
	}

	// Belt-and-braces: ensure the surrounding resolveUncachedDNSResponse
	// path also returns the mock without touching upstream. We run
	// through it with mockingEnabled=true and mode=MODE_TEST.
	entry := p.resolveUncachedDNSResponse(q, models.MODE_TEST, true, time.Now(), nil)
	if entry.Msg == nil || len(entry.Msg.Answer) != 1 {
		t.Fatalf("resolveUncachedDNSResponse did not return the mocked answer")
	}
	if got, _ := entry.Msg.Answer[0].(*dns.A); got == nil || !got.A.Equal(net.ParseIP("42.42.42.42")) {
		t.Errorf("resolveUncachedDNSResponse returned wrong answer: %+v", entry.Msg.Answer)
	}
}

// TestResolveUncachedDNSResponse_TestMode_UpstreamForwardSuccess
// validates case #2 of the test matrix: in MODE_TEST with a mock
// miss, the forwarder resolves the query via the fake upstream and
// the caller returns the real answer (not the 127.0.0.1 synthetic
// fallback). This is the direct fix for the sap-demo crashloop.
func TestResolveUncachedDNSResponse_TestMode_UpstreamForwardSuccess(t *testing.T) {
	addr, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Authoritative = true
		if len(r.Question) > 0 && r.Question[0].Qtype == dns.TypeA {
			m.Answer = append(m.Answer, &dns.A{
				Hdr: dns.RR_Header{
					Name:   r.Question[0].Name,
					Rrtype: dns.TypeA,
					Class:  dns.ClassINET,
					Ttl:    60,
				},
				A: net.ParseIP("10.0.0.7"),
			})
		}
		_ = w.WriteMsg(m)
	})
	defer stop()

	p := newProxyWithUpstream(t, addr, 2*time.Second)
	// Empty mock manager — every query misses locally.
	emptyMgr := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(emptyMgr.Close)
	p.mockManager = emptyMgr

	q := dns.Question{Name: "mysql.sap-demo.svc.cluster.local.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	entry := p.resolveUncachedDNSResponse(q, models.MODE_TEST, true, time.Now(), nil)

	if entry.Msg == nil {
		t.Fatalf("expected forwarded response, got nil Msg")
	}
	if !entry.FromUpstream {
		t.Errorf("entry.FromUpstream = false; expected true for forwarded answer")
	}
	if len(entry.Msg.Answer) != 1 {
		t.Fatalf("expected 1 answer from upstream, got %d", len(entry.Msg.Answer))
	}
	a, ok := entry.Msg.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("answer not *dns.A: %T", entry.Msg.Answer[0])
	}
	if !a.A.Equal(net.ParseIP("10.0.0.7")) {
		t.Errorf("A = %v, want 10.0.0.7 (from fake upstream, NOT 127.0.0.1 synthetic)", a.A)
	}
}

// TestResolveUncachedDNSResponse_TestMode_UpstreamTimeoutFallback
// validates case #3: when the upstream is unreachable the forwarder
// times out and the caller falls back to the legacy default response
// (which for TypeA is the proxy's 127.0.0.1). The fallback preserves
// pre-existing behaviour for app DNS mocks that never pointed at an
// in-cluster resolver in the first place.
func TestResolveUncachedDNSResponse_TestMode_UpstreamTimeoutFallback(t *testing.T) {
	// Point upstream at a discard address — UDP to a port nothing
	// listens on yields "connection refused" on Linux, which the
	// forwarder treats the same as a timeout for fallback purposes.
	// To make the test behaviour identical across OSes we use a
	// stalling real server and squeeze the timeout down.
	addr, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		if len(r.Question) > 0 && strings.HasPrefix(r.Question[0].Name, "probe.test") {
			m := new(dns.Msg)
			m.SetReply(r)
			_ = w.WriteMsg(m)
			return
		}
		// Stall just long enough to exercise the 80 ms forwarder
		// deadline; short enough that the test exits quickly.
		time.Sleep(400 * time.Millisecond)
	})
	defer stop()

	p := newProxyWithUpstream(t, addr, 80*time.Millisecond)
	emptyMgr := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(emptyMgr.Close)
	p.mockManager = emptyMgr

	q := dns.Question{Name: "unreachable.example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	entry := p.resolveUncachedDNSResponse(q, models.MODE_TEST, true, time.Now(), nil)

	// On fallback we expect the synthetic A response pointing at
	// p.IP4 (127.0.0.1). FromUpstream must be false because this is
	// the proxy default, not a real answer.
	if entry.Msg == nil {
		t.Fatalf("expected fallback response, got nil Msg")
	}
	if entry.FromUpstream {
		t.Errorf("entry.FromUpstream = true; expected false for fallback")
	}
	if len(entry.Msg.Answer) != 1 {
		t.Fatalf("expected 1 fallback A answer, got %d", len(entry.Msg.Answer))
	}
	a, ok := entry.Msg.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("fallback answer not *dns.A: %T", entry.Msg.Answer[0])
	}
	if !a.A.Equal(net.ParseIP("127.0.0.1")) {
		t.Errorf("fallback A = %v, want 127.0.0.1 (proxy IP4)", a.A)
	}
}

// TestResolveUncachedDNSResponse_TestMode_UpstreamNXDOMAIN_FallsBackToProxyIP
// is the regression test for issue #2006.
//
// Root cause (Fix B): in test mode, a mock-miss triggers forwardDNSUpstream.
// When the app runs in docker-compose the upstream is Docker's embedded DNS
// 127.0.0.11:53 (port 53 ≠ proxy's DNSPort 26789, so it passes the
// self-referential-loopback filter). For a bare service name like "localstack"
// Docker's DNS answers NXDOMAIN (Rcode=3). The old code returned ANY non-nil
// upstream response, so the NXDOMAIN was relayed straight to the app,
// bypassing defaultDNSResponse — the synthetic A→proxy-IP fallback that is
// supposed to catch every unknown name in test mode.
//
// After Fix B: forwardDNSUpstream's answer is only accepted when
// Rcode == RcodeSuccess AND len(Answer) > 0. A NXDOMAIN falls through to
// defaultDNSResponse, which returns the proxy IP so eBPF can intercept the
// TCP connection and match it against the recorded HTTP mocks.
func TestResolveUncachedDNSResponse_TestMode_UpstreamNXDOMAIN_FallsBackToProxyIP(t *testing.T) {
	// Fake upstream that always returns NXDOMAIN (Docker embedded DNS
	// behaviour for bare service names like "localstack" that have no
	// matching entry in the docker-compose network).
	addr, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Rcode = dns.RcodeNameError // NXDOMAIN
		// No Answer RRs — this is exactly what Docker's 127.0.0.11:53 sends.
		_ = w.WriteMsg(m)
	})
	defer stop()

	p := newProxyWithUpstream(t, addr, 2*time.Second)
	emptyMgr := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(emptyMgr.Close)
	p.mockManager = emptyMgr

	// "localstack" is the bare service name from the issue — no FQDN,
	// so Docker DNS returns NXDOMAIN for it.
	q := dns.Question{Name: "localstack.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	entry := p.resolveUncachedDNSResponse(q, models.MODE_TEST, true, time.Now(), nil)

	// The result MUST be the proxy's synthetic A record (127.0.0.1), not
	// NXDOMAIN. eBPF intercepts all TCP regardless of dest IP, so any
	// resolvable address lets the mock match.
	if entry.Msg == nil {
		t.Fatalf("expected synthetic fallback response, got nil Msg")
	}
	if entry.Msg.Rcode == dns.RcodeNameError {
		t.Errorf("upstream NXDOMAIN leaked to app (Rcode=%d); expected synthetic proxy-IP fallback (Rcode=0) — this is issue #2006", entry.Msg.Rcode)
	}
	if entry.Msg.Rcode != dns.RcodeSuccess {
		t.Errorf("Rcode = %d, want %d (RcodeSuccess)", entry.Msg.Rcode, dns.RcodeSuccess)
	}
	if len(entry.Msg.Answer) != 1 {
		t.Fatalf("expected 1 synthetic A answer, got %d answers", len(entry.Msg.Answer))
	}
	a, ok := entry.Msg.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("answer not *dns.A: %T", entry.Msg.Answer[0])
	}
	if !a.A.Equal(net.ParseIP("127.0.0.1")) {
		t.Errorf("synthetic A = %v, want 127.0.0.1 (proxy IP4, not NXDOMAIN)", a.A)
	}
	if entry.FromUpstream {
		t.Errorf("entry.FromUpstream = true; must be false for synthetic fallback")
	}
}

// TestResolveUncachedDNSResponse_TestMode_UpstreamNODATA_RelayedAsIs is the
// regression test for the NODATA relay fix. An AAAA query for an IPv4-only
// cluster service returns NOERROR with zero Answer RRs (NODATA) — a *valid*
// negative answer, not a resolution failure. Before the fix this fell through
// to ErrMockNotFound / the proxy-IP fallback, breaking replay for every app
// whose libc issues A+AAAA in parallel (glibc getaddrinfo). After the fix the
// empty NOERROR is relayed as-is, with mocking enabled, and the SOA in the
// authority section preserved.
func TestResolveUncachedDNSResponse_TestMode_UpstreamNODATA_RelayedAsIs(t *testing.T) {
	// Fake upstream: NOERROR + zero answers + an SOA in the authority section,
	// exactly what a resolver returns for an AAAA query on an IPv4-only name.
	addr, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Rcode = dns.RcodeSuccess // NOERROR
		// No Answer RRs — NODATA.
		soa, _ := dns.NewRR("cluster.local.\t30\tIN\tSOA\tns.cluster.local. hostmaster.cluster.local. 1 3600 600 86400 30")
		if soa != nil {
			m.Ns = []dns.RR{soa}
		}
		_ = w.WriteMsg(m)
	})
	defer stop()

	p := newProxyWithUpstream(t, addr, 2*time.Second)
	emptyMgr := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(emptyMgr.Close)
	p.mockManager = emptyMgr

	q := dns.Question{Name: "postgres.checkr.svc.cluster.local.", Qtype: dns.TypeAAAA, Qclass: dns.ClassINET}
	entry := p.resolveUncachedDNSResponse(q, models.MODE_TEST, true /*mockingEnabled*/, time.Now(), nil)

	if entry.Msg == nil {
		t.Fatal("expected the relayed NODATA response, got nil Msg (ErrMockNotFound path?)")
	}
	if entry.Msg.Rcode != dns.RcodeSuccess {
		t.Errorf("Rcode = %d, want %d (NOERROR relayed as-is, NOT steered to proxy IP)", entry.Msg.Rcode, dns.RcodeSuccess)
	}
	if len(entry.Msg.Answer) != 0 {
		t.Errorf("expected 0 answers (NODATA), got %d", len(entry.Msg.Answer))
	}
	if len(entry.Msg.Ns) == 0 {
		t.Error("SOA authority record was dropped; NODATA relay should preserve it")
	}
	if !entry.FromUpstream {
		t.Error("entry.FromUpstream = false; a relayed upstream NODATA must be cached as FromUpstream")
	}
}

// TestResolveUncachedDNSResponse_TestMode_UpstreamNODATA_AQuery_RelayedNotSteered
// pins the deliberate, qtype-agnostic scope of the NODATA relay. An A-query
// NODATA (name exists, no A record) is relayed as an honest empty answer rather
// than steered to the proxy IP (#2006). This is the one behavioral change to the
// steering path: previously a bare-name A that didn't resolve fell through to
// the synthetic proxy-IP A. The decision is that NODATA means the name
// authoritatively has no A record, so fabricating a proxy IP would be less
// correct than the truthful empty answer; genuinely-absent names still return
// NXDOMAIN and are still steered (covered by the NXDOMAIN test above).
func TestResolveUncachedDNSResponse_TestMode_UpstreamNODATA_AQuery_RelayedNotSteered(t *testing.T) {
	addr, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Rcode = dns.RcodeSuccess // NOERROR, zero answers -> NODATA on the A query
		_ = w.WriteMsg(m)
	})
	defer stop()

	p := newProxyWithUpstream(t, addr, 2*time.Second)
	emptyMgr := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(emptyMgr.Close)
	p.mockManager = emptyMgr

	q := dns.Question{Name: "localstack.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	entry := p.resolveUncachedDNSResponse(q, models.MODE_TEST, true /*mockingEnabled*/, time.Now(), nil)

	if entry.Msg == nil {
		t.Fatal("expected the relayed NODATA response, got nil Msg")
	}
	if entry.Msg.Rcode != dns.RcodeSuccess {
		t.Errorf("Rcode = %d, want RcodeSuccess (empty NOERROR relayed, not steered)", entry.Msg.Rcode)
	}
	if len(entry.Msg.Answer) != 0 {
		t.Fatalf("A-query NODATA must be relayed empty, not steered to a synthetic proxy-IP A; got %d answers: %v",
			len(entry.Msg.Answer), entry.Msg.Answer)
	}
	if !entry.FromUpstream {
		t.Error("relayed NODATA must be cached as FromUpstream")
	}
}

// TestGenerateCacheKey_QtypeIsolated locks the invariant that makes the
// qtype-agnostic NODATA relay safe: an AAAA NODATA cached under one key must not
// be served for a following A query on the same name. The cache key includes the
// qtype today; this guards it against a future cache-key refactor.
func TestGenerateCacheKey_QtypeIsolated(t *testing.T) {
	name := "postgres.checkr.svc.cluster.local"
	aKey := generateCacheKey(name, dns.TypeA)
	aaaaKey := generateCacheKey(name, dns.TypeAAAA)
	if aKey == aaaaKey {
		t.Fatalf("A and AAAA cache keys must differ (else an AAAA NODATA would be served for an A query); both = %q", aKey)
	}
}

// TestResolveUncachedDNSResponse_TestMode_UpstreamNODATA_DualEmpty documents the
// intended behavior for a name that returns NODATA on BOTH A and AAAA (unmocked):
// each leg is relayed as an honest empty answer rather than steered. The net app
// effect is EAI_NODATA (no address) — an accepted trade for the mocked-service
// case, since a genuinely-needed unmocked name resolves to NXDOMAIN (still
// steered), not NODATA.
func TestResolveUncachedDNSResponse_TestMode_UpstreamNODATA_DualEmpty(t *testing.T) {
	addr, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Rcode = dns.RcodeSuccess // NODATA for whatever qtype is asked
		_ = w.WriteMsg(m)
	})
	defer stop()

	p := newProxyWithUpstream(t, addr, 2*time.Second)
	emptyMgr := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(emptyMgr.Close)
	p.mockManager = emptyMgr

	for _, qtype := range []uint16{dns.TypeA, dns.TypeAAAA} {
		q := dns.Question{Name: "ipv-less.svc.", Qtype: qtype, Qclass: dns.ClassINET}
		entry := p.resolveUncachedDNSResponse(q, models.MODE_TEST, true, time.Now(), nil)
		if entry.Msg == nil || entry.Msg.Rcode != dns.RcodeSuccess || len(entry.Msg.Answer) != 0 {
			t.Fatalf("%s: expected relayed empty NOERROR, got %+v", dns.TypeToString[qtype], entry.Msg)
		}
		if !entry.FromUpstream {
			t.Errorf("%s: relayed NODATA must be FromUpstream", dns.TypeToString[qtype])
		}
	}
}

// TestResolveUncachedDNSResponse_TestMode_UpstreamSuccessPassesThrough
// confirms that a *valid* upstream answer (Rcode=0, with Answer RRs) for a
// real cluster-internal name still passes through unchanged after Fix B.
// This guards against an over-broad filter that would break the sap-demo
// use case (mysql.svc.cluster.local → real CoreDNS answer → real IP).
func TestResolveUncachedDNSResponse_TestMode_UpstreamSuccessPassesThrough(t *testing.T) {
	addr, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		m.Rcode = dns.RcodeSuccess
		if len(r.Question) > 0 {
			m.Answer = append(m.Answer, &dns.A{
				Hdr: dns.RR_Header{
					Name:   r.Question[0].Name,
					Rrtype: dns.TypeA,
					Class:  dns.ClassINET,
					Ttl:    30,
				},
				A: net.ParseIP("10.96.5.1"),
			})
		}
		_ = w.WriteMsg(m)
	})
	defer stop()

	p := newProxyWithUpstream(t, addr, 2*time.Second)
	emptyMgr := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(emptyMgr.Close)
	p.mockManager = emptyMgr

	q := dns.Question{Name: "mysql.sap-demo.svc.cluster.local.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	entry := p.resolveUncachedDNSResponse(q, models.MODE_TEST, true, time.Now(), nil)

	if entry.Msg == nil {
		t.Fatalf("expected forwarded response, got nil Msg")
	}
	if !entry.FromUpstream {
		t.Errorf("entry.FromUpstream = false; expected true for valid upstream answer")
	}
	if len(entry.Msg.Answer) != 1 {
		t.Fatalf("expected 1 upstream answer, got %d", len(entry.Msg.Answer))
	}
	a, ok := entry.Msg.Answer[0].(*dns.A)
	if !ok {
		t.Fatalf("answer not *dns.A: %T", entry.Msg.Answer[0])
	}
	if !a.A.Equal(net.ParseIP("10.96.5.1")) {
		t.Errorf("A = %v, want 10.96.5.1 (real cluster IP from upstream, not proxy IP)", a.A)
	}
}

// TestCaptureDNSUpstream_ReadsResolvConf verifies the startup
// capture reads a resolv.conf file and correctly filters
// self-referential loopback entries (loopback at the proxy's own DNS
// listen port). Uses a temp file + the package-level resolvConfPath
// hook.
func TestCaptureDNSUpstream_ReadsResolvConf(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resolv.conf")
	// resolv.conf has no port syntax — nameserver lines are IP-only
	// and the default port is 53. With p.DNSPort == 53, the 127.0.0.1
	// entry is self-referential and must be dropped.
	content := "nameserver 10.96.0.10\nnameserver 127.0.0.1\nsearch cluster.local\noptions ndots:5\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write temp resolv.conf: %v", err)
	}

	orig := resolvConfPath
	resolvConfPath = path
	defer func() { resolvConfPath = orig }()

	// Simulate the self-forward condition: proxy is listening on port
	// 53 (same as default resolv.conf port), so the loopback entry
	// would loop back into us.
	p := &Proxy{logger: zap.NewNop(), DNSPort: 53}
	p.captureDNSUpstream()

	if len(p.dnsUpstreamServers) != 1 {
		t.Fatalf("dnsUpstreamServers = %v; want exactly [10.96.0.10] after self-forward filter", p.dnsUpstreamServers)
	}
	if p.dnsUpstreamServers[0] != "10.96.0.10" {
		t.Errorf("dnsUpstreamServers[0] = %q; want 10.96.0.10", p.dnsUpstreamServers[0])
	}
	if p.dnsUpstreamPort != "53" {
		t.Errorf("dnsUpstreamPort = %q; want 53", p.dnsUpstreamPort)
	}
}

// TestCaptureDNSUpstream_KeepsNonSelfLoopback is the round-3 fix
// check: a resolv.conf that lists ONLY a loopback resolver (common in
// pods behind systemd-resolved at 127.0.0.53 or a local dnsmasq) must
// leave the upstream list non-empty so forward-on-miss still works.
// The previous filter dropped every IsLoopback() entry and silently
// disabled forwarding in those environments.
func TestCaptureDNSUpstream_KeepsNonSelfLoopback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resolv.conf")
	content := "nameserver 127.0.0.1\nsearch cluster.local\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write temp resolv.conf: %v", err)
	}

	orig := resolvConfPath
	resolvConfPath = path
	defer func() { resolvConfPath = orig }()

	// Proxy listens on 26789 — resolv.conf's implied port is 53, so
	// 127.0.0.1:53 is NOT our own listener and must be kept.
	p := &Proxy{logger: zap.NewNop(), DNSPort: 26789}
	p.captureDNSUpstream()

	if len(p.dnsUpstreamServers) != 1 {
		t.Fatalf("dnsUpstreamServers = %v; want [127.0.0.1] (loopback at non-self port must be kept)", p.dnsUpstreamServers)
	}
	if p.dnsUpstreamServers[0] != "127.0.0.1" {
		t.Errorf("dnsUpstreamServers[0] = %q; want 127.0.0.1", p.dnsUpstreamServers[0])
	}
	if !p.hasDNSUpstream() {
		t.Errorf("hasDNSUpstream() = false; expected true so forward-on-miss still fires in loopback-only resolv.conf environments")
	}
}

// TestCaptureDNSUpstream_DropsSelfReferentialLoopback is the explicit
// anti-loop guardrail: when resolv.conf's implied port (53) matches
// the proxy's own DNS listen port, the loopback entry IS our own
// listener and forwarding there would loop. Confirm that specific
// case is still filtered alongside a non-loopback entry.
func TestCaptureDNSUpstream_DropsSelfReferentialLoopback(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resolv.conf")
	content := "nameserver 10.0.0.53\nnameserver ::1\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write temp resolv.conf: %v", err)
	}

	orig := resolvConfPath
	resolvConfPath = path
	defer func() { resolvConfPath = orig }()

	// Proxy listens on 53 (the default resolv.conf port). ::1:53 is
	// self-referential and must be dropped; 10.0.0.53 must be kept.
	p := &Proxy{logger: zap.NewNop(), DNSPort: 53}
	p.captureDNSUpstream()

	if len(p.dnsUpstreamServers) != 1 {
		t.Fatalf("dnsUpstreamServers = %v; want [10.0.0.53] only (IPv6 loopback self-ref dropped)", p.dnsUpstreamServers)
	}
	if p.dnsUpstreamServers[0] != "10.0.0.53" {
		t.Errorf("dnsUpstreamServers[0] = %q; want 10.0.0.53", p.dnsUpstreamServers[0])
	}
}

// TestCaptureDNSUpstream_MissingFile asserts that a missing
// resolv.conf does NOT panic or error out; the agent must keep
// running with "no upstream" and fall back to synthetic responses.
func TestCaptureDNSUpstream_MissingFile(t *testing.T) {
	orig := resolvConfPath
	resolvConfPath = filepath.Join(t.TempDir(), "does-not-exist")
	defer func() { resolvConfPath = orig }()

	p := &Proxy{logger: zap.NewNop()}
	p.captureDNSUpstream()

	if p.hasDNSUpstream() {
		t.Errorf("hasDNSUpstream() = true; expected false when resolv.conf is missing")
	}
}

// TestIsForwardableQType covers the allowlist — ensures new DNS
// record types aren't silently added to forwarding without human
// review, and that the mandatory A/AAAA/SRV/PTR are all accepted.
func TestIsForwardableQType(t *testing.T) {
	cases := []struct {
		qtype uint16
		want  bool
	}{
		{dns.TypeA, true},
		{dns.TypeAAAA, true},
		{dns.TypeSRV, true},
		{dns.TypePTR, true},
		{dns.TypeCNAME, true},
		{dns.TypeMX, true},
		{dns.TypeTXT, true},
		{dns.TypeNS, true},
		{dns.TypeSOA, true},
		{dns.TypeCAA, true},
		{dns.TypeANY, true},

		{dns.TypeRRSIG, false},
		{dns.TypeDNSKEY, false},
		{dns.TypeOPT, false},
		{0, false},
	}
	for _, c := range cases {
		got := isForwardableQType(c.qtype)
		if got != c.want {
			t.Errorf("isForwardableQType(%s) = %v; want %v",
				dns.TypeToString[c.qtype], got, c.want)
		}
	}
}

// TestForwardDNSUpstream_NoUpstreamConfigured makes sure calling the
// forwarder on a Proxy with no captured upstream returns a clear
// error rather than dialing nothing.
func TestForwardDNSUpstream_NoUpstreamConfigured(t *testing.T) {
	p := &Proxy{
		logger:            zap.NewNop(),
		dnsForwardTimeout: 2 * time.Second,
	}
	q := dns.Question{Name: "example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	_, err := p.forwardDNSUpstream(q)
	if err == nil {
		t.Fatalf("expected error when no upstream configured, got nil")
	}
	if !strings.Contains(err.Error(), "no upstream") {
		t.Errorf("err = %v; want message containing 'no upstream'", err)
	}
}
