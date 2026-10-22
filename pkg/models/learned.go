package models

import "encoding/json"

// WHAT THE LEARN STEP LEARNED OF A LEAF'S WORDS (word-level learn).
//
// Under the proven switch, record's full round F learns of a drifted request
// leaf only the words that changed between the recording (R) and F, and a
// later replay keeps checking the rest of the leaf. What it learned rides on
// the mock: as noise, under the noise block's words (MockSpec.LearnedWords,
// on disk noise.words.<place>), and for an echo or a variable on the variable
// itself (MockVariable.Words, on disk variables.<name>.words). Nothing here
// is written with the switch off, and a mock that carries none of it encodes
// as before.
//
//	noise:
//	    words:
//	        body.note:
//	            digest: 9f2c41d07a3be815
//	            layout: 4be1c09a5d3e7f21
//	            slots:
//	                - {seg: 3, kind: token}
//	                - {seg: 5, run: 0}
//	                - {seg: 7, kind: instant, layout: iso8601, skeleton: "d4-d2-d2Td2:d2:d2FZ"}
//
// How a value splits into words is package matcher/learn's (its segments,
// runs and classes): a value splits into segments at each run of structural
// characters (whitespace and " ' ` { } [ ] ( ) : , ; & < > | \ = / ? # +, a
// closed class applied byte by byte), and a segment into runs at every other
// byte that is neither a letter nor a digit, and wherever the character class
// changes. An accepted clock read is one segment and one run.

// The kinds of a WordSlot.
const (
	// WordSlotRun is a slot of one run: the run Seg.Run changed, and only a
	// run of ASCII letters and digits may stand there, its neighbouring
	// separators fixed. The run alone in its segment admits any ASCII token.
	// Written with no kind.
	WordSlotRun = ""
	// WordSlotToken is a slot of a whole segment: its runs drifted in their
	// layout, ASCII on both sides, so any ASCII token may stand there.
	WordSlotToken = "token"
	// WordSlotInstant is a slot of an accepted clock read: segment Seg is the
	// read, in the clock layout Layout, written as Skeleton.
	WordSlotInstant = "instant"
)

// WordSlot is one place of a leaf where the learn step saw words change, and
// the only place a later replay lets them change (WordsEntry).
type WordSlot struct {
	// Seg is the segment the slot is in, counted from 0 in the recorded
	// value, after its accepted clock reads collapse.
	Seg int `json:"seg" yaml:"seg" bson:"seg"`
	// Run is the run within segment Seg, counted from 0, for a slot of one
	// run (WordSlotRun); 0 for every other kind.
	Run int `json:"run" yaml:"run" bson:"run"`
	// Kind is WordSlotRun, WordSlotToken or WordSlotInstant. A kind this
	// build does not know voids the entry.
	Kind string `json:"kind,omitempty" yaml:"kind,omitempty" bson:"kind,omitempty"`
	// Layout is, for an instant, the name of the clock layout the read is in
	// (one of matcher/learn's declared layouts); "" for every other kind.
	Layout string `json:"layout,omitempty" yaml:"layout,omitempty" bson:"layout,omitempty"`
	// Skeleton is, for an instant, the recorded match written in classes:
	// d<length> for each digit run, W for a weekday's name, M for a month's,
	// D for the padded day, F for a fraction (absent on either side or not),
	// N for a zone's name from the declared list, and every other character
	// as itself. "" for every other kind.
	Skeleton string `json:"skeleton,omitempty" yaml:"skeleton,omitempty" bson:"skeleton,omitempty"`
}

// wordSlotDoc is a WordSlot as it is written: the run only for a slot of one
// run, or when it is set on another kind (a malformed slot keeps it, so the
// entry stays void when read back).
type wordSlotDoc struct {
	Seg      int    `json:"seg" yaml:"seg"`
	Run      *int   `json:"run,omitempty" yaml:"run,omitempty"`
	Kind     string `json:"kind,omitempty" yaml:"kind,omitempty"`
	Layout   string `json:"layout,omitempty" yaml:"layout,omitempty"`
	Skeleton string `json:"skeleton,omitempty" yaml:"skeleton,omitempty"`
}

func (s WordSlot) doc() wordSlotDoc {
	d := wordSlotDoc{Seg: s.Seg, Kind: s.Kind, Layout: s.Layout, Skeleton: s.Skeleton}
	if s.Kind == WordSlotRun || s.Run != 0 {
		run := s.Run
		d.Run = &run
	}
	return d
}

// MarshalYAML writes the slot with its run only where the run names one
// (wordSlotDoc).
func (s WordSlot) MarshalYAML() (interface{}, error) { return s.doc(), nil }

// MarshalJSON writes the slot as MarshalYAML does.
func (s WordSlot) MarshalJSON() ([]byte, error) { return json.Marshal(s.doc()) }

// WordsEntry is what the learn step learned of one leaf's words: the words
// that changed between the recording and record's full round, as slots, and
// the two guards that tie the slots to the value they were learned of. A
// replay compares every byte of the leaf outside the slots, and each slot as
// its kind says (matcher/learn.WordsAdmit). An entry whose guards no longer
// hold, or that is malformed in any way, is void, and the leaf is compared
// whole: it fails closed.
type WordsEntry struct {
	// Digest is the first 16 hex digits of the sha256 of the recorded leaf
	// in its learn form.
	Digest string `json:"digest" yaml:"digest" bson:"digest"`
	// Layout is the first 16 hex digits of the sha256 of the recorded
	// value's segments, runs, classes and collapsed clock-read spans.
	Layout string `json:"layout" yaml:"layout" bson:"layout"`
	// Slots are the places that may change, sorted by Seg, then Run. A
	// segment holds one token or instant slot, or slots of its runs.
	Slots []WordSlot `json:"slots" yaml:"slots" bson:"slots"`
}

