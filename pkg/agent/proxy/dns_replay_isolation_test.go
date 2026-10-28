package proxy

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// askedName is the name the tests below resolve; countingUpstream counts only
// it, not startFakeUpstream's own readiness probes.
const askedName = "api.example.com."

// countingUpstream is a fake resolver that answers every A question with
// 10.0.0.7 and counts the times it was asked askedName: the "real network" a
// replay must not reach unless a knob opens it.
func countingUpstream(t *testing.T) (string, *atomic.Int32, func()) {
	t.Helper()
	var asked atomic.Int32
	addr, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		if len(r.Question) > 0 && r.Question[0].Name == askedName {
			asked.Add(1)
		}
		m := new(dns.Msg)
		m.SetReply(r)
		if len(r.Question) > 0 && r.Question[0].Qtype == dns.TypeA {
			m.Answer = append(m.Answer, &dns.A{
				Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
				A:   net.ParseIP("10.0.0.7"),
			})
		}
		_ = w.WriteMsg(m)
	})
	return addr, &asked, stop
}

// hostResolverProxy is a replay proxy whose only upstream is the host's
// resolver: none of its resolvers is inside the replay's own network, as on a
// native replay.
func hostResolverProxy(t *testing.T, addr string) *Proxy {
	t.Helper()
	p := newProxyWithUpstream(t, addr, 2*time.Second)
	p.dnsInNetworkServers = nil
	p.errChannel = make(chan error, 4)
	mgr := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mgr.Close)
	p.mockManager = mgr
	return p
}

// A replay's DNS mock miss never reaches a resolver outside the replay's own
// network by default: the host's resolver is not asked, and the app gets
// keploy's own answer.
func TestReplayDNSMiss_HostResolverIsNotAskedByDefault(t *testing.T) {
	addr, asked, stop := countingUpstream(t)
	defer stop()
	p := hostResolverProxy(t, addr)

	q := dns.Question{Name: askedName, Qtype: dns.TypeA, Qclass: dns.ClassINET}
	entry := p.resolveUncachedDNSResponse(q, models.MODE_TEST, true, time.Now(), nil)

	if n := asked.Load(); n != 0 {
		t.Fatalf("the host's resolver was asked %d time(s) in a replay that opened no egress; want 0", n)
	}
	if entry.FromUpstream {
		t.Fatalf("the answer is marked FromUpstream, but no upstream may be asked")
	}
	if entry.Msg == nil || len(entry.Answer) != 1 {
		t.Fatalf("want keploy's own one-record answer, got %v", entry.Msg)
	}
	if a, ok := entry.Answer[0].(*dns.A); !ok || !a.A.Equal(net.ParseIP(p.IP4)) {
		t.Fatalf("answer = %v, want keploy's own address %s", entry.Answer[0], p.IP4)
	}
}

// Each knob that opens real egress sends the miss to the host's resolver, and
// the app gets the real answer.
func TestReplayDNSMiss_KnobsThatOpenEgressForwardToTheHostResolver(t *testing.T) {
	cases := []struct {
		name    string
		set     func(p *Proxy)
		mocking bool
	}{
		{name: "test.allowEgress dns", set: func(p *Proxy) { p.allowEgressDNS = true }, mocking: true},
		{name: "global passthrough", set: func(p *Proxy) { p.GlobalPassthrough = true }, mocking: true},
		{name: "mocking off", set: func(*Proxy) {}, mocking: false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, asked, stop := countingUpstream(t)
			defer stop()
			p := hostResolverProxy(t, addr)
			tc.set(p)

			q := dns.Question{Name: askedName, Qtype: dns.TypeA, Qclass: dns.ClassINET}
			entry := p.resolveUncachedDNSResponse(q, models.MODE_TEST, tc.mocking, time.Now(), nil)

			if n := asked.Load(); n != 1 {
				t.Fatalf("the host's resolver was asked %d time(s); want 1", n)
			}
			if !entry.FromUpstream || entry.Msg == nil || len(entry.Answer) != 1 {
				t.Fatalf("want the upstream's answer relayed, got FromUpstream=%v %v", entry.FromUpstream, entry.Msg)
			}
			if a, ok := entry.Answer[0].(*dns.A); !ok || !a.A.Equal(net.ParseIP("10.0.0.7")) {
				t.Fatalf("answer = %v, want the upstream's 10.0.0.7", entry.Answer[0])
			}
		})
	}
}

// mock.real names are an opt-in of their own and keep using every upstream.
func TestRealDNS_StillUsesEveryUpstream(t *testing.T) {
	addr, asked, stop := countingUpstream(t)
	defer stop()
	p := hostResolverProxy(t, addr)

	entry := p.realDNS(dns.Question{Name: askedName, Qtype: dns.TypeA, Qclass: dns.ClassINET})
	if n := asked.Load(); n != 1 || !entry.FromUpstream {
		t.Fatalf("a mock.real name asked the upstream %d time(s) (FromUpstream=%v); want 1, true", n, entry.FromUpstream)
	}
}

func TestInNetworkResolvers(t *testing.T) {
	_, composeNet, _ := net.ParseCIDR("172.18.0.0/16")
	local := []*net.IPNet{composeNet}
	servers := []string{"127.0.0.11", "172.18.0.1", "8.8.8.8", "10.96.0.10", "fd00::53"}

	cases := []struct {
		name              string
		isDocker, cluster bool
		want              []string
	}{
		{name: "native: the host's resolvers only", want: nil},
		{name: "container: docker's resolver and the network's own", isDocker: true, want: []string{"127.0.0.11", "172.18.0.1"}},
		{name: "in-cluster: the pod's resolvers are the cluster's", isDocker: true, cluster: true, want: servers},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := inNetworkResolvers(servers, tc.isDocker, tc.cluster, local)
			if !slices.Equal(got, tc.want) {
				t.Fatalf("inNetworkResolvers = %v, want %v", got, tc.want)
			}
		})
	}
}

// captureDNSUpstream sorts a container agent's resolv.conf: docker's embedded
// resolver is in the replay's network, a public resolver docker copied from
// the host is not.
func TestCaptureDNSUpstream_ContainerKeepsOnlyDockerResolverInNetwork(t *testing.T) {
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte("nameserver 127.0.0.11\nnameserver 8.8.8.8\n"), 0600); err != nil {
		t.Fatal(err)
	}
	orig := resolvConfPath
	resolvConfPath = path
	defer func() { resolvConfPath = orig }()
	t.Setenv("KUBERNETES_SERVICE_HOST", "")

	p := &Proxy{logger: zap.NewNop(), DNSPort: 26789, IsDocker: true, agentMode: models.MODE_TEST}
	p.captureDNSUpstream()

	if !slices.Equal(p.dnsUpstreamServers, []string{"127.0.0.11", "8.8.8.8"}) {
		t.Fatalf("dnsUpstreamServers = %v", p.dnsUpstreamServers)
	}
	if !slices.Equal(p.replayMissServers(true), []string{"127.0.0.11"}) {
		t.Fatalf("a replay miss goes to %v, want only docker's 127.0.0.11", p.replayMissServers(true))
	}
}
