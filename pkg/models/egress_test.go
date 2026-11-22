package models

import (
	"slices"
	"strings"
	"testing"
)

func TestNormalizeAllowEgress(t *testing.T) {
	got, err := NormalizeAllowEgress([]string{" DNS ", "dns"})
	if err != nil {
		t.Fatalf("NormalizeAllowEgress: %v", err)
	}
	if !slices.Equal(got, []string{AllowEgressDNS}) {
		t.Fatalf("NormalizeAllowEgress = %v, want [%s]", got, AllowEgressDNS)
	}
	if !AllowsEgress(got, AllowEgressDNS) {
		t.Fatalf("AllowsEgress(%v, %q) = false", got, AllowEgressDNS)
	}
	if AllowsEgress(nil, AllowEgressDNS) {
		t.Fatalf("an empty list opens %q", AllowEgressDNS)
	}
}

// An entry keploy does not act on is refused, never kept: a list naming a
// destination keploy then does not open would read as an opt-in that works.
func TestNormalizeAllowEgress_RefusesAnEntryKeployDoesNotActOn(t *testing.T) {
	for _, e := range []string{"sqs.us-east-1.amazonaws.com:443", "0.0.0.0/0", "", "dns; rm -rf /"} {
		got, err := NormalizeAllowEgress([]string{"dns", e})
		if err == nil {
			t.Fatalf("NormalizeAllowEgress accepted %q (got %v)", e, got)
		}
		if !strings.Contains(err.Error(), AllowEgressDNS) {
			t.Fatalf("the refusal of %q does not name the supported entries: %v", e, err)
		}
	}
}
