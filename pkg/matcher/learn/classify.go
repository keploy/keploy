package learn

import (
	"encoding/json"
	"sort"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Side is the half of an exchange a value is in.
type Side string

// The two sides.
const (
	Request  Side = "request"
	Response Side = "response"
)

// Seen is one value of an exchange F served earlier in the test: the leaf
// the recording holds there, the value F saw there (the live request's, or
// the answer keploy served, with its variables' values), and the leaf's
// variable place ("" when no variable place names it: VarPlace).
type Seen struct {
	Side  Side
	Leaf  Leaf
	Live  string
	Place string
}

// Earlier is an HTTP mock the learn step served earlier in the same test,
// leaf by leaf: a variable found there has a place a runtime can bind it at.
type Earlier struct {
	Mock string
	// At is when the mock's request was recorded.
	At time.Time
	// Own: the mock is one of the test's own, not the pool's. Only a test's
	// own mocks carry its variables.
	Own  bool
	Seen []Seen
}

// Raw is any other mock of the test's recording, of any kind: served earlier
// in the test (Served), or recorded before the request L classifies (At).
// Text is its recorded request and answer, the payload only, as one text: a
// value found there flowed from that mock, at a place no leaf names.
type Raw struct {
	Mock   string
	At     time.Time
	Served bool
	Text   string
}

// Kind is what a drifted leaf is learned as.
type Kind string

// The three kinds (design §7.2 step 6; an echo is the design's
// correlation). They never collapse into each other: noise is not compared,
// an echo is bound at the request and answered, a variable is asserted equal
// to the value at its earlier place.
const (
	KindNoise    Kind = "noise"
	KindEcho     Kind = "echo"
	KindVariable Kind = "variable"
)

// Place is where a value is: a mock, and, when a leaf names it, a side and
// the leaf. A place found in a mock's raw text has no side and no path.
type Place struct {
	Mock  string
	Side  Side
	Path  string
	Noise string
	// Var is the leaf's variable place, "" when it has none (Seen.Place).
	Var string
	// Live is the value F saw at the leaf.
	Live string
	// Whole: the leaf held the value whole (not as a word of a longer
	// value), so the value can be bound there.
	Whole bool
	// Own: the mock is one of the test's own (Earlier.Own).
	Own bool
}

// Learned is what L learned about one drifted leaf of the paired mock.
type Learned struct {
	Kind     Kind
	Recorded Leaf
	Live     Leaf
	// From is a variable's earlier place, where it is bound.
	From Place
	// Echo are an echo's places in the paired mock's recorded answer.
	Echo []Leaf
}

// Classify decides each drifted leaf of the mock rec was paired with
// (design §7.2 step 6). at is when that mock's request was recorded;
// earlier are the HTTP mocks F served before it in the same test, leaf by
// leaf, and raw every other mock of the test's recording that came before
// it; resp are the leaves of its recorded answer.
//
//   - Variable: the recorded value appeared earlier in the test's recording,
//     whatever its type there, as a whole value or as a token of a longer
//     one, in any kind of mock. It is asserted equal to the live value at
//     that earlier place, wherever F saw what: a value that came from
//     somewhere is never stopped being compared. The earliest place a leaf
//     names binds it; failing that, the first raw mock holding it.
//   - Echo: the recorded answer echoes the recorded value (a scalar equal to
//     it, or a string holding it as a token).
//   - Noise otherwise: not compared, its type still enforced.
//
// An empty recorded value is never followed: it carries nothing to follow.
func Classify(drift []string, live, rec Shape, at time.Time, earlier []Earlier, raw []Raw, resp []Leaf) []Learned {
	served := append([]Earlier(nil), earlier...)
	sort.SliceStable(served, func(i, j int) bool { return served[i].At.Before(served[j].At) })
	out := make([]Learned, 0, len(drift))
	for _, p := range drift {
		r, l := rec.Leaves[p], live.Leaves[p]
		ln := Learned{Kind: KindNoise, Recorded: r, Live: l}
		if r.Value != "" {
			if from, ok := appeared(searched(r.Value, l.Value), at, served, raw); ok {
				ln.Kind, ln.From = KindVariable, from
			} else if echo := echoes(r, resp); len(echo) > 0 {
				ln.Kind, ln.Echo = KindEcho, echo
			}
		}
		out = append(out, ln)
	}
	return out
}

// searched are the values a drifted leaf's recorded value may have come
// from: the whole value, and each of its words the live value does not have
// (a token minted earlier and sent in "Bearer <token>", an id in a path).
// Words are split at anything but letters, digits, '-' and '_'.
func searched(recorded, live string) []string {
	out := []string{recorded}
	liveWords := map[string]bool{}
	for _, w := range words(live) {
		liveWords[w] = true
	}
	for _, w := range words(recorded) {
		if w != recorded && !liveWords[w] {
			out = append(out, w)
		}
	}
	return out
}

func words(s string) []string {
	return strings.FieldsFunc(s, func(r rune) bool {
		return !(r >= '0' && r <= '9' || r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r == '-' || r == '_')
	})
}

// appeared is the earlier place of the test's recording that holds one of
// values: a leaf of an HTTP mock F served before (every one of them came
// earlier in the test) that F saw drift, else a raw mock served before or
// recorded before at (of which F saw no live value).
func appeared(values []string, at time.Time, served []Earlier, raw []Raw) (Place, bool) {
	for _, e := range served {
		for _, s := range e.Seen {
			// A place F saw hold its recorded value did not drift: the
			// value there is the same every run, and a value that drifted
			// here did not come from it (a "00" of a timestamp is not the
			// "00" of an earlier Date header).
			if s.Live == s.Leaf.Value {
				continue
			}
			for i, v := range values {
				if s.Leaf.Value == v || s.Leaf.Type == String && ContainsToken(s.Leaf.Value, v) {
					return Place{Mock: e.Mock, Side: s.Side, Path: s.Leaf.Path, Noise: s.Leaf.Noise,
						Var: s.Place, Live: s.Live, Whole: i == 0 && s.Leaf.Value == v, Own: e.Own}, true
				}
			}
		}
	}
	for _, x := range raw {
		if !x.Served && !x.At.Before(at) {
			continue
		}
		for _, v := range values {
			if ContainsToken(x.Text, v) {
				return Place{Mock: x.Mock}, true
			}
		}
	}
	return Place{}, false
}

// echoes are the answer leaves that hold r's recorded value: a scalar equal
// to it (WholeEcho), or a string holding it as a token.
func echoes(r Leaf, resp []Leaf) []Leaf {
	var out []Leaf
	for _, a := range resp {
		switch {
		case a.Unnamed:
		case WholeEcho(r, a):
			out = append(out, a)
		case a.Type == String && ContainsToken(a.Value, r.Value):
			out = append(out, a)
		}
	}
	return out
}

// WholeEcho reports whether answer leaf a holds r's recorded value as its
// whole value, a string, a number or a boolean (an id sent as text may come
// back as a number): a place the value can be answered at.
func WholeEcho(r, a Leaf) bool {
	if a.Unnamed || r.Value == "" || a.Value != r.Value {
		return false
	}
	switch a.Type {
	case String, Number, Bool:
		return true
	}
	return false
}

// payloadKeys are the fields of a mock's spec that say where and when it was
// recorded, not what it carried: a value found in them did not flow from the
// mock.
var payloadKeys = map[string]bool{
	"metadata": true, "created": true, "reqtimestampmock": true, "restimestampmock": true,
	"reqbodynoise": true, "correlations": true, "variables": true, "timestamp": true, "async": true,
}

// RawOf is a mock as Classify reads it when no leaf names its values: its
// recorded request and answer, the payload only, as one text.
func RawOf(m *models.Mock, served bool) Raw {
	r := Raw{Mock: m.Name, At: m.Spec.ReqTimestampMock, Served: served}
	b, err := json.Marshal(m.Spec)
	if err != nil {
		return r
	}
	var v interface{}
	if json.Unmarshal(b, &v) != nil {
		return r
	}
	b, err = json.Marshal(dropKeys(v))
	if err == nil {
		r.Text = string(b)
	}
	return r
}

// dropKeys is v without the object keys payloadKeys names, at any depth.
func dropKeys(v interface{}) interface{} {
	switch t := v.(type) {
	case map[string]interface{}:
		out := make(map[string]interface{}, len(t))
		for k, child := range t {
			if payloadKeys[strings.ToLower(k)] {
				continue
			}
			out[k] = dropKeys(child)
		}
		return out
	case []interface{}:
		out := make([]interface{}, len(t))
		for i, child := range t {
			out[i] = dropKeys(child)
		}
		return out
	}
	return v
}