// Clone returns a copy of e that shares no slice with it.
func (e WordsEntry) Clone() WordsEntry {
	if e.Slots != nil {
		e.Slots = append(make([]WordSlot, 0, len(e.Slots)), e.Slots...)
	}
	return e
}

// cloneWords returns a copy of words that shares no map or slice with it;
// nil for nil.
func cloneWords(words map[string]WordsEntry) map[string]WordsEntry {
	if words == nil {
		return nil
	}
	out := make(map[string]WordsEntry, len(words))
	for place, e := range words {
		out[place] = e.Clone()
	}
	return out
}

// The learn step's files of a test set (keploy/<set>/). Neither is written
// with the proven switch off, and both are git-ignored before either is
// written (matcher/learn.EnsureLearnedIgnored).
const (
	// LearnedFile is learned.yaml, every leaf the set's mocks learned
	// (LearnedLeaf) with the recorded words, derived from the mocks
	// (matcher/learn.Legible). It is never uploaded: the mocks carry what
	// it is derived from.
	LearnedFile = "learned.yaml"
	// LearnedLocalFile is learned.local.yaml, record's full round's words of
	// the same leaves, by ID. It stays on the machine that recorded.
	LearnedLocalFile = "learned.local.yaml"
	// LearnedTempPrefix starts the name of a learned file being written,
	// renamed into place once whole.
	LearnedTempPrefix = ".learned-"
)

// LearnBasis is one piece of evidence a learned leaf rests on. The set is
// closed: a reader (a person, an LLM) acts on its values.
type LearnBasis string

// The bases.
const (
	// BasisChanged: record's full round changed these runs of the leaf and
	// kept every other byte of it.
	BasisChanged LearnBasis = "changed"
	// BasisToken: a segment's runs drifted in their layout, ASCII on both
	// sides, so the segment is one token: nothing inside it is checked.
	BasisToken LearnBasis = "token"
	// BasisClockRead: an instant both runs read of their clocks, moved by
	// the full round's lead or within the call's own window.
	BasisClockRead LearnBasis = "clock-read"
	// BasisEcho: the recorded answer echoes the value: a variable of this
	// call alone.
	BasisEcho LearnBasis = "echo"
	// BasisEarlier: the value appeared earlier in the test, and is followed
	// from there.
	BasisEarlier LearnBasis = "earlier"
	// BasisWhole: every run of the leaf changed, so the whole leaf is noise.
	BasisWhole LearnBasis = "whole"
)

// LearnRule is the rule that decided a learned leaf. The set is closed.
type LearnRule string

// The rules.
const (
	// RuleNoise: the leaf's slots are not compared; every other byte is.
	RuleNoise LearnRule = "noise"
	// RuleWholeNoise: the whole leaf is not compared, its type kept
	// (noise.req). A leaf learned whole before keploy learned words reads
	// "re-record to tighten".
	RuleWholeNoise LearnRule = "whole-noise"
	// RuleEcho: a variable of this call alone, bound at its request and
	// answered with the live value; its slots may change at the bind.
	RuleEcho LearnRule = "echo"
	// RuleVariable: a variable followed from where it first appeared in the
	// test; its slots may change at the bind.
	RuleVariable LearnRule = "variable"
)

// LearnedLeaf is one leaf learned onto a recording, as learned.yaml (and,
// with F's words, learned.local.yaml) says it to a reader: where it is, what
// was recorded, what was learned of it and on what evidence, and the leaf
// written with each slot marked. Values are redacted and cut where the
// learned files redact (matcher/learn.RedactLearnedValue).
type LearnedLeaf struct {
	// ID is stable across runs of one recording: the hash of the set, the
	// mock, the path and the words' digest. learned.local.yaml merges by it.
	ID string `json:"id" yaml:"id"`
	// Mock is the name of the mock the leaf is learned on.
	Mock string `json:"mock" yaml:"mock"`
	// Variable names the variable whose words these are (RuleEcho,
	// RuleVariable); "" for noise.
	Variable string `json:"variable,omitempty" yaml:"variable,omitempty"`
	// Path is the leaf's exact place: body.<json path> (array positions
	// bracketed), header.<name>, url.query.<name>, url.path.<i>, and a
	// form's or a multipart body's field.
	Path string `json:"path" yaml:"path"`
	// Recorded is the leaf's recorded value.
	Recorded string `json:"recorded" yaml:"recorded"`
	// F is the leaf's value in record's full round: only in
	// learned.local.yaml.
	F     string       `json:"f,omitempty" yaml:"f,omitempty"`
	Basis []LearnBasis `json:"basis" yaml:"basis"`
	Rule  LearnRule    `json:"rule" yaml:"rule"`
	// Why is the item's plain sentence: what was learned, and how to undo
	// it.
	Why string `json:"why" yaml:"why"`
	// Slots are the leaf's words entry's slots (WordsEntry.Slots); none for
	// RuleWholeNoise.
	Slots []WordSlot `json:"slots,omitempty" yaml:"slots,omitempty"`
	// Rendered is Path = Recorded with each slot written ⟨changes⟩.
	Rendered string `json:"rendered" yaml:"rendered"`
}
