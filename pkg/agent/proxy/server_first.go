package proxy

import (
	"bufio"
	"bytes"
	"context"
	"net"
	"sync"

	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/agent/realdeps"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// reasonServerFirst is the record probe's reason for a destination that
// spoke first, before its client, with the greeting of a server-first
// protocol keploy has no parser for, whose client waits to be greeted
// (serverFirstGreeting, relayServerFirst).
const reasonServerFirst = "greeting-server-first"

// reasonRefusedAtAccept is the record probe's reason, under the proven
// switch, for a destination that refused the connection at accept, before
// its client spoke (refusalAtAccept): relayed, and its port learns nothing.
const reasonRefusedAtAccept = "refused-at-accept"

// serverFirstGreeting reports whether greeting, what a destination sent at
// accept before its client spoke (neither MySQL's nor a refusal), is a
// server-first protocol's, whose client waits to be greeted: relayed from it
// (relayServerFirst), not dispatched on the client's first bytes, which
// would never come. The coordinator's ruling splits it by the switch, so
// that without it no recording that works today changes:
//   - Without the switch, only the greetings of the server-first protocols
//     known to wait (knownServerFirstGreeting). Any other greeting is
//     dispatched on the client's bytes, as it always was.
//   - Under the switch, any greeting but what the server of a protocol
//     whose client speaks without waiting may send at accept
//     (clientFirstPreamble). That includes a server-first protocol whose
//     client speaks at once (IRC, Telnet), and a client-first protocol whose
//     server sends something else at accept: generic dispatch recorded
//     those, but no recording of a server-first protocol replays (nothing
//     greets first), so each test their data passes in is not judged
//     (CaptureLossServerFirst).
func serverFirstGreeting(greeting []byte) bool {
	if !provenswitch.On() {
		return knownServerFirstGreeting(greeting)
	}
	return !clientFirstPreamble(greeting)
}

// knownServerFirstGreeting reports whether greeting is the greeting of a
// server-first protocol known to wait to be greeted: SMTP's and FTP's 220,
// POP3's +OK, IMAP's * OK, NATS's INFO, VNC's RFB version. A greeting cut
// short counts when what came is the start of one.
func knownServerFirstGreeting(greeting []byte) bool {
	for _, prefix := range []string{"220", "+OK", "* OK", "INFO {", "RFB "} {
		if startsAs(greeting, prefix) {
			return true
		}
	}
	return false
}

// clientFirstPreamble reports whether greeting, what a destination sent at
// accept before its client spoke, is what the server of a protocol whose
// client speaks without waiting to be greeted may send first. Such a client
// sends its first bytes anyway, so its connection is dispatched on them, as
// it always was, not relayed as a server-first protocol:
//   - an HTTP/2 server's connection preface, a SETTINGS frame (h2c, gRPC),
//     which a server sends at once, before it reads its client's;
//   - an HTTP/1 answer (an idle connection's 408);
//   - SSH's version line: both ends send theirs at once.
//
// A greeting cut short counts when what came is the start of one.
func clientFirstPreamble(greeting []byte) bool {
	return h2SettingsPreface(greeting) || startsAs(greeting, "HTTP/") || startsAs(greeting, "SSH-")
}

// refusalAtAccept reports whether greeting is a destination refusing the
// connection at accept, before it closes it: a RESP simple error (Redis
// past its maxclients, or in protected mode: "-ERR ...", "-DENIED ...";
// POP3's "-ERR" refusal is the same bytes), or a MySQL ERR packet in place
// of its handshake (Too many connections). It says nothing about what the
// port speaks once the destination accepts, so nothing is learned of it.
func refusalAtAccept(greeting []byte) bool {
	if len(greeting) >= 5 && greeting[3] == 0 && greeting[4] == 0xff {
		return true
	}
	if len(greeting) < 2 || greeting[0] != '-' {
		return false
	}
	for i, c := range greeting[1:] {
		switch {
		case c >= 'A' && c <= 'Z':
		case i > 0 && (c == ' ' || c == '\r' || c == '\n'):
			return true
		default:
			return false
		}
	}
	return true
}

// startsAs reports whether b starts with prefix, or is the start of it.
func startsAs(b []byte, prefix string) bool {
	n := min(len(b), len(prefix))
	return n > 0 && bytes.Equal(b[:n], []byte(prefix[:n]))
}

// h2SettingsPreface reports whether b is (the start of) an HTTP/2 server's
// connection preface: a SETTINGS frame, not an acknowledgement, on stream 0,
// whose payload is whole settings (6 bytes each). b holds 4 bytes at least
// (the probe reads a greeting of fewer only when the destination closed).
func h2SettingsPreface(b []byte) bool {
	if len(b) < 4 || b[3] != 0x04 {
		return false
	}
	if length := int(b[0])<<16 | int(b[1])<<8 | int(b[2]); length%6 != 0 {
		return false
	}
	if len(b) > 4 && b[4] != 0 {
		return false
	}
	// The stream identifier, 0, its reserved high bit aside.
	if len(b) > 5 && b[5]&0x7f != 0 {
		return false
	}
	for i := 6; i < min(len(b), 9); i++ {
		if b[i] != 0 {
			return false
		}
	}
	return true
}

// saidOnceBound is how many destinations a saidOnce holds; past it, the one
// added longest ago is forgotten.
const saidOnceBound = 4096

// saidOnce is a set of destinations: those a line was said for in this
// recording session (Proxy.serverFirstSaid, reset by Record), each said once,
// or those found to speak a server-first protocol (Proxy.serverFirstDests).
// Bounded (saidOnceBound): past it the destination added longest ago is
// forgotten, one at a time, so a run that reaches many destinations says one
// again, or probes it as a port's first connection, rather than holding them
// all, and never forgets the rest at once.
type saidOnce struct {
	mu    sync.Mutex
	said  map[string]struct{}
	order []string
}

// first adds dest, and reports whether it was not in the set. A dest
// already in it is seen again: it is now the one added last, the last to be
// forgotten.
func (s *saidOnce) first(dest string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.said[dest]; ok {
		for i, d := range s.order {
			if d == dest {
				s.order = append(append(s.order[:i:i], s.order[i+1:]...), dest)
				break
			}
		}
		return false
	}
	if s.said == nil {
		s.said = map[string]struct{}{}
	}
	for len(s.said) >= saidOnceBound && len(s.order) > 0 {
		oldest := s.order[0]
		s.order = s.order[1:]
		delete(s.said, oldest)
	}
	s.said[dest] = struct{}{}
	s.order = append(s.order, dest)
	return true
}

// has reports whether dest is in the set.
func (s *saidOnce) has(dest string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.said[dest]
	return ok
}

// drop takes dest out of the set.
func (s *saidOnce) drop(dest string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, ok := s.said[dest]; !ok {
		return
	}
	delete(s.said, dest)
	for i, d := range s.order {
		if d == dest {
			s.order = append(s.order[:i], s.order[i+1:]...)
			break
		}
	}
}

func (s *saidOnce) reset() {
	s.mu.Lock()
	s.said, s.order = nil, nil
	s.mu.Unlock()
}

// relayServerFirst serves, in record, a connection whose destination spoke
// first with a server-first protocol's greeting (the probe's
// reasonServerFirst, serverFirstGreeting), or, a later connection to such a
// destination (its port learned, the probe skipped), or, under the switch,
// one it refused at accept (reasonRefusedAtAccept, refused). A server-first
// protocol keploy has no parser for, SMTP, FTP, POP3, IMAP or NATS: its
// client sends nothing until it is greeted, and dispatch picks a parser by
// the client's first bytes, so waiting for them hung the call for good: the
// probe had read the greeting, and held it. Instead the connection is
// relayed both ways from the greeting (realdeps.Relay, the greeting first:
// the upstream the probe handed on replays it), every byte, with no clock:
// the application's call is made, and nothing of it is recorded, which
// keploy says once per destination in a recording session (P250).
//
// Under the proven switch each piece of the application's data the
// connection carries is a capture loss of a call keploy cannot record
// (models.CaptureLossServerFirst, decision 20), charged to the test it is
// sent in, or, sent between two tests, to the next: not judged, never passed
// or failed. What the destination sends on its own (its greeting, a notice
// between calls) charges nothing. A server-first protocol is S9's to record
// and replay, greeting at accept from the recorded destination; until then
// it is relayed so.
func (p *Proxy) relayServerFirst(ctx context.Context, src net.Conn, dstAddr string, pre *util.Predialed, refused bool, logger *zap.Logger) error {
	dst, err := util.DialDestination(ctx, logger, "tcp", util.DialTarget{Addr: dstAddr, Predialed: pre})
	if err != nil {
		utils.LogError(logger, err, "failed to dial the conn to destination server", zap.String("next_step", util.NextStepDialDestination))
		return err
	}
	switch {
	case refused && p.serverFirstSaid.first("refused "+dstAddr):
		logger.Warn("keploy passed through "+dstAddr+"'s refusal at accept, not recorded",
			zap.String("next_step", "check why "+dstAddr+" refuses the application's connections (its connection limit, or its protected mode)"))
	case !refused && p.serverFirstSaid.first(dstAddr):
		next := "list " + dstAddr + " in keploy.yml bypassRules to pass it through by design"
		if provenswitch.On() {
			next = "list " + dstAddr + " in keploy.yml mock.real to reach it for real by design"
		}
		logger.Warn("keploy cannot record "+dstAddr+": a server-first protocol; passed through, not recorded",
			zap.String("next_step", next))
	}
	relay := &realdeps.Relay{Client: src, In: bufio.NewReader(src), Dest: dst}
	if provenswitch.On() {
		what, kind := "to "+dstAddr, models.CaptureLossServerFirst
		if refused {
			kind = models.CaptureLossRefusedAtAccept
		}
		relay.Sent = func(int) { p.losses.Add(kind, what) }
	}
	return relay.Run(ctx)
}
