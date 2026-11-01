package learn

import (
	"slices"
	"strconv"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// Decide is learning's one entry: every word that changed between a call's
// recording and the same call in record's full round is exactly one
// WordOutcome, a Clock, a Value (bound to the earliest place that shows it,
// or fresh at its first call) or Refused, each with one line a person or a
// tool reads. It is called only under the proven switch. Until an order is
// registered (decideBy) it refuses every changed word as one keploy cannot
// check yet: it fails closed.

// WordOutcome is what a changed word is (Outcome is what L decided about a
// request).
type WordOutcome uint8

const (
	// OutcomeRefused is the zero value: set aside with its Refusal.
	OutcomeRefused WordOutcome = iota
	// OutcomeClock is a read of the application's clock, plus at most one
	// offset, checked in its call's span.
	OutcomeClock
	// OutcomeValue is bound to the earliest place of the call's scope or the
	// start-up that shows it, and checked at every later use.
	OutcomeValue
	// OutcomeFresh is bound at its first call and checked at every later use.
	OutcomeFresh
)

// RefusalKind is why a word is refused: one of the ten below.
type RefusalKind string

// The ten refusals.
const (
	RefusalPick            RefusalKind = "pick"
	RefusalNoClockRead     RefusalKind = "no-clock-read"
	RefusalTwoSources      RefusalKind = "two-sources"
	RefusalConflict        RefusalKind = "conflict"
	RefusalUnknown         RefusalKind = "unknown"
	RefusalAnotherScope    RefusalKind = "another-scope"
	RefusalUnread          RefusalKind = "unread"
	RefusalChangedShape    RefusalKind = "changed-shape"
	RefusalCannotCheck     RefusalKind = "cannot-check"
	RefusalLandedElsewhere RefusalKind = "landed-elsewhere"
)

// EditKind is an edit a person or a tool makes to accept a refused word.
// keploy never makes it itself.
type EditKind string

// The edits.
const (
	EditMoveToMocks   EditKind = "move-to-mocks"
	EditMoveToStartup EditKind = "move-to-startup"
	EditMoveToWorker  EditKind = "move-to-worker"
	EditNoiseAtWord   EditKind = "noise-at-word"
	EditNoiseAtField  EditKind = "noise-at-field"
	EditGive          EditKind = "give"
	EditReRecord      EditKind = "re-record"
	EditFixApp        EditKind = "fix-app"
	EditNone          EditKind = "none"
)

// Edit is one edit that would accept a refused word: Mock and Field name
// where, Name a given's name; Word -1 is the whole field.
type Edit struct {
	Kind  EditKind `json:"kind"`
	Mock  string   `json:"mock,omitempty"`
	Field string   `json:"field,omitempty"`
	Name  string   `json:"name,omitempty"`
	Word  int      `json:"word,omitempty"`
}

// Refusal is a refused word: its kind, where it stands, both values (as
// recorded, and in Full: record's full round), the kind's clause (which
// place, which state, which rule), at most three "mock-N <path>" places, and
// the edits that would accept it, likeliest first.
type Refusal struct {
	Kind     RefusalKind `json:"kind"`
	Test     string      `json:"test,omitempty"`
	Mock     string      `json:"mock,omitempty"`
	Field    string      `json:"field,omitempty"`
	Word     int         `json:"word,omitempty"`
	Recorded string      `json:"recorded,omitempty"`
	Full     string      `json:"full,omitempty"`
	Why      string      `json:"why,omitempty"`
	Places   []string    `json:"places,omitempty"`
	Edits    []Edit      `json:"edits,omitempty"`
}

// Mode is where a word's sources may be.
type Mode uint8

const (
	// ModeOwn: the call's own scope and the start-up.
	ModeOwn Mode = iota
	// ModeElsewhere: every earlier place of the run, for a call that landed
	// in another window; its recording is kept only where every word is a
	// Clock or noise keploy.yml declares.
	ModeElsewhere
)

// Caps is what the call's adapter can do at replay: bind a Value at a word
// address, answer an echo at one, and state what the full round's answers
// held.
type Caps struct{ BindAtWord, AnswerAtWord, FViewStated bool }

// Scope is where mappings.yaml names a mock, as models.Mapping.MockPlaces
// writes it: "test <name>" for a test's own list, "startup:", "worker:", or
// another section. ScopeUnknown: no lookup, or a mock the lookup cannot
// place (named in no place, or in more than one); a decision then reads
// Earlier's marks alone.
type Scope string

// The scopes a decision tells apart (ScopeTest names a test's own list).
const (
	ScopeUnknown Scope = ""
	ScopeStartup Scope = "startup:"
	ScopeWorker  Scope = "worker:"
)

// ScopeTest is the scope of test name's own list.
func ScopeTest(name string) Scope { return Scope("test " + name) }

// Landing is where the full round's call reached keploy.
type Landing uint8

const (
	// LandedUnknown is the zero value: the adapter did not say.
	LandedUnknown Landing = iota
	// LandedOwn: in its own test's window.
	LandedOwn
	// LandedOther: in another test's window, Landed.Test.
	LandedOther
	// LandedBetween: between tests' windows.
	LandedBetween
)

// Landed is where the full round's call reached keploy; Test names the
// other test
// of LandedOther.
type Landed struct {
	Where Landing
	Test  string
}

// String is l as a line names it.
func (l Landed) String() string {
	switch l.Where {
	case LandedOwn:
		return "its own test's window"
	case LandedOther:
		if l.Test == "" {
			return "another test's window"
		}
		return l.Test + "'s window"
	case LandedBetween:
		return "between tests"
	}
	return "a window keploy was not told"
}

// Input is one call to decide: its drifted leaves, both shapes, when it was
// recorded and when the full round's call reached keploy, the earlier
// places, the raw recordings, its answer's leaves, its window, the test's
// givens, which test and mock it is, the Mode, the adapter's Caps, the noise
// keploy.yml declares (word -1: the whole field), each mock's scope in
// mappings.yaml (nil: none known; read it through ScopeOf), where the full
// round's call landed, how the adapter names a leaf's place (nil:
// VarPlace; read it through AddressOf), and the instant a client test
// began at in its recording (RecBegan: the tracker's recorded begin, zero
// where none is known), which the test's own value must precede where no
// window tells it.
type Input struct {
	Drifts    *DriftCache
	Drift     []string
	Live, Rec Shape
	At        time.Time
	Arrived   time.Time
	RecBegan  time.Time
	Earlier   []Earlier
	Raw       []Raw
	Resp      []Leaf
	Window    *Window
	Given     GivenEarlier
	Test      string
	Mock      string
	Mode      Mode
	Caps      Caps
	Declared  func(field string, word int) bool
	Scope     func(mock string) Scope
	Landed    Landed
	PlaceOf   func(l Leaf, answer bool) (string, bool)
}

// AddressOf is the address of l, a leaf of the call's request (answer
// false) or of its answer, with word -1 (the whole leaf). in.PlaceOf names
// the place where the adapter set it (a shape with no Body, as Mongo's): a
// word address in Place's grammar (body.<path>, url.path[<n>],
// url.query.<name> or header.<name>), then any steps that decode the leaf
// (`|b64`). A string in any other form names no place. Else VarPlace names
// it: a request leaf as JSON only where the request's body is, an answer
// leaf as JSON (Resp holds a JSON answer's leaves). The zero Address (Word
// -1) where neither names a place that parses: its words are refused with
// no address. Every order builds each Decision's At and Echo here, and its
// FromAt from the earlier answer's leaf (answer true).
func (in Input) AddressOf(l Leaf, answer bool) variables.Address {
	place := func(l Leaf, answer bool) (string, bool) { return VarPlace(l, answer || in.Rec.Body != nil) }
	if in.PlaceOf != nil {
		place = in.PlaceOf
	}
	if s, ok := place(l, answer); ok {
		if a, err := variables.ParseAddress(s); err == nil {
			a.Word = -1
			return a
		}
	}
	return variables.Address{Word: -1}
}

// ScopeOf is mock's scope by in.Scope, ScopeUnknown where in has none.
func (in Input) ScopeOf(mock string) Scope {
	if in.Scope == nil {
		return ScopeUnknown
	}
	return in.Scope(mock)
}

// Decision is one changed word: where it stands, its values as recorded and
// in record's full round (Full), its WordOutcome, a Value's bind place, the
// answer places that echo it, a Clock's check (its Why set) or the Refusal,
// and its line. At reads Recorded in the recording's leaf (At.WordOf).
type Decision struct {
	At             variables.Address
	Recorded, Full string
	Outcome        WordOutcome
	From           Place
	FromAt         variables.Address
	Echo           []variables.Address
	Clock          *models.ClockOffset
	Refusal        *Refusal
	Line           string
}

// Decide is every changed word of in, decided (see above).
func Decide(in Input) []Decision { return decideBy(in) }

// The seams other parts of learning set from their own files: decideBy is
// the order a changed word is decided in; clockInWindow is a Clock the
// call's window holds and clockSinceBegin one read since its test began.
// Their defaults are main's readings, and an order that refuses every word.
var (
	decideBy        = refuseAll
	clockInWindow   = offsetReading
	clockSinceBegin = sinceTestBegin
)

// refuseAll refuses every changed word of in as one keploy cannot check.
// Both values are opened through the steps of the leaf's address, and their
// words split there: each word of a leaf whose words pair by position, else
// the opened text whole (a word added or dropped, a separator changed, a
// typed instant). Where a step does not open a side, or both open to one
// text (only the encoding changed), the field is refused whole as written,
// at its place with no step, so its line shows what changed.
func refuseAll(in Input) []Decision {
	var out []Decision
	for _, path := range in.Drift {
		rec, inRec := in.Rec.Leaves[path]
		live := in.Live.Leaves[path]
		at := in.AddressOf(rec, false)
		if !inRec { // a leaf only the full round has: its place is the full round's
			at = in.AddressOf(live, false)
		}
		r, rok := at.WordOf(rec.Value)
		f, fok := at.WordOf(live.Value)
		opened := rok && fok && r != f
		if !opened {
			at.Steps, r, f = nil, rec.Value, live.Value
		}
		n := len(out)
		if rw, lw := variables.Words(r), variables.Words(f); opened && rec.Type != Instant && live.Type != Instant && len(rw) == len(lw) && slices.Equal(apart(r, rw), apart(f, lw)) {
			for i := range rw {
				if a, b := r[rw[i][0]:rw[i][1]], f[lw[i][0]:lw[i][1]]; a != b {
					out = append(out, cannotCheck(in, path, at, i, a, b))
				}
			}
		}
		if len(out) == n {
			out = append(out, cannotCheck(in, path, at, -1, r, f))
		}
	}
	return out
}

// apart is what separates s's words ws: the text before each, then after
// the last.
func apart(s string, ws [][2]int) []string {
	out, last := make([]string, 0, len(ws)+1), 0
	for _, w := range ws {
		out, last = append(out, s[last:w[0]]), w[1]
	}
	return append(out, s[last:])
}

func cannotCheck(in Input, field string, at variables.Address, word int, r, f string) Decision {
	at.Word = word
	why := "keploy cannot check this word yet: no order decides it"
	where := in.Test + " " + in.Mock + " " + field
	if word >= 0 {
		where += " word " + strconv.Itoa(word)
	} else {
		why = "keploy cannot check this field yet: no order decides it"
	}
	edit := Edit{Kind: EditNoiseAtField, Mock: in.Mock, Field: field, Word: -1}
	return Decision{
		At: at, Recorded: r, Full: f, Outcome: OutcomeRefused,
		Refusal: &Refusal{Kind: RefusalCannotCheck, Test: in.Test, Mock: in.Mock, Field: field, Word: word, Recorded: r, Full: f, Why: why, Edits: []Edit{edit}},
		Line:    where + ": recorded " + strconv.Quote(r) + ", round F " + strconv.Quote(f) + ": " + why + ". Noise at the field until keploy can; this is keploy's limit",
	}
}
