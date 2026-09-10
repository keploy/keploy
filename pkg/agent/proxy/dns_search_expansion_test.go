package proxy

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A Kubernetes resolver with ndots:5 walks its search list before trying a name
// absolutely, so `appdb.ns.svc.cluster.local` is first probed as
// `appdb.ns.svc.cluster.local.ns.svc.cluster.local.` — the search domain glued
// onto a name that already carried it. Such a name cannot resolve and cannot
// have been recorded: capture skips non-Success rcodes by design.
//
// #4565 already suppresses the mock-miss report for these, but only when
// upstream actually answers NXDOMAIN. Every other way the forward can end —
// fwdErr != nil (no upstream configured, unreachable, or the 2s loop-wide
// deadline expiring) and SERVFAIL/REFUSED — still reports. Which of those
// fires in the enterprise selfhosted-cloud-replay-e2e lane is UNKNOWN: the
// mismatch was seen on pipelines 8804, 8802 and 8796 (all on
// appdb.java-mysql.svc.cluster.local.java-mysql.svc.cluster.local.), but the
// agent's in-cluster DNS debug logs were never obtained, so no claim is made
// here about resolver timing being the cause.
//
// This fix does not need to know: it closes all of those paths at once.
//
// A doubled search suffix is decidable from the name alone, so the report does
// not need to ask upstream at all.
func TestDNSMockMiss_RedundantSearchExpansion_IsNotReported(t *testing.T) {
	// No upstream configured: forwardDNSUpstream fails immediately, which is
	// the path #4565 deliberately keeps reportable. That isolates this fix —
	// the suppression must come from the name's shape, not from an rcode.
	p := &Proxy{
		logger:     zap.NewNop(),
		errChannel: make(chan error, 4),
		dnsSearch:  []string{"java-mysql.svc.cluster.local", "svc.cluster.local", "cluster.local"},
		IP4:        "127.0.0.1",
	}

	for _, qtype := range []uint16{dns.TypeA, dns.TypeAAAA} {
		q := dns.Question{
			Name:   "appdb.java-mysql.svc.cluster.local.java-mysql.svc.cluster.local.",
			Qtype:  qtype,
			Qclass: dns.ClassINET,
		}
		_ = p.resolveUncachedDNSResponse(q, models.MODE_TEST, true, time.Now(), nil)
		select {
		case err := <-p.errChannel:
			t.Fatalf("%s: a doubled search suffix can never have a mock, so it must not be "+
				"reported as one; got %v", dns.TypeToString[qtype], err)
		default:
		}
	}
}

// CONTROL: a name the resolver expanded ONCE is a real name that is expected to
// resolve. If it misses and upstream cannot confirm it is absent, that is still
// a candidate defect and must stay reported — otherwise this fix would silence
// every in-cluster miss.
func TestDNSMockMiss_SingleSearchExpansion_IsStillReported(t *testing.T) {
	p := &Proxy{
		logger:     zap.NewNop(),
		errChannel: make(chan error, 4),
		dnsSearch:  []string{"java-mysql.svc.cluster.local", "svc.cluster.local", "cluster.local"},
		IP4:        "127.0.0.1",
	}

	q := dns.Question{
		Name:   "appdb.java-mysql.svc.cluster.local.",
		Qtype:  dns.TypeA,
		Qclass: dns.ClassINET,
	}
	_ = p.resolveUncachedDNSResponse(q, models.MODE_TEST, true, time.Now(), nil)
	select {
	case <-p.errChannel:
		// expected: a legitimately-expanded name that missed is still a miss.
	default:
		t.Fatal("a singly-expanded name is a real name; its miss must still be reported")
	}
}

func TestIsRedundantSearchExpansion(t *testing.T) {
	p := &Proxy{dnsSearch: []string{"java-mysql.svc.cluster.local", "svc.cluster.local", "cluster.local"}}
	for _, tc := range []struct {
		name string
		want bool
	}{
		{"appdb.java-mysql.svc.cluster.local.java-mysql.svc.cluster.local.", true},
		{"appdb.svc.cluster.local.svc.cluster.local.", true},
		{"appdb.cluster.local.cluster.local.", true},
		{"appdb.java-mysql.svc.cluster.local.", false}, // expanded once — a real name
		{"appdb.", false},
		{"java-mysql.svc.cluster.local.", false}, // the search domain itself
		{"appdb.example.com.", false},            // unrelated
		// Label boundary: "mysvc.cluster.local" is a DIFFERENT name that
		// merely ends in the same characters. Appending svc.cluster.local
		// to it is a normal single expansion, so it stays reportable.
		{"mysvc.cluster.local.svc.cluster.local.", false},
		{"APPDB.JAVA-MYSQL.SVC.CLUSTER.LOCAL.JAVA-MYSQL.SVC.CLUSTER.LOCAL.", true}, // case-insensitive
	} {
		if got := p.isRedundantSearchExpansion(tc.name); got != tc.want {
			t.Errorf("isRedundantSearchExpansion(%q) = %v, want %v", tc.name, got, tc.want)
		}
	}
	// The search list is normalised, not trusted verbatim: resolv.conf may
	// spell an entry in any case, and with or without a trailing dot. DNS
	// names are case-insensitive, so both must classify identically.
	for _, entry := range []string{"SVC.CLUSTER.LOCAL", "svc.cluster.local."} {
		q := &Proxy{dnsSearch: []string{entry}}
		if !q.isRedundantSearchExpansion("appdb.svc.cluster.local.svc.cluster.local.") {
			t.Errorf("search entry %q must still classify the doubled name", entry)
		}
	}

	// With no search list nothing can be classified.
	empty := &Proxy{}
	if empty.isRedundantSearchExpansion("appdb.java-mysql.svc.cluster.local.java-mysql.svc.cluster.local.") {
		t.Error("with no search list, no name should be classified as a redundant expansion")
	}
}

// The predicate is only useful if the search list actually arrives from
// resolv.conf. Every other test here sets dnsSearch directly, which would keep
// passing even if the capture never populated it — and an empty list makes
// isRedundantSearchExpansion return false for everything, silently restoring
// the old behaviour in production.
func TestCaptureDNSUpstream_CapturesSearchList(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "resolv.conf")
	content := "nameserver 10.96.0.10\n" +
		"search java-mysql.svc.cluster.local svc.cluster.local cluster.local\n" +
		"options ndots:5\n"
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatalf("write temp resolv.conf: %v", err)
	}
	orig := resolvConfPath
	resolvConfPath = path
	defer func() { resolvConfPath = orig }()

	p := &Proxy{logger: zap.NewNop(), DNSPort: 53}
	p.captureDNSUpstream()

	want := []string{"java-mysql.svc.cluster.local", "svc.cluster.local", "cluster.local"}
	if len(p.dnsSearch) != len(want) {
		t.Fatalf("dnsSearch = %v; want %v", p.dnsSearch, want)
	}
	for i := range want {
		if p.dnsSearch[i] != want[i] {
			t.Fatalf("dnsSearch = %v; want %v", p.dnsSearch, want)
		}
	}
	// End to end: the captured list must classify the name from the pipelines.
	if !p.isRedundantSearchExpansion("appdb.java-mysql.svc.cluster.local.java-mysql.svc.cluster.local.") {
		t.Error("the captured search list should classify the doubled name as a redundant expansion")
	}
}
