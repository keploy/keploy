package proxy

import (
	"errors"
	"maps"
	"net/netip"
	"os"
	"path/filepath"
	"slices"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// withAdapters makes readDNSAdapters hand back adapters (or err) for the
// rest of the test, and counts how often it is asked.
func withAdapters(t *testing.T, adapters []dnsAdapter, err error) *int {
	t.Helper()
	asked := new(int)
	orig := readDNSAdapters
	readDNSAdapters = func() ([]dnsAdapter, error) {
		*asked++
		return adapters, err
	}
	t.Cleanup(func() { readDNSAdapters = orig })
	return asked
}

func server(t *testing.T, addr string, metric uint32) dnsAdapterServer {
	t.Helper()
	return dnsAdapterServer{addr: netip.MustParseAddr(addr), metric: metric}
}

// On Windows, which has no resolv.conf, the upstream is the DNS servers of
// the machine's adapters, on port 53, read where nothing names a file.
func TestOnWindowsTheUpstreamIsTheAdaptersDNSServers(t *testing.T) {
	asked := withAdapters(t, []dnsAdapter{
		{up: true, servers: []dnsAdapterServer{server(t, "10.8.0.1", 5)}},
	}, nil)

	config, adapter, source, err := upstreamConfig("windows")
	if err != nil {
		t.Fatalf("upstreamConfig(windows): %v", err)
	}
	if *asked != 1 {
		t.Fatalf("the adapters were read %d times, want once", *asked)
	}
	if !slices.Equal(config.Servers, []string{"10.8.0.1"}) || config.Port != "53" {
		t.Fatalf("servers %v port %q, want [10.8.0.1] on 53", config.Servers, config.Port)
	}
	if !maps.Equal(adapter, map[string]int{"10.8.0.1": 0}) {
		t.Fatalf("the adapters the servers are asked on: %v, want 10.8.0.1 on the first", adapter)
	}
	if source != "the DNS servers of the machine's network adapters" {
		t.Fatalf("source %q does not say the adapters were read", source)
	}
}

// Elsewhere the adapters are not read: resolv.conf is.
func TestOffWindowsTheUpstreamIsResolvConf(t *testing.T) {
	asked := withAdapters(t, nil, errors.New("not on this OS"))
	_, _, source, _ := upstreamConfig("linux")
	if *asked != 0 || source != defaultResolvConfPath {
		t.Fatalf("on linux: adapters read %d times, source %q; want resolv.conf alone", *asked, source)
	}
}

// A row that names its own resolv.conf reads it on Windows too.
func TestOnWindowsANamedResolvConfIsRead(t *testing.T) {
	asked := withAdapters(t, nil, errors.New("must not be read"))
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte("nameserver 192.0.2.53\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := resolvConfPath
	resolvConfPath = path
	t.Cleanup(func() { resolvConfPath = orig })

	config, adapter, source, err := upstreamConfig("windows")
	if err != nil || *asked != 0 || source != path || !slices.Equal(config.Servers, []string{"192.0.2.53"}) || adapter != nil {
		t.Fatalf("got %v on adapters %v from %q (err %v, adapters read %d times); want the named file's server, on no adapter", config, adapter, source, err, *asked)
	}
}

// When the adapters cannot be read, upstreamConfig says so and names where
// it looked; captureDNSUpstream then has no upstream.
func TestOnWindowsAnAdapterReadFailureIsSaid(t *testing.T) {
	withAdapters(t, []dnsAdapter{{up: true, servers: []dnsAdapterServer{server(t, "10.8.0.1", 1)}}}, errors.New("GetAdaptersAddresses: access denied"))
	config, adapter, source, err := upstreamConfig("windows")
	if err == nil || config != nil || adapter != nil || source != "the DNS servers of the machine's network adapters" {
		t.Fatalf("got config %v, source %q, err %v; want the failure and where it looked", config, source, err)
	}
}

// Windows asks the servers of the adapters that are up, the preferred one's
// first; an adapter that is down, the loopback, a site-local placeholder
// (fec0:0:0:ffff::1 to ::3) and an empty address are not asked, and a server
// two adapters share is asked once. A link-local server keeps its zone.
func TestTheResolversOfAdaptersAreTheOnesWindowsAsks(t *testing.T) {
	ll := netip.MustParseAddr("fe80::1").WithZone("12")
	adapters := []dnsAdapter{
		{up: true, servers: []dnsAdapterServer{
			server(t, "192.168.1.1", 25), server(t, "fec0:0:0:ffff::1", 25), server(t, "192.168.1.2", 25),
			{addr: ll, metric: 25}, {metric: 1}, server(t, "0.0.0.0", 1), server(t, "::", 1),
		}},
		{up: false, servers: []dnsAdapterServer{server(t, "10.0.0.53", 1)}},
		{up: true, loopback: true, servers: []dnsAdapterServer{server(t, "127.0.0.53", 1)}},
		{up: true, servers: []dnsAdapterServer{server(t, "10.8.0.1", 5), server(t, "192.168.1.2", 5)}},
	}
	want := []string{"10.8.0.1", "192.168.1.2", "192.168.1.1", "fe80::1%12"}
	got, adapter := resolversOfAdapters(adapters)
	if !slices.Equal(got, want) {
		t.Fatalf("resolversOfAdapters = %v, want %v", got, want)
	}
	// Each server is asked on its adapter (its index in the list); the one
	// two adapters share on the one that asks it first.
	wantAdapter := map[string]int{"10.8.0.1": 3, "192.168.1.2": 3, "192.168.1.1": 0, "fe80::1%12": 0}
	if !maps.Equal(adapter, wantAdapter) {
		t.Fatalf("the adapters the servers are asked on: %v, want %v", adapter, wantAdapter)
	}
	// Equal metrics keep the order Windows lists them in.
	same := []dnsAdapter{{up: true, servers: []dnsAdapterServer{server(t, "10.0.0.2", 7), server(t, "10.0.0.1", 7)}}}
	if got, _ := resolversOfAdapters(same); !slices.Equal(got, []string{"10.0.0.2", "10.0.0.1"}) {
		t.Fatalf("equal metrics: %v, want Windows's order [10.0.0.2 10.0.0.1]", got)
	}
}

func TestSiteLocalIsFec0Slash10Only(t *testing.T) {
	for addr, want := range map[string]bool{
		"fec0::1": true, "fec0:0:0:ffff::3": true, "feff::1": true,
		"fe80::1": false, "febf::1": false, "fe00::1": false, "ffc0::1": false,
		"::ffff:254.192.0.1": false, "254.192.0.1": false, "2001:db8::1": false,
	} {
		if got := isSiteLocal(netip.MustParseAddr(addr)); got != want {
			t.Errorf("isSiteLocal(%s) = %v, want %v", addr, got, want)
		}
	}
}

// adapterOf reads Windows's operational status and interface type.
func TestAnAdapterIsUpAndLoopbackAsWindowsSays(t *testing.T) {
	for _, c := range []struct {
		oper, ifType uint32
		want         dnsAdapter
	}{
		{1, 6, dnsAdapter{up: true}},
		{2, 6, dnsAdapter{}},
		{7, 71, dnsAdapter{}},
		{1, 24, dnsAdapter{up: true, loopback: true}},
		{2, 24, dnsAdapter{loopback: true}},
	} {
		if got := adapterOf(c.oper, c.ifType); got.up != c.want.up || got.loopback != c.want.loopback {
			t.Errorf("adapterOf(%d, %d) = %+v, want %+v", c.oper, c.ifType, got, c.want)
		}
	}
}

// serverOf reads an IPv4 server with the IPv4 metric, an IPv6 one with the
// IPv6 metric, keeps a link-local server's scope ID as its zone (not a
// global one's, nor a scope ID of 0), and no other family.
func TestAServerIsReadWithItsFamilysMetricAndZone(t *testing.T) {
	v6 := func(s string) [16]byte { return netip.MustParseAddr(s).As16() }
	for _, c := range []struct {
		family uint16
		v4     [4]byte
		v6     [16]byte
		scope  uint32
		want   string
		metric uint32
		ok     bool
	}{
		{2, [4]byte{10, 8, 0, 1}, v6("2001:db8::9"), 9, "10.8.0.1", 25, true},
		{23, [4]byte{10, 8, 0, 1}, v6("2001:db8::1"), 9, "2001:db8::1", 35, true},
		{23, [4]byte{}, v6("fe80::1"), 12, "fe80::1%12", 35, true},
		{23, [4]byte{}, v6("fe80::1"), 0, "fe80::1", 35, true},
		{17, [4]byte{10, 8, 0, 1}, v6("2001:db8::1"), 0, "", 0, false},
	} {
		got, ok := serverOf(c.family, c.v4, c.v6, c.scope, 25, 35)
		if ok != c.ok || (ok && (got.addr.String() != c.want || got.metric != c.metric)) {
			t.Errorf("serverOf(family %d) = %v %d %v, want %q %d %v", c.family, got.addr, got.metric, ok, c.want, c.metric, c.ok)
		}
	}
}

// captureDNSUpstream keeps a server with a zone (a link-local resolver),
// drops a line that is no address, and drops an IPv4-mapped loopback that
// points at the proxy's own DNS port as it drops 127.0.0.1.
func TestCaptureKeepsAZonedServerAndDropsAMappedSelfLoopback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	content := "nameserver fe80::1%eth0\nnameserver not-an-address\nnameserver ::ffff:127.0.0.1\nnameserver 10.96.0.10\n"
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	orig := resolvConfPath
	resolvConfPath = path
	t.Cleanup(func() { resolvConfPath = orig })

	p := &Proxy{logger: zap.NewNop(), DNSPort: 53}
	p.captureDNSUpstream()
	if want := []string{"fe80::1%eth0", "10.96.0.10"}; !slices.Equal(p.dnsUpstreamServers, want) {
		t.Fatalf("dnsUpstreamServers = %v, want %v", p.dnsUpstreamServers, want)
	}
}

// A resolver list that cannot be read is said, naming where keploy looked,
// and leaves no upstream.
func TestAnUnreadableResolverListIsSaid(t *testing.T) {
	orig := resolvConfPath
	resolvConfPath = filepath.Join(t.TempDir(), "missing-resolv.conf")
	t.Cleanup(func() { resolvConfPath = orig })

	core, logs := observer.New(zap.InfoLevel)
	p := &Proxy{logger: zap.New(core), DNSPort: 53}
	p.captureDNSUpstream()
	said := logs.FilterMessageSnippet("could not read the machine's DNS resolvers").All()
	if len(said) != 1 || said[0].ContextMap()["source"] != resolvConfPath || p.hasDNSUpstream() {
		t.Fatalf("said %d time(s) (%v), upstream %v; want it said once naming %s, and no upstream",
			len(said), logs.All(), p.dnsUpstreamServers, resolvConfPath)
	}
}
