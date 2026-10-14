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
	default:
		return fmt.Sprintf("%d capture loss(es) of kind %s", c.Count, c.Kind)
	}
}
