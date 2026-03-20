package proxy

import (
	"testing"

	"github.com/miekg/dns"
)

func TestGenerateDNSDedupeKey_SameQueryDifferentIPs(t *testing.T) {
	// Simulate the AWS SQS scenario: same DNS query, different answer IPs.
	// The dedup key must be identical regardless of the response IPs.
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
