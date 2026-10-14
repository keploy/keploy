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
	// CaptureLossPIDMap: the kernel refused a process a place in one of the
	// agent's PID maps (pid_map_insert_failures): its calls were not
	// intercepted (target_namespace_pids), or its children's calls were
	// taken for the app's (excluded_pids).
	CaptureLossPIDMap CaptureLossKind = "pid_map_refused"
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
	// CaptureLossUnixSocket (4c a): a process of the application connected
	// to a Unix socket no process of it owns, and that is not allowlisted (a
	// bus, the journal, the Docker daemon, a name service): a call over it
	// goes nowhere keploy's proxy sees, neither recorded nor mocked.
	CaptureLossUnixSocket CaptureLossKind = "unix_socket"
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
		if c.What != "" {
			s += " (" + c.What + ")"
		}
		parts = append(parts, s)
	}
	return "capture incomplete: " + strings.Join(parts, "; ")
}

func captureLossText(c CaptureLoss) string {
	switch c.Kind {
	case CaptureLossPIDMap:
		return fmt.Sprintf("the kernel refused %d insert(s) into keploy's PID maps, so a process's calls were not intercepted while this test or one before it ran", c.Count)
	case CaptureLossTLSAbort:
		return fmt.Sprintf("%d TLS handshake(s) with keploy's proxy failed on the client's side (a client that does not trust keploy's CA aborts it), so those calls were neither recorded nor mocked", c.Count)
	case CaptureLossSTARTTLS:
		return fmt.Sprintf("keploy could not make %d STARTTLS upgrade(s), so what followed on those connections was not parsed", c.Count)
	case CaptureLossParserStall:
		return fmt.Sprintf("%d connection(s)' parser stopped taking what keploy captured, and what was still queued for it was abandoned, neither recorded nor mocked", c.Count)
	case CaptureLossDropped:
		return fmt.Sprintf("keploy dropped captured bytes %d time(s) for want of room, so the calls they belong to were not recorded whole", c.Count)
	case CaptureLossUnixSocket:
		return fmt.Sprintf("the application made %d connect(s) to Unix sockets keploy does not see, so calls over them were neither recorded nor mocked; reach such a dependency over a TCP address", c.Count)
	default:
		return fmt.Sprintf("%d capture loss(es) of kind %s", c.Count, c.Kind)
	}
}
