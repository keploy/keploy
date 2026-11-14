package proxy

import (
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/agent"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A DNS answer the manager drops as it is handed over gives its question back
// (dnsRecordDedupe.giveBack): the question's next answer is recorded. Held,
// the mark would skip it for recordedDNSMocksTTL as a duplicate of an answer
// no recording holds. The manager's output is closed, as at a recording's
// end, so it drops every mock: a question asked again records a mock again
// (one more drop) only if its mark was given back. Only the dropped answer's
// question is given back: a question whose answer was taken stays held,
// whether its answer is negative, as the dropped one is (the same tracker),
// or resolved (the other tracker).
func TestDNS_AnAnswerTheManagerDropsGivesItsQuestionBack(t *testing.T) {
	for _, synchronous := range []bool{false, true} {
		name := "sent at once (SendConfigMock)"
		if synchronous {
			name = "synchronous (AddMock)"
		}
		t.Run(name, func(t *testing.T) {
			host, port := negativeUpstream(t, dns.RcodeNameError, negativeRootSOA)
			mgr := syncMock.Get()
			t.Cleanup(func() { mgr.SetOutputChannel(nil) })
			p := &Proxy{logger: zap.NewNop(), dnsUpstreamServers: []string{host}, dnsUpstreamPort: port, recordedDNSMocks: newRecordedDNSMocksCache()}
			ask := func(q dns.Question, mc chan *models.Mock) {
				t.Helper()
				p.resolveUncachedDNSResponse(q, models.MODE_RECORD, true, time.Now(), &agent.Session{Mode: models.MODE_RECORD, MC: mc, Synchronous: synchronous})
			}
			closedOutput := func() chan *models.Mock {
				mc := make(chan *models.Mock, 8)
				mgr.SetOutputChannel(mc)
				mgr.CloseOutChan()
				return mc
			}

			// The control: an answer taken holds its question. negativeUpstream
			// answers sentinelName alone, so kept's answer is NXDOMAIN and
			// resolved's is an A record.
			kept := dns.Question{Name: "kept.keploy.test.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
			resolved := dns.Question{Name: sentinelName, Qtype: dns.TypeA, Qclass: dns.ClassINET}
			open := make(chan *models.Mock, 8)
			ask(kept, open)
			ask(resolved, open)
			mc := closedOutput()
			before := mgr.DropCount()
			ask(kept, mc)
			ask(resolved, mc)
			if got := mgr.DropCount(); got != before {
				t.Fatalf("setup: two questions asked again after their answers were taken made %d mocks, want none (their duplicates)", got-before)
			}

			gone := dns.Question{Name: "gone.keploy.test.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
			before = mgr.DropCount()
			ask(gone, mc)
			if got := mgr.DropCount(); got != before+1 {
				t.Fatalf("setup: the manager dropped %d mocks of the first answer, want 1", got-before)
			}
			ask(gone, mc)
			if got := mgr.DropCount(); got != before+2 {
				t.Fatalf("the question asked again after its answer was dropped made %d mocks, want 1: the dropped answer kept its mark, and the recording holds no answer for it", got-before-1)
			}
			// The give-back lets go of the dropped answer's question alone:
			// each answer taken before still holds its own.
			for _, q := range []dns.Question{kept, resolved} {
				before = mgr.DropCount()
				ask(q, mc)
				if got := mgr.DropCount(); got != before {
					t.Fatalf("%s, whose answer was taken, asked again after another question's answer was given back, made %d mocks, want none: the give-back let go of other questions", q.Name, got-before)
				}
			}
		})
	}
}
