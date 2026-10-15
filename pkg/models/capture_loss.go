package models

import (
	"errors"
	"fmt"
	"sort"
	"strings"
)

// CaptureLossKind names one way a run's capture can be incomplete: a call the
// agent did not intercept, or intercepted and could not record or mock
// (design v14 §3.2, rule 12: capture is never silent).
type CaptureLossKind string

const (
	// CaptureLossPIDMap: the kernel refused a process a place in the agent's
	// target_namespace_pids (pid_map_insert_failures): its calls were not
	// intercepted.
	CaptureLossPIDMap CaptureLossKind = "pid_map_refused"
	// CaptureLossPIDMapExcluded: the kernel refused a process a place in the
	// agent's excluded_pids, so a process that should have been left alone
	// was not excluded, and its children's calls may have been captured as
	// the application's.
	CaptureLossPIDMapExcluded CaptureLossKind = "pid_map_refused_excluded"
	// CaptureLossTLSAbort: a client-side TLS handshake with keploy's proxy
	// failed other than by the client giving it up: the client rejected the
	// certificate keploy answered with (it does not trust keploy's CA), or
	// keploy refused its hello or could not make its certificate. Its call
	// was neither recorded nor mocked. A client that hung up (a cancel, a
	// timeout, its own stop) made no call to lose and is not counted. Charged
	// to the test it happened in only (PerTest).
	CaptureLossTLSAbort CaptureLossKind = "tls_handshake_failed"
	// CaptureLossSTARTTLS: a parser asked to upgrade a connection to TLS
	// (STARTTLS, Postgres SSLRequest) and keploy could not make the upgrade,
	// so what followed was not parsed.
	CaptureLossSTARTTLS CaptureLossKind = "starttls_failed"
	// CaptureLossParserStall (W8): a connection's parser stopped taking what
	// the relay captured for it, and the chunks still queued were abandoned
	// once it had made no progress for the stall window (counted in time
	// the process could run, not wall time): what they carried was neither
	// recorded nor mocked.
	CaptureLossParserStall CaptureLossKind = "parser_stalled"
	// CaptureLossDropped: the relay dropped captured bytes for want of room
	// (the per-connection cap, memory pressure), so the call they belong to
	// was not recorded whole.
	CaptureLossDropped CaptureLossKind = "capture_dropped"
	// CaptureLossAppConnLost: a connection of the application's that keploy
	// redirected to its proxy, whose connect() had returned, closed (or its
	// port was reused) before the proxy's side of it reached the proxy: what
	// it carried was neither recorded nor mocked.
	CaptureLossAppConnLost CaptureLossKind = "app_connection_lost"
	// CaptureLossUnpairedConn: a connection reached keploy's proxy that the
	// kernel could not tie to a connection of the application's keploy
	// redirected (the proxy sees other addresses than the application's: a
	// NAT between them, among the causes), so the proxy could not tell
	// where it was going: it was refused, neither recorded nor mocked.
	CaptureLossUnpairedConn CaptureLossKind = "unpaired_connection"
	// CaptureLossRealUncaptured: connections to a mock.real destination's
	// address keploy could neither capture nor relay as real (class e,
	// design v14 §4): it could not read which name one was for, or a name
	// one carried is not listed. What they carried reached nothing keploy
	// saw. What names each destination and what to change. Unlike the other
	// kinds it is never an agent-life count: it is a test's own, counting
	// the connections undecided or used while that test ran (§3.2 as
	// amended for class e), and never taints a later test.
	CaptureLossRealUncaptured CaptureLossKind = "real_uncaptured"
	// CaptureLossAppBytesLost: bytes the application had written to a
	// connection the proxy holds (one it opened earlier, a pooled
	// connection) never reached the proxy: the connection was reset, or the
	// proxy closed it, first. What they carried was neither recorded nor
	// mocked.
	CaptureLossAppBytesLost CaptureLossKind = "app_bytes_lost"
	// CaptureLossUnixSocket (4c a, decision 20): a process of the
	// application connected, or sent a datagram, to a Unix socket no process
	// of it owns and that is not on the allow-list (UnixAllowList): a call
	// over it goes nowhere keploy's proxy sees, so keploy cannot capture it
	// at all. It is not a loss of something keploy saw: the test it taints
	// is NOT JUDGED (NotJudged), never failed.
	CaptureLossUnixSocket CaptureLossKind = "unix_socket"
	// CaptureLossUnixUnseen (decision 20): keploy cannot see the
	// application's calls over Unix sockets (a kernel that loads the report
	// neither way, a report that stopped), so whether a test made one cannot
	// be told. A state, not a count: a mark never takes it away (Since), and
	// every test from then on carries it, NOT JUDGED, as a call over one
	// would be. What says why.
	CaptureLossUnixUnseen CaptureLossKind = "unix_unseen"
	// CaptureLossUDPReply (4c c): the application's UDP other than DNS got a
	// reply; keploy leaves such UDP where the application sends it, so it was
	// answered live, neither recorded nor mocked. Only under the proven
	// switch.
	CaptureLossUDPReply CaptureLossKind = "udp_reply"
)

