package models

import "sort"

// WHAT THE LEARN STEP LEARNED OF A LEAF'S WORDS (word-level learn).
//
// Under the proven switch, record's full round F learns of a drifted request
// leaf only the words that changed between the recording (R) and F, and a
// later replay keeps checking the rest of the leaf. What it learned rides on
// the mock: as noise, under the noise block's words (MockSpec.LearnedWords,
// on disk noise.words.<place>), and for an echo or a variable on the variable
// itself (MockVariable.Words, on disk variables.<name>.words). Nothing here
// is learned with the switch off, and a mock that carries none of it encodes
// as before.
//
//	noise:
//	    words:
//	        body.note:
//	            v: 1
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
//
// The format is versioned (WordsEntry.V, WordsVersion). Its rule for every
// decoder, encoder and rewrite (a prune, a noise persist, an edit), whatever
// the switch says: read an entry and write it back, a key this build does not
// know included (learned_codec.go); only its use is gated on the switch. A
// variable's words ride with the variable, and noise.words with the noise
// block (platform/yaml's NewDocNoise and ResolveWords, used by every mock
// decoder and encoder). An entry this build cannot read admits nothing
// (WordsEntry.Unreadable), so its leaf is compared whole: it fails closed.

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

// WordsVersion is the version of the words format this build writes and
// reads (WordsEntry.V). How a value splits into words (the structural
// characters, the run classes, the clock layouts and the zone table, all of
// which the layout guard hashes) is the version's: a build that splits
// otherwise writes a new version and keeps reading this one as it was, so an
// upgrade never voids what an older keploy learned.
const WordsVersion = 1

// Why an entry this build cannot read admits nothing (WordsEntry.Unreadable).
const (
	// WordsWrittenByNewer: the entry is of a version other than
	// WordsVersion, or it (or one of its slots) holds a key this build does
	// not know.
	WordsWrittenByNewer = "written by a newer keploy; upgrade"
	// WordsUnversioned: the entry has no version (no v, or v: 0), so it was
	// not written as keploy writes words.
	WordsUnversioned = "the learned words have no version (v); delete the entry or re-record"
)

// WordsRaw is a value the words format carries without reading it, as
// compact JSON (which YAML reads too): the reserved seen's value
// (WordsEntry.Seen), or, as one JSON object by key, the keys of an entry or a
// slot this build does not know (WordsEntry.Unknown, WordSlot.Unknown). ""
// is none. It is a string, so a WordSlot stays comparable, and YAML, JSON and
// gob each carry it whole: a rewrite by this build writes back what a newer
// keploy wrote, after the keys it knows (learned_codec.go).
type WordsRaw string

// WordSlot is one place of a leaf where the learn step saw words change, and
// the only place a later replay lets them change (WordsEntry).
type WordSlot struct {
	// Seg is the segment the slot is in, counted from 0 in the recorded
	// value, after its accepted clock reads collapse.
	Seg int `json:"seg" yaml:"seg" bson:"seg"`
	// Run is the run within segment Seg, counted from 0, for a slot of one
	// run (WordSlotRun); 0 for every other kind. It is written only where
	// it names one: for a slot of one run always (run 0 too), and on
	// another kind only when it is set (a malformed slot keeps it, so the
	// entry stays void when read back).
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
	// Unknown is the slot's keys this build does not know, carried as
	// written (WordsRaw); "" for none. A slot with any voids its entry
	// (WordsWrittenByNewer).
	Unknown WordsRaw `json:"-" yaml:"-" bson:"unknown,omitempty"`
}

