package realdeps

import (
	"net/netip"
	"testing"

	"github.com/stretchr/testify/require"
)

func addrs(s ...string) []netip.Addr {
	out := make([]netip.Addr, len(s))
	for i, a := range s {
		out[i] = netip.MustParseAddr(a)
	}
	return out
}

func mustParse(t *testing.T, entries ...string) *List {
	t.Helper()
	l, err := Parse(entries)
	require.NoError(t, err)
	return l
}

func TestAnAddressOrContainerEntryMakesAConnectionRealAtOnce(t *testing.T) {
	d := Decider{List: mustParse(t, "localhost:5432", "container:redis"), Book: &Book{}}
	got := d.AtAccept(netip.MustParseAddrPort("[::1]:5432"), "")
	require.Equal(t, Decision{Verdict: Real, Label: "localhost:5432"}, got)
	got = d.AtAccept(netip.MustParseAddrPort("172.17.0.4:6379"), "redis")
	require.Equal(t, Decision{Verdict: Real, Label: "container:redis"}, got)
	got = d.AtAccept(netip.MustParseAddrPort("172.17.0.5:5432"), "postgres")
	require.Equal(t, Dependency, got.Verdict)
}

func TestTheOneNameKeploysDNSGaveAnAddressDecidesAtAccept(t *testing.T) {
	l := mustParse(t, "auth.example.com:8443")
	b := &Book{}
	b.Answered("Auth.Example.Com.", addrs("10.1.1.1"))
	d := Decider{List: l, Book: b}

	got := d.AtAccept(netip.MustParseAddrPort("10.1.1.1:8443"), "")
	require.Equal(t, Real, got.Verdict)
	require.Equal(t, "auth.example.com:8443", got.Label)
	require.NotNil(t, got.Check, "a connection made real by its name has its names checked")
	require.True(t, got.Check.Allows("auth.example.com", 0))
	require.False(t, got.Check.Allows("pay.example.com", 0))

	require.Equal(t, Dependency, d.AtAccept(netip.MustParseAddrPort("10.1.1.1:443"), "").Verdict, "another port")
	require.Equal(t, Dependency, d.AtAccept(netip.MustParseAddrPort("10.1.1.2:8443"), "").Verdict, "another address")
}

func TestAnAddressDNSGaveAnotherNameIsADependency(t *testing.T) {
	l := mustParse(t, "auth.example.com:443")
	b := &Book{}
	// The system resolver says auth.example.com is 10.1.1.1, but the app asked
	// keploy's DNS only for pay.example.com, which shares it.
	b.Resolved("auth.example.com", addrs("10.1.1.1"))
	b.Answered("pay.example.com", addrs("10.1.1.1"))
	got := Decider{List: l, Book: b}.AtAccept(netip.MustParseAddrPort("10.1.1.1:443"), "")
	require.Equal(t, Dependency, got.Verdict)
}

func TestAnAddressWithNoOrManyNamesWaitsForTheName(t *testing.T) {
	l := mustParse(t, "auth.example.com:443")

	// Only the system resolver (the hosts file) knows it: no name from DNS.
	b := &Book{}
	b.Resolved("auth.example.com", addrs("10.1.1.1"))
	got := Decider{List: l, Book: b}.AtAccept(netip.MustParseAddrPort("10.1.1.1:443"), "")
	require.Equal(t, Decision{Verdict: NeedName, Candidates: []string{"auth.example.com"}}, got)

	// A shared ingress: DNS gave two names for one address.
	b = &Book{}
	b.Answered("auth.example.com", addrs("10.1.1.1"))
	b.Answered("pay.example.com", addrs("10.1.1.1"))
	d := Decider{List: l, Book: b}
	got = d.AtAccept(netip.MustParseAddrPort("10.1.1.1:443"), "")
	require.Equal(t, NeedName, got.Verdict)

	// The name read decides: only auth.example.com is live.
	real := d.ByName(Name{Host: "AUTH.example.com", Via: "sni"}, 443)
	require.Equal(t, Real, real.Verdict)
	require.Equal(t, "auth.example.com:443", real.Label)
	require.Equal(t, Dependency, d.ByName(Name{Host: "pay.example.com", Via: "sni"}, 443).Verdict)
}

func TestACONNECTTargetIsReadAtItsOwnPort(t *testing.T) {
	l := mustParse(t, "egress.example.net:3128", "api.example.com:443")
	b := &Book{}
	b.Answered("egress.example.net", addrs("10.2.2.2"))
	b.Answered("proxy.example.net", addrs("10.2.2.2"))
	d := Decider{List: l, Book: b}
	require.Equal(t, NeedName, d.AtAccept(netip.MustParseAddrPort("10.2.2.2:3128"), "").Verdict)
	got := d.ByName(Name{Host: "api.example.com", Port: 443, Via: "connect"}, 3128)
	require.Equal(t, Real, got.Verdict)
	require.Equal(t, "api.example.com:443", got.Label)
	require.Equal(t, Dependency, d.ByName(Name{Host: "other.example.com", Port: 443, Via: "connect"}, 3128).Verdict,
		"listing an egress proxy makes nothing behind it live")
}

func TestAnUnlistedAddressIsADependencyWithoutReadingIt(t *testing.T) {
	d := Decider{List: mustParse(t, "auth.example.com:443"), Book: &Book{}}
	require.Equal(t, Dependency, d.AtAccept(netip.MustParseAddrPort("10.9.9.9:443"), "").Verdict)
	d = Decider{List: mustParse(t, "auth.example.com:443")}
	require.Equal(t, Dependency, d.AtAccept(netip.MustParseAddrPort("10.9.9.9:443"), "").Verdict, "no book")
}