// NotJudged reports whether a loss of kind k leaves the tests it taints
// without a verdict (decision 20), rather than failing them: a call keploy
// cannot capture at all, as opposed to one it saw and lost.
func (k CaptureLossKind) NotJudged() bool {
	return k == CaptureLossUnixSocket || k == CaptureLossUnixUnseen
}

// Split is l's losses that fail a test (judged) and those that leave it
// without a verdict (notJudged, CaptureLossKind.NotJudged).
func (l CaptureLosses) Split() (judged, notJudged CaptureLosses) {
	for _, c := range l {
		if c.Kind.NotJudged() {
			notJudged = append(notJudged, c)
		} else {
			judged = append(judged, c)
		}
	}
	return judged, notJudged
}

// SinceWithin is what grew from base on for the losses that fail a test, and
// from runBase on for those that leave it without a verdict: a call keploy
// cannot capture at all is over a connection that outlives the test, the test
// set and the session it was opened in, so the application's first one
// taints every test after it in the run, however far back it was made.
func (l CaptureLosses) SinceWithin(base, runBase CaptureLosses) CaptureLosses {
	judged, _ := l.Since(base).Split()
	_, notJudged := l.Since(runBase).Split()
	out := append(judged, notJudged...)
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// NotJudgedText is keploy's sentence for losses that leave a test without a
// verdict (Split's notJudged): what keploy cannot capture, and that it
// therefore cannot prove the test's calls were all captured.
func (l CaptureLosses) NotJudgedText() string {
	if len(l) == 0 {
		return ""
	}
	var parts []string
	for _, c := range l {
		s := captureLossText(c)
		if c.What != "" {
			s += " (" + c.What + ")"
		}
		parts = append(parts, s)
	}
	return "not judged: " + strings.Join(parts, "; ") + ", so keploy cannot prove this test's calls were all captured"
}

// CaptureLoss is how many times one kind of capture loss happened in the
// agent's life, with what the latest one was (never a value the app sent);
// for CaptureLossRealUncaptured, how many connections were charged to one
// test.
type CaptureLoss struct {
	Kind  CaptureLossKind `json:"kind"`
	Count uint64          `json:"count"`
	What  string          `json:"what,omitempty"`
}

// CaptureLosses are the agent's counts of every kind of capture loss, each
// only ever growing for the life of the agent.
type CaptureLosses []CaptureLoss

// ErrCaptureLossesUnsupported reports that an agent cannot count its capture
// losses (hooks without the counters). It means "unknown", never "none".
var ErrCaptureLossesUnsupported = errors.New("this agent cannot tell whether it captured every call")

// PerTest says a loss of kind k, in the agent's counts, is the test's it
// happened in, not a taint of every later test (design v14 §3.2's taint,
// which the agent-life kinds carry): a client whose TLS handshake failed lost
// that one call, and its later calls are captured, or lost, on their own. One
// between two tests is the next test's, as a miss between tests is.
// (CaptureLossRealUncaptured is a test's own too, but never in the agent's
// counts: the agent charges it to its test itself.)
func (k CaptureLossKind) PerTest() bool {
	return k == CaptureLossTLSAbort
}

// Charged is what a test is charged from l, the counts as it ends: each kind
// that taints every later test as SinceWithin(base, runBase) has it (base:
// the counts as the run, or the test set, began), and each kind that is the
// test's own (PerTest) as it grew since test (the counts as the previous test
// ended, or base for the first). Sorted by kind.
func (l CaptureLosses) Charged(base, runBase, test CaptureLosses) CaptureLosses {
	var out CaptureLosses
	for _, c := range l.SinceWithin(base, runBase) {
		if !c.Kind.PerTest() {
			out = append(out, c)
		}
	}
	for _, c := range l.Since(test) {
		if c.Kind.PerTest() {
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// Since is what grew from base on: each kind whose count is higher than in
// base, with the growth as its count. A count lower than base's (an agent
// restarted under the run) is taken whole. Sorted by kind.
func (l CaptureLosses) Since(base CaptureLosses) CaptureLosses {
	was := make(map[CaptureLossKind]uint64, len(base))
	for _, b := range base {
		was[b.Kind] = b.Count
	}
	var out CaptureLosses
	for _, c := range l {
		if c.Kind == CaptureLossUnixUnseen && c.Count > 0 {
			out = append(out, c) // a state: no mark takes it away
			continue
		}
		b := was[c.Kind]
		switch {
		case c.Count > b:
			out = append(out, CaptureLoss{Kind: c.Kind, Count: c.Count - b, What: c.What})
		case c.Count < b && c.Count > 0:
			out = append(out, c)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// Max is l and o merged kind by kind, each with the larger count (its What
// with it): two reads of losses counted from one base, taken in either order
// (the ends of parallel tests' reads), are as much as the later one. Sorted
// by kind.
func (l CaptureLosses) Max(o CaptureLosses) CaptureLosses {
	at := make(map[CaptureLossKind]int, len(l)+len(o))
	var out CaptureLosses
	for _, c := range append(append(CaptureLosses(nil), l...), o...) {
		if i, ok := at[c.Kind]; ok {
			if c.Count > out[i].Count {
				out[i] = c
			}
			continue
		}
		at[c.Kind] = len(out)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// Plus is l and o added kind by kind (each with the What of the later of
// the two, o's, when it has one). Sorted by kind.
func (l CaptureLosses) Plus(o CaptureLosses) CaptureLosses {
	at := make(map[CaptureLossKind]int, len(l)+len(o))
	var out CaptureLosses
	for _, c := range append(append(CaptureLosses(nil), l...), o...) {
		if i, ok := at[c.Kind]; ok {
			out[i].Count += c.Count
			if c.What != "" {
				out[i].What = c.What
			}
			continue
		}
		at[c.Kind] = len(out)
		out = append(out, c)
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Kind < out[j].Kind })
	return out
}

// Text is keploy's sentence for the losses, for a test's reason: what was
// not captured, and that the test's verdict therefore rests on calls keploy
// never saw.
func (l CaptureLosses) Text() string {
	if len(l) == 0 {
		return ""
	}
	var parts []string
	for _, c := range l {
		s := captureLossText(c)
		if w := captureWhatShape(c.What); w != "" {
			s += " (" + w + ")"
		}
		parts = append(parts, s)
	}
	return "capture incomplete: " + strings.Join(parts, "; ")
}

// captureWhatShape is what a capture loss was, as a report names it. Its
// producers give keploy's own words and a destination (host and port), which
// are kept as they are. A word that carries a path, a query or credentials
// (one of / ? # @ =) is written as a miss's target is (targetShape): a
// host-first destination keeps its host and port when they read as one
// (hostPort), an absolute URL keeps only its path's shape, credentials and
// the host they precede are {}, query values are {}, and path segments are
// shaped by PathShape, which keeps a segment that reads as a name.
func captureWhatShape(what string) string {
	words := strings.Fields(what)
	for i, w := range words {
		if strings.ContainsAny(w, "/?#@=") {
			words[i] = targetShape(w)
		}
	}
	return strings.Join(words, " ")
}

func captureLossText(c CaptureLoss) string {
	switch c.Kind {
	case CaptureLossPIDMap:
		return fmt.Sprintf("the kernel refused %d insert(s) into keploy's PID map target_namespace_pids, so a process's calls were not intercepted while this test or one before it ran", c.Count)
	case CaptureLossPIDMapExcluded:
		return fmt.Sprintf("the kernel refused %d insert(s) into keploy's PID map excluded_pids, so a process that predates the run was not excluded, and its children's calls may have been captured as the application's", c.Count)
	case CaptureLossTLSAbort:
		return fmt.Sprintf("%d TLS handshake(s) with keploy's proxy failed other than by the client hanging up (a client that does not trust keploy's CA rejects keploy's certificate), so those calls were neither recorded nor mocked", c.Count)
	case CaptureLossSTARTTLS:
		return fmt.Sprintf("keploy could not make %d STARTTLS upgrade(s), so what followed on those connections was not parsed", c.Count)
	case CaptureLossParserStall:
		return fmt.Sprintf("%d connection(s)' parser stopped taking what keploy captured, and what was still queued for it was abandoned, neither recorded nor mocked", c.Count)
	case CaptureLossDropped:
		return fmt.Sprintf("keploy dropped captured bytes %d time(s) for want of room, so the calls they belong to were not recorded whole", c.Count)
	case CaptureLossUnpairedConn:
		return fmt.Sprintf("%d connection(s) reached keploy's proxy that it could not tie to a connection of the application's (another address in between, a NAT among the causes), so keploy could not tell where they were going and refused them", c.Count)
	case CaptureLossAppBytesLost:
		return fmt.Sprintf("on %d connection(s) to keploy's proxy, bytes the application had written never reached the proxy (the connection was reset, or closed, first), so what they carried was neither recorded nor mocked", c.Count)
	case CaptureLossAppConnLost:
		return fmt.Sprintf("%d connection(s) of the application's to keploy's proxy closed before the proxy received them, so what they carried was neither recorded nor mocked", c.Count)
	case CaptureLossRealUncaptured:
		return fmt.Sprintf("keploy could neither capture nor relay as real %d connection(s) to a mock.real destination's address", c.Count)
	case CaptureLossUDPReply:
		return fmt.Sprintf("the application's UDP was answered live %d time(s) (a socket counted once per read it was answered before), which keploy neither records nor mocks (UDP other than DNS); add a port-only bypassRules entry for it if it is meant to be live, or reach it over TCP", c.Count)
	case CaptureLossUnixSocket:
		return fmt.Sprintf("the application called a Unix socket keploy cannot capture (%d time(s)) while this test or one before it ran", c.Count)
	case CaptureLossUnixUnseen:
		return "keploy cannot see the application's calls over Unix sockets here, so whether this test or one before it made one cannot be told"
	default:
		return fmt.Sprintf("%d capture loss(es) of kind %s", c.Count, c.Kind)
	}
}

// CaptureLossesUnverified says, for a session whose capture-loss counts could
// not be read although the agent keeps them (under the proven switch), what
// that leaves unproven: like CaptureUnverified, it is no evidence of a loss.
func CaptureLossesUnverified(err error) string {
	return fmt.Sprintf("capture unverified: keploy could not read whether the agent captured every call (%v), so it cannot prove that no call went unrecorded or unmocked. Check the agent's log for why its count could not be read", err)
}
