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
	// CaptureLossTLSAbort: a client aborted the TLS handshake keploy's proxy
	// answered with a certificate from keploy's CA (it does not trust that
	// CA, or the handshake failed): its call was neither recorded nor mocked.
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
)

// CaptureLoss is how many times one kind of capture loss happened in the
// agent's life, with what the latest one was (never a value the app sent).
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
		return fmt.Sprintf("%d TLS handshake(s) with keploy's proxy failed on the client's side (a client that does not trust keploy's CA aborts it), so those calls were neither recorded nor mocked", c.Count)
	case CaptureLossSTARTTLS:
		return fmt.Sprintf("keploy could not make %d STARTTLS upgrade(s), so what followed on those connections was not parsed", c.Count)
	case CaptureLossParserStall:
		return fmt.Sprintf("%d connection(s)' parser stopped taking what keploy captured, and what was still queued for it was abandoned, neither recorded nor mocked", c.Count)
	case CaptureLossDropped:
		return fmt.Sprintf("keploy dropped captured bytes %d time(s) for want of room, so the calls they belong to were not recorded whole", c.Count)
	case CaptureLossUnpairedConn:
		return fmt.Sprintf("%d connection(s) reached keploy's proxy that it could not tie to a connection of the application's (another address in between, a NAT among the causes), so keploy could not tell where they were going and refused them", c.Count)
	case CaptureLossAppConnLost:
		return fmt.Sprintf("%d connection(s) of the application's to keploy's proxy closed before the proxy received them, so what they carried was neither recorded nor mocked", c.Count)
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
