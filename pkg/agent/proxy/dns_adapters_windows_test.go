//go:build windows

package proxy

import (
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/agent"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sys/windows"
)

// psAdapter is an adapter that is up, as listAdaptersByPowerShell gives it.
type psAdapter struct {
	Index  int
	Name   string
	Metric int
	V4, V6 []string
}

// listAdaptersByPowerShell lists the adapters that are up, with their IPv4
// interface metric and their IPv4 and IPv6 DNS servers.
func listAdaptersByPowerShell(t *testing.T) []psAdapter {
	t.Helper()
	script := `$v4 = Get-NetIPInterface -AddressFamily IPv4 -ErrorAction SilentlyContinue
$rows = foreach ($a in (Get-NetAdapter | Where-Object Status -eq 'Up')) {
  $m = ($v4 | Where-Object InterfaceIndex -eq $a.ifIndex | Select-Object -First 1).InterfaceMetric
  if ($null -eq $m) { continue }
  $s4 = @(Get-DnsClientServerAddress -InterfaceIndex $a.ifIndex -AddressFamily IPv4 -ErrorAction SilentlyContinue | ForEach-Object { $_.ServerAddresses } | Where-Object { $_ })
  $s6 = @(Get-DnsClientServerAddress -InterfaceIndex $a.ifIndex -AddressFamily IPv6 -ErrorAction SilentlyContinue | ForEach-Object { $_.ServerAddresses } | Where-Object { $_ })
  [pscustomobject]@{ Index = [int]$a.ifIndex; Name = $a.Name; Metric = [int]$m; V4 = $s4; V6 = $s6 }
}
ConvertTo-Json -InputObject @($rows) -Compress -Depth 3`
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).Output()
	if err != nil {
		t.Fatalf("listing the adapters with PowerShell: %v", err)
	}
	var adapters []psAdapter
	if err := json.Unmarshal(out, &adapters); err != nil {
		t.Fatalf("reading PowerShell's adapter list %q: %v", out, err)
	}
	return adapters
}

func powershell(t *testing.T, script string) string {
	t.Helper()
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		t.Fatalf("PowerShell %q: %v: %s", script, err, out)
	}
	return strings.TrimSpace(string(out))
}

// deniedBy reports whether a server's answer says the name does not exist
// or has no record of the type asked.
func deniedBy(m *dns.Msg) bool {
	return m != nil && (m.Rcode == dns.RcodeNameError || (m.Rcode == dns.RcodeSuccess && len(m.Answer) == 0))
}

// secondAdapterServer is the DNS server the Windows row sets on a second
// adapter, served by the row itself.
const secondAdapterServer = "127.0.0.53"

// windowsAnswer is what Windows's own resolver (the DNS Client service, which
// asks every adapter) answers q, one record per line, or its error.
func windowsAnswer(t *testing.T, q dns.Question) string {
	t.Helper()
	return powershell(t, `Clear-DnsClientCache; try { Resolve-DnsName -Name '`+q.Name+`' -Type `+dns.TypeToString[q.Qtype]+
		` -DnsOnly -ErrorAction Stop | ForEach-Object { "$($_.Type) $($_.IPAddress)$($_.NameTarget)" } } catch { "ERROR $($_.Exception.Message)" }`)
}

