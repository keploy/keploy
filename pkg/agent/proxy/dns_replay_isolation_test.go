package proxy

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
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
	if got := p.replayMissServers(true, "db."); !slices.Equal(got, []string{"127.0.0.11"}) {
		t.Fatalf("a replay miss for the service name db goes to %v, want only docker's 127.0.0.11", got)
	}
	if got := p.replayMissServers(true, askedName); got != nil {
		t.Fatalf("a replay miss for %s goes to %v; docker's resolver would recurse it to the host's, want none", askedName, got)
	}
}

// ownNameUpstream is a fake resolver that answers every A question with
// 10.0.0.7 and counts, per name, the times it was asked. It stands for docker's
// 127.0.0.11 or cluster DNS: a resolver inside the replay's network that
// answers every name, as those do by recursing to the host's resolvers for a
// name they do not own.
func ownNameUpstream(t *testing.T) (string, func(string) int32, func()) {
	t.Helper()
	var mu sync.Mutex
	asked := map[string]int32{}
	addr, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		if len(r.Question) > 0 {
			mu.Lock()
			asked[strings.ToLower(r.Question[0].Name)]++
			mu.Unlock()
			if r.Question[0].Qtype == dns.TypeA {
				m.Answer = append(m.Answer, &dns.A{
					Hdr: dns.RR_Header{Name: r.Question[0].Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 60},
					A:   net.ParseIP("10.0.0.7"),
				})
			}
		}
		_ = w.WriteMsg(m)
	})
	count := func(name string) int32 {
		mu.Lock()
		defer mu.Unlock()
		return asked[strings.ToLower(name)]
	}
	return addr, count, stop
}

// A container or cluster replay forwards a DNS mock miss to its in-network
// resolver only for a name that network owns. Docker's 127.0.0.11 and cluster
// DNS recurse every other name to the host's or public resolvers, so such a
// name gets keploy's own answer, exactly as in a native replay.
func TestReplayDNSMiss_InNetworkResolverGetsOnlyTheNetworksOwnNames(t *testing.T) {
	cases := []struct {
		name          string
		clusterDomain string
		forwarded     []string
		answered      []string
	}{
		{
			name:      "container network",
			forwarded: []string{"db.", "keploy-agent.", "Riso-App."},
			answered: []string{askedName, "sqs.us-east-1.amazonaws.com.", "db.keploy-network.",
				"db.corp.example.com.", "mysql.default.svc.cluster.local."},
		},
		{
			name:          "cluster network",
			clusterDomain: "cluster.local",
			forwarded: []string{"db.", "mysql.default.svc.cluster.local.", "MySQL.Default.SVC.Cluster.Local.",
				"sqs.us-east-1.amazonaws.com.default.svc.cluster.local.", "cluster.local."},
			answered: []string{askedName, "sqs.us-east-1.amazonaws.com.", "sqs.us-east-1.amazonaws.com.ec2.internal.",
				"cluster.local.evil.example.", "notcluster.local."},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			addr, asked, stop := ownNameUpstream(t)
			defer stop()
			p := hostResolverProxy(t, addr)
			p.errChannel = make(chan error, 64) // every answered name is a reported miss
			p.IsDocker = true
			p.dnsInNetworkServers = append([]string(nil), p.dnsUpstreamServers...)
			p.dnsClusterDomain = tc.clusterDomain

			for _, name := range tc.forwarded {
				q := dns.Question{Name: name, Qtype: dns.TypeA, Qclass: dns.ClassINET}
				before := asked(name) // the count ignores case, as DNS does
				entry := p.resolveUncachedDNSResponse(q, models.MODE_TEST, true, time.Now(), nil)
				if n := asked(name) - before; n != 1 {
					t.Errorf("%s, the network's own name, was forwarded %d time(s); want 1", name, n)
				}
				if !entry.FromUpstream || len(entry.Answer) != 1 {
					t.Errorf("%s: want the in-network resolver's answer, got FromUpstream=%v %v", name, entry.FromUpstream, entry.Msg)
				}
			}
			for _, name := range tc.answered {
				q := dns.Question{Name: name, Qtype: dns.TypeA, Qclass: dns.ClassINET}
				entry := p.resolveUncachedDNSResponse(q, models.MODE_TEST, true, time.Now(), nil)
				if n := asked(name); n != 0 {
					t.Errorf("%s was forwarded %d time(s) to a resolver that recurses it off the machine; want 0", name, n)
				}
				if entry.FromUpstream || entry.Msg == nil || len(entry.Answer) != 1 {
					t.Errorf("%s: want keploy's own one-record answer, got FromUpstream=%v %v", name, entry.FromUpstream, entry.Msg)
					continue
				}
				if a, ok := entry.Answer[0].(*dns.A); !ok || !a.A.Equal(net.ParseIP(p.IP4)) {
					t.Errorf("%s: answer = %v, want keploy's own address %s", name, entry.Answer[0], p.IP4)
				}
			}

			// The opt-in still forwards every name, to every resolver.
			p.allowEgressDNS = true
			q := dns.Question{Name: "opted-in.example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
			if entry := p.resolveUncachedDNSResponse(q, models.MODE_TEST, true, time.Now(), nil); asked(q.Name) != 1 || !entry.FromUpstream {
				t.Errorf("with test.allowEgress dns, %s was forwarded %d time(s) (FromUpstream=%v); want 1, true", q.Name, asked(q.Name), entry.FromUpstream)
			}
		})
	}
}

