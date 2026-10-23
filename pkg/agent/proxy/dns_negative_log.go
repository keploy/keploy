package proxy

import (
	"github.com/miekg/dns"
	"go.keploy.io/server/v3/pkg/agent/proxy/dnsnegative"
)

// noteNegativeAnswer notes, while recording, the answer keploy passes on to
// the app, unchanged, for its question, asked with the Ticket asked: the
// resolver's, from just now or from keploy's DNS cache. An answer that
// did not resolve the app's lookup (NXDOMAIN, SERVFAIL, REFUSED, ...) gets a
// line once a recording for each (name, qtype, answer), at INFO with a next
// step, or at DEBUG when it is routine (dnsnegative.Log.Note).
//
// The relay used to be silent. An app run natively that dials a name only a
// Docker network resolves (a compose service's, such as "mongoDb") got
// NXDOMAIN from the host's resolver through keploy, its database driver timed
// out 30 s later, and nothing in keploy's output said keploy had seen the
// lookup at all. Recording keeps no mock of such an answer (recordDNSMock),
// so the log is its only trace.
func (p *Proxy) noteNegativeAnswer(question dns.Question, resp dnsCacheEntry, asked dnsnegative.Ticket) {
	if resp.Msg == nil {
		p.negativeAnswers.Unanswered(asked)
		return
	}
	p.negativeAnswers.Note(p.logger, p.negativeAnswerConfig(), asked, dnsnegative.Answer{
		Question:  question,
		Rcode:     resp.Rcode,
		Records:   len(resp.Answer),
		Upstream:  resp.upstream,
		Synthetic: !resp.FromUpstream,
	})
}

// negativeAnswerConfig is the resolver configuration the relay's lines are
// judged by: resolv.conf's search list and ndots as keploy read them
// (captureDNSUpstream), and where the relay runs.
func (p *Proxy) negativeAnswerConfig() dnsnegative.Config {
	where := dnsnegative.Host
	if p.IsDocker {
		where = dnsnegative.AgentContainer
	}
	return dnsnegative.Config{Search: p.dnsSearch, Ndots: p.dnsNdots, Where: where, Relayed: true}
}

// DNSNegativeLog is the log of the DNS answers that did not resolve the app's
// lookups in this proxy's recording: the relay notes the answers it passes
// on, and a capture that sees the app's lookups another way (the enterprise
// proxyless DNS capture) notes what it sees, so a recording says each once.
// One exchange is noted by one of them only: noted twice, its second
// question reads as the app asking again. What it holds back is logged and
// counted when the recording stops (SetGracefulShutdown), and the next
// recording starts afresh (ResetRecordedDNSMocks).
func (p *Proxy) DNSNegativeLog() *dnsnegative.Log {
	return &p.negativeAnswers
}
