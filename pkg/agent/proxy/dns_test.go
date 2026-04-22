package proxy

import (
	"net"
	"testing"

	"github.com/miekg/dns"
	"go.uber.org/zap"
)

func TestGenerateDNSDedupeKey_SameQuerySameKey(t *testing.T) {
	// The dedup key is derived solely from the question fields, so
	// identical questions must always produce identical keys.
	question := dns.Question{
		Name:   "sqs.us-east-1.amazonaws.com.",
		Qtype:  dns.TypeA,
		Qclass: dns.ClassINET,
	}

	key1 := generateDNSDedupeKey(question)
	key2 := generateDNSDedupeKey(question)

	if key1 != key2 {
		t.Errorf("same query must produce same dedup key, got %q vs %q", key1, key2)
	}
}

func TestGenerateDNSDedupeKey_DifferentQueryTypes(t *testing.T) {
	qA := dns.Question{Name: "example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	qAAAA := dns.Question{Name: "example.com.", Qtype: dns.TypeAAAA, Qclass: dns.ClassINET}

	if generateDNSDedupeKey(qA) == generateDNSDedupeKey(qAAAA) {
		t.Error("different query types must produce different dedup keys")
	}
}

func TestGenerateDNSDedupeKey_DifferentNames(t *testing.T) {
	q1 := dns.Question{Name: "sqs.us-east-1.amazonaws.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	q2 := dns.Question{Name: "sqs.us-west-2.amazonaws.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}

	if generateDNSDedupeKey(q1) == generateDNSDedupeKey(q2) {
		t.Error("different domain names must produce different dedup keys")
	}
}

func TestGenerateDNSDedupeKey_NormalizesName(t *testing.T) {
	// Without trailing dot — generateDNSDedupeKey uses dns.Fqdn which adds it.
	q1 := dns.Question{Name: "example.com", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	q2 := dns.Question{Name: "example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}

	if generateDNSDedupeKey(q1) != generateDNSDedupeKey(q2) {
		t.Error("FQDN normalization should make these keys equal")
	}
}

func TestGenerateDNSDedupeKey_CaseInsensitive(t *testing.T) {
	// DNS names are case-insensitive per RFC 4343.
	q1 := dns.Question{Name: "Example.COM.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	q2 := dns.Question{Name: "example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}

	if generateDNSDedupeKey(q1) != generateDNSDedupeKey(q2) {
		t.Error("DNS dedup key must be case-insensitive")
	}
}

// newTestProxyForDNS builds the minimum Proxy needed to exercise
// defaultDNSResponse's AAAA branch. The synthesiser only reads
// EnableIPv6Redirect, IP4, IP6 and the logger.
func newTestProxyForDNS(enableIPv6Redirect bool) *Proxy {
	return &Proxy{
		logger:             zap.NewNop(),
		IP4:                "127.0.0.1",
		IP6:                "::1",
		EnableIPv6Redirect: enableIPv6Redirect,
	}
}

func TestDefaultDNSResponse_AAAA_SynthesisWhenEnabled(t *testing.T) {
	p := newTestProxyForDNS(true)

	question := dns.Question{
		Name:   "localhost.",
		Qtype:  dns.TypeAAAA,
		Qclass: dns.ClassINET,
	}

	entry := p.defaultDNSResponse(question)

	if len(entry.Answer) != 1 {
		t.Fatalf("expected 1 AAAA answer when EnableIPv6Redirect=true, got %d", len(entry.Answer))
	}
	aaaa, ok := entry.Answer[0].(*dns.AAAA)
	if !ok {
		t.Fatalf("expected *dns.AAAA answer, got %T", entry.Answer[0])
	}
	want := net.ParseIP("::1")
	if !aaaa.AAAA.Equal(want) {
		t.Errorf("AAAA answer = %v, want %v", aaaa.AAAA, want)
	}
	if aaaa.Hdr.Rrtype != dns.TypeAAAA {
		t.Errorf("Hdr.Rrtype = %d, want %d (AAAA)", aaaa.Hdr.Rrtype, dns.TypeAAAA)
	}
	if entry.Rcode != dns.RcodeSuccess {
		t.Errorf("Rcode = %d, want %d (NOERROR)", entry.Rcode, dns.RcodeSuccess)
	}
}

func TestDefaultDNSResponse_AAAA_EmptyWhenDisabled(t *testing.T) {
	// Compatibility fallback: with the flag disabled we must not
	// synthesise ::1, otherwise v4-only deployments would steer clients
	// toward an unreachable IPv6 destination.
	p := newTestProxyForDNS(false)

	question := dns.Question{
		Name:   "localhost.",
		Qtype:  dns.TypeAAAA,
		Qclass: dns.ClassINET,
	}

	entry := p.defaultDNSResponse(question)

	if len(entry.Answer) != 0 {
		t.Fatalf("expected empty AAAA answer when EnableIPv6Redirect=false, got %d answers: %v", len(entry.Answer), entry.Answer)
	}
	if entry.Rcode != dns.RcodeSuccess {
		// Still NOERROR with no records — matches the pre-existing behaviour.
		t.Errorf("Rcode = %d, want %d (NOERROR)", entry.Rcode, dns.RcodeSuccess)
	}
}

func TestDefaultDNSResponse_A_AlwaysSynthesised(t *testing.T) {
	// Sanity check that the TypeA path is unchanged by the flag — it
	// should always return the proxy's v4 address.
	for _, enable := range []bool{true, false} {
		p := newTestProxyForDNS(enable)
		entry := p.defaultDNSResponse(dns.Question{
			Name:   "localhost.",
			Qtype:  dns.TypeA,
			Qclass: dns.ClassINET,
		})
		if len(entry.Answer) != 1 {
			t.Fatalf("[enable=%v] expected 1 A answer, got %d", enable, len(entry.Answer))
		}
		a, ok := entry.Answer[0].(*dns.A)
		if !ok {
			t.Fatalf("[enable=%v] expected *dns.A, got %T", enable, entry.Answer[0])
		}
		if !a.A.Equal(net.ParseIP("127.0.0.1")) {
			t.Errorf("[enable=%v] A = %v, want 127.0.0.1", enable, a.A)
		}
	}
}
