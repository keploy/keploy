package learn

import (
	"strconv"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// Decide is learning's one entry: every word that changed between a call's
// recording (R) and round F is exactly one WordOutcome, a Clock, a Value
// (bound to the earliest place that shows it, or fresh at its first call)
// or Refused, each with one line a person or a tool reads. It is called
// only under the proven switch. Until an order is registered (decideBy) it
// refuses every changed word as one keploy cannot check yet: it fails
// closed.

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

// RefusalKind is why a word is refused: one of the nine below.
type RefusalKind string

// The nine refusals.
const (
	RefusalPick         RefusalKind = "pick"
	RefusalNoClockRead  RefusalKind = "no-clock-read"
	RefusalTwoSources   RefusalKind = "two-sources"
	RefusalConflict     RefusalKind = "conflict"
	RefusalUnknown      RefusalKind = "unknown"
	RefusalAnotherScope RefusalKind = "another-scope"
	RefusalUnread       RefusalKind = "unread"
	RefusalChangedShape RefusalKind = "changed-shape"
	RefusalCannotCheck  RefusalKind = "cannot-check"
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
	Kind              EditKind
	Mock, Field, Name string
	Word              int
}

// Refusal is a refused word: its kind, where it stands, both values (as
// recorded, and in Full: record's full round, round F), the kind's clause (which place,
// which state, which rule), at most three "mock-N <path>" places, and the
// edits that would accept it, likeliest first.
type Refusal struct {
	Kind              RefusalKind
	Test, Mock, Field string
	Word              int
	Recorded, Full    string
	Why               string
	Places            []string
	Edits             []Edit
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
// address, answer an echo at one, and state what round F's answers held.
type Caps struct{ BindAtWord, AnswerAtWord, FViewStated bool }

// Scope is where mappings.yaml names a mock, as models.Mapping.MockPlaces
// writes it: "test <name>" for a test's own list, "startup:", "worker:", or
// another section. ScopeUnknown: no lookup, or a mock named in no place or
// in more than one; a decision then reads Earlier's marks alone.
type Scope string

// The scopes a decision tells apart (ScopeTest names a test's own list).
const (
	ScopeUnknown Scope = ""
	ScopeStartup Scope = "startup:"
	ScopeWorker  Scope = "worker:"
)

// ScopeTest is the scope of test name's own list.
func ScopeTest(name string) Scope { return Scope("test " + name) }

// ScopesOf is the scope lookup (Input.Scope) of mappings.yaml m, one rule
// for every adapter.
func ScopesOf(m *models.Mapping) func(mock string) Scope {
	places := m.MockPlaces()
	return func(mock string) Scope {
		if p := places[mock]; len(p) == 1 {
			return Scope(p[0])
		}
		return ScopeUnknown
	}
}

// Landing is where round F's call reached keploy.
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

// Landed is where round F's call reached keploy; Test names the other test
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
// recorded and when round F's call reached keploy, the earlier places, the
// raw recordings, its answer's leaves, its window, the test's givens, which
// test and mock it is, the Mode, the adapter's Caps, the noise keploy.yml
// declares (word -1: the whole field), each mock's scope in mappings.yaml
// (nil: none known; read it through ScopeOf), and where round F's call
// landed.
type Input struct {
	Drifts    *DriftCache
	Drift     []string
	Live, Rec Shape
	At        time.Time
	Arrived   time.Time
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
}

// ScopeOf is mock's scope by in.Scope, ScopeUnknown where in has none.
func (in Input) ScopeOf(mock string) Scope {
	if in.Scope == nil {
		return ScopeUnknown
	}
	return in.Scope(mock)
}

// Decision is one changed word: where it stands, its recorded and round F
// (Full: record's full round) values, its WordOutcome, a Value's bind place, the answer places that echo
// it, a Clock's check (its Why set) or the Refusal, and its line.
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

// The seams v5's other parts set from their own files: decideBy is the
// order (DESIGN §3); clockInWindow is a Clock the call's window holds and
// clockSinceBegin one read since its test began. Their defaults are main's
// readings, and an order that refuses every word.
var (
	decideBy        = refuseAll
	clockInWindow   = offsetReading
	clockSinceBegin = sinceTestBegin
)

// refuseAll refuses every changed word of in as one keploy cannot check:
// each word of a drifted leaf whose words pair by position, else the leaf
// whole (a word added or dropped, a separator changed, a typed instant).
func refuseAll(in Input) []Decision {
	var out []Decision
	for _, path := range in.Drift {
		rec, live := in.Rec.Leaves[path], in.Live.Leaves[path]
		at := variables.Address{Word: -1}
		if s, ok := VarPlace(rec, in.Rec.Body != nil); ok {
			if a, err := variables.ParseAddress(s); err == nil {
				at = a
			}
		}
		n := len(out)
		if rw, lw := variables.Words(rec.Value), variables.Words(live.Value); rec.Type != Instant && live.Type != Instant && len(rw) == len(lw) && apart(rec.Value, rw) == apart(live.Value, lw) {
			for i := range rw {
				r, f := rec.Value[rw[i][0]:rw[i][1]], live.Value[lw[i][0]:lw[i][1]]
				if r != f {
					out = append(out, cannotCheck(in, path, at, i, r, f))
				}
			}
		}
		if len(out) == n {
			out = append(out, cannotCheck(in, path, at, -1, rec.Value, live.Value))
		}
	}
	return out
}

// apart is s with each of its words cut out: what separates them.
func apart(s string, ws [][2]int) string {
	b, last := make([]byte, 0, len(s)), 0
	for _, w := range ws {
		b = append(append(b, s[last:w[0]]...), 0)
		last = w[1]
	}
	return string(append(b, s[last:]...))
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
