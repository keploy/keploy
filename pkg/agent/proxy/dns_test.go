package proxy

import (
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
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

func TestRecordDNSMock_RecordsNegativeResponses(t *testing.T) {
	origConfigReader := dnsClientConfigFromFile
	origExchange := dnsExchange
	t.Cleanup(func() {
		dnsClientConfigFromFile = origConfigReader
		dnsExchange = origExchange
	})

	dnsClientConfigFromFile = func(string) (*dns.ClientConfig, error) {
		return &dns.ClientConfig{
			Servers: []string{"127.0.0.1"},
			Port:    "53",
		}, nil
	}

	dnsExchange = func(_ *dns.Client, _ *dns.Msg, _ string) (*dns.Msg, time.Duration, error) {
		return &dns.Msg{
			MsgHdr: dns.MsgHdr{
				Rcode:              dns.RcodeNameError,
				Authoritative:      true,
				RecursionAvailable: true,
			},
		}, 0, nil
	}

	p := &Proxy{
		logger:           testLogger(),
		recordedDNSMocks: newRecordedDNSMocksCache(),
	}

	mocks := make(chan *models.Mock, 1)
	session := &agent.Session{MC: mocks}
	question := dns.Question{
		Name:   "db.example.internal.",
		Qtype:  dns.TypeA,
		Qclass: dns.ClassINET,
	}

	resp, err := p.recordDNSMock(question, time.Unix(1, 0).UTC(), session)
	if err != nil {
		t.Fatalf("recordDNSMock returned error: %v", err)
	}
	if resp.Rcode != dns.RcodeNameError {
		t.Fatalf("recordDNSMock rcode = %d, want %d", resp.Rcode, dns.RcodeNameError)
	}

	select {
	case mock := <-mocks:
		if mock.Spec.DNSResp == nil {
			t.Fatal("recorded mock is missing DNS response")
		}
		if mock.Spec.DNSResp.Rcode != dns.RcodeNameError {
			t.Fatalf("recorded DNS rcode = %d, want %d", mock.Spec.DNSResp.Rcode, dns.RcodeNameError)
		}
		if mock.Spec.DNSReq == nil {
			t.Fatal("recorded mock is missing DNS request")
		}
		if mock.Spec.DNSReq.Name != question.Name {
			t.Fatalf("recorded DNS name = %q, want %q", mock.Spec.DNSReq.Name, question.Name)
		}
	default:
		t.Fatal("expected negative DNS response to be recorded")
	}
}

func TestGetMockedDNSResponse_ReplaysNegativeResponses(t *testing.T) {
	question := dns.Question{
		Name:   "db.example.internal.",
		Qtype:  dns.TypeA,
		Qclass: dns.ClassINET,
	}

	p := &Proxy{logger: testLogger()}
	mgr := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), testLogger())
	mgr.SetFilteredMocks([]*models.Mock{{
		Name: "mocks",
		Kind: models.DNS,
		Spec: models.MockSpec{
			DNSReq: &models.DNSReq{
				Name:   question.Name,
				Qtype:  question.Qtype,
				Qclass: question.Qclass,
			},
			DNSResp: &models.DNSResp{
				Rcode:              dns.RcodeNameError,
				Authoritative:      true,
				RecursionAvailable: true,
			},
		},
	}})
	p.setMockManager(mgr)

	resp, ok := p.getMockedDNSResponse(question)
	if !ok {
		t.Fatal("expected mocked DNS response to be found")
	}
	if resp.Rcode != dns.RcodeNameError {
		t.Fatalf("mocked DNS rcode = %d, want %d", resp.Rcode, dns.RcodeNameError)
	}
	if len(resp.Answer) != 0 {
		t.Fatalf("mocked DNS answers = %d, want 0", len(resp.Answer))
	}
}
