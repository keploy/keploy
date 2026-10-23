package proxy

import (
	"net"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// rrType is the record type of an address written as text.
func rrType(addr string) string {
	if strings.Contains(addr, ":") {
		return "AAAA"
	}
	return "A"
}

// liveProxy is a proxy in a replay whose DNS answers each name of recorded
// from a mock (an A or AAAA record per address), and forwards what no mock
// answers to an upstream resolver that answers a name of now with its
// records now, whatever the question's type (a test that wants the type
// respected lists only records of it).
func liveProxy(t *testing.T, recorded map[string][]string, now map[string][]string) *Proxy {
	t.Helper()
	upstream, stop := startFakeUpstream(t, func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		if len(r.Question) > 0 {
			q := r.Question[0]
			addrs, known := now[q.Name]
			for _, a := range addrs {
				rr, err := dns.NewRR(q.Name + " 60 IN " + rrType(a) + " " + a)
				if err != nil {
					t.Error(err)
					continue
				}
				m.Answer = append(m.Answer, rr)
			}
			if !known && !strings.HasPrefix(q.Name, "probe.test") {
				m.Rcode = dns.RcodeNameError
			}
		}
		_ = w.WriteMsg(m)
	})
	t.Cleanup(stop)
	p := newProxyWithUpstream(t, upstream, 2*time.Second)
	var mocks []*models.Mock
	for name, addrs := range recorded {
		byType := map[string][]string{}
		for _, a := range addrs {
			byType[rrType(a)] = append(byType[rrType(a)], name+" 60 IN "+rrType(a)+" "+a)
		}
		for typ, answers := range byType {
			mocks = append(mocks, &models.Mock{
				Name: "dns-" + typ + "-" + name,
				Kind: models.DNS,
				Spec: models.MockSpec{
					Metadata: map[string]string{"type": "config"},
					DNSReq:   &models.DNSReq{Name: name, Qtype: dns.StringToType[typ], Qclass: dns.ClassINET},
					DNSResp:  &models.DNSResp{Rcode: dns.RcodeSuccess, Answers: answers},
				},
			})
		}
	}
	mgr := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mgr.Close)
	mgr.SetFilteredMocks(mocks)
	p.mockManager = mgr
	p.setSession(&agent.Session{Mode: models.MODE_TEST, OutgoingOptions: models.OutgoingOptions{Mocking: true}})
	// What InitIntegrations' installRedisSkip says in a build without a
	// Redis parser.
	p.redisSkipOn.Store(true)
	return p
}

// answerWriter keeps the answer ServeDNS writes; it is a TCP client's, so
// nothing is truncated.
type answerWriter struct{ msg *dns.Msg }

func (w *answerWriter) LocalAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 53}
}
func (w *answerWriter) RemoteAddr() net.Addr {
	return &net.TCPAddr{IP: net.IPv4(127, 0, 0, 1), Port: 5353}
}
func (w *answerWriter) WriteMsg(m *dns.Msg) error   { w.msg = m; return nil }
func (w *answerWriter) Write(b []byte) (int, error) { return len(b), nil }
func (w *answerWriter) Close() error                { return nil }
func (w *answerWriter) TsigStatus() error           { return nil }
func (w *answerWriter) TsigTimersOnly(bool)         {}
func (w *answerWriter) Hijack()                     {}

// lookup is the application's lookup of name (A, or AAAA when v6) in the
// replay, through keploy's DNS server, its cache included.
func lookup(t *testing.T, p *Proxy, name string, v6 bool) []string {
	t.Helper()
	qtype := dns.TypeA
	if v6 {
		qtype = dns.TypeAAAA
	}
	q := new(dns.Msg)
	q.SetQuestion(name, qtype)
	w := &answerWriter{}
	p.ServeDNS(w, q)
	var out []string
	for _, a := range answerAddrs(w.msg) {
		out = append(out, a.String())
	}
	return out
}