// On Windows the resolvers are those of the machine's adapters, and Windows
// asks a name on the next adapter when one adapter's server says it does not
// exist: a VPN's or a LAN's own name is answered by its adapter's server.
// The row gives a second adapter a DNS server of its own, which answers two
// questions the preferred adapter's server answers negatively (a name it
// says does not exist, and a type it has no record of), asks Windows and a
// recording each, and wants the recording to answer as Windows does: the
// second adapter's address for the first, and the preferred adapter's
// NODATA for the second, as Windows 11 (10.0.26200) answers them.
// It changes the machine's DNS configuration, so it runs only as an
// administrator and puts the adapter back as it was (no DNS server of its
// own) when it ends, and first undoes what a run killed before its cleanup
// left.
func TestOnWindowsARecordingAsksEachAdapterAsWindowsDoes(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("giving an adapter a DNS server needs an administrator")
	}
	reset := func(index int) {
		powershell(t, fmt.Sprintf("Set-DnsClientServerAddress -InterfaceIndex %d -ResetServerAddresses; Clear-DnsClientCache", index))
	}
	adapters := listAdaptersByPowerShell(t)
	for _, a := range adapters {
		if slices.Contains(a.V4, secondAdapterServer) {
			t.Logf("adapter %d (%s) still has the DNS server %s a killed run gave it: resetting it", a.Index, a.Name, secondAdapterServer)
			reset(a.Index)
		}
	}
	adapters = listAdaptersByPowerShell(t)
	t.Logf("the adapters that are up: %+v", adapters)
	var preferred, second *psAdapter
	for i, a := range adapters {
		if len(a.V4) > 0 && (preferred == nil || a.Metric < preferred.Metric) {
			preferred = &adapters[i]
		}
	}
	if preferred == nil {
		t.Fatal("no adapter that is up has an IPv4 DNS server")
	}
	for i, a := range adapters {
		v6Set := slices.ContainsFunc(a.V6, func(s string) bool { return !strings.HasPrefix(strings.ToLower(s), "fec0:") })
		if len(a.V4) == 0 && !v6Set && a.Metric > preferred.Metric {
			second = &adapters[i]
			break
		}
	}
	if second == nil {
		t.Skipf("no second adapter that is up, has no DNS server of its own and a higher metric than %s's %d", preferred.Name, preferred.Metric)
	}
	t.Logf("preferred adapter %d (%s, metric %d, DNS %v); the row's server %s goes on adapter %d (%s, metric %d)",
		preferred.Index, preferred.Name, preferred.Metric, preferred.V4, secondAdapterServer, second.Index, second.Name, second.Metric)

	// Two questions the preferred adapter's server answers negatively and the
	// row's server answers: a name that does not exist (.test, RFC 6761) and
	// a type example.com has no record of. No program on the machine asks
	// either, so the row's server answers nothing else (REFUSED).
	nx := dns.Question{Name: fmt.Sprintf("kp%d.split-horizon.test.", time.Now().UnixNano()), Qtype: dns.TypeA, Qclass: dns.ClassINET}
	nodata := dns.Question{Name: "example.com.", Qtype: dns.TypeSRV, Qclass: dns.ClassINET}
	const ownA, ownTarget = "10.9.8.7", "split.keploy.test."
	var asked atomic.Int32
	pc, err := net.ListenPacket("udp", net.JoinHostPort(secondAdapterServer, "53"))
	if err != nil {
		t.Fatalf("serving DNS on %s:53: %v", secondAdapterServer, err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		switch q := r.Question[0]; {
		case q.Name == nx.Name && q.Qtype == nx.Qtype:
			asked.Add(1)
			m.Answer = append(m.Answer, &dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 1}, A: net.ParseIP(ownA)})
		case strings.EqualFold(q.Name, nodata.Name) && q.Qtype == nodata.Qtype:
			asked.Add(1)
			m.Answer = append(m.Answer, &dns.SRV{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: 1}, Port: 443, Target: ownTarget})
		default:
			m.Rcode = dns.RcodeRefused
		}
		_ = w.WriteMsg(m)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })

	powershell(t, fmt.Sprintf("Set-DnsClientServerAddress -InterfaceIndex %d -ServerAddresses %s", second.Index, secondAdapterServer))
	t.Cleanup(func() { reset(second.Index) })

	p := &Proxy{logger: zap.NewNop(), DNSPort: 26789, dnsForwardTimeout: 2 * time.Second, recordedDNSMocks: newRecordedDNSMocksCache()}
	p.captureDNSUpstream()
	t.Logf("a recording asks %v", p.dnsUpstreamServers)
	if i := slices.Index(p.dnsUpstreamServers, secondAdapterServer); i <= 0 {
		t.Fatalf("a recording asks %v, want %s after the preferred adapter's server", p.dnsUpstreamServers, secondAdapterServer)
	}
	mc := make(chan *models.Mock, 8)
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
	session := &agent.Session{Mode: models.MODE_RECORD, MC: mc}

	reproduced := 0
	for _, c := range []struct {
		q   dns.Question
		own string
	}{{nx, ownA}, {nodata, ownTarget}} {
		qtype := dns.TypeToString[c.q.Qtype]
		direct := askServer(c.q, preferred.V4[0])
		if !deniedBy(direct) {
			t.Logf("%s %s: the preferred adapter's server %s answers %q, not negatively; this question cannot tell", c.q.Name, qtype, preferred.V4[0], answerOf(direct))
			continue
		}
		before := asked.Load()
		windowsSays := windowsAnswer(t, c.q)
		windowsUsedSecond := strings.Contains(strings.ToLower(windowsSays), strings.ToLower(strings.TrimSuffix(c.own, ".")))
		t.Logf("%s %s: the preferred adapter's server %s says %q; Windows answers %q (it asked the second adapter's server %d time(s))",
			c.q.Name, qtype, preferred.V4[0], answerOf(direct), windowsSays, asked.Load()-before)
		recorded := p.resolveUncachedDNSResponse(c.q, models.MODE_RECORD, true, time.Now(), session)
		got := answerOf(recorded.Msg)
		recordedSecond := strings.Contains(got, strings.ToLower(c.own))
		if recordedSecond != windowsUsedSecond {
			t.Errorf("%s %s: the recording answered %q, Windows %q: want the recording to answer as Windows does", c.q.Name, qtype, got, windowsSays)
		}
		if windowsUsedSecond {
			reproduced++
		}
	}
	if reproduced == 0 {
		t.Error("Windows answered no question from the second adapter's server: this machine does not show what the row checks")
	}
}
