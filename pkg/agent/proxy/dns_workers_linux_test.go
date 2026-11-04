//go:build linux

package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/models"
)

// A DNS question carries no kernel stamp of the process that asked it, so
// while test workers are served keploy's DNS server cannot tell whose test a
// question no DNS mock answers belongs to. It is counted as a call keploy
// cannot attribute, which leaves each test open then without a verdict; it
// is never filed in the run's misses window, which no worker's test reads.
// Before any worker is served the run's test reads it, as before. keploy's
// DNS server answers every recorded question from the run's mock manager,
// which every staging of the set reaches, so every worker's question gets
// the same answer: a recorded negative one is served while workers run, and
// nothing is counted for it.
func TestADNSMissWhileTestWorkersAreServedIsACallKeployCannotAttribute(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	p, _ := storeProxy(t, ipv4Dest(t, "127.0.0.1:50093"))
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.StartErrorDrain(ctx)
	unattributed := func() models.CaptureLoss {
		t.Helper()
		for _, l := range p.CaptureLosses() {
			if l.Kind == models.CaptureLossUnattributed {
				return l
			}
		}
		return models.CaptureLoss{}
	}
	ask := func(name string) dnsCacheEntry {
		t.Helper()
		q := dns.Question{Name: name, Qtype: dns.TypeA, Qclass: dns.ClassINET}
		return p.resolveUncachedDNSResponse(q, models.MODE_TEST, true, time.Now(), nil)
	}

	p.ContinueTestErrorCapture()
	ask("first.example.com.")
	if got := readCalls(t, "the run", p); len(got) != 1 || got[0] != "A first.example.com.@0" {
		t.Fatalf("with no worker served, the run's test read %v, want its DNS miss", got)
	}
	if got := unattributed(); got.Count != 0 {
		t.Fatalf("with no worker served, a DNS miss was counted unattributed: %+v", got)
	}

	gone := &models.Mock{
		Version: models.GetVersion(),
		Name:    "dns-gone",
		Kind:    models.DNS,
		Spec: models.MockSpec{
			Metadata: map[string]string{"type": "config"},
			DNSReq:   &models.DNSReq{Name: "gone.example.com.", Qtype: dns.TypeA, Qclass: dns.ClassINET},
			DNSResp:  &models.DNSResp{Rcode: dns.RcodeNameError},
		},
	}
	if err := p.SetMocks(ctx, nil, []*models.Mock{gone}); err != nil {
		t.Fatal(err)
	}
	w1, w2 := servedWorker(t, p, 1), servedWorker(t, p, 2)
	w1.ContinueTestErrorCapture()
	w2.ContinueTestErrorCapture()
	p.ContinueTestErrorCapture()

	if got := ask("gone.example.com."); got.Msg == nil || got.Rcode != dns.RcodeNameError {
		t.Fatalf("a recorded NXDOMAIN while workers are served answered %+v, want NXDOMAIN", got.Msg)
	}
	if got := unattributed(); got.Count != 0 {
		t.Fatalf("a DNS question a mock answered was counted unattributed: %+v", got)
	}

	ask("missing.example.com.")
	want := "the latest: a DNS question for missing.example.com. (A) that no DNS mock answers, asked while test workers are served: a DNS question carries no stamp of the test worker that asked it"
	if got := unattributed(); got.Count != 1 || got.What != want {
		t.Fatalf("a DNS miss while workers are served counted %+v, want 1 with %q", got, want)
	}
	for _, r := range []struct {
		name string
		w    workerMisses
	}{{"worker 1", w1}, {"worker 2", w2}, {"the run", p}} {
		if got := readCalls(t, r.name, r.w); len(got) != 0 {
			t.Fatalf("%s's test read %v, a DNS miss no worker can be told to have made", r.name, got)
		}
	}
}
