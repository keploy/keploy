package learn

import (
	"sort"
	"time"
)

// Side is the half of an exchange a value is in.
type Side string

// The two sides.
const (
	Request  Side = "request"
	Response Side = "response"
)

// Seen is one value of an exchange F served earlier in the test: the leaf
// the recording holds there, and the value F saw there (the live request's,
// or the answer keploy served, with its correlations rendered).
type Seen struct {
	Side Side
	Leaf Leaf
	Live string
}

// Earlier is a mock F served earlier in the same test.
type Earlier struct {
	Mock string
	// At is when the mock's request was recorded: "earlier in the test's
	// recording" is earlier than this.
	At   time.Time
	Seen []Seen
}

// Kind is what a drifted leaf is learned as.
type Kind string

// The three kinds (design §7.2 step 6). They never collapse into each other:
// noise is not compared, a correlation is captured and rendered into the
// answer, a variable is asserted equal to the value at its earlier place.
const (
	KindNoise       Kind = "noise"
	KindCorrelation Kind = "correlation"
	KindVariable    Kind = "variable"
)

// Place is where a value is: a mock, a side and a leaf.
type Place struct {
	Mock  string
	Side  Side
	Path  string
	Noise string
}

// Learned is what L learned about one drifted leaf of the paired mock.
type Learned struct {
	Kind     Kind
	Recorded Leaf
	Live     Leaf
	// From is a variable's earlier place, where it is bound.
	From Place
	// Echo are a correlation's places in the paired mock's recorded answer.
	Echo []Leaf
}

// Classify decides each drifted leaf of the mock rec was paired with
// (design §7.2 step 6). at is when that mock's request was recorded, earlier
// the mocks F served before it in the same test, resp the leaves of its
// recorded answer.
//
//   - Variable: the recorded value appeared earlier in the test's
//     recording, at a place where F saw the request's live value too: the
//     value flowed from there to here. The earliest such place binds it. A
//     place where the recording holds the value but F saw another one did
//     not feed this request, and binds nothing.
//   - Correlation: the recorded answer echoes the recorded value (a leaf
//     equal to it, or a string holding it as a token), and the request
//     names it alone (no other leaf of the request shares its noise path, so
//     a later replay can tell which value to capture).
//   - Noise otherwise: not compared, its type still enforced.
//
// An empty recorded value is never followed: it carries nothing to follow.
func Classify(drift []string, live, rec Shape, at time.Time, earlier []Earlier, resp []Leaf) []Learned {
	order := append([]Earlier(nil), earlier...)
	sort.SliceStable(order, func(i, j int) bool { return order[i].At.Before(order[j].At) })
	out := make([]Learned, 0, len(drift))
	for _, p := range drift {
		r, l := rec.Leaves[p], live.Leaves[p]
		ln := Learned{Kind: KindNoise, Recorded: r, Live: l}
		if r.Value != "" {
			if from, ok := flowedFrom(r, l, at, order); ok {
				ln.Kind, ln.From = KindVariable, from
			} else if echo := echoes(r, resp); len(echo) > 0 && namesAlone(live, l) {
				ln.Kind, ln.Echo = KindCorrelation, echo
			}
		}
		out = append(out, ln)
	}
	return out
}

// flowedFrom is the earliest earlier place holding r's recorded value where
// F saw l's live value.
func flowedFrom(r, l Leaf, at time.Time, earlier []Earlier) (Place, bool) {
	for _, e := range earlier {
		if !e.At.Before(at) {
			continue
		}
		for _, s := range e.Seen {
			if s.Leaf.Type == r.Type && s.Leaf.Value == r.Value && s.Live == l.Value {
				return Place{Mock: e.Mock, Side: s.Side, Path: s.Leaf.Path, Noise: s.Leaf.Noise}, true
			}
		}
	}
	return Place{}, false
}

// echoes are the answer leaves that hold r's recorded value: equal to it,
// or a string holding it as a token.
func echoes(r Leaf, resp []Leaf) []Leaf {
	var out []Leaf
	for _, a := range resp {
		switch {
		case a.Unnamed:
		case a.Type == r.Type && a.Value == r.Value:
			out = append(out, a)
		case a.Type == String && ContainsToken(a.Value, r.Value):
			out = append(out, a)
		}
	}
	return out
}

// namesAlone reports whether no other leaf of the request shares l's noise
// path.
func namesAlone(s Shape, l Leaf) bool {
	for p, o := range s.Leaves {
		if p != l.Path && o.Noise == l.Noise {
			return false
		}
	}
	return !l.Unnamed
}
