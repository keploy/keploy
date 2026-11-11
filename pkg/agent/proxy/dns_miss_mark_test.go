package proxy

import (
	"net"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A DNS question no mock answers carries the round it was asked in
// (StampMark), taken as it reaches keploy: forwarding it to the replay's
// own resolver takes until that resolver's timeout, and the replay can
// stage the next set before the miss is filed, which is listed where its
// question was asked, not where keploy decided it.
func TestADNSMissIsMarkedInTheRoundItsQuestionWasAskedIn(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	asked := mm.StagingEpoch()
	// The resolver the miss is forwarded to: when the question reaches it,
	// the replay stages the next set, and it never answers.
	conn, err := net.ListenPacket("udp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	got := make(chan struct{})
	go func() {
		buf := make([]byte, 1500)
		if _, _, err := conn.ReadFrom(buf); err != nil {
			return
		}
		mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
		close(got)
	}()
	_, port, _ := net.SplitHostPort(conn.LocalAddr().String())
	p := &Proxy{
		logger:              zap.NewNop(),
		errChannel:          make(chan error, 4),
		dnsUpstreamServers:  []string{"127.0.0.1"},
		dnsInNetworkServers: []string{"127.0.0.1"},
		dnsClusterDomain:    "cluster.local",
		dnsUpstreamPort:     port,
		dnsForwardTimeout:   500 * time.Millisecond,
	}
	p.setMockManager(mm)

	q := dns.Question{Name: "some-service.default.svc.cluster.local.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
	_ = p.resolveUncachedDNSResponse(q, models.MODE_TEST, true, time.Now(), nil)
	select {
	case <-got:
	case <-time.After(5 * time.Second):
		t.Fatal("the question was not forwarded, or the set was not staged after it was asked (this case's premise)")
	}
	if mm.StagingEpoch() == asked {
		t.Fatal("the staging started no staging epoch (this case's premise)")
	}
	reports := collectMockMissReports(t, p)
	if len(reports) != 1 {
		t.Fatalf("want one DNS miss report; got %d: %+v", len(reports), reports)
	}
	if r := reports[0]; r.Mark != models.RoundMarkedSettled || r.MarkEpoch != asked {
		t.Fatalf("the DNS miss is marked %q in staging %d; want settled, in staging %d, where its question was asked", r.Mark, r.MarkEpoch, asked)
	}
}
