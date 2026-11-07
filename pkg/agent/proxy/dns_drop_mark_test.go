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
// (one more drop) only if its mark was given back.
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

			// The control: an answer taken holds its question.
			kept := dns.Question{Name: "kept.keploy.test.", Qtype: dns.TypeA, Qclass: dns.ClassINET}
			ask(kept, make(chan *models.Mock, 8))
			mc := closedOutput()
			before := mgr.DropCount()
			ask(kept, mc)
			if got := mgr.DropCount(); got != before {
				t.Fatalf("setup: a question asked again after its answer was taken made %d mocks, want none (its duplicate)", got-before)
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
		})
	}
}
