package proxy

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/miekg/dns"
	"go.keploy.io/server/v3/internal/logleak"
	"go.keploy.io/server/v3/pkg/agent"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A DNS question's name is the app's (a tenant's subdomain, a customer's own
// domain): the agent's lines and the miss's error name it by its labels'
// shape beside its tag under the session the proxy serves
// (models.HostLogName), never whole; the miss's report, for the user's own
// view, keeps it whole. It logged the name whole, and its miss error quoted
// it.
func TestADNSQuestionsNameIsLoggedByItsShapeAndTag(t *testing.T) {
	const s = logleak.Canary
	logger, logs := logleak.Observe()
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	p := &Proxy{
		logger:            logger,
		errChannel:        make(chan error, 4),
		dnsForwardTimeout: 200 * time.Millisecond,
	}
	p.setMockManager(mm)
	keys := models.NewLogKeySession()
	p.setSession(&agent.Session{Mode: models.MODE_TEST, LogKeys: keys})

	name := s + ".tenant.example.com."
	q := dns.Question{Name: name, Qtype: dns.TypeA, Qclass: dns.ClassINET}
	_ = p.resolveUncachedDNSResponse(q, models.MODE_TEST, true, time.Now(), p.getSession())

	if logs.Len() == 0 {
		t.Fatal("precondition: the question drove no line")
	}
	logleak.Check(t, logs, s, "the DNS proxy")
	named := false
	for _, e := range logs.All() {
		if e.ContextMap()["query"] == keys.HostLogName(name) {
			named = true
		}
	}
	if !named || keys.HostLogName(name) != "{}.{}.{}.{}. "+keys.HostTag(name) {
		t.Fatalf("no line names the question by its shape and its tag under the session's key, %q: %+v", keys.HostLogName(name), logs.All())
	}
	select {
	case err := <-p.errChannel:
		pe, ok := err.(models.ParserError)
		if !ok || pe.MismatchReport == nil {
			t.Fatalf("the miss is %T %v, want a ParserError with its report", err, err)
		}
		if logleak.Holds(pe.Err.Error(), s) {
			t.Errorf("the miss's error, which the agent logs, names the question: %q", pe.Err)
		}
		if !strings.Contains(pe.MismatchReport.ActualSummary, name) {
			t.Errorf("the miss's report, for the user's own view, lost the name: %q", pe.MismatchReport.ActualSummary)
		}
		if pe.MismatchReport.HostTag != keys.HostTag(name) {
			t.Errorf("the miss's report carries host tag %q, want %q, the tag its lines name the question by, for the console to print beside it", pe.MismatchReport.HostTag, keys.HostTag(name))
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the question no mock answered was reported as no miss")
	}
}

// A question a recording forwards and records is the app's too: the
// recording's lines name it by its labels' shape beside its tag under the
// session it is recorded in, as a replay's do. Its "Recording new DNS mock"
// line named its dedupe key, the question's name whole and lower-cased,
// beside the shaped query. The mock keeps the name whole.
func TestARecordedDNSQuestionsNameIsLoggedByItsShapeAndTag(t *testing.T) {
	const s = logleak.Canary
	host, port := negativeUpstream(t, dns.RcodeNameError, negativeRootSOA)
	t.Cleanup(func() { syncMock.Get().SetOutputChannel(nil) })
	logger, logs := logleak.Observe()
	keys := models.NewLogKeySession()
	p := &Proxy{logger: logger, dnsUpstreamServers: []string{host}, dnsUpstreamPort: port, recordedDNSMocks: newRecordedDNSMocksCache()}
	mc := make(chan *models.Mock, 8)
	session := &agent.Session{Mode: models.MODE_RECORD, MC: mc, LogKeys: keys}
	p.setSession(session)

	name := s + ".tenant.example.com."
	q := dns.Question{Name: name, Qtype: dns.TypeA, Qclass: dns.ClassINET}
	p.resolveUncachedDNSResponse(q, models.MODE_RECORD, true, time.Now(), session)

	select {
	case m := <-mc:
		if m.Spec.DNSReq == nil || m.Spec.DNSReq.Name != name {
			t.Fatalf("the recorded mock does not keep the question whole: %+v", m.Spec.DNSReq)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("precondition: the question's answer was not recorded")
	}
	recorded := false
	for _, e := range logs.FilterMessage("Recording new DNS mock").All() {
		if e.ContextMap()["query"] == keys.HostLogName(name) {
			recorded = true
		}
	}
	if !recorded {
		t.Fatalf("no line says the question was recorded, by its shape and tag under the recording's key %q: %+v", keys.HostLogName(name), logs.All())
	}
	logleak.Check(t, logs, s, "the DNS proxy recording a question")
}

// A recorded answer keploy cannot parse (a hand edit of a DNS mock) is left
// out of the answer, and its line says the record's size and the parser's
// error with what it quotes withheld: the record names the host it answered
// whole. It logged the record, and the error quoted its token.
func TestARecordedDNSAnswerKeployCannotParseIsLoggedByItsSize(t *testing.T) {
	logger, logs := logleak.Observe()
	for _, raw := range []string{
		strings.ToLower(logleak.Canary) + ".tenant.example. 300 IN A not-an-address",
		"api.example.com. 300 IN A " + logleak.Canary,
	} {
		if _, ok := decodeDNSRR(logger, raw); ok {
			t.Fatalf("%q decoded", raw)
		}
	}
	if logs.FilterMessage("failed to parse dns rr").Len() != 2 {
		t.Fatalf("the refused records were not said: %+v", logs.All())
	}
	logleak.Check(t, logs, logleak.Canary, "a recorded DNS answer keploy cannot parse")
	for _, e := range logs.All() {
		if strings.Contains(fmt.Sprint(e.ContextMap()["error"]), "example") {
			t.Errorf("the line names the record's host: %v", e.ContextMap())
		}
	}
}