// WordsEntry is what the learn step learned of one leaf's words: the words
// that changed between the recording and record's full round, as slots, and
// the two guards that tie the slots to the value they were learned of. A
// replay compares every byte of the leaf outside the slots, and each slot as
// its kind says (matcher/learn.WordsAdmit). An entry whose guards no longer
// hold, that this build cannot read (Unreadable), or that is malformed in any
// way, is void, and the leaf is compared whole: it fails closed.
type WordsEntry struct {
	// V is the version of the words format the entry is written in:
	// WordsVersion for every entry this build writes. Of an entry of any
	// other version only V is read; the rest is carried whole in Unknown,
	// and the entry admits nothing (WordsWrittenByNewer).
	V int `json:"v" yaml:"v" bson:"v"`
	// Digest is the first 16 hex digits of the sha256 of the recorded leaf
	// in its learn form.
	Digest string `json:"digest" yaml:"digest" bson:"digest"`
	// Layout is the first 16 hex digits of the sha256 of the recorded
	// value's segments, runs, classes and collapsed clock-read spans.
	Layout string `json:"layout" yaml:"layout" bson:"layout"`
	// Slots are the places that may change, sorted by Seg, then Run. A
	// segment holds one token or instant slot, or slots of its runs.
	Slots []WordSlot `json:"slots" yaml:"slots" bson:"slots"`
	// Seen is reserved for what record's full round saw of the leaf, which
	// a later keploy writes once, at F, for its UI to show. This build
	// writes none, carries one as written (WordsRaw), and never compares
	// it: it voids nothing and admits nothing.
	Seen WordsRaw `json:"seen,omitempty" yaml:"seen,omitempty" bson:"seen,omitempty"`
	// Unknown is the entry's keys this build does not know, carried as
	// written (WordsRaw): of an entry of this version, a key beside the
	// known ones (which voids it, WordsWrittenByNewer); of an entry of any
	// other version, every key but v. "" for none.
	Unknown WordsRaw `json:"-" yaml:"-" bson:"unknown,omitempty"`
}

// Unreadable is why this build cannot read e, so that e admits nothing and
// its leaf is compared whole: WordsWrittenByNewer when e is of a version
// other than WordsVersion or holds a key, its own or a slot's, this build
// does not know; WordsUnversioned when it has no version. "" when this build
// reads it. Seen is never read, so it makes no entry unreadable. Whether a
// readable entry's guards and slots hold is matcher/learn's (WordsAdmit).
func (e WordsEntry) Unreadable() string {
	if e.V != WordsVersion {
		if e.V == 0 && !e.Unknown.has("v") {
			return WordsUnversioned
		}
		return WordsWrittenByNewer
	}
	if e.Unknown != "" {
		return WordsWrittenByNewer
	}
	for _, s := range e.Slots {
		if s.Unknown != "" {
			return WordsWrittenByNewer
		}
	}
	return ""
}

// Clone returns a copy of e that shares no slice with it.
func (e WordsEntry) Clone() WordsEntry {
	if e.Slots != nil {
		e.Slots = append(make([]WordSlot, 0, len(e.Slots)), e.Slots...)
	}
	return e
}

// WordsVoidLayout is the layout guard of a void entry (VoidWords): the learn
// step learned the leaf twice, of two values or two layouts, so neither
// entry's slots say what may change, and the leaf is compared whole.
const WordsVoidLayout = "void"

// VoidWords is the void entry of a leaf whose recorded value's digest is
// digest: it admits nothing, so the leaf is compared whole (fails closed),
// and stays void through every later merge (MergeWordsEntry).
func VoidWords(digest string) WordsEntry {
	return WordsEntry{V: WordsVersion, Digest: digest, Layout: WordsVoidLayout, Slots: []WordSlot{}}
}

// Void reports whether e is a void entry (VoidWords).
func (e WordsEntry) Void() bool { return e.Layout == WordsVoidLayout }

// MergeWordsEntry is two entries the learn step learned of one leaf as one
// (neither is changed).
//
// An entry this build cannot read (Unreadable: a newer keploy's, or one with
// no version) is kept as written, on whichever side it is: this build cannot
// tell what its slots say, so it merges nothing into it and drops nothing of
// it (its unknown keys, its Seen), and it admits nothing, so its leaf is
// compared whole, until a keploy that reads it does. Of two such entries,
// a's is kept. A merge never makes an unreadable entry readable.
//
// Entries of one recorded value in one layout (the same digest and the same
// layout guard) union their slots: what either saw change may change. Any
// other pair, or a pair of which either is void, is void (VoidWords): the two
// runs did not read the leaf alike, and its slots would say nothing reliable
// of it, so it is compared whole. Either way Seen, never read, is a's, else
// b's.
func MergeWordsEntry(a, b WordsEntry) WordsEntry {
	switch {
	case a.Unreadable() != "":
		return a.Clone()
	case b.Unreadable() != "":
		return b.Clone()
	}
	seen := a.Seen
	if seen == "" {
		seen = b.Seen
	}
	if a.Void() || b.Void() || a.Digest != b.Digest || a.Layout != b.Layout {
		digest := a.Digest
		if digest == "" {
			digest = b.Digest
		}
		out := VoidWords(digest)
		out.Seen = seen
		return out
	}
	out := a.Clone()
	out.Slots = SortWordSlots(append(out.Slots, b.Slots...))
	out.Seen = seen
	return out
}

