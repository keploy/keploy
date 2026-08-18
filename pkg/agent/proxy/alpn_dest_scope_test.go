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
		// Ports outside the TCP range are NOT real ports. net.SplitHostPort
		// does no numeric validation and strconv.Atoi accepts negatives and
		// values past math.MaxUint32, so mock data can carry nonsense here;
		// it must come back as "unknown" (0), never as a usable port.
		{"host:4294968296", "https", "host", 0}, // 2^32 + 1000: would truncate to 1000
		{"host:-1", "https", "host", 0},         // negative: would wrap to 4294967295
		{"host:70000", "https", "host", 0},      // above 65535
		{"host:0", "https", "host", 0},          // port 0 is not dialable
		{"host:abc", "https", "host", 0},        // unparseable
		{"host:65535", "https", "host", 65535},  // upper bound stays valid
		{"host:1", "https", "host", 1},          // lower bound stays valid
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
		// An out-of-range port in the recorded authority must not alias a real
		// port: 2^32+1000 truncated to uint32 is 1000, so a width-unsafe
		// comparison would upgrade a connection to :1000 onto h2.
		{"overflowing port does not alias", "vault.svc:4294968296", "https", "", 1000, false},
		// ...and with nothing else known there is no discriminator left, so the
		// mock cannot claim the destination at all.
		{"overflowing port alone is not a match", "vault.svc:4294968296", "https", "", 8200, false},
		// A negative port wraps to a huge uint32; treating it as KNOWN would
		// reject a destination whose host genuinely matches, which violates the
		// "an unknown discriminator never rejects" contract.
		{"negative port falls back to host", "vault.svc:-1", "https", "vault.svc", 443, true},
		// Same for a port above 65535.
		{"port above 65535 falls back to host", "vault.svc:70000", "https", "vault.svc", 443, true},
		// An unusable port with no SNI leaves nothing to match on.
		{"unusable port and no SNI", "vault.svc:70000", "https", "", 443, false},
		// A garbage port must still not let a DIFFERENT host match.
		{"unusable port different host", "vaultA.svc:-1", "https", "vaultB.svc", 443, false},
	}
	for _, c := range cases {
		got := http2AuthorityMatchesDest(c.authority, c.scheme, c.sniHost, c.port)
		if got != c.want {
			t.Errorf("%s: http2AuthorityMatchesDest(%q,%q,%q,%d) = %v, want %v",
				c.name, c.authority, c.scheme, c.sniHost, c.port, got, c.want)
		}
	}
}

// TestHTTP2DestMatchesPortWidth pins the port comparison directly (bypassing
// the authority parser): aPort is an int fed from recorded mock data, and it
// must never be narrowed into the uint32 connection-port domain — 2^32+1000
// truncated to uint32 is 1000, and -1 becomes 4294967295.
func TestHTTP2DestMatchesPortWidth(t *testing.T) {
	cases := []struct {
		name    string
		aHost   string
		aPort   int
		sniHost string
		port    uint32
		want    bool
	}{
		{"2^32+1000 must not alias 1000", "", 4294968296, "", 1000, false},
		{"2^32 must not alias 0-port conn", "", 4294967296, "", 8200, false},
		{"-1 must not alias 4294967295", "", -1, "", 4294967295, false},
		{"-1 does not reject a matching host", "vault.svc", -1, "vault.svc", 443, true},
		{"out-of-range port, no host -> no discriminator", "", 70000, "", 70000, false},
		{"in-range port still matches", "vault.svc", 8200, "vault.svc", 8200, true},
	}
	for _, c := range cases {
		got := http2DestMatches(c.aHost, c.aPort, c.sniHost, c.port)
		if got != c.want {
			t.Errorf("%s: http2DestMatches(%q,%d,%q,%d) = %v, want %v",
				c.name, c.aHost, c.aPort, c.sniHost, c.port, got, c.want)
		}
	}
}
