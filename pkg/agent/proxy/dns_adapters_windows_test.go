//go:build windows

package proxy

import (
	"encoding/json"
	"fmt"
	"net"
	"os/exec"
	"slices"
	"strconv"
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

// The Windows row's own adapter, an internal Hyper-V switch's, and the two DNS
// servers it gives only that adapter, served by the row itself. Its address is
// in RFC 2544's benchmarking range, which no real network uses.
const (
	rowSwitch       = "kp-dns-row"
	rowAdapter      = "vEthernet (" + rowSwitch + ")"
	rowAdapterIP    = "198.18.53.1"
	firstRowServer  = "127.0.0.53"
	secondRowServer = "127.0.0.54"
)

// windowsAnswer is what Windows's own resolver (the DNS Client service, which
// asks every adapter) answers q, one record per line, or its error.
func windowsAnswer(t *testing.T, q dns.Question) string {
	t.Helper()
	return powershell(t, `Clear-DnsClientCache; try { Resolve-DnsName -Name '`+q.Name+`' -Type `+dns.TypeToString[q.Qtype]+
		` -DnsOnly -ErrorAction Stop | ForEach-Object { "$($_.Type) $($_.IPAddress)$($_.NameTarget)" } } catch { "ERROR $($_.Exception.Message)" }`)
}

// addRowAdapter adds the row's own adapter, up, with an address, the given
// IPv4 metric and the row's two DNS servers, and removes it (its switch) when
// the test ends. It first removes one a run killed before its cleanup left.
// No other adapter is changed. It returns the adapter's interface index.
func addRowAdapter(t *testing.T, metric int) int {
	t.Helper()
	remove := fmt.Sprintf("Get-VMSwitch -Name '%s' -ErrorAction SilentlyContinue | Remove-VMSwitch -Force -ErrorAction Stop; Clear-DnsClientCache", rowSwitch)
	if left := powershell(t, fmt.Sprintf("@(Get-VMSwitch -Name '%s' -ErrorAction SilentlyContinue).Count", rowSwitch)); left != "0" {
		t.Logf("removing the adapter %s a killed run left", rowAdapter)
		powershell(t, remove)
	}
	powershell(t, fmt.Sprintf("New-VMSwitch -Name '%s' -SwitchType Internal -ErrorAction Stop | Out-Null", rowSwitch))
	t.Cleanup(func() { powershell(t, remove) })
	out := powershell(t, fmt.Sprintf(`$ErrorActionPreference = 'Stop'
$deadline = (Get-Date).AddSeconds(30)
do {
  $a = Get-NetAdapter -Name '%[1]s' -ErrorAction SilentlyContinue
  if ($a -and $a.Status -eq 'Up') { break }
  Start-Sleep -Milliseconds 250
} while ((Get-Date) -lt $deadline)
if (-not $a -or $a.Status -ne 'Up') { throw "the adapter %[1]s is not up 30 s after its switch was added" }
New-NetIPAddress -InterfaceIndex $a.ifIndex -IPAddress %[2]s -PrefixLength 24 | Out-Null
Set-NetIPInterface -InterfaceIndex $a.ifIndex -AddressFamily IPv4 -InterfaceMetric %[3]d
Set-DnsClientServerAddress -InterfaceIndex $a.ifIndex -ServerAddresses %[4]s,%[5]s
Clear-DnsClientCache
"index=$($a.ifIndex)"`, rowAdapter, rowAdapterIP, metric, firstRowServer, secondRowServer))
	lines := strings.Split(out, "\n")
	index, err := strconv.Atoi(strings.TrimPrefix(strings.TrimSpace(lines[len(lines)-1]), "index="))
	if err != nil {
		t.Fatalf("reading the index of the adapter %s from %q: %v", rowAdapter, out, err)
	}
	return index
}

// serveDNS answers DNS on addr:53 until the test ends: answer fills the reply
// to a question and reports whether the question is one of the row's; any
// other is REFUSED. It returns the count of the row's questions it was asked.
func serveDNS(t *testing.T, addr string, answer func(q dns.Question, m *dns.Msg) bool) *atomic.Int32 {
	t.Helper()
	asked := new(atomic.Int32)
	pc, err := net.ListenPacket("udp", net.JoinHostPort(addr, "53"))
	if err != nil {
		t.Fatalf("serving DNS on %s:53: %v", addr, err)
	}
	srv := &dns.Server{PacketConn: pc, Handler: dns.HandlerFunc(func(w dns.ResponseWriter, r *dns.Msg) {
		m := new(dns.Msg)
		m.SetReply(r)
		if answer(r.Question[0], m) {
			asked.Add(1)
		} else {
			m.Rcode = dns.RcodeRefused
		}
		_ = w.WriteMsg(m)
	})}
	go func() { _ = srv.ActivateAndServe() }()
	t.Cleanup(func() { _ = srv.Shutdown() })
	return asked
}

// On Windows the resolvers are those of the machine's adapters, and Windows
// asks a name on the next adapter when one adapter's server says it does not
// exist: a VPN's or a LAN's own name is answered by its adapter's server.
// The row adds an adapter of its own, after every other in Windows's order,
// with two DNS servers it serves itself, and asks three questions the
// preferred adapter's server answers negatively, of Windows and of a
// recording each: a name it says does not exist, which the row's first
// server answers; a type it has no record of, which the row's first server
// answers; and a name it says does not exist, which the row's first server
// fails to answer (SERVFAIL) and its second answers. It wants the recording
// to answer each as Windows does. Windows 11 (10.0.26200) gives the row's
// adapter's address for the first and the preferred adapter's NODATA for the
// second.
// Adding an adapter needs an administrator and Hyper-V. The row gives DNS
// servers only to its own adapter, removes it when it ends, and first removes
// one a killed run left: the machine's other adapters are not changed.
func TestOnWindowsARecordingAsksEachAdapterAsWindowsDoes(t *testing.T) {
	if !windows.GetCurrentProcessToken().IsElevated() {
		t.Skip("adding an adapter needs an administrator")
	}
	if powershell(t, "if ((Get-Command New-VMSwitch -ErrorAction SilentlyContinue) -and (Get-Service vmms -ErrorAction SilentlyContinue).Status -eq 'Running') { 'yes' } else { 'no' }") != "yes" {
		t.Skip("adding an adapter needs Hyper-V (New-VMSwitch and a running vmms service), which this machine does not have")
	}
	adapters := listAdaptersByPowerShell(t)
	t.Logf("the adapters that are up: %+v", adapters)
	var preferred *psAdapter
	highest := 0
	for i, a := range adapters {
		if a.Name == rowAdapter {
			continue
		}
		if len(a.V4) > 0 && (preferred == nil || a.Metric < preferred.Metric) {
			preferred = &adapters[i]
		}
		highest = max(highest, a.Metric)
	}
	if preferred == nil {
		t.Fatal("no adapter that is up has an IPv4 DNS server")
	}
	// Last in Windows's order: above every other adapter's metric.
	metric := min(highest+1, 9999)
	if metric <= preferred.Metric {
		t.Fatalf("no IPv4 metric is left above the preferred adapter %s's %d", preferred.Name, preferred.Metric)
	}

	// Three questions the preferred adapter's server answers negatively and
	// the row's servers answer: two names that do not exist (.test, RFC
	// 6761) and a type example.com has no record of. No program on the
	// machine asks any of them, so the row's servers answer nothing else
	// (REFUSED). The second server answers the first two as the first does,
	// whichever of the two Windows takes.
	stamp := time.Now().UnixNano()
	nx := dns.Question{Name: fmt.Sprintf("kp%d.split-horizon.test.", stamp), Qtype: dns.TypeA, Qclass: dns.ClassINET}
	nodata := dns.Question{Name: "example.com.", Qtype: dns.TypeSRV, Qclass: dns.ClassINET}
	servfail := dns.Question{Name: fmt.Sprintf("kp%d.servfail.test.", stamp), Qtype: dns.TypeA, Qclass: dns.ClassINET}
	const ownA, ownTarget, nextA = "10.9.8.7", "split.keploy.test.", "10.9.8.6"
	record := func(q dns.Question, addr string) dns.RR {
		return &dns.A{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeA, Class: dns.ClassINET, Ttl: 1}, A: net.ParseIP(addr)}
	}
	answerTheFirstTwo := func(q dns.Question, m *dns.Msg) bool {
		switch {
		case q.Name == nx.Name && q.Qtype == nx.Qtype:
			m.Answer = append(m.Answer, record(q, ownA))
		case strings.EqualFold(q.Name, nodata.Name) && q.Qtype == nodata.Qtype:
			m.Answer = append(m.Answer, &dns.SRV{Hdr: dns.RR_Header{Name: q.Name, Rrtype: dns.TypeSRV, Class: dns.ClassINET, Ttl: 1}, Port: 443, Target: ownTarget})
		default:
			return false
		}
		return true
	}
	askedFirst := serveDNS(t, firstRowServer, func(q dns.Question, m *dns.Msg) bool {
		if q.Name == servfail.Name && q.Qtype == servfail.Qtype {
			m.Rcode = dns.RcodeServerFailure
			return true
		}
		return answerTheFirstTwo(q, m)
	})
	askedSecond := serveDNS(t, secondRowServer, func(q dns.Question, m *dns.Msg) bool {
		if q.Name == servfail.Name && q.Qtype == servfail.Qtype {
			m.Answer = append(m.Answer, record(q, nextA))
			return true
		}
		return answerTheFirstTwo(q, m)
	})

	index := addRowAdapter(t, metric)
	t.Logf("preferred adapter %d (%s, metric %d, DNS %v); the row's adapter %d (%s, metric %d, DNS [%s %s])",
		preferred.Index, preferred.Name, preferred.Metric, preferred.V4, index, rowAdapter, metric, firstRowServer, secondRowServer)

	p := &Proxy{logger: zap.NewNop(), DNSPort: 26789, dnsForwardTimeout: 2 * time.Second, recordedDNSMocks: newRecordedDNSMocksCache()}
	p.captureDNSUpstream()
	t.Logf("a recording asks %v", p.dnsUpstreamServers)
	if i := slices.Index(p.dnsUpstreamServers, firstRowServer); i <= 0 || i+1 >= len(p.dnsUpstreamServers) || p.dnsUpstreamServers[i+1] != secondRowServer {
		t.Fatalf("a recording asks %v, want %s then %s after the preferred adapter's server", p.dnsUpstreamServers, firstRowServer, secondRowServer)
	}
	mc := make(chan *models.Mock, 8)
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
	session := &agent.Session{Mode: models.MODE_RECORD, MC: mc}

	reproduced := 0
	for _, c := range []struct {
		q   dns.Question
		own string
	}{{nx, ownA}, {nodata, ownTarget}, {servfail, nextA}} {
		qtype := dns.TypeToString[c.q.Qtype]
		direct := askServer(c.q, preferred.V4[0])
		if !deniedBy(direct) {
			t.Logf("%s %s: the preferred adapter's server %s answers %q, not negatively; this question cannot tell", c.q.Name, qtype, preferred.V4[0], answerOf(direct))
			continue
		}
		first, second := askedFirst.Load(), askedSecond.Load()
		windowsSays := windowsAnswer(t, c.q)
		windowsUsedRows := strings.Contains(strings.ToLower(windowsSays), strings.ToLower(strings.TrimSuffix(c.own, ".")))
		t.Logf("%s %s: the preferred adapter's server %s says %q; Windows answers %q (it asked the row's servers %d and %d time(s))",
			c.q.Name, qtype, preferred.V4[0], answerOf(direct), windowsSays, askedFirst.Load()-first, askedSecond.Load()-second)
		recorded := p.resolveUncachedDNSResponse(c.q, models.MODE_RECORD, true, time.Now(), session)
		got := answerOf(recorded.Msg)
		recordedRows := strings.Contains(got, strings.ToLower(c.own))
		if recordedRows != windowsUsedRows {
			t.Errorf("%s %s: the recording answered %q, Windows %q: want the recording to answer as Windows does", c.q.Name, qtype, got, windowsSays)
		}
		if windowsUsedRows {
			reproduced++
		}
	}
	if reproduced == 0 {
		t.Error("Windows answered no question from the row's adapter: this machine does not show what the row checks")
	}
}