// An address keploy's DNS answered from a recorded mock is dialled where the
// name is now when the name no longer has it, and as the application dialled
// it when the name still has it, among others or alone (one node of a name
// with many is not another). An address of the same family is chosen; a
// dual-stack client's IPv4-mapped destination is read as the IPv4 address it
// reaches; an address no mock gave, or whose name no longer resolves, is
// dialled as it is.
func TestLiveAddrDialsTheNamesAddressOnlyWhenTheRecordedOneIsGone(t *testing.T) {
	p := liveProxy(t,
		map[string][]string{
			"moved.test.": {"10.0.0.2"},
			"kept.test.":  {"10.0.0.5"},
			"gone.test.":  {"10.0.0.9"},
			"mixed.test.": {"10.0.0.12"},
			"v6.test.":    {"fd00::2"},
		},
		map[string][]string{
			"moved.test.": {"10.0.0.3", "10.0.0.4"},
			"kept.test.":  {"10.0.0.6", "10.0.0.5"},
			// An answer whose first address is of the other family.
			"mixed.test.": {"fd00::9", "10.0.0.11"},
			"v6.test.":    {"10.0.0.20", "fd00::3"},
		})
	for _, name := range []string{"moved.test.", "kept.test.", "gone.test.", "mixed.test."} {
		if got := lookup(t, p, name, false); len(got) != 1 {
			t.Fatalf("the replay's DNS answered %s with %v, want its one recorded address", name, got)
		}
	}
	if got := lookup(t, p, "v6.test.", true); len(got) != 1 || got[0] != "fd00::2" {
		t.Fatalf("the replay's DNS answered v6.test. AAAA with %v, want its recorded fd00::2", got)
	}
	cases := []struct{ dst, want, why string }{
		{"10.0.0.2:6379", "10.0.0.3:6379", "the name no longer has the recorded address"},
		{"[10.0.0.2]:6379", "10.0.0.3:6379", "a dual-stack client's destination, the name moved"},
		{"[::ffff:10.0.0.2]:6379", "10.0.0.3:6379", "an IPv4-mapped destination, the name moved"},
		{"10.0.0.5:6380", "10.0.0.5:6380", "the name still has the recorded address, not first"},
		{"10.0.0.12:6379", "10.0.0.11:6379", "the name moved; the first address now is IPv6"},
		{"[fd00::2]:6379", "[fd00::3]:6379", "an IPv6 name moved; the first address now is IPv4"},
		{"10.0.0.9:6379", "10.0.0.9:6379", "the name does not resolve now"},
		{"192.0.2.1:6379", "192.0.2.1:6379", "no mock gave the address"},
		{"not an address", "not an address", "a destination that is no address"},
	}
	for _, c := range cases {
		if got := p.liveAddr(c.dst); got != c.want {
			t.Errorf("%s: %s is dialled at %s, want %s", c.why, c.dst, got, c.want)
		}
	}
}

// Whose an address is follows the latest answer keploy's DNS handed out that
// gave it, from its cache too: here a.test.'s recording gives 10.0.0.3, which
// is b.test.'s now (no mock), and a.test. is at 10.0.0.7 now. After b.test.
// is looked up last, a connection to 10.0.0.3 is b.test.'s, dialled as it
// is; after a.test. is, it is a.test.'s, dialled at 10.0.0.7.
func TestLiveAddrFollowsTheLatestAnswerHandedOut(t *testing.T) {
	p := liveProxy(t,
		map[string][]string{"a.test.": {"10.0.0.3"}},
		map[string][]string{"a.test.": {"10.0.0.7"}, "b.test.": {"10.0.0.3"}})
	for round := 0; round < 2; round++ { // the second round's answers come from the cache
		if got := lookup(t, p, "a.test.", false); len(got) != 1 || got[0] != "10.0.0.3" {
			t.Fatalf("round %d: a.test. was answered %v, want its recorded 10.0.0.3", round, got)
		}
		if got := p.liveAddr("10.0.0.3:6379"); got != "10.0.0.7:6379" {
			t.Errorf("round %d: after a.test. was answered, 10.0.0.3 is dialled at %s, want a.test.'s address now", round, got)
		}
		if got := lookup(t, p, "b.test.", false); len(got) != 1 || got[0] != "10.0.0.3" {
			t.Fatalf("round %d: b.test. was answered %v, want 10.0.0.3", round, got)
		}
		if got := p.liveAddr("10.0.0.3:6379"); got != "10.0.0.3:6379" {
			t.Errorf("round %d: after b.test. was answered for real, 10.0.0.3 is dialled at %s, want as it is", round, got)
		}
	}
	if _, ok := p.dnsAnswered.name(netip.MustParseAddr("10.0.0.3")); ok {
		t.Error("an address a real answer gave last is still taken for a mocked name's")
	}
}

// A build that links a Redis parser (Keploy Enterprise's) installs no Redis
// skip, and its DNS notes nothing of what it answers: nothing would read it.
func TestDNSNotesNothingWithoutTheRedisSkip(t *testing.T) {
	p := liveProxy(t, map[string][]string{"a.test.": {"10.0.0.3"}}, map[string][]string{"a.test.": {"10.0.0.7"}})
	p.redisSkipOn.Store(false)
	if got := lookup(t, p, "a.test.", false); len(got) != 1 || got[0] != "10.0.0.3" {
		t.Fatalf("a.test. was answered %v, want its recorded 10.0.0.3", got)
	}
	if n, ok := p.dnsAnswered.name(netip.MustParseAddr("10.0.0.3")); ok {
		t.Fatalf("with no Redis skip installed, 10.0.0.3 was noted as %s's", n)
	}
}