func TestClusterDomain(t *testing.T) {
	cases := []struct {
		search []string
		want   string
	}{
		{search: []string{"shop.svc.cluster.local", "svc.cluster.local", "cluster.local", "ec2.internal"}, want: "cluster.local"},
		{search: []string{"ec2.internal", "svc.corp.k8s."}, want: "corp.k8s"},
		{search: []string{"Prod.SVC.Example.Org"}, want: "example.org"},
		{search: []string{"ec2.internal", "corp.example.com"}, want: ""},
		{search: []string{"svc.", "x.svc."}, want: ""},
		{search: nil, want: ""},
	}
	for _, tc := range cases {
		if got := clusterDomain(tc.search); got != tc.want {
			t.Errorf("clusterDomain(%v) = %q, want %q", tc.search, got, tc.want)
		}
	}
}

// writeResolvConf points captureDNSUpstream at a resolv.conf with content.
func writeResolvConf(t *testing.T, content string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "resolv.conf")
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	orig := resolvConfPath
	resolvConfPath = path
	t.Cleanup(func() { resolvConfPath = orig })
}

const podResolvConf = "nameserver 10.96.0.10\nsearch shop.svc.cluster.local svc.cluster.local cluster.local ec2.internal\noptions ndots:5\n"

// An in-cluster agent (a container the kubelet started) reads the cluster
// domain from its pod's search list, and forwards only the cluster's names.
func TestCaptureDNSUpstream_InClusterAgentForwardsTheClustersNames(t *testing.T) {
	writeResolvConf(t, podResolvConf)
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")

	p := &Proxy{logger: zap.NewNop(), DNSPort: 26789, IsDocker: true, agentMode: models.MODE_TEST}
	p.captureDNSUpstream()

	if p.dnsClusterDomain != "cluster.local" {
		t.Fatalf("dnsClusterDomain = %q, want cluster.local", p.dnsClusterDomain)
	}
	if got := p.replayMissServers(true, "mysql.shop.svc.cluster.local."); !slices.Equal(got, []string{"10.96.0.10"}) {
		t.Fatalf("a miss for a cluster service goes to %v, want the cluster DNS", got)
	}
	if got := p.replayMissServers(true, "sqs.us-east-1.amazonaws.com."); got != nil {
		t.Fatalf("a miss for an outside name goes to %v; cluster DNS would recurse it, want none", got)
	}
}

// A native agent is not in-cluster even where KUBERNETES_SERVICE_HOST is set
// (a CI runner pod, a shell in a pod): its resolv.conf is the machine's, so no
// resolver in it is inside the replay, and no name is forwarded.
func TestCaptureDNSUpstream_NativeAgentInAPodIsNotInCluster(t *testing.T) {
	writeResolvConf(t, podResolvConf)
	t.Setenv("KUBERNETES_SERVICE_HOST", "10.96.0.1")

	p := &Proxy{logger: zap.NewNop(), DNSPort: 26789, IsDocker: false, agentMode: models.MODE_TEST}
	p.captureDNSUpstream()

	if p.dnsInNetworkServers != nil || p.dnsClusterDomain != "" {
		t.Fatalf("a native agent got in-network resolvers %v and cluster domain %q; want none", p.dnsInNetworkServers, p.dnsClusterDomain)
	}
	for _, name := range []string{"mysql.shop.svc.cluster.local.", "db."} {
		if got := p.replayMissServers(true, name); got != nil {
			t.Fatalf("a native replay's miss for %s goes to %v; want none", name, got)
		}
	}
}

// Loopback is never the replay's own network: a host-network agent whose
// resolv.conf names systemd-resolved's 127.0.0.53 must not forward there, for
// it recurses to the host's upstreams.
func TestNetworksOfLeavesOutLoopbackAndLinkLocal(t *testing.T) {
	addrs := []net.Addr{
		&net.IPNet{IP: net.ParseIP("127.0.0.1"), Mask: net.CIDRMask(8, 32)},
		&net.IPNet{IP: net.ParseIP("::1"), Mask: net.CIDRMask(128, 128)},
		&net.IPNet{IP: net.ParseIP("172.18.0.5"), Mask: net.CIDRMask(16, 32)},
		&net.IPNet{IP: net.ParseIP("fe80::1"), Mask: net.CIDRMask(64, 128)},
	}
	nets := networksOf(addrs)
	if len(nets) != 1 || !nets[0].IP.Equal(net.ParseIP("172.18.0.5")) {
		t.Fatalf("networksOf = %v, want only 172.18.0.5/16", nets)
	}
	got := inNetworkResolvers([]string{"127.0.0.53", "127.0.0.11", "172.18.0.1", "169.254.0.53"}, true, false, nets)
	if !slices.Equal(got, []string{"127.0.0.11", "172.18.0.1"}) {
		t.Fatalf("inNetworkResolvers = %v, want docker's 127.0.0.11 and the network's 172.18.0.1 only", got)
	}
}
