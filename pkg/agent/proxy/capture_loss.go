package proxy

import (
	"go.keploy.io/server/v3/pkg/agent"
	"go.keploy.io/server/v3/pkg/agent/proxy/relay"
	"go.keploy.io/server/v3/pkg/models"
)

// CaptureLosses counts, by kind, the calls the proxy intercepted and could not
// record or mock since it started: a client-side TLS handshake that failed
// against keploy's CA, a TLS upgrade it could not make. Each only ever grows.
func (p *Proxy) CaptureLosses() models.CaptureLosses {
	return p.losses.Snapshot()
}

// Compile-time proof that the proxy reports its capture losses: were the
// method renamed, the agent's type assertion would stop matching and those
// losses would go unreported.
var _ agent.CaptureLossReporter = (*Proxy)(nil)

// teeDropLoss is the capture loss a relay tee's drop reason is, if any. A
// parser that stopped taking what was captured for it, whose queue was then
// abandoned (W8), and bytes dropped for want of room lose what they carried.
// A drop while the direction was paused (the parser asked for it, or a TLS
// upgrade was in flight) loses nothing the parser wanted, and a drop after a
// desync is the consequence of one already counted.
func teeDropLoss(reason string) (models.CaptureLossKind, bool) {
	switch reason {
	case relay.DropConsumerGone:
		return models.CaptureLossParserStall, true
	case relay.DropPerConnCap, relay.DropMemoryPressure:
		return models.CaptureLossDropped, true
	}
	return "", false
}

// teeDropped wraps a relay's drop hook (the session's MarkMockIncomplete) so
// that a drop that loses captured bytes is also counted as a capture loss of
// the connection to dst, which every test that ends after it is charged with
// under the proven switch. Not once the recording is stopping (stopping): a
// stop retires the parsers, and what they still had queued is abandoned as
// the stop's, not a test's loss -- as a TLS handshake a stopping agent cut
// short is not counted either.
func (p *Proxy) teeDropped(mark func(reason string), dst func() string, stopping func() bool) func(reason string) {
	return func(reason string) {
		if mark != nil {
			mark(reason)
		}
		if stopping != nil && stopping() {
			return
		}
		if kind, ok := teeDropLoss(reason); ok {
			p.losses.Add(kind, reason+" on a connection to "+dst())
		}
	}
}
