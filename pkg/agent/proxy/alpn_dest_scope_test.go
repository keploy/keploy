package proxy

import "testing"

// TestHostPortFromAuthority covers the :authority / dst-URL parsing used to
// scope the replay PreferH2 decision to a destination.
func TestHostPortFromAuthority(t *testing.T) {
	cases := []struct {
		in, scheme string
		wantHost   string
		wantPort   int
	}{
		{"openbao.h2test.svc:8200", "https", "openbao.h2test.svc", 8200},
		{"10.0.8.29:8200", "", "10.0.8.29", 8200},
		{"api.example.com", "https", "api.example.com", 443},
		{"api.example.com", "http", "api.example.com", 80},
		{"api.example.com", "", "api.example.com", 443},
		{"https://api.example.com:9000/v1/x", "", "api.example.com", 9000},
		{"", "https", "", 0},
	}
	for _, c := range cases {
		h, p := hostPortFromAuthority(c.in, c.scheme)
		if h != c.wantHost || p != c.wantPort {
			t.Errorf("hostPortFromAuthority(%q, %q) = (%q, %d), want (%q, %d)",
				c.in, c.scheme, h, p, c.wantHost, c.wantPort)
		}
	}
}

// TestHTTP2AuthorityMatchesDest is the mixed-session guard: an Http2 mock for
// one destination must NOT cause a different destination (different port or
// host) to be pushed onto h2.
func TestHTTP2AuthorityMatchesDest(t *testing.T) {
	cases := []struct {
		name              string
		authority, scheme string
		sniHost           string
		port              uint32
		want              bool
	}{
		// Same host+port -> match.
		{"same host+port", "vault.svc:8200", "https", "vault.svc", 8200, true},
		// Same port, SNI absent -> match on port alone (best effort).
		{"port match, no SNI", "vault.svc:8200", "https", "", 8200, true},
		// IP authority, SNI absent, port matches -> match.
		{"ip authority port match", "10.0.8.29:8200", "", "", 8200, true},
		// DIFFERENT PORT -> reject (the core mixed-session fix: an h2 mock on
		// :8200 must not upgrade an http/1.1 connection to :443).
		{"different port", "vault.svc:8200", "https", "vault.svc", 443, false},
		// Same port, DIFFERENT host (SNI disambiguates same-port services).
		{"same port different host", "vaultA.svc:443", "https", "vaultB.svc", 443, false},
		// Same host+port via scheme-inferred port.
		{"inferred https port", "api.example.com", "https", "api.example.com", 443, true},
		// Host matches, port unknown on mock side but inferred -> still checks.
		{"host match inferred port mismatch", "api.example.com", "http", "api.example.com", 443, false},
	}
	for _, c := range cases {
		got := http2AuthorityMatchesDest(c.authority, c.scheme, c.sniHost, c.port)
		if got != c.want {
			t.Errorf("%s: http2AuthorityMatchesDest(%q,%q,%q,%d) = %v, want %v",
				c.name, c.authority, c.scheme, c.sniHost, c.port, got, c.want)
		}
	}
}