// SortWordSlots is slots sorted by Seg, then Run, then Kind, without repeats,
// and with a segment's run slots left out where a token or an instant slot
// takes the whole segment: the order and form WordsEntry.Slots are written
// in. slots is reused.
func SortWordSlots(slots []WordSlot) []WordSlot {
	whole := map[int]bool{}
	for _, s := range slots {
		if s.Kind == WordSlotToken {
			whole[s.Seg] = true
		}
	}
	out := slots[:0]
	for _, s := range slots {
		if s.Kind == WordSlotRun && whole[s.Seg] {
			continue
		}
		out = append(out, s)
	}
	sort.SliceStable(out, func(i, j int) bool {
		a, b := out[i], out[j]
		if a.Seg != b.Seg {
			return a.Seg < b.Seg
		}
		if a.Run != b.Run {
			return a.Run < b.Run
		}
		if a.Kind != b.Kind {
			return a.Kind < b.Kind
		}
		if a.Layout != b.Layout {
			return a.Layout < b.Layout
		}
		return a.Skeleton < b.Skeleton
	})
	dedup := out[:0]
	for i, s := range out {
		if i > 0 && s == out[i-1] {
			continue
		}
		dedup = append(dedup, s)
	}
	if dedup == nil {
		dedup = []WordSlot{}
	}
	return dedup
}

// MergeWords is two leaves-to-words maps (MockSpec.LearnedWords) as one
// (neither is changed): a place in both is MergeWordsEntry of the two, a
// place in one keeps its entry. nil when both are empty.
func MergeWords(a, b map[string]WordsEntry) map[string]WordsEntry {
	if len(a) == 0 && len(b) == 0 {
		return nil
	}
	out := make(map[string]WordsEntry, len(a)+len(b))
	for place, e := range a {
		out[place] = e.Clone()
	}
	for place, e := range b {
		if cur, ok := out[place]; ok {
			out[place] = MergeWordsEntry(cur, e)
			continue
		}
		out[place] = e.Clone()
	}
	return out
}

// MergeVariableWords is the words of one variable two blocks declare (a, b:
// either nil), as MergeVariables keeps them for a variable both bind at one
// place: one side's when the other has none (a side without words never
// strips them), else MergeWordsEntry of the two. Neither is changed.
func MergeVariableWords(a, b *WordsEntry) *WordsEntry { return mergeVariableWords(a, b) }

func mergeVariableWords(a, b *WordsEntry) *WordsEntry {
	switch {
	case a == nil && b == nil:
		return nil
	case a == nil:
		w := b.Clone()
		return &w
	case b == nil:
		w := a.Clone()
		return &w
	}
	w := MergeWordsEntry(*a, *b)
	return &w
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

// LearnedDocument is a learned file (LearnedFile, LearnedLocalFile) as it is
// written and read: the set's learned leaves, in matcher/learn.Legible's
// order, under one key, so every writer (keploy's record, and the enterprise
// verbs' pull, status and copy-back) writes one shape.
type LearnedDocument struct {
	Learned []LearnedLeaf `json:"learned" yaml:"learned"`
}

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
	// BasisWhole: every run of the leaf changed, so the whole leaf is noise:
	// how keploy learns a leaf of a kind it learns no words of yet (Mongo,
	// Postgres; design r9 §5).
	BasisWhole LearnBasis = "whole"
	// BasisBeforeWords: the leaf was learned whole (noise.req), or the
	// variable bound whole (no words), before keploy learned words, of a
	// kind it now learns words of: a re-record learns only the words that
	// change ("re-record to tighten", design r9 N6). What changed of it then
	// is not known.
	BasisBeforeWords LearnBasis = "before-words"
)

// LearnRule is the rule that decided a learned leaf. The set is closed.
type LearnRule string

// The rules.
const (
	// RuleNoise: the leaf's slots are not compared; every other byte is.
	RuleNoise LearnRule = "noise"
	// RuleWholeNoise: the whole leaf is not compared, its type kept
	// (noise.req). A leaf learned whole before keploy learned words
	// (BasisBeforeWords) reads "re-record to tighten"; one of a kind keploy
	// learns only whole (BasisWhole) does not.
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
