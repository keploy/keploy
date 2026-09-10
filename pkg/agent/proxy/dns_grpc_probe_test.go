package proxy

import (
	"testing"

	"github.com/miekg/dns"
)

// TestIsGrpcServiceConfigProbe pins which DNS queries are exempt from
// mock-miss reporting.
//
// grpc-go's default resolver probes _grpc_config.<target> for a service-config
// TXT record on EVERY target, and the absence of that record is the normal
// answer — "no service config, use defaults". Reporting it as a missing mock
// fails a replay that is behaving correctly: the first run of the integrations
// gRPC e2e lane failed every test case on
// "Mock mismatch: [DNS] TXT _grpc_config.upstream" without reaching a single
// gRPC assertion.
//
// The exemption is deliberately narrow. A missing TXT mock for any other name
// is still a real miss — an application that reads its own TXT records would
// otherwise be silently served an empty answer.
func TestIsGrpcServiceConfigProbe(t *testing.T) {
	cases := []struct {
		name  string
		qname string
		qtype uint16
		want  bool
		why   string
	}{
		{"grpc_config_probe", "_grpc_config.upstream.", dns.TypeTXT, true,
			"grpc-go's service-config lookup; absence is the normal answer"},
		{"grpc_config_with_search_domain", "_grpc_config.upstream.local.", dns.TypeTXT, true,
			"resolv.conf search domains produce this variant too"},
		{"grpc_config_uppercase", "_GRPC_CONFIG.Upstream.", dns.TypeTXT, true,
			"DNS names are case-insensitive"},
		{"ordinary_txt", "example.com.", dns.TypeTXT, false,
			"an app reading its own TXT records must still see a real miss"},
		{"spf_txt", "_spf.example.com.", dns.TypeTXT, false,
			"other underscore-prefixed TXT names are not grpc's"},
		{"grpc_config_but_A", "_grpc_config.upstream.", dns.TypeA, false,
			"only the TXT probe is exempt; an A lookup is a real dependency"},
		{"srv_probe", "_grpclb._tcp.upstream.", dns.TypeSRV, false,
			"SRV is a different query and not covered here"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := isGrpcServiceConfigProbe(dns.Question{Name: tc.qname, Qtype: tc.qtype})
			if got != tc.want {
				t.Errorf("isGrpcServiceConfigProbe(%q, %s) = %v, want %v — %s",
					tc.qname, dns.TypeToString[tc.qtype], got, tc.want, tc.why)
			}
		})
	}
}
