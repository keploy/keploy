//go:build windows

package proxy

import (
	"net"
	"os/exec"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/agent"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// machineResolversByPowerShell lists, the way Windows's own tools do, the
// DNS servers of the machine's adapters that are up, each once and without
// its zone, leaving out the site-local placeholders (fec0:0:0:ffff::1 to ::3)
// Windows lists on an adapter with no IPv6 DNS server.
func machineResolversByPowerShell(t *testing.T) []string {
	t.Helper()
	script := `Get-DnsClientServerAddress | Where-Object { ` +
		`(Get-NetAdapter -InterfaceIndex $_.InterfaceIndex -IncludeHidden -ErrorAction SilentlyContinue).Status -eq 'Up' } | ` +
		`ForEach-Object { $_.ServerAddresses }`
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		t.Fatalf("listing the machine's DNS servers with PowerShell: %v", err)
	}
	var servers []string
	for _, line := range strings.Fields(string(out)) {
		s := withoutZone(line)
		if strings.HasPrefix(strings.ToLower(s), "fec0:") || slices.Contains(servers, s) {
			continue
		}
		servers = append(servers, s)
	}
	slices.Sort(servers)
	return servers
}

func withoutZone(s string) string {
	if i := strings.IndexByte(s, '%'); i >= 0 {
		return s[:i]
	}
	return s
}

// answerOf is what an answer says, without its TTLs: its rcode and its
// records' data, sorted. "" when there is no answer.
func answerOf(m *dns.Msg) string {
	if m == nil {
		return ""
	}
	var data []string
	for _, rr := range m.Answer {
		data = append(data, strings.ToLower(strings.TrimPrefix(rr.String(), rr.Header().String())))
	}
	slices.Sort(data)
	return dns.RcodeToString[m.Rcode] + " " + strings.Join(data, ",")
}

func askServer(q dns.Question, server string) *dns.Msg {
	c := &dns.Client{Net: "udp", Timeout: 2 * time.Second}
	m := new(dns.Msg)
	m.SetQuestion(q.Name, q.Qtype)
	m.RecursionDesired = true
	in, _, err := c.Exchange(m, net.JoinHostPort(server, "53"))
	if err != nil {
		return nil
	}
	return in
}

// onlyTheMachineAnswers finds a question the machine's own resolvers answer
// otherwise than a public one, and returns it with the machine's answer, or
// nil when this machine has none. It tries names whose answers do not move:
//   - the PTR of each of the machine's private IPv4 addresses, which a LAN's
//     or a cloud network's DNS answers and a public resolver does not
//     (RFC 6303);
//   - localhost, which some resolvers answer themselves (unbound);
//   - a nip.io name for a private address, whose answer a resolver that
//     guards against DNS rebinding drops (unbound's private-address).
func onlyTheMachineAnswers(t *testing.T, servers []string) (*dns.Question, string) {
	t.Helper()
	addrs, err := net.InterfaceAddrs()
	if err != nil {
		t.Fatalf("listing this machine's addresses: %v", err)
	}
	var qs []dns.Question
	var private []string
	for _, a := range addrs {
		ipnet, ok := a.(*net.IPNet)
		if !ok || ipnet.IP.To4() == nil || !ipnet.IP.IsPrivate() {
			continue
		}
		private = append(private, ipnet.IP.String())
		if name, err := dns.ReverseAddr(ipnet.IP.String()); err == nil {
			qs = append(qs, dns.Question{Name: name, Qtype: dns.TypePTR, Qclass: dns.ClassINET})
		}
	}
	qs = append(qs, dns.Question{Name: "localhost.", Qtype: dns.TypeA, Qclass: dns.ClassINET})
	for _, ip := range private {
		qs = append(qs, dns.Question{Name: ip + ".nip.io.", Qtype: dns.TypeA, Qclass: dns.ClassINET})
	}
	for _, q := range qs {
		public := answerOf(askServer(q, "8.8.8.8"))
		for _, s := range servers {
			mine := askServer(q, s)
			if mine == nil || (mine.Rcode != dns.RcodeSuccess && mine.Rcode != dns.RcodeNameError) {
				continue
			}
			t.Logf("%s %s: %q from the machine's resolver %s, %q from 8.8.8.8",
				q.Name, dns.TypeToString[q.Qtype], answerOf(mine), s, public)
			if answerOf(mine) != public {
				return &q, answerOf(mine)
			}
			break
		}
	}
	return nil, ""
}

// On Windows a recording asks the machine's own resolvers, the DNS servers of
// its adapters that are up, as it asks resolv.conf's on Linux: a name only
// they answer is recorded with their answer, the one the application gets,
// not a public resolver's.
func TestOnWindowsTheRecordingAsksTheMachinesResolvers(t *testing.T) {
	want := machineResolversByPowerShell(t)
	t.Logf("the machine's resolvers, as PowerShell lists them: %v", want)
	if len(want) == 0 {
		t.Fatal("no adapter that is up has a DNS server, so this row cannot tell which resolvers a recording asks")
	}

	p := &Proxy{logger: zap.NewNop(), DNSPort: 26789, recordedDNSMocks: newRecordedDNSMocksCache()}
	p.captureDNSUpstream()
	var got []string
	for _, s := range p.dnsUpstreamServers {
		got = append(got, withoutZone(s))
	}
	slices.Sort(got)
	if !slices.Equal(got, want) {
		t.Errorf("a recording asks the resolvers %v, want the machine's %v", p.dnsUpstreamServers, want)
	}

	q, mine := onlyTheMachineAnswers(t, want)
	if q == nil {
		t.Log("no name on this machine that only its resolvers answer; the row checked the resolver list alone")
		return
	}
	mc := make(chan *models.Mock, 8)
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
	session := &agent.Session{Mode: models.MODE_RECORD, MC: mc}
	recorded := p.resolveUncachedDNSResponse(*q, models.MODE_RECORD, true, time.Now(), session)
	if got := answerOf(recorded.Msg); got != mine {
		t.Errorf("recording %s %s answered %q, want the machine's answer %q", q.Name, dns.TypeToString[q.Qtype], got, mine)
	}
}
