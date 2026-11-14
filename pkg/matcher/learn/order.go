package learn

import (
	"cmp"
	"maps"
	"math"
	"regexp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// The order: every changed word of a call, read by the one
// reader, is decided here. A word that is not an instant is a Value bound at
// the earliest source of the call's scope or the start-up; refused where a
// place disproves it by its text or leaves it unknown, or where its only
// source lies outside that scope (naming the edit); else made anew each
// run, fresh. An instant no earlier place holds is a Clock where its call's
// window holds it. Every case this file does not decide yet is refused: it
// fails closed.

func init() { decideBy = decideOrder }

// decideOrder is Decide's order: Mode Elsewhere's guard around the words
// decided with every earlier place in scope, or the call's own order.
func decideOrder(in Input) []Decision {
	if in.Mode == ModeElsewhere {
		ds, fields := decideBoth(in, true)
		out := elsewhere(in, ds, fields)
		(&decider{in: in}).ruled(out, fields)
		return out
	}
	ds, _ := decideBoth(in, false)
	return ds
}

// decideBoth decides in's words as main reads its exchanges served leaf by
// leaf (classifyGiven). One recorded after the call gave the recorded
// application nothing before it: it is read as a recording F gave before
// the call, outside the call's own test, so only what F served there
// counts, never its leaves as a place both runs were given
// (leafedAfterCall). One recorded at the call's own instant, or at a time
// keploy cannot read (timedEarlier, timedRaw), may have come before the
// call or after it: the call is read both ways (leafedAtCall), and a word
// either reading refuses is refused, as is one the two readings learn
// differently, naming both (bothWays).
func decideBoth(in Input, allScope bool) ([]Decision, []string) {
	in.Earlier, in.Raw = timedEarlier(arrivedBefore(ofOwnCall(in, in.Earlier), in.At, in.Arrived), in.At), timedRaw(in.Raw, in.At)
	in.Earlier, in.Raw = leafedAfterCall(in.Earlier, in.Raw, in.At)
	ds, fields := decideWords(in, allScope)
	kept, late := leafedAtCall(in.Earlier, in.At)
	if len(late) == 0 {
		return ds, fields
	}
	alt := in
	alt.Earlier, alt.Raw = kept, append(append([]Raw(nil), in.Raw...), late...)
	as, afields := decideWords(alt, allScope)
	return bothWays(in, ds, fields, as, afields), fields
}

// bothWays is ds, the call read with its exchanges at the call's own
// instant as earlier places of its test, with each word the reading as
// recordings made after the call (as) refuses, or learns otherwise,
// refused.
func bothWays(in Input, ds []Decision, fields []string, as []Decision, afields []string) []Decision {
	// A word is keyed by its field, its steps and its index: two texts one
	// leaf opens to number their words each from 0.
	key := func(f string, a variables.Address) string {
		for _, st := range a.Steps {
			f += "|" + string(st)
		}
		return f + "\x00" + strconv.Itoa(a.Word)
	}
	late := make(map[string]Decision, len(as))
	for i, a := range as {
		late[key(afields[i], a.At)] = a
	}
	dc := &decider{in: in}
	names := [...]string{"refused", "a read of the clock", "a value bound to an earlier place", "a value made anew each run"}
	for i, d := range ds {
		a, ok := late[key(fields[i], d.At)]
		if !ok {
			a, ok = late[key(fields[i], variables.Address{Word: -1})]
		}
		switch {
		case !ok || d.Outcome == OutcomeRefused || a.Outcome == d.Outcome && a.Outcome != OutcomeRefused:
		case a.Outcome == OutcomeRefused:
			rf := *a.Refusal
			rf.Word, rf.Recorded, rf.Full = d.At.Word, d.Recorded, d.Full
			rf.Why += "; read as an earlier place of the test instead, it would be " + names[d.Outcome] + ", yet keploy refuses a value either reading refuses"
			hideSecrets(&rf)
			ds[i] = Decision{At: d.At, Recorded: d.Recorded, Full: d.Full, Refusal: &rf, Line: RefusalLine(rf)}
		default:
			ds[i] = dc.refuse(fields[i], d.At, d.At.Word, d.Recorded, d.Full, RefusalUnknown,
				"an exchange recorded at the call's own instant read as an earlier place of the test makes it "+names[d.Outcome]+
					", read as a recording made after the call "+names[a.Outcome]+": keploy cannot tell which the application did",
				nil, dc.fieldNoise(fields[i]))
		}
	}
	return ds
}

// decider is one call's state: its input, places, index and scope.
type decider struct {
	in       Input
	ps       []evPlace
	x        *readerIndex
	allScope bool
	// unread names what of the call keploy could not read, "" where none.
	unread string
	// byWords are the client tests' requests keploy could not read that keep
	// their words (placesOf): unread for a value whose change one may hold
	// (unreadByWords).
	byWords []Earlier
	// noPayload is payloadless(in): an exchange before the call whose
	// payload keploy has not read, and why; its mock "" where none.
	noPayload struct{ mock, why string }
	// dr is the call's drift cache, or an empty one.
	dr *DriftCache
	// set: the recordings the set read once (Input.Given); op: what each
	// recording outside the call's scope holds (outside), read once.
	set []GivenPlaces
	op  *offsetPlaces
	// oh: the call's ask of the set's index of op's word lists (held),
	// built at its first ask; pi: its ask of the set's postings of its
	// places' words (places), built at its first ask.
	oh *outsideHeld
	pi *placeIndex
	// mintedIDs: by an id's long words (mintedKey), mintedAnew's answer, read
	// once for every piece of the id the call decides.
	mintedIDs map[string]bool
	// longInTurn: by a word's recorded and round F text, the recording a word
	// the call made was bound at as a value sent in either order
	// (Decision.EitherOrder): mintedLong reads such a word as made anew, and
	// longGlued reads nothing of it at that recording's own place.
	longInTurn map[string]string
	// readClock: the refusal instantLearned last gave is one where learning
	// read the value as the application's clock (heldRead), which the time
	// default settles on any field (timeDefault).
	readClock bool
	// around: the instant instantLearned last refused at a word of a text
	// that changed around it, checked by its form at its recorded word under
	// the time default (amongBinds), which text keeps only where every other
	// changed word of the text is put back at replay.
	around *Decision
	// encoded is the leaf's inDecodedForm, read once and only where a word
	// of it would be learned fresh.
	encoded func() (string, *evPlace)
	// unchecked is why main's readings cannot read the leaf whole in a form
	// they check (valueDrift.unparsed), read once and only where a word of
	// it would be learned fresh; "" where they can.
	unchecked func() string
	// passes counts the instants atFinest reads, each read counted where
	// it is made, and walked the bytes it walks to place them: what keeps
	// its work linear in them.
	passes, walked int
	// asked counts the call's asks of heldElsewhere: one for each moment,
	// cut and text its answers ask of a read (instantEchoed), however many
	// instants hold them, so an answer of many is linear in them.
	asked int
	// halved: each place's halves, by its index in ps, kept for the call
	// (halves); laid: the fields of each half kept, laidNow of each built
	// past the budget for the instant being decided. halfBuilds counts the
	// places halved, halfKept and halfNow are the bytes of the halves kept
	// and of the instant's, and halfPeak the most of both at once.
	halved                      map[int][2]evPlace
	laid, laidNow               map[*readerText][]rawField
	halfBuilds                  int
	halfKept, halfNow, halfPeak int64
	// answer is the call's answer index (answerIndex), built at its first
	// ask.
	answer *answerWords
	// texts: each place's texts as shownInText reads them, by its index in
	// ps, opened at the call's first read of the place and kept for the
	// call (textsOf); clients: Earlier.incoming by mock, as clientCase
	// reads it, built at the first place that holds a '.'. opened counts
	// the places opened so, decoded the texts percent-decoded, clientMaps
	// the builds of clients, and inText the places shownInText read for a
	// bind (fromAt): what keeps its work for a call of many fields one open
	// of each place and one build of clients, not one for each field or
	// place.
	texts                               map[int]*placeTexts
	clients                             map[string]bool
	opened, decoded, clientMaps, inText int
	// unreadFrom is the count of unread texts x held as this decider began:
	// a Reader's index serves several Decides, and each refuses only for a
	// recording it asked about itself (unreadHere).
	unreadFrom int
}

// unreadHere reports whether this decider asked about a recording keploy
// could not read (readerIndex.unread, counted since it began).
func (dc *decider) unreadHere() bool { return dc.x.unreadTexts > dc.unreadFrom }

// decideWords decides every changed word of in, and the field each
// decision is of; allScope reads every earlier place of the run as the
// call's scope (Mode Elsewhere).
func decideWords(in Input, allScope bool) ([]Decision, []string) {
	// The call's Reader where its caller gives one (it lets it go), so
	// the call's Decides read each text into its views once.
	x := in.Reader.index(in.Drifts)
	if x == nil {
		x = newReaderIndex(in.Drifts)
		defer x.release() // what the index keeps goes back to the set's budget
	}
	return newDecider(in, allScope, x).decide()
}

// newDecider is the decider of the call in, reading its texts through x;
// allScope as decideWords takes it.
func newDecider(in Input, allScope bool, x *readerIndex) *decider {
	ps, unread, byWords := placesOf(in)
	set, setNotRead := setUnread(in)
	dc := &decider{in: in, unreadFrom: x.unreadTexts, x: x, allScope: allScope, set: set, dr: in.Drifts, unread: cmp.Or(unread, setNotRead), byWords: byWords}
	dc.ps = withTexts(x, ps)
	dc.noPayload.mock, dc.noPayload.why = payloadless(in)
	if dc.dr == nil {
		dc.dr = NewDriftCache()
	}
	return dc
}

// decide decides every changed word of dc's call, and the field each
// decision is of (decideWords).
func (dc *decider) decide() ([]Decision, []string) {
	var out []Decision
	var fields []string // fields[i] is the field out[i] decides
	for _, path := range dc.in.Drift {
		ds := dc.leaf(path, dc.unread)
		if src, ok := dc.source(path); ok && src.Kind == SourceMinted {
			ds = dc.minted(path, src, ds)
		} else if ok {
			ds = dc.clockOnly(path, src, ds)
		}
		out = append(out, ds...)
		for len(fields) < len(out) {
			fields = append(fields, path)
		}
	}
	if dc.noPayload.why != "" {
		// A word made anew may have been built on the answer of an
		// exchange keploy holds no payload of: refused, as main's word rule
		// refuses it, before one value's other uses bind to it (a read of
		// the clock: clock).
		for i, d := range out {
			if d.Outcome == OutcomeFresh {
				out[i] = dc.noPayloadRefusal(fields[i], d.At, d.At.Word, d.Recorded, d.Full)
				out[i].AnsweredAfter = d.AnsweredAfter
			}
		}
	}
	out, fields = dc.mintedUses(out, fields)
	dc.oneValue(out, fields)
	if dc.unreadHere() {
		// A recording keploy could not read again: every change of the
		// call is refused.
		for i, d := range out {
			out[i] = dc.refuse(fields[i], d.At, d.At.Word, d.Recorded, d.Full, RefusalUnread,
				dc.unreadWhy(), nil,
				Edit{Kind: EditReRecord}, dc.fieldNoise(fields[i]))
		}
	}
	dc.ruled(out, fields)
	return out, fields
}

// oneValue makes one new value the call sent at several places one
// variable: bound at its first place, made anew each run, and used at the
// others, each use naming the first in its line, so a replay that sends
// the places apart misses. A value recorded at several places that round F
// sent as two different new values is two reads keploy cannot tell apart:
// each such place is refused, naming the first. Each keeps the recordings
// it set aside as answered after the call. A place a minted rule decided
// (Decision.Minted) is the first wherever it stands, so the rule's check
// binds the value and the others use it.
func (dc *decider) oneValue(out []Decision, fields []string) {
	first := map[string]int{}
	for i, d := range out {
		if _, ok := first[d.Recorded]; !ok && d.Minted != nil {
			first[d.Recorded] = i
		}
	}
	name := func(i int) string {
		if w := out[i].At.Word; w >= 0 {
			return dc.in.Mock + " " + fields[i] + " word " + strconv.Itoa(w)
		}
		return dc.in.Mock + " " + fields[i]
	}
	for i, d := range out {
		j, ok := first[d.Recorded]
		switch {
		case d.Outcome != OutcomeFresh || ok && j == i:
		case !ok:
			first[d.Recorded] = i
		case out[j].Outcome == OutcomeFresh && out[j].Full == d.Full:
			out[i] = dc.learned(fields[i], d.At, d.At.Word, d.Recorded, d.Full, OutcomeValue,
				"used the value bound at "+name(j)+", made anew in this call and sent at both places in both runs", []string{name(j)})
			out[i].From = Place{Mock: dc.in.Mock, Side: Request, Path: fields[j], Live: d.Full, Whole: out[j].At.Word < 0}
			out[i].FromAt = out[j].At
			out[i].AnsweredAfter = d.AnsweredAfter
		default:
			why := "the recorded value stands at " + name(j) + " and " + name(i) + ", and round F sent " + strconv.Quote(out[j].Full) + " and " + strconv.Quote(d.Full) + " there: two values, and keploy cannot tell which one the recording's is"
			for _, k := range []int{j, i} {
				if out[k].Outcome == OutcomeFresh {
					aa := out[k].AnsweredAfter
					out[k] = dc.refuse(fields[k], out[k].At, out[k].At.Word, out[k].Recorded, out[k].Full, RefusalTwoSources, why,
						[]string{name(j), name(i)}, dc.fieldNoise(fields[k]))
					out[k].AnsweredAfter = aa
				}
			}
		}
	}
}

// unnamedWhy refuses a leaf no path names alone (Leaf.Unnamed): its key
// holds '.', '[' or ']', or its document holds its key twice (a BSON
// document may). Its keploy.yml noise names it, and also the other field
// its path spells, which the line says.
const unnamedWhy = "no path names this field alone: its key holds '.', '[' or ']', or its document holds that key twice, so its noise in keploy.yml also names the other field its path spells"

// unreadByWords names the first of dc.byWords that may hold the change of
// a value from rec to live (mayHold), as placesOf names a place keploy
// could not read; "" where none may.
func (dc *decider) unreadByWords(rec, live Leaf) string {
	var c valueChange
	for i, e := range dc.byWords {
		if i == 0 {
			c = dc.dr.changeOf(rec, live)
		}
		if e.unreadWords.mayHold(c) {
			return unreadName(e.Mock, e) + " (" + e.Unread + ")"
		}
	}
	return ""
}

// leaf decides the changed words of the drifted leaf at path.
func (dc *decider) leaf(path, unread string) []Decision {
	rec, live := dc.in.Rec.Leaves[path], dc.in.Live.Leaves[path]
	at := dc.in.AddressOf(rec, false)
	if unread == "" {
		unread = dc.unreadByWords(rec, live)
	}
	switch {
	case unread != "":
		return []Decision{dc.refuse(path, at, -1, rec.Value, live.Value, RefusalUnread,
			"keploy could not read "+unread+", which came before this call", nil,
			Edit{Kind: EditReRecord}, dc.fieldNoise(path))}
	case rec.Unnamed || live.Unnamed:
		return []Decision{dc.cannotCheck(path, at, -1, rec.Value, live.Value, unnamedWhy)}
	case rec.Type == Instant || live.Type == Instant:
		r, okR := readTyped(rec)
		l, okL := readTyped(live)
		iw, _ := readTypedPair(rec, live)
		if !okR || !okL || iw == nil || iw.Rec == nil || iw.Live == nil {
			return []Decision{dc.cannotCheck(path, at, -1, rec.Value, live.Value, "one run's value here is no instant")}
		}
		return []Decision{dc.instant(path, at, -1, changedInstant{r: rec.Value, l: live.Value, ri: r, li: l, rm: iw.Rec, lm: iw.Live})}
	}
	return dc.leafText(path, at, rec.Value, live.Value)
}

// leafText decides the changed words of a drifted text leaf, recorded rec
// and live in round F: opened first by the steps its adapter names at its
// place (at.Steps, which readLeaf opens), then by each step the reader
// takes (readLeaf: percent escapes, a JSON string, base64, a JWT's claims,
// a cookie's pairs), and read word by word in each text opened, so an
// encoded instant is read as the instant it is. A step of the adapter's that does not open both runs,
// or opens them to one text (only the encoding changed), refuses the field
// whole as written, at its place with no step. A refusal of the field
// whole (bare: its place, no step) ends it, as written.
func (dc *decider) leafText(path string, at variables.Address, rec, live string) []Decision {
	bare := variables.Address{Place: at.Place, Word: -1}
	r, rok := at.WordOf(rec)
	f, fok := at.WordOf(live)
	switch {
	case len(at.Steps) == 0:
	case !rok || !fok:
		return []Decision{dc.cannotCheck(path, bare, -1, rec, live, "a decode step its adapter names here ("+at.String()+") does not open both runs")}
	case r == f:
		return []Decision{dc.cannotCheck(path, bare, -1, rec, live, "only its encoding changed")}
	}
	var out []Decision
	between := textReading{Rec: rec, Live: live}
	rds := readLeaf(dc.in.Drifts, at, rec, live)
	dc.encoded = sync.OnceValues(func() (string, *evPlace) {
		if why, p := dc.inDecodedForm(rds); p != nil {
			return why, p
		}
		return dc.movedInAForm(rds)
	})
	dc.unchecked = sync.OnceValue(func() string { return dc.dr.ofIn(rec, live, dc.in.Window, dc.in.At).unparsed })
	for _, rd := range rds {
		if len(between.Separators) == 0 {
			between = rd
		}
		ds, whole := dc.text(path, bare, rec, live, rd)
		if out = append(out, ds...); whole {
			return out
		}
	}
	if len(out) == 0 && rec != live {
		out = append(out, dc.cannotCheck(path, bare, -1, rec, live, betweenWords(between)))
	}
	return out
}

// betweenWords is the reason a text whose words did not change is refused:
// the text between them that did, named (CJK or accented text holds none
// of keploy's words, so all of it is between them), first three.
func betweenWords(rd textReading) string {
	seps := rd.Separators
	if len(seps) == 0 {
		seps = []sepChange{{RecAt: [2]int{0, len(rd.Rec)}, LiveAt: [2]int{0, len(rd.Live)}}}
	}
	var named []string
	for _, sc := range seps[:min(len(seps), maxLinePlaces)] {
		named = append(named, strconv.Quote(rd.Rec[sc.RecAt[0]:sc.RecAt[1]])+" to "+strconv.Quote(rd.Live[sc.LiveAt[0]:sc.LiveAt[1]]))
	}
	return "the text between its words changed, " + strings.Join(named, " and ") + " (keploy's words are runs of ASCII letters and digits)"
}

// text decides the changed words of rd, one text the leaf at bare opens;
// whole reports a refusal of the field whole, which ends the leaf.
func (dc *decider) text(path string, bare variables.Address, rec, live string, rd textReading) ([]Decision, bool) {
	if n := len(rd.At.Steps); n > 0 && rd.At.Steps[n-1] == variables.StepJWTSig {
		return dc.signature(path, bare, rec, live, rd)
	}
	if d, ok := dc.oneToken(path, bare, rec, live, rd); ok {
		return []Decision{d}, true
	}
	if d, ok := dc.wholeCopy(path, bare, rec, live, rd); ok {
		return []Decision{d}, true
	}
	if rd.ChangedShape {
		why := "a word was added or dropped"
		if slices.ContainsFunc(rd.Separators, func(sc sepChange) bool { return len(sc.Tokens) == 0 }) {
			why = betweenWords(rd)
		}
		return []Decision{dc.refuse(path, bare, -1, rec, live, RefusalChangedShape, why, nil, dc.fieldNoise(path))}, true
	}
	var out []Decision
	// arounds: the reads in out (by index) refused because the text changed
	// around them, each with its check by form (dc.around).
	type aroundAt struct {
		at int
		d  Decision
	}
	var arounds []aroundAt
	done := map[*instantWord]bool{}
	// A piece of one id is decided after the other words, since it asks the
	// verdicts of the id's long words (mintedLong); it keeps its place in
	// out. fresh: the words, by index into rd.Words, learned fresh.
	type piece struct {
		at, k int
		long  []int
	}
	var pieces []piece
	var whole *Decision
	fresh := map[int]bool{}
words:
	for k, w := range rd.Words {
		switch {
		case w.Instant != nil:
			if done[w.Instant] {
				continue
			}
			done[w.Instant] = true
			dc.around = nil
			out = append(out, dc.textInstant(path, bare, rec, live, rd, w))
			if dc.around != nil {
				arounds = append(arounds, aroundAt{at: len(out) - 1, d: *dc.around})
				dc.around = nil
			}
		case w.RecIdx < 0 || w.LiveIdx < 0:
			d := dc.unpaired(path, bare, rec, live, rd)
			whole = &d
			break words
		default:
			if long := idLongWords(rd, w); len(long) > 0 {
				pieces = append(pieces, piece{at: len(out), k: k, long: long})
				out = append(out, Decision{})
				continue
			}
			d := dc.word(path, bare, rec, live, rd.Rec, w, pieceOfAnId(rd, w), nil)
			fresh[k] = d.Outcome == OutcomeFresh
			if d.EitherOrder {
				// Made at the call too, and sent at another call in the
				// other order in each run: that call's request is where the
				// word was bound (a Value: wordBy sets the flag on nothing
				// else), and it gave the application nothing.
				fresh[k] = true
				if dc.longInTurn == nil {
					dc.longInTurn = map[string]string{}
				}
				dc.longInTurn[w.Rec+"\x00"+w.Live] = d.From.Mock
			}
			out = append(out, d)
		}
	}
	for _, p := range pieces {
		w := rd.Words[p.k]
		out[p.at] = dc.word(path, bare, rec, live, rd.Rec, w, pieceOfAnId(rd, w), mintedLong(rd, fresh, p.long))
	}
	if whole != nil {
		return append(out, *whole), true
	}
	for _, a := range arounds {
		if allPutBack(out, a.at) {
			out[a.at] = a.d
		}
	}
	return out, false
}

// allPutBack reports whether every decision of out, a text's changed words,
// but the one at skip is a word the replay puts its recorded value back at
// (Caps.ClockAmongBinds): a Value or a Fresh value a variable binds there.
// A read of the clock, a refusal, or any other word is not put back, so the
// text around the read at skip would not compare as recorded.
func allPutBack(out []Decision, skip int) bool {
	for i, d := range out {
		if i != skip && (d.Refusal != nil || d.Outcome != OutcomeValue && d.Outcome != OutcomeFresh) {
			return false
		}
	}
	return true
}

// mintedLong is long (idLongWords, indexes into rd.Words) as words where
// this call made every one of them anew: each was learned fresh (fresh),
// no place before the call showing it, or bound only at another call that
// sent it too, in the other order in each run (Decision.EitherOrder, read
// as fresh: that call's request gave the application nothing, and the
// either-order rule refuses where any other place decides the word, so no
// place before the call showed it either). Only then is the id one the call
// minted, so a place that shows none of its long words may hold its piece
// by chance (idChance, oneRun). nil where one is bound at a source, refused, or not
// decided (it stands past a word one run lacks, which refuses the field
// whole), or is an instant's word (the run clock's, read whole by its
// moment): an id built from a value the application took may hold a piece
// it took elsewhere too, so the piece is read as for any word (fail
// closed).
func mintedLong(rd textReading, fresh map[int]bool, long []int) []readWord {
	ws := make([]readWord, 0, len(long))
	for _, k := range long {
		if !fresh[k] {
			return nil
		}
		ws = append(ws, rd.Words[k])
	}
	return ws
}

// tokenBits is the fewest bits (wordBits) the changed part of a URL-safe
// base64 token carries in each run for keploy to read it as one random
// value, about eleven characters: a short id whose separator changed
// ("order-17_x" to "order-42-x") is no such token.
const tokenBits = 64

// oneToken decides a field that is one base64 token in each run, of the
// URL-safe alphabet whose '-' and '_' moved or the standard one whose '+'
// and '/' did (tokenAlphabet). They are letters of that alphabet there, not
// separators, so the runs' words do not pair and replay could never write
// them: the token is one value, read as a word whole. A word of 32 bits or
// more that an earlier place holds, where no place shows the whole token,
// may be carried inside it, so it is left to the word rules.
//
// Where the runs' words happen to pair, the token is still one value when
// it is a standard base64 encoder's output in both runs (whole groups of
// four, the same '=' padding: a 256-bit key with no '+' or '/' in either
// run, or one '+' in each at another spot) and every one of its words
// changed: learned word by word, the next run's key, whose '+' and '/'
// fall elsewhere, could never be written back. Unless a place shows the
// whole token, no earlier place may hold any of its words, of any size: a
// word handed to the call or sent before may be carried inside it. It is
// one value too, of either alphabet and whatever stayed in it, where an
// earlier place shows it whole in both runs and it binds whole at a place
// whose text at the bind address is the token in both runs (boundWhole):
// the call copied it whole, and bound
// word by word ("kepreq_" and one random run in each run, no '-' or '_' in
// it), the next run's random part, split by its own '-' and '_', could
// never be written back (a replay puts the recorded word back and keeps
// the rest of the new token). Bound whole, the replay holds the whole
// token to the value bound at that place: a leaf that holds it whole, or a
// longer text that holds it as one of its tokens (fromAt's |token.<n>, or
// |urltoken.<n> in a URL's path, through |pct behind escapes: a consent
// URL, a Location header). A place that shows it only so, whose base64 runs
// hold no run of it (shownInText: "/requests/<id>", "code=<key>" escaped),
// shows it too, and the token stands there only where it binds whole: where
// the reader's words see no earlier place of it (an escaped key), it is
// bound at the earliest such place of the call's scope, never fresh. Any
// other paired text, one no earlier
// place shows whole or one read whole that binds nowhere so (a refusal, a
// text no address reads it whole in), with a word that stayed ("user-"
// before an id) or of the URL-safe alphabet, whose '-' and '_' separate an
// id's parts as often as they are letters of a token, is read word by word.
// An unpaired token follows the same rule: a Value at an earlier leaf
// stands only where its bind address reads the token whole in both runs
// (never the whole URL that holds it, which no next run's token could
// equal); else ok is false, and the refusals after this decide it. ok is
// false where the field is no such token
// (randomToken: two alphabets mixed, another length or padding, a run that
// decodes to text).
func (dc *decider) oneToken(path string, bare variables.Address, rec, live string, rd textReading) (Decision, bool) {
	if len(rd.Instants) > 0 || len(bare.Steps) > 0 || !randomToken(rec, live) {
		return Decision{}, false
	}
	whole := readWord{At: bare, Rec: rec, Live: live, Tokens: []wholeToken{{Kind: tokenB64, Rec: rec, Live: live}}}
	shown := slices.ContainsFunc(dc.ps, func(p evPlace) bool { return tokenShown(placedFor(p, rec), whole) })
	// Shown only inside a longer text whose base64 runs hold it in no run of
	// its own (a URL's path, an escaped query): it stands only where it
	// binds whole there.
	var inText []evPlace
	if !shown {
		inText = dc.shownInText(rec, live)
		shown = len(inText) > 0
	}
	held := func(least float64) bool {
		return slices.ContainsFunc(rd.Words, func(w readWord) bool {
			return slices.ContainsFunc(dc.ps, func(p evPlace) bool {
				return wordBits(w.Rec) >= least && evHolds(p.t.rec, w.Rec) || wordBits(w.Live) >= least && evHolds(p.t.live, w.Live)
			})
		})
	}
	paired := !rd.ChangedShape && len(rd.Separators) == 0 && !slices.ContainsFunc(rd.Words, func(w readWord) bool { return w.RecIdx < 0 || w.LiveIdx < 0 })
	copied := false
	if paired && (!stdGroups(rec, live) || len(rd.Words) != len(rd.RecWords) || len(rd.Words) != len(rd.LiveWords) || !shown && held(0)) {
		if !shown {
			return Decision{}, false
		}
		copied = true
	}
	if !shown && held(chanceBits) {
		return Decision{}, false
	}
	d := dc.word(path, bare, rec, live, rec, whole, false, nil)
	if d.Outcome == OutcomeFresh {
		// The reader's words and runs see no earlier place of it (a standard
		// key behind escapes, "%2B" and "%2F" for its '+' and '/'), but a
		// place shows it whole: it is bound at the earliest such place of
		// the call's scope, as a whole copy is (wholeCopy). With none, it
		// is never fresh: the rules after this decide it.
		if own := slices.DeleteFunc(inText, func(p evPlace) bool { return !dc.allScope && p.otherTest }); len(own) > 0 {
			d = dc.bound(path, bare, -1, changedInstant{r: rec, l: live, rec: rec}, own, ", the earliest place before the call that shows it whole in both runs")
		}
	}
	if (copied || len(inText) > 0 || d.Outcome == OutcomeValue) && !dc.boundWhole(d, rec, live) {
		// Read whole, the token binds at no address that reads it whole
		// in both runs: a paired one is left to its words, an unpaired
		// one to the rules after this.
		return Decision{}, false
	}
	return d, true
}

// boundWhole reports whether d binds the token rec (live in round F) whole
// at a place whose text at d's bind address is the token in both runs: a
// leaf that holds it whole, or a longer text whose token, jwtsig or cookie
// step there reads it, never the longer text whole, which the next run's
// token could never equal. A recording's place (p.raw: Decide names no
// address of it) binds so too: its adapter names the place of the
// recording that holds the value whole or as one word, or refuses it. Any
// other place is held to what its address reads: one with no leaf reads an
// empty text, which no token is. A token is read whole (oneToken) only
// so: any other Value of it whole is no bind, and a paired token's other
// verdicts leave it to its words.
func (dc *decider) boundWhole(d Decision, rec, live string) bool {
	return d.Outcome == OutcomeValue && slices.ContainsFunc(dc.ps, func(p evPlace) bool {
		if p.mock != d.From.Mock || p.path != d.From.Path || p.side != d.From.Side {
			return false
		}
		return p.raw > 0 || readsWhole(d.FromAt, p, rec, live)
	})
}

// readsWhole reports whether a, an address of p, an exchange's place,
// reads rec in its recorded text and live in round F's: a text the address
// does not open reads "", which no token is.
func readsWhole(a variables.Address, p evPlace, rec, live string) bool {
	r, _ := a.WordOf(p.leaf.Value)
	l, _ := a.WordOf(p.live)
	return r == rec && l == live
}

// shownInText is the earlier exchanges' places, each one that changed
// between the runs, placed for rec (placedFor), that show the token rec
// (live in round F) whole inside a longer text in both runs, at the address
// fromAt names for it there (a URL-safe token of a URL's path, a key read
// through |pct): the shapes tokenShown's whole runs miss, a run of the
// base64 alphabets taking in a path's '/' and an escape's '%' ending one. A
// place's texts (as its address opens them) are read only where they hold
// the longest word of each run's token, as written or percent-decoded (an
// escape may hide a letter of it); a client test's request's, also where
// they hold a '.': a JWT there (a Bearer token, a cookie pair's) may hold it
// in a claim (fromAt's claimWord, cookieToken), base64url in the text. Each
// place's texts are opened and decoded once for the call (textsOf), however
// many fields it decides; a field reads a place for a bind (fromAt) only
// where both its texts may show the token so.
func (dc *decider) shownInText(rec, live string) []evPlace {
	rw, lw := longestWord(rec), longestWord(live)
	var out []evPlace
	for i, p := range dc.ps {
		if p.raw > 0 || p.leaf.Path == "" {
			continue
		}
		if t := dc.textsOf(i, p); !t.rec.mayShow(rw, t.client) || !t.live.mayShow(lw, t.client) {
			continue
		}
		dc.inText++
		if q := placedFor(p, rec); q.state == stateBefore && !q.same() && readsWhole(dc.fromAt(q, rec, live), q, rec, live) {
			out = append(out, q)
		}
	}
	return out
}

// placeTexts is a place's two texts, as recorded and as round F saw them,
// as its address opens them (decider.textsOf); client: the place is a
// client test's request's (clientCase), read only where a text holds a '.'.
type placeTexts struct {
	rec, live openedText
	client    bool
}

// openedText is a text a place's address opens: pct is the text
// percent-decoded, once, where it holds an escape that decodes, and "" else
// (where mayShow's w is "", a token with no word, the text as written holds
// it already); dot, whether it holds a '.'.
type openedText struct {
	text, pct string
	dot       bool
}

// mayShow reports whether o may show a token whose longest word is w at an
// address fromAt names that reads it whole: o holds w as written (a word,
// a token step, a JWT's signature), or decoded (|pct, an escape hiding a
// letter of it), or, a client test's request's, o holds a '.' (a JWT whose
// claim holds it: claimWord, cookieToken). Every such address reads the
// token in one of those texts, so a text that shows none of them is never
// read for it.
func (o openedText) mayShow(w string, client bool) bool {
	return strings.Contains(o.text, w) || strings.Contains(o.pct, w) || client && o.dot
}

// textsOf is p's texts as shownInText reads them, p the i-th of dc.ps:
// opened at p's address, decoded where they hold an escape, at the call's
// first read of p, and kept for the call.
func (dc *decider) textsOf(i int, p evPlace) *placeTexts {
	if t, ok := dc.texts[i]; ok {
		return t
	}
	open := func(s string) openedText {
		o := openedText{text: s, dot: strings.Contains(s, ".")}
		if strings.Contains(s, "%") {
			dc.decoded++
			o.pct, _ = variables.Decode(variables.StepPct, s)
		}
		return o
	}
	a := dc.in.AddressOf(p.leaf, p.side == Response)
	r, _ := a.WordOf(p.leaf.Value)
	l, _ := a.WordOf(p.live)
	t := &placeTexts{rec: open(r), live: open(l)}
	if t.rec.dot || t.live.dot {
		if dc.clients == nil {
			dc.clientMaps++
			dc.clients = map[string]bool{}
			for _, e := range dc.in.Earlier {
				if _, ok := dc.clients[e.Mock]; !ok {
					dc.clients[e.Mock] = e.incoming
				}
			}
		}
		t.client = dc.clients[p.mock]
	}
	if dc.texts == nil {
		dc.texts = map[int]*placeTexts{}
	}
	dc.texts[i] = t
	dc.opened++
	return t
}

// longestWord is s's longest word (variables.Words), the first of that
// length: "" where s holds none.
func longestWord(s string) string {
	w := ""
	for _, at := range variables.Words(s) {
		if at[1]-at[0] > len(w) {
			w = s[at[0]:at[1]]
		}
	}
	return w
}

// signature decides rd, the signatures of two JWTs (the jwtsig step,
// jwtSigSpan). A signature is checked by its form only (the founder's
// 2026-10-31 ruling; variables.SignatureForm: base64url of one length), so
// it is one value, read whole as oneToken reads a token: bound where an
// earlier place shows it, else made anew at the call and bound there,
// never word by word, whose count changes with its '-' and '_'. Nothing
// is carried inside a signature, so no word of it is asked apart. Two of
// another form (another length: the header's algorithm, compared exactly,
// changed with it) are refused as a changed shape, the field whole.
func (dc *decider) signature(path string, bare variables.Address, rec, live string, rd textReading) ([]Decision, bool) {
	if !variables.SignatureForm(rd.Rec, rd.Live) {
		return []Decision{dc.refuse(path, bare, -1, rec, live, RefusalChangedShape,
			"its JWT's signature changed form (keploy checks a signature by its form: base64url of one length)", nil, dc.fieldNoise(path))}, true
	}
	whole := readWord{At: rd.At, Rec: rd.Rec, Live: rd.Live, Tokens: []wholeToken{{Kind: tokenB64, Rec: rd.Rec, Live: rd.Live}}}
	return []Decision{dc.word(path, bare, rec, live, rd.Rec, whole, false, nil)}, false
}

// randomToken reports whether rec and live are one value of one generator:
// a run of one base64 alphabet of one length in each run (tokenAlphabet),
// whose changed part, past what they share at each end, carries tokenBits in
// both, read by its letters and digits. Text keploy can name is no such
// value: hex groups (a UUID's, with or without a word beside them) and a run
// that decodes to text (a name of a script keploy does not split into words,
// inside base64 JSON).
func randomToken(rec, live string) bool {
	hexGroups := func(s string) bool { return strings.Trim(strings.ToLower(s), "-_0123456789abcdef") == "" }
	if len(rec) != len(live) || !tokenAlphabet(rec, live) || hexGroups(rec) || hexGroups(live) {
		return false
	}
	head := 0
	for head < min(len(rec), len(live)) && rec[head] == live[head] {
		head++
	}
	r, l := rec[head:], live[head:]
	for len(r) > 0 && len(l) > 0 && r[len(r)-1] == l[len(l)-1] {
		r, l = r[:len(r)-1], l[:len(l)-1]
	}
	return wordBits(lettersOf(r)) >= tokenBits && wordBits(lettersOf(l)) >= tokenBits && b64Text(rec, nil) == "" && b64Text(live, nil) == ""
}

// lettersOf is s without the standard base64 alphabet's '+' and '/', which
// wordBits would count as 8 bits each: a token's bits are its letters' and
// digits'.
func lettersOf(s string) string {
	return strings.Map(func(c rune) rune {
		if c == '+' || c == '/' {
			return -1
		}
		return c
	}, s)
}

// tokenAlphabet reports whether rec and live are both of one base64
// alphabet: the URL-safe one ('-' and '_' among the letters), or the
// standard one ('+' and '/'), whole groups of four characters there with the
// same '=' padding in each run (Go's base64.StdEncoding, a 256-bit key's 44
// characters). A text that mixes the two alphabets is neither.
func tokenAlphabet(rec, live string) bool {
	const letters = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	if strings.Trim(rec, "-_"+letters) == "" && strings.Trim(live, "-_"+letters) == "" {
		return true
	}
	return stdGroups(rec, live)
}

// stdGroups reports whether rec and live are both of the standard base64
// alphabet ('+' and '/'), whole groups of four characters with the same '='
// padding in each run, at most two: what Go's base64.StdEncoding writes.
func stdGroups(rec, live string) bool {
	const letters = "0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	r, l := strings.TrimRight(rec, "="), strings.TrimRight(live, "=")
	return len(rec)%4 == 0 && len(rec)-len(r) <= 2 && len(rec)-len(r) == len(live)-len(l) &&
		strings.Trim(r, "+/"+letters) == "" && strings.Trim(l, "+/"+letters) == ""
}

// wholeCopy decides a text whose two runs' words do not pair (a word was
// added or dropped, or the text between them changed, rd.ChangedShape) as
// one value where an earlier leaf holds the field whole in both runs: the
// recorded text whole in R and round F's whole in F (a client_id of
// base64url parts, sent back as it was given, whose '-' and '_' fall
// elsewhere in each run). Word by word replay could never write it back;
// whole, it is that leaf's value in each run, bound at the earliest such
// leaf of the call's scope placed before it, as a word one place shows in
// both runs is (wordBy). A text that holds an instant is left to the
// instant's rules, and one no such leaf holds to the refusals after this:
// ok is false there.
//
// The leaves read are an exchange's (p.rec and p.live: a recording's place
// keeps its Raw, never a text), so stateBefore alone places one before the
// call in both runs: an exchange sent after the call, or one round F served
// only after the call reached keploy, is not before (earlierState). The
// call's answer reads back no whole text (readBack reads back a word inside
// a longer value), so no echo is answered here, as none is for a whole word
// wordBy binds.
func (dc *decider) wholeCopy(path string, bare variables.Address, rec, live string, rd textReading) (Decision, bool) {
	if len(rd.Instants) > 0 || !rd.ChangedShape && !slices.ContainsFunc(rd.Words, func(w readWord) bool { return w.RecIdx < 0 || w.LiveIdx < 0 }) {
		return Decision{}, false
	}
	var src []evPlace
	for _, p := range dc.ps {
		if p.state == stateBefore && (dc.allScope || !p.otherTest) && p.rec == rec && p.live == live {
			src = append(src, p)
		}
	}
	if len(src) == 0 {
		return Decision{}, false
	}
	return dc.bound(path, bare, -1, changedInstant{r: rec, l: live, rec: rec}, src, ", the earliest place before the call that holds it whole in both runs"), true
}

// unpaired decides a text where a word was added or dropped: refused whole
// as unknown where a place keploy reads holds a word of what changed (it
// may be a source of part of it), else as a changed shape.
func (dc *decider) unpaired(path string, at variables.Address, rec, live string, rd textReading) Decision {
	held := slices.DeleteFunc(slices.Clone(dc.ps), func(p evPlace) bool {
		return !slices.ContainsFunc(rd.Words, func(w readWord) bool {
			return w.Rec != "" && evHolds(p.t.rec, w.Rec) || w.Live != "" && evHolds(p.t.live, w.Live)
		})
	})
	if len(held) == 0 {
		return dc.refuse(path, at, -1, rec, live, RefusalChangedShape, "a word was added or dropped", nil, dc.fieldNoise(path))
	}
	return dc.refuse(path, at, -1, rec, live, RefusalUnknown,
		"keploy cannot tell what round F gave at "+held[0].name+" before this call (a word was added or dropped beside one it holds)", names(held),
		Edit{Kind: EditReRecord}, dc.fieldNoise(path))
}

// evHolds reports whether t holds the word w, read or not.
func evHolds(t *readerText, w string) bool {
	h, _ := t.holdsWord(w)
	return h
}

// unreadFloorBits is the wordBits under which a piece of a longer value
// (pieceOfAnId) is no evidence at a recording round F never served: one or
// two characters of any class (two of letters and digits carry 10.3 bits), or
// three digits. A piece that short stands all over a set, so it names no
// source. A 4-hex word there stays refused as unknown, as does a word of any
// size that is the field's value, or stands beside words that stayed: that may
// be the very value the recording gave ("50", "user-12").
const unreadFloorBits = 11

// pieceOfAnId reports whether w is under chanceBits and another changed word of
// its text carries them in both runs ("08c8" of a UUID, "k" of
// "k-Rq7Lm2Xv9Tp4Wn8Zc"): the text is one value, and w a piece of it.
func pieceOfAnId(rd textReading, w readWord) bool {
	return wordBits(w.Rec) < chanceBits && wordBits(w.Live) < chanceBits && slices.ContainsFunc(rd.Words, func(o readWord) bool {
		return wordBits(o.Rec) >= chanceBits && wordBits(o.Live) >= chanceBits
	})
}

// idLongWords are the words that judge a place for w where w is a piece of
// one id: the other changed words of the whole id token around it (tokenID,
// a run of letters, digits, '-' and '_' in both runs) that carry chanceBits
// in both runs, as a v4 UUID's 32-bit and 48-bit groups do for its 16-bit
// ones, as indexes into rd.Words. An application makes or copies such an
// id whole, so where the call made the id's long words anew (mintedLong), a
// place that shows nothing of them in either run is no source of the id:
// its holding the piece is chance (the 4-hex groups of api-server's fresh
// UUIDs stand all over its sets), unless each run holds its own piece at a
// place it had, where the application may have cut the piece alone, or
// one run's piece stands at a place the other run was not given before the
// call (oneRun). nil where w carries chanceBits itself, stands in no such token,
// or its long words stand outside its token ("qty 12 ref <random>": 12 is
// no piece of the ref, and stays judged by its own text).
func idLongWords(rd textReading, w readWord) []int {
	if wordBits(w.Rec) >= chanceBits || wordBits(w.Live) >= chanceBits {
		return nil
	}
	i := slices.IndexFunc(w.Tokens, func(t wholeToken) bool { return t.Kind == tokenID })
	if i < 0 {
		return nil
	}
	tok := w.Tokens[i]
	within := func(at, span [2]int) bool { return span[0] <= at[0] && at[1] <= span[1] }
	// w itself carries fewer bits, and a word one run lacks has no text
	// there: neither is one of them. An instant's word is one, and is never
	// learned fresh, so an id that holds one is no id the call minted.
	var long []int
	for k, o := range rd.Words {
		if wordBits(o.Rec) >= chanceBits && wordBits(o.Live) >= chanceBits && within(o.RecAt, tok.RecAt) && within(o.LiveAt, tok.LiveAt) {
			long = append(long, k)
		}
	}
	return long
}

// idChance reports, for w, a piece of one id the call minted (long:
// mintedLong), whether the place at i holds it as chance would. It is no
// value the test gave keploy, nor a body that declares no media type (the
// test made the one, and keploy cannot tell the words of the other from
// its framing). It shows nothing of any of the id's long words in either
// run (idShownAt is false, every text read), and holds the piece only
// inside longer tokens of its own, never as a whole id token
// (pieceWholeAt). The application makes an id whole, so the place cannot
// be where it took the id, nor a piece of it: a value of that place's own
// ("<code>-<random>" built from a place's "<code>") stands there whole. It
// may still be where the application cut the piece out of another value,
// at one place or at another in each run: the piece is chance there only
// where one run's own piece stands at no place that run had, and the other
// run's only at places the first was given before the call (oneRun),
// asked apart. A place that shows or disproves the piece itself is read as
// for any word. false for every place where long is empty. Each place is
// asked once, on demand.
func (dc *decider) idChance(ps *wordPlaces, w readWord, long []readWord) func(i int) bool {
	if len(long) == 0 {
		return func(int) bool { return false }
	}
	// known: by place, made at the first ask, so a pass that asks no
	// place costs nothing per place of the call.
	var known map[int]int8
	return func(i int) bool {
		if known == nil {
			known = map[int]int8{}
		}
		k, ok := known[i]
		if !ok {
			p := *ps.at(i)
			k = 1
			if p.gave || p.undeclared || pieceWholeAt(p, w.Rec, w.Live) || idShownAt(p, long) {
				k = -1
			}
			known[i] = k
		}
		return k > 0
	}
}

// oneRun reports, for w, a piece of one id the call minted, why no place
// holds it as chance would, "" where one may: one run's application made
// its piece with the id, and the other run's piece stands only at places
// the first run was given before the call, so its application had each to
// cut the piece from and did not. A run's holders: the recording's, the
// recorded piece in the text of a place not answered after the call
// (recAfter), or in the answer of a set recording given by then
// (givenBefore: recorded and answered by then, where an answer with no
// time counts, or recorded at a time keploy cannot read); round F's, at any
// time, its piece in the text of a place, in a view its serves gave there,
// or in any list of a set recording (its answer, F's view of it, a view
// its serves gave). The application runs the same code in each run, so one
// that cut the piece out of a value it was given finds its own piece at a
// place in each run, at one place or at another in each: where both runs
// hold theirs, keploy cannot tell that from chance (fail closed,
// bothRunsWhy). Where one run's piece stands at no place, that run's
// application cut it from nothing it was given, so it made it, with the
// id; but that shows nothing of a place it was not given before the call,
// so each place the other run's piece stands at must be one it was:
//   - round F made its piece: F gave the application each recording holder
//     before the call, with a text keploy reads there (fGave, not
//     fUnstated: no view unstated, unread, or one of several that differ;
//     an outside recording by givenWords.fRead, never one F served only
//     after the call reached keploy, fAfter);
//   - the recording made its piece: it gave the application each of F's
//     holders before the call, by times keploy reads (rBefore,
//     answeredBefore), or round F gave it only after the call (not fGave),
//     so neither run's application had it to cut from.
//
// Else the piece is refused as unknown (oneSideWhy), naming the place and
// which of round F's two reasons holds there (fNotBefore, fNoView). It
// is so at each place idChance or idOutside left. held[k] is what
// asked[k]'s texts hold of w (heldAt), set for every asked place before
// the first ask; a place the word's check does not ask holds neither word
// and has no views (a place with views is read always, showsUntold), so it
// is not read. inR and inF are the set recordings' lists that hold R's and
// F's piece (outsideHeld.holding). Asked once, on demand.
func (dc *decider) oneRun(ps *wordPlaces, w readWord, asked []int32, held []evHeld, inR, inF map[int32]heldRoles) func() string {
	known, why := false, ""
	return func() string {
		if known {
			return why
		}
		known = true
		gs, at := dc.outside().words, dc.in.At
		// r, f: a run holds its piece at a place. rOut, fOut: the first
		// holder of each run's the other run was not given (above), the
		// set's in its order, so the line names the same one each time.
		// rNot says which of the two reasons holds at rOut: round F did
		// not give it the application before the call, or gave it with
		// no view keploy reads.
		r, f := false, false
		rOut, rNot, fOut := "", "", ""
		for _, j := range slices.Sorted(maps.Keys(inR)) {
			if inR[j]&inOwn != 0 && gs[j].givenBefore(at) {
				r = true
				if rOut == "" && !gs[j].fRead {
					rOut, rNot = gs[j].mock, fNoView
					if gs[j].fAfter {
						rNot = fNotBefore
					}
				}
			}
		}
		for _, j := range slices.Sorted(maps.Keys(inF)) {
			f = true
			if fOut == "" && !gs[j].answeredBefore(at) {
				fOut = gs[j].mock
			}
		}
		for k, i32 := range asked {
			p := ps.at(int(i32))
			if held[k].hr && !p.recAfter {
				r = true
				if rOut == "" && (!p.fGave || p.fUnstated) {
					rOut, rNot = p.name, fNoView
					if !p.fGave {
						rNot = fNotBefore
					}
				}
			}
			if held[k].hf || slices.ContainsFunc(dc.ps[i32].t.views, func(v *readerText) bool { return evHolds(v, w.Live) }) {
				f = true
				if fOut == "" && p.fGave && !p.rBefore {
					fOut = p.name
				}
			}
		}
		switch {
		case r && f:
			why = bothRunsWhy
		case r && rOut != "":
			why = oneSideWhy("the recording's", rOut, rNot, "round F")
		case f && fOut != "":
			why = oneSideWhy("round F's", fOut, "the recording did not give the application before the call, by a time keploy reads", "the recording")
		}
		return why
	}
}

// cutUnlessMinted is oneRun, asked only where keploy mock's minted default
// does not read the piece as chance (mintedAnew): where it does, no place
// is where the application cut the piece from, whichever run's piece
// stands where, so "".
func (dc *decider) cutUnlessMinted(long []readWord, oneRun func() string) func() string {
	return func() string {
		if dc.mintedAnew(long) {
			return ""
		}
		return oneRun()
	}
}

// mintedAnew reports, for the long words of an id the call minted (long:
// mintedLong, each learned fresh: no place before the call shows it as a
// word), whether a short piece of the id is chance at every place idChance
// or idOutside leaves to oneRun: the founder's minted default (2026-11-11,
// read for a piece on 2026-11-14), a rule of keploy mock's learn step alone
// (DriftCache.ClientTests). A piece shorter than the 16-byte bound of an
// id-shaped value is no source where none of the value's long words (32
// bits or more), as recorded or as round F saw it, stands anywhere either
// run gave the application before the call, not even glued inside a longer
// word there (longGlued): each run then minted its id whole, so a place
// holding its piece holds it by chance, wherever the other run's piece
// stands. That loosens oneRun's both-runs refusal (bothRunsWhy) and its
// one-run refusals for such pieces alone; keploy test reads the piece as
// before. Asked once per id for the call (mintedIDs).
func (dc *decider) mintedAnew(long []readWord) bool {
	if len(long) == 0 || !dc.dr.ClientTests() {
		return false
	}
	key := mintedKey(long)
	if v, ok := dc.mintedIDs[key]; ok {
		return v
	}
	v := !dc.longGlued(long)
	if dc.mintedIDs == nil {
		dc.mintedIDs = map[string]bool{}
	}
	dc.mintedIDs[key] = v
	return v
}

// mintedKey names an id's long words, each run's, for mintedAnew's cache.
func mintedKey(long []readWord) string {
	var b strings.Builder
	for _, o := range long {
		b.WriteString(o.Rec)
		b.WriteByte(0)
		b.WriteString(o.Live)
		b.WriteByte(0)
	}
	return b.String()
}

// longGlued reports whether one of an id's long words (long), as recorded or
// as round F saw it, stands where mintedAnew reads it: in the text of a
// place of the call, or a view round F's serves gave there, anywhere in it
// (inside a longer word too), as the place's evidence of a word reads that
// (evidenceOf: the recording's text where it was given before the call,
// round F's at any time), or a text keploy could not read (fail closed); or
// either run's word inside a longer word of a recording of the set, in any
// of its lists, at any time (setGlued: fail closed, as mintedID reads a set
// recording). Each long word was learned fresh, so a place holding it as a
// whole word gave it nowhere (mintedLong): what this finds is the word
// glued, where the application may have cut the id from (fail closed, as
// movedFrom does). A word bound in either order (longInTurn) is not read at
// the place of the call it was bound at: that request holds it whole in the
// recording by the rule's own check, and gave the application nothing.
func (dc *decider) longGlued(long []readWord) bool {
	mintedAsks.Add(1)
	for i := range dc.ps {
		for _, o := range long {
			p := placedFor(dc.ps[i], o.Rec)
			if m := dc.longInTurn[o.Rec+"\x00"+o.Live]; m != "" && p.mock == m {
				continue
			}
			hr, okR := p.t.rec.contains(o.Rec)
			hf, okF := p.t.live.contains(o.Live)
			if ev, read := evidenceOf(p, evHeld{hr: hr, hf: hf, read: okR && okF}, o.Rec, o.Live, false); !read || ev != evNone {
				return true
			}
			if slices.ContainsFunc(p.t.views, func(v *readerText) bool { h, ok := v.contains(o.Live); return h || !ok }) {
				return true
			}
		}
	}
	gs := dc.outside().words
	for j := range gs {
		for _, o := range long {
			if setGlued(gs[j], o.Rec) || setGlued(gs[j], o.Live) {
				return true
			}
		}
	}
	return false
}

// mintedAsks counts the times longGlued reads a call's places for an id's
// long words, for the rows that bound a call's work: once per id.
var mintedAsks atomic.Int64

// setGlued reports whether a list of g (its answer's, F's view of it, a
// view F's serves gave) holds t inside a longer word of a word reading
// (isWord, wordByte), stamps read as any text. The id reading is not read:
// an id token holds its own long words whole.
func setGlued(g givenWords, t string) bool {
	return listsGlued(g.answerLists, t) || listsGlued(g.view, t) || slices.ContainsFunc(g.views, func(l answerLists) bool { return listsGlued(l, t) })
}

// listsGlued reports whether l holds t inside a longer word of a word
// reading (setGlued).
func listsGlued(l answerLists, t string) bool {
	id := len(readings) - 1
	for _, by := range [][][]string{l.words, l.stamped} {
		for i := 0; i < len(by) && i < id; i++ {
			if slices.ContainsFunc(by[i], func(k string) bool { return len(k) > len(t) && strings.Contains(k, t) }) {
				return true
			}
		}
	}
	return false
}

// Why round F's application was not given a place the recording's piece
// of an id stands at (oneRun), as its line says it: round F served it only
// after the call reached keploy, or not at all (fNotBefore), or served it
// with no view keploy reads there: none stated, one keploy could not keep,
// or several that differ (fNoView).
const (
	fNotBefore = "round F did not give the application before the call"
	fNoView    = "round F gave the application with no view keploy reads"
)

// oneSideWhy is why a piece of one id the call minted is no chance where
// one run's piece stands at no place it had (oneRun) but the other's, of
// holder, stands at place, which that run was not given: its making its
// own piece shows nothing of whether the application cuts it from there.
func oneSideWhy(holder, place, notGiven, run string) string {
	return holder + " own piece of the id stands at " + place + ", which " + notGiven + ": " + run + "'s making its own piece shows nothing of whether the application cut it from there"
}

// bothRunsWhy is why a place that holds a piece of one id the call minted
// as chance would (idChance, idOutside) is no chance (oneRun). The refusal
// it leads says it in a sentence of its own (bothRunsLine).
const bothRunsWhy = "each run's own piece of the id stands at a place that run had, where the application may have cut it from"

// bothRunsLine is the refusal line where the first place left unknown is
// no chance by bothRunsWhy: name is that place, one of those the refusal
// names.
func bothRunsLine(name string) string {
	return bothRunsWhy + " (" + name + " among them): keploy cannot tell a cut from chance"
}

// idShownAt reports whether p shows anything of one of an id's long words
// (evidenceOf is not evNone), or keploy could not read its texts for one: a
// place it cannot read for the id may be where the application took it
// (fail closed).
func idShownAt(p evPlace, long []readWord) bool {
	return slices.ContainsFunc(long, func(o readWord) bool {
		ev, read := evidenceOf(p, heldAt(p, o.Rec, o.Live), o.Rec, o.Live, false)
		return ev != evNone || !read
	})
}

// pieceWholeAt reports whether a text of p holds the piece as a whole id
// token (main's idByte reading, holdsToken): the recorded word in the
// recording's text, round F's in round F's or in a view F's serves gave.
// A text keploy could not read may: fail closed.
func pieceWholeAt(p evPlace, r, f string) bool {
	whole := func(t *readerText, w string) bool {
		h, ok := t.holdsToken(tokenID, w)
		return h || !ok
	}
	return whole(p.t.rec, r) || whole(p.t.live, f) || slices.ContainsFunc(p.t.views, func(v *readerText) bool { return whole(v, f) })
}

// idOutside is, for w, a piece of one id the call minted (long:
// mintedLong), whether the set's recording j may be where the application
// took it, the outside recordings' idChance: it shows anything of one of
// the id's long words (setEvidence is not evNone), or one of its lists
// holds the recorded or round F's piece as a whole id token. One that does
// neither, and leaves the piece unknown, holds it as chance would, and is
// chance where oneRun holds. One where keploy cannot tell what round F
// gave (unstated, unread) shows the id's long words as unknown
// (setEvidence), so it is no chance. nil where long is empty: every
// recording is read as for any word. The lists holding the long words are
// asked once, on the first recording that holds neither piece whole.
func (dc *decider) idOutside(oh *outsideHeld, w readWord, long []readWord) func(j int32) bool {
	if len(long) == 0 {
		return nil
	}
	gs := dc.outside().words
	type held struct{ r, f map[int32]heldRoles }
	var hs []held
	return func(j int32) bool {
		if gs[j].holdsWholeID(w.Rec) || gs[j].holdsWholeID(w.Live) {
			return true
		}
		if hs == nil {
			hs = make([]held, len(long))
			for k, o := range long {
				hs[k] = held{oh.holding(o.Rec), oh.holding(o.Live)}
			}
		}
		return slices.ContainsFunc(hs, func(h held) bool {
			ev, _ := setEvidence(gs[j], h.r[j], h.f[j], dc.in.At, false)
			return ev != evNone
		})
	}
}

// holdsWholeID reports whether one of g's lists (its answer's, the
// embedded answerLists; F's view of it; a view F's serves gave) holds t as
// a whole id token: a word of main's idByte reading, stamps read as any
// text.
func (g givenWords) holdsWholeID(t string) bool {
	id := len(readings) - 1
	if g.holds(id, t, true) || g.view.holds(id, t, true) {
		return true
	}
	return slices.ContainsFunc(g.views, func(l answerLists) bool { return l.holds(id, t, true) })
}

// neverServed reports whether the set's read saw no serve of mock.
func (dc *decider) neverServed(mock string) bool {
	return slices.ContainsFunc(dc.set, func(g GivenPlaces) bool { return g.mock == mock && g.first == 0 })
}

// word decides one changed word that is no instant (any other word), at
// its address (w.At) in text, the text the leaf at bare opens to, and
// lists the recordings it set aside as answered after the call. piece:
// pieceOfAnId; under unreadFloorBits it is floor. long: mintedLong, the
// words that judge a place for a piece of one id token the call minted.
func (dc *decider) word(path string, bare variables.Address, rec, live, text string, w readWord, piece bool, long []readWord) Decision {
	d := dc.wordAt(path, bare, rec, live, text, w, piece, long)
	d.AnsweredAfter = dc.answeredAfter(w.Rec)
	return d
}

// answeredAfter are the recordings whose answer holds r that the
// answer-time rule set aside, so r there decided nothing (evidenceOf,
// setEvidence): each asked at or before the call's own instant and
// answered after it. An exchange's place names its leaf, a recording's its
// answer whole. One asked after the call is after it by its request too,
// and a word its request holds is placed by when it was sent (placedFor):
// neither is listed. A recording handed in twice is listed once. Only the
// places whose recorded text the set's postings say may hold r are placed
// and asked (recAsked): any other does not hold it.
func (dc *decider) answeredAfter(r string) []Place {
	var out []Place
	add := func(p Place) {
		if !slices.Contains(out, p) {
			out = append(out, p)
		}
	}
	at, all := dc.places().recAsked(r)
	for _, i := range askedPlaces(at, all, len(dc.ps)) {
		p := dc.ps[i]
		answeredPlaced.Add(1)
		if p = placedFor(p, r); !p.recAfter || p.at.After(dc.in.At) {
			continue
		}
		if hr, _ := p.t.rec.holdsWord(r); hr {
			add(Place{Mock: p.mock, Side: p.side, Path: p.path})
		}
	}
	gs, oh := dc.outside().words, dc.held()
	inR := oh.holding(r)
	for _, j := range oh.asked(false, inR) {
		if g := gs[j]; !g.noTime && !g.at.After(dc.in.At) && g.answered.After(dc.in.At) && inR[j]&inOwn != 0 {
			add(Place{Mock: g.mock, Side: Response})
		}
	}
	return out
}

// answeredPlaced counts the places answeredAfter places and asks for a
// word, for the rows that bound a call's work.
var answeredPlaced atomic.Int64

// outsideAsked counts the outside recordings a word's check decides
// (setEvidence), for the rows that bound a call's work: those whose lists
// hold its words or whose state leaves them unknown, not every one.
var outsideAsked atomic.Int64

// wordPasses counts the times a word is decided (wordBy), for the rows
// that bound a call's work: twice only where wordAt decides it again.
var wordPasses atomic.Int64

// wordAt is word's verdict. Where serve order made a candidate place
// unknown (orderUnknown) and the word is refused as unknown, it is decided
// again with the order not asked: a conflict there, a candidate that held
// R's word where F's did not follow, is the truer reason, and is named
// instead. A conflict wins over an unknown place (wordBy), so it can come
// only from a place the order marked. Any other verdict there keeps the
// order's refusal: the second decision names a refusal, never loosens one.
func (dc *decider) wordAt(path string, bare variables.Address, rec, live, text string, w readWord, piece bool, long []readWord) Decision {
	ordered := false
	d := dc.wordBy(path, bare, rec, live, text, w, piece, long, &ordered)
	if ordered && d.Refusal != nil && d.Refusal.Kind == RefusalUnknown {
		if c := dc.wordBy(path, bare, rec, live, text, w, piece, long, nil); c.Refusal != nil && c.Refusal.Kind == RefusalConflict {
			return c
		}
	}
	return d
}

// wordBy is word's verdict, serve order asked where ordered is not nil
// (set where the order made a place unknown), and not asked where it is.
func (dc *decider) wordBy(path string, bare variables.Address, rec, live, text string, w readWord, piece bool, long []readWord, ordered *bool) Decision {
	wordPasses.Add(1)
	at, n := w.At, w.At.Word
	ps := newWordPlaces(dc.ps, w.Rec)
	cand := map[string]bool{}
	floor := piece && wordBits(w.Rec) < unreadFloorBits && wordBits(w.Live) < unreadFloorBits
	// moved: why a place that shows the word shows it at another field.
	// shown: a candidate shows R's word in R and F's in F at one field. That
	// proves the link, so serve order, which tells which candidate the
	// application had last, decides only where no place shows it: a candidate
	// that kept the recorded word is not where F's word came from, and replay
	// checks the bound value, so a wrong bind goes red.
	// By place, only where one shows it moved: most words have none.
	var moved map[int]string
	shown := false
	// held: what each place's texts hold of the word, asked once here,
	// before any place is judged (oneRun reads every asked place's), and
	// read again below, where orderUnknown and swapUnknown may have changed
	// a place's state but no text.
	// asked: the places that may show anything of the word (placeIndex),
	// in order, held[k] what asked[k]'s texts hold of it; each other holds
	// neither word nor a token around R's, and shows nothing (evNone), so
	// both loops pass it.
	asked, all := dc.places().asked(w)
	asked = askedPlaces(asked, all, len(dc.ps))
	held := make([]evHeld, len(asked))
	for k, i32 := range asked {
		held[k] = heldAt(*ps.at(int(i32)), w.Rec, w.Live)
	}
	// Only the outside recordings whose lists hold R's or F's word, or whose
	// state leaves them unknown, show anything of it (outsideHeld).
	gs, oh := dc.outside().words, dc.held()
	inR, inF := oh.holding(w.Rec), oh.holding(w.Live)
	idChance, oneRun := dc.idChance(ps, w, long), dc.cutUnlessMinted(long, dc.oneRun(ps, w, asked, held, inR, inF))
	for k, i32 := range asked {
		i := int(i32)
		p := *ps.at(i)
		ev, _ := evidenceOf(p, held[k], w.Rec, w.Live, piece)
		if ev == evNone || ev == evUnknown && idChance(i) && oneRun() == "" {
			continue
		}
		cand[p.mock] = true
		if ev == evShown {
			if why := movedField(p, w.Rec, w.Live); why != "" {
				if moved == nil {
					moved = map[int]string{}
				}
				moved[i] = why
			} else {
				shown = true
			}
		}
	}
	if !shown && ordered != nil {
		*ordered = orderUnknown(dc.in, ps, cand)
	}
	swapUnknown(dc.in, ps, w, asked, held)
	// aside: places that held R's word where F's did not follow, too
	// little to be a conflict; a fresh word's line names them. unserved:
	// recordings round F never served, which say nothing of a piece.
	// chance: places unknown for a piece of one id the call minted that
	// show nothing of its long words (idChance) where one run at most
	// holds its own piece (oneRun), named on a fresh word's line.
	// cut: why oneRun found no place holds the piece as chance would, set
	// where a place it left took it.
	// inTurn: requests of another call of the test that sent the value
	// too, in the other order in each run (sentInTurn).
	var src, outside, conflict, unknown, aside, unserved, chance, inTurn []evPlace
	cut := ""
	lateHeld := map[string][2]int{}
	for k, i32 := range asked {
		i := int(i32)
		p := *ps.at(i)
		ev, read := evidenceOf(p, held[k], w.Rec, w.Live, piece)
		if floor && ev == evUnknown && p.fUnstated && !p.fGave {
			unserved = append(unserved, p)
			continue
		}
		if !read {
			unknown = append(unknown, evPlace{name: p.name, why: "past the call's budget"})
			continue
		}
		switch ev {
		case evNone, evDisproven:
			if tokenShown(p, w) {
				ev = evShown
			} else if ev == evNone && dc.gluedAt(p, w) {
				ev, p.why = evUnknown, "glued to the bytes around it, in a body that declares no media type (no Content-Type): keploy cannot tell the words the application was given there from the bytes that frame them"
			} else if ev == evNone && p.gave && p.rec != p.live && p.rec != "" && p.live != "" && strings.Contains(w.Rec, p.rec) && strings.Contains(w.Live, p.live) {
				// A given glued inside the word in both runs is a part of
				// it (#5016), at any length: the test gave it, so the word
				// changed with it, and keploy binds a given only whole.
				ev, p.why = evUnknown, "the given stands inside the word in both runs, glued to the text around it: keploy binds a given only as a whole word"
			}
		case evShown:
			if moved[i] != "" {
				ev, p.why = evUnknown, moved[i]
			}
		}
		switch ev {
		case evShown:
			if dc.allScope || !p.otherTest {
				src = append(src, p)
			} else {
				outside = append(outside, p)
			}
		case evDisproven:
			// tok: R's whole token around w that p kept in both runs, at
			// any earlier place of the run, an older test's too: main
			// refuses a word of an earlier test that did not drift with it,
			// and a whole token there is no chance of a short word.
			took, tok := "", sameTokenAround(p, w)
			if p.lateCall != "" && !piece {
				took = dc.lateCallTook(ps, i, w, lateHeld)
			}
			switch {
			case p.gave || countsByText(p, w) || (dc.allScope || !p.otherTest) && heldWhole(p, w, text):
				conflict = append(conflict, p)
			case tok != "":
				p.why = strconv.Quote(tok) + " stood whole at " + p.name + " in both runs, and the value did not follow it: keploy cannot tell whether the value came from there"
				conflict = append(conflict, p)
			case p.outsideTest != "" && !piece:
				// F gave it only before the call's test began, in an older
				// test or before it: what F gave there shows nothing of
				// what the application did with the word since, so it is no
				// copy moved apart. A piece of a longer value (pieceOfAnId)
				// is still set aside as chance: the ruling covers whole
				// words.
				p.why = p.outsideTest
				unknown = append(unknown, p)
			case took != "":
				p.why = took
				unknown = append(unknown, p)
			default:
				aside = append(aside, p)
			}
		case evUnknown:
			if dc.sentInTurn(p, w, rec, live) {
				inTurn = append(inTurn, p)
				break
			}
			if idChance(i) {
				why := oneRun()
				if why == "" {
					chance = append(chance, p)
					break
				}
				p.why, cut = why, why
			}
			unknown = append(unknown, p)
		}
	}
	idShows := dc.idOutside(oh, w, long)
	for _, j := range oh.asked(true, inR, inF) {
		g := gs[j]
		outsideAsked.Add(1)
		p := evPlace{name: g.mock, mock: g.mock, otherTest: true, scope: "another test, or after this call", at: g.at}
		ev, changed := setEvidence(g, inR[j], inF[j], dc.in.At, piece)
		if changed {
			p.live = withoutMark + w.Live
		}
		switch ev {
		case evShown:
			outside = append(outside, p)
		case evUnknown:
			if floor && dc.neverServed(g.mock) {
				unserved = append(unserved, p)
				break
			}
			if idShows != nil && !idShows(j) {
				why := oneRun()
				if why == "" {
					chance = append(chance, p)
					break
				}
				p.why, cut = why, why
			} else {
				p.why = setUnknownWhy(g, dc.in.At)
			}
			unknown = append(unknown, p)
		case evDisproven:
			if inR[j]&inOwn == 0 {
				p.why = strconv.Quote(w.Live) + " stood whole at " + p.name + ", served as recorded, where " + strconv.Quote(w.Rec) + " never stood: the value did not follow"
			}
			if wordBits(w.Rec) >= 32 || wordBits(w.Live) >= 32 {
				conflict = append(conflict, p)
			} else {
				aside = append(aside, p)
			}
		}
	}
	switch {
	case len(src) > 0:
		echo, no := dc.answered(path, bare, w, rec, live, false)
		if no != nil {
			return *no
		}
		return dc.bound(path, at, n, changedInstant{r: w.Rec, l: w.Live, valueEcho: echo, rec: rec}, src, ", the earliest place before the call that shows it in both runs")
	case len(conflict) > 0:
		p := conflict[0]
		return dc.refuse(path, at, n, w.Rec, w.Live, RefusalConflict,
			cmp.Or(p.why, strconv.Quote(w.Rec)+" stood whole at "+p.name+"; round F gave "+gaveAt(p, w.Live)+" there and the value did not follow"), names(conflict),
			Edit{Kind: EditFixApp}, dc.fieldNoise(path), Edit{Kind: EditGive, Name: givenName(path)})
	case len(unknown) > 0:
		p := unknown[0]
		why := cmp.Or(p.why, "both values hold it, or only part of a word")
		if p.why == "" && (p.fMay || len(p.views) > 0) {
			why = "round F served it and keploy cannot tell what it gave there"
		}
		line := "keploy cannot tell what round F gave at " + p.name + " before this call (" + why + ")"
		switch {
		case p.why == bothRunsWhy:
			// What round F gave there is not the reason, so the line
			// says only the reason: a cut cannot be told from chance.
			line = bothRunsLine(p.name)
		case p.why != "" && p.why == cut:
			// oneSideWhy names its place, and says the reason alone.
			line = p.why
		case p.why == servedAfterWhy:
			// The reason is the whole answer: round F gave the
			// application nothing there before the call.
			line = "keploy cannot tell what round F gave at " + p.name + " before this call: " + servedAfterWhy
		}
		return dc.refuse(path, at, n, w.Rec, w.Live, RefusalUnknown, line, names(unknown),
			Edit{Kind: EditReRecord}, dc.fieldNoise(path))
	case len(outside) > 0:
		p := outside[0]
		return dc.refuse(path, at, n, w.Rec, w.Live, RefusalAnotherScope,
			"the value "+p.name+" sent in "+scopeName(p)+"; keploy binds only within a test and the start-up", names(outside),
			Edit{Kind: EditMoveToMocks, Mock: p.mock}, Edit{Kind: EditMoveToStartup, Mock: p.mock}, dc.fieldNoise(path))
	case len(inTurn) > 0:
		echo, no := dc.answered(path, bare, w, rec, live, false)
		if no != nil {
			return *no
		}
		d := dc.bound(path, at, n, changedInstant{r: w.Rec, l: w.Live, valueEcho: echo, rec: rec}, inTurn, sentInTurnRule)
		d.EitherOrder = true
		return d
	}
	if why, p := dc.encoded(); p != nil {
		if p.otherTest && !dc.allScope {
			return dc.refuse(path, at, n, w.Rec, w.Live, RefusalAnotherScope, why+", sent in "+scopeName(*p)+"; keploy binds only within a test and the start-up", []string{p.name},
				Edit{Kind: EditMoveToMocks, Mock: p.mock}, Edit{Kind: EditMoveToStartup, Mock: p.mock}, dc.fieldNoise(path))
		}
		return dc.refuse(path, at, n, w.Rec, w.Live, RefusalUnknown, why, []string{p.name}, Edit{Kind: EditReRecord}, dc.fieldNoise(path))
	}
	// What only a form main's readings check shows (its quoted-printable
	// text, the text its base64 runs decode to, the value as written where
	// it holds no escape) is refused there, as on main: a '%' or '\' that
	// is no escape, text of a script keploy does not split into words, a
	// run of the base64 alphabet that does not decode. keploy cannot read
	// such a value whole, so a word of it is never learned fresh.
	if why := dc.unchecked(); why != "" {
		return dc.cannotCheck(path, at, n, w.Rec, w.Live, "keploy cannot read this value whole, so it cannot tell the word was made anew: "+why)
	}
	echo, no := dc.answered(path, bare, w, rec, live, true)
	if no != nil {
		return *no
	}
	if len(echo) == 0 {
		if a := dc.answerHoldsWord(w.Rec); a != "" {
			return dc.cannotCheck(path, at, n, w.Rec, w.Live, "a word of it comes back in the answer at "+a+", which keploy cannot answer with this run's value")
		}
	}
	rule := "no place before the call shows it"
	if len(aside) > 0 {
		p := aside[0]
		rule += " (" + p.name + " held " + strconv.Quote(w.Rec) + " and round F gave " + gaveAt(p, w.Live) + " there: set aside, as a copy moves with its source)"
	}
	if len(unserved) > 0 {
		rule += " (round F never served " + unserved[0].name + ", and a piece of a longer value under " + strconv.Itoa(unreadFloorBits) + " bits is no evidence at such a place)"
	}
	if len(chance) > 0 {
		rule += " (" + chance[0].name + " holds " + strconv.Quote(w.Rec) + " or " + strconv.Quote(w.Live) + " as a piece of an id, and nothing of the id's words of " + strconv.Itoa(chanceBits) + " bits or more, which this call made anew: an id is made whole, so the piece there is chance)"
	}
	d := dc.learned(path, at, n, w.Rec, w.Live, OutcomeFresh, rule+"; bound at its first call"+answeredAt(echo, rec), names(append(append(aside, unserved...), chance...)))
	d.Echo = echo
	return d
}

// lateCallTook is why ps[i], a request an older test's call left the run
// (evPlace.lateCall) that disproves w, excuses no word: the value that lacks
// the word it holds (the recorded value where round F's text there holds
// F's word, else F's) holds a word that another place a run gave the
// application before the call holds, in either run's text there (fail
// closed), as main's late-call rule reads it (olderTook); "" where no other
// place does, and the word is set aside. The places that hold a value are
// found once a value for the word's check (holders): the first two, in
// order, that a run gave the application before the call (-1 for none),
// the first of them other than ps[i] being the one each late place names.
// The search reads only the places whose texts the set's postings say may
// hold the value (placeIndex): any other holds it in neither run.
func (dc *decider) lateCallTook(ps *wordPlaces, i int, w readWord, holders map[string][2]int) string {
	p, lacks := *ps.at(i), w.Rec
	if !evHolds(p.t.live, w.Live) {
		lacks = w.Live
	}
	stands := func(name string) string {
		return strconv.Quote(lacks) + " stands in " + name + ", which a run gave the application before the call, and the value that lacks the word " + p.name + " holds holds it; " + p.lateCall
	}
	hs, ok := holders[lacks]
	if !ok {
		hs = [2]int{-1, -1}
		at, all := dc.places().asked(readWord{Rec: lacks, Live: lacks})
		at = askedPlaces(at, all, len(ps.base))
		for k, n := 0, 0; k < len(at) && n < 2; k++ {
			j := int(at[k])
			if q := ps.at(j); q.state != stateNotBefore && (evHolds(q.t.rec, lacks) || evHolds(q.t.live, lacks)) {
				hs[n], n = j, n+1
			}
		}
		holders[lacks] = hs
	}
	if j := hs[0]; j >= 0 && j != i {
		return stands(ps.base[j].name)
	}
	if j := hs[1]; j >= 0 {
		return stands(ps.base[j].name)
	}
	gs, oh := dc.outside().words, dc.held()
	in := oh.holding(lacks)
	for _, j := range oh.asked(false, in) {
		if in[j]&(inOwn|inView) != 0 {
			return stands(gs[j].mock)
		}
	}
	return ""
}

// olderTestWhy and beforeTestWhy are why a word held at a place F gave the
// application only before the call's test began, where F gave another
// text, says nothing of the value (evPlace.outsideTest): F gave it only in
// an older test of the set, or only before the test (the application may
// have kept it).
const (
	olderTestWhy  = "round F gave it to the application only in an older test of the set, and what it gave there shows nothing of what the application did with the value in the tests since, so keploy cannot tell a copy from a varying value"
	beforeTestWhy = "round F gave it to the application only before the call's test began, and the application may have kept what it gave there, so keploy cannot tell a copy from a varying value"
)

// inDecodedForm is why no word of a leaf may be learned fresh, and the place
// that says so: a word of 32 bits or more that only the decoded forms of
// one run's text hold, and no form of the other run's, stands at a place
// keploy reads for the call, in either run's text there or a view of what
// round F served there. That is an id carried inside an encoding the reader
// opens no step for (base64 wrapped between other text, base32, 0x hex, a
// signed message, a quoted-printable escape): keploy binds no word in such
// a form, so it can check no copy of the place, and the words of the
// encoded text would go unchecked at their first call. Main refuses such a
// value wherever the place held the word, recorded or in round F, and
// whatever the place gave in the other run (unaccounted: it did not drift
// with the value), or binds it where the value followed the place. A place
// that holds the word only in the other run's text, which main reads only
// for a recording's payload, refuses too: one rule across places. The forms
// are the value's in the call's window, as main reads them
// (valueDrift.recTok and liveTok): a run that is a read of the clock there
// is not decoded. It binds nothing.
func (dc *decider) inDecodedForm(rds []textReading) (string, *evPlace) {
	for _, rd := range rds {
		v := dc.dr.ofIn(rd.Rec, rd.Live, dc.in.Window, dc.in.At)
		for side, ws := range [2][]string{decodedOnly(v.recTok, v.liveTok, rd.Rec), decodedOnly(v.liveTok, v.recTok, rd.Live)} {
			which := "the recorded value"
			if side == 1 {
				which = "round F's value"
			}
			for _, w := range ws {
				held := func(t *readerText) bool { return evHolds(t, w) }
				for _, p := range dc.ps {
					if held(p.t.rec) || held(p.t.live) || slices.ContainsFunc(p.t.views, held) {
						return p.name + " held " + strconv.Quote(w) + ", which only " + which + " carries, in a decoded form, where keploy binds no word: keploy cannot tell whether it came from there", &p
					}
				}
			}
		}
	}
	return "", nil
}

// decodedOnly are the words of 32 bits or more that own, the words of a
// run's text in every declared form by reading, holds and the text s as
// written does not, in any case, nor other (the other run's), sorted so a
// word as it stands comes before its lower-cased form.
func decodedOnly(own, other []map[string]bool, s string) []string {
	var out []string
	plain := tokensOfForms([]string{strings.ToLower(s)})
	for i, set := range own {
		for w := range set {
			if wordBits(w) >= 32 && !plain[i][strings.ToLower(w)] && !other[i][w] {
				out = append(out, w)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
}

// movedInAForm is why no word of a leaf may be learned fresh where a word of
// 32 bits or more moved in a form of one of its texts (as written, or a
// decoded form), and a place before the call holds it, recorded for a word
// of the recorded value, in round F for one of F's. A word moved there is
// one both runs hold in that form, in another order or once more (main's
// valueDrift.movedRec and movedLive, read in the call's window, less what
// one run lost or gained: an id swapped behind quoted-printable escapes,
// inside a base64 run's text, or as written beside another changed word).
// keploy cannot tell whether the leaf's other changed words came from
// there, as main refuses the value, so it refuses them too, even where the
// reader reads the moved word as a changed word itself (the reader judges
// that word by its own rules, but not the words beside it). A word one run
// lost or gained in a decoded form is inDecodedForm's. A place keploy could
// not read refuses every change of the call (decide).
func (dc *decider) movedInAForm(rds []textReading) (string, *evPlace) {
	for _, rd := range rds {
		v := dc.dr.ofIn(rd.Rec, rd.Live, dc.in.Window, dc.in.At)
		for i := range v.movedRec {
			for side, moved := range [2]map[string]bool{v.movedRec[i], v.movedLive[i]} {
				recorded, changed := side == 0, v.gained[i]
				if recorded {
					changed = v.lost[i]
				}
				for _, t := range v.inOrder(moved, recorded) {
					if wordBits(t) < chanceBits || changed[t] {
						continue
					}
					for _, p := range dc.ps {
						text := p.t.live
						if recorded {
							text = p.t.rec
						}
						if held, _ := text.holdsWord(t); held && placedFor(p, t).state != stateNotBefore {
							return p.name + " held " + strconv.Quote(t) + ", which moved in this value (in another order, or once more), so keploy cannot tell where its changed words came from", &p
						}
					}
				}
			}
		}
	}
	return "", nil
}

// answered is where the call's answer reads back w, a word of the leaf at
// bare; no is its refusal where the adapter cannot bind it inside a longer
// value, or cannot answer those places (unanswerable).
func (dc *decider) answered(path string, bare variables.Address, w readWord, rec, live string, withWhole bool) (echo []variables.Address, no *Decision) {
	if !dc.in.Caps.BindAtWord && (w.Rec != rec || w.Live != live) {
		d := dc.cannotCheck(path, bare, -1, rec, live, "the adapter cannot bind a word inside this field yet")
		return nil, &d
	}
	echo, unnamed := dc.readBack(path, w.At, w, rec, withWhole)
	if withWhole && (w.At.Word < 0 || len(w.At.Steps) > 0) {
		// A fresh word behind a decode step, or a token read whole: an
		// answer leaf whose recorded text is the request's whole reads it
		// back there, as main's WholeEcho answers it.
		echo, unnamed = dc.wholeEchoes(path, w.At, rec)
	}
	if d, refused := dc.unanswerable(path, bare, rec, live, echo, unnamed); refused {
		return nil, &d
	}
	return echo, nil
}

// tokenShown reports whether p shows a whole token around w: one place
// holds that whole token in each run.
func tokenShown(p evPlace, w readWord) bool {
	return p.state == stateBefore && !p.same() && slices.ContainsFunc(w.Tokens, func(t wholeToken) bool {
		return tokenIn(p.t.rec, t.Kind, t.Rec) && tokenIn(p.t.live, t.Kind, t.Live)
	})
}

// textInstant decides an instant the changed words of rd, a text the leaf
// at bare opens to, are part of, at its first word's address (w.At); a
// number that does not parse as one is matched by its exact text.
func (dc *decider) textInstant(path string, bare variables.Address, rec, live string, rd textReading, w readWord) Decision {
	iw := w.Instant
	if iw.RecAt[0] < 0 || iw.LiveAt[0] < 0 || (iw.Rec == nil || iw.Live == nil) && !iw.ByText {
		return dc.cannotCheck(path, bare, -1, rec, live, "keploy cannot read this instant in both runs")
	}
	c := changedInstant{r: rd.Rec[iw.RecAt[0]:iw.RecAt[1]], l: rd.Live[iw.LiveAt[0]:iw.LiveAt[1]], byText: iw.ByText, text: true}
	if (iw.Rec == nil) != (iw.Live == nil) {
		// One run's word reads as an instant and the other's is a number
		// that is no time (an audit stamp round F sent as 8357): as a typed
		// instant leaf of one run only, keploy cannot check it, and it is
		// never matched by its text or bound fresh.
		return dc.cannotCheck(path, w.At, w.At.Word, c.r, c.l, "one run's value here is no instant")
	}
	c.word, c.noWord = clockWordOf(rec, live, rd, iw)
	c.around = aroundWordOf(rec, iw, c.noWord)
	c.claim = claimWhole(rd, iw)
	if iw.Rec != nil && iw.Live != nil && !iw.ByText {
		c.rm, c.lm = iw.Rec, iw.Live
		c.ri = typedInstant{at: time.Unix(0, iw.Rec.key.at).UTC(), res: iw.Rec.res, names: namesMoment}
		c.li = typedInstant{at: time.Unix(0, iw.Live.key.at).UTC(), res: iw.Live.res, names: namesMoment}
	}
	c.echo, c.unnamed = dc.readBack(path, w.At, w, rec, true)
	c.rec, c.live = rec, live
	if w.Rec != rec {
		c.valueEcho = c.echo
	}
	d := dc.instant(path, w.At, w.At.Word, c)
	if r, no := dc.unanswerable(path, bare, rec, live, d.Echo, c.unnamed); no && len(d.Echo) > 0 {
		return r
	}
	return d
}

// instantHold is what one earlier place holds of a changed instant: R's
// value in R's text (hr), F's in F's (hl), each by moment or by its exact
// text, and whether that hold is at a fine cut (fr, fl).
type instantHold struct {
	p evPlace
	// coarseR: R's text holds R's value only as an instant of a coarser
	// unit (a day, an hour) that the value, cut to it, is (coarserOnly).
	// A recording's place is held in one of its halves: a moment F's view
	// of it holds only in the request the application sent there is a
	// send, which the send rules decide, never a pick from what F gave.
	hr, hl, fr, fl, coarseR bool
	// rank orders the places that name a value: by its exact text (0),
	// else by the finest cut that gives it (its unit in nanoseconds).
	rank int64
}

// inAView reports whether a view round F served at p holds F's moment of c,
// or could not be read (it may: fail closed).
func (dc *decider) inAView(p evPlace, c changedInstant) bool {
	return slices.ContainsFunc(p.t.views, func(v *readerText) bool {
		h, _, ok := dc.holds(p, v, c.l, c.lm, c.byText)
		return h || !ok
	})
}

// clockCarriedWhy is why step 5 of the instant order reads o, the offset
// the reading found at path, as no read of the clock: place, a call of
// another test, sent the same moment before this call in both runs.
func clockCarriedWhy(path string, o models.ClockOffset, place string) string {
	return path + " = " + o.String() + " in both runs, but " + place + " sent that moment before this call in both runs: the offset is the gap between the two calls, which a replay moves, so keploy reads it as that call's value carried, never a read of this call's clock"
}

// sameRead reports whether learned, the read of the clock an earlier call's
// learn step learned its send as (evPlace.offset), is a read plus the same
// offset as off, the one this call's window holds a value written at res
// at in both runs: learned at a cut (one with no Res holds nothing) finer
// than res, of an offset no meet emptied, and sharing with off an offset
// both hold (their ranges meet; a plain read's never meets an offset's: the
// reading signs every offset).
func sameRead(learned *models.ClockOffset, off models.ClockOffset, res time.Duration) bool {
	return learned != nil && learned.Res > 0 && learned.Res < res && !learned.Empty() && max(learned.Min, off.Min) < min(learned.Max, off.Max)
}

// heldWhere is the place of each of hs that keep keeps, in order.
func heldWhere(hs []instantHold, keep func(instantHold) bool) []evPlace {
	var out []evPlace
	for _, h := range hs {
		if keep(h) {
			out = append(out, h.p)
		}
	}
	return out
}

// changedInstant is one changed instant: its text in each run, its typed
// reading for the window, its moments (nil for a number that does not
// parse: matched by its exact text alone, byText).
type changedInstant struct {
	r, l   string
	ri, li typedInstant
	rm, lm *moment
	// text: an instant in a text, whose layout the Clock keeps; word, where
	// it stands in the leaf's recorded text (clockWordOf); noWord, why a
	// Clock there is no check a replay makes.
	byText, text bool
	word         models.ClockWord
	noWord       string
	// around: where the text changed around the instant (noWordAround),
	// the word it stands at in the recorded text (aroundWordOf).
	around models.ClockWord
	// claim is the JWT claim whose value whole the instant is, where the
	// text is one claim of a token keploy opened (claimWhole); "" otherwise.
	claim string
	// echo is where the call's answer reads the word back (readBack, whole
	// values too), valueEcho the same for a Value (the text whole is the
	// adapter's echo), unnamed the first such place keploy cannot name, rec
	// the leaf's recorded text and live its text in round F (an instant in a
	// text's; leaf reads them).
	echo, valueEcho    []variables.Address
	unnamed, rec, live string
	// aside names the earlier places that held R's value where F's does
	// not stand: a Clock's line says it set them aside.
	aside []string
}

// leaf is the changed leaf's whole text in each run: the instant's own,
// where it is no instant in a text.
func (c changedInstant) leaf() (rec, live string) {
	if c.text {
		return c.rec, c.live
	}
	return c.r, c.l
}

// clockWordOf is where iw, an instant rd reads, stands in rec, its leaf's
// recorded text, for a Clock (models.ClockOffset.Word): zero where it is
// the text whole; its bytes in rec where it is one word of rec as written
// and the text on each side of it is the same in both runs, which a replay
// checks by their hashes. noWord is why not otherwise: a part of the text
// keploy opened (a decode step, a cookie's or a token's part), where a
// replay reads no clock yet, or a text that changed around it too.
func clockWordOf(rec, live string, rd textReading, iw *instantWord) (word models.ClockWord, noWord string) {
	if len(rd.At.Steps) > 0 || rd.Rec != rec || rd.Live != live {
		return word, "it stands in a part of the text keploy opened, where a replay reads no clock yet"
	}
	rs, re, ls, le := iw.RecAt[0], iw.RecAt[1], iw.LiveAt[0], iw.LiveAt[1]
	switch {
	case rs == 0 && re == len(rec) && ls == 0 && le == len(live):
		return word, ""
	case rec[:rs] != live[:ls] || rec[re:] != live[le:]:
		return word, noWordAround
	}
	return models.ClockWordOf(rec, rs, re), ""
}

// noWordAround is clockWordOf's noWord for a read whose text changed on a
// side of it.
const noWordAround = "the text around it changed too"

// aroundWordOf is the word iw, an instant rd reads, stands at in rec, its
// leaf's recorded text as written, where noWord says the text changed
// around it (noWordAround) and text stands on a side of it: the word a
// replay that puts the text's other words back reads it at (amongBinds).
// Zero otherwise.
func aroundWordOf(rec string, iw *instantWord, noWord string) models.ClockWord {
	if rs, re := iw.RecAt[0], iw.RecAt[1]; noWord == noWordAround && (rs > 0 || re < len(rec)) {
		return models.ClockWordOf(rec, rs, re)
	}
	return models.ClockWord{}
}

// claimWhole is the claim of a JWT that rd, a reading of a leaf's text,
// opened as its one step, where iw, an instant rd reads, is that claim's
// value whole in both runs: "" otherwise (a word of a claim, a claim of a
// token inside another decoded text, any other step).
func claimWhole(rd textReading, iw *instantWord) string {
	if len(rd.At.Steps) != 1 || iw.RecAt != [2]int{0, len(rd.Rec)} || iw.LiveAt != [2]int{0, len(rd.Live)} {
		return ""
	}
	claim, ok := strings.CutPrefix(string(rd.At.Steps[0]), string(variables.JWTClaim("")))
	if !ok {
		return ""
	}
	return claim
}

// inZone is m, a wall clock of no zone, read in the application's zone, as
// main's echo reads it too (a pick from a timetable written in UTC); false
// for any other moment, or where keploy knows no zone.
func (dc *decider) inZone(m moment) (moment, bool) {
	zone := dc.dr.zone
	if m.zoned || m.key.day || zone == nil {
		return m, false
	}
	at := wallIn(m.key.at, zone)
	_, off := at.Zone()
	return moment{key: instantKey{at: at.UnixNano()}, wall: m.wall, res: m.res, zone: int64(off) * int64(time.Second), zoned: true, typed: m.typed}, true
}

// holds is what t, one run's text at p, holds of w (that run's word)
// and m (its moment): a test's given by its text alone, any other
// place by its text or by moment. fine: the hold is at a cut finer than a
// millisecond, or by the exact text of a number that does not parse.
func (dc *decider) holds(p evPlace, t *readerText, w string, m *moment, byText bool) (held, fine, read bool) {
	h, ok := t.holdsWord(w)
	// An instant of several words (a date and a time) stands in a text
	// as those words in order.
	if ok && !h && len(variables.Words(w)) > 1 {
		h, ok = t.contains(w)
	}
	if !ok {
		return false, false, false
	}
	if m == nil || p.gave {
		return h, h && byText, true
	}
	at := *m
	hm, fm, ok := t.holdsMoment(at)
	if z, zok := dc.inZone(*m); ok && !hm && zok {
		at = z
		hm, fm, ok = t.holdsMoment(z)
	}
	if !ok {
		return false, false, false
	}
	// A text with escapes is read as it opens: as written, the date alone
	// of a percent-encoded time reads as a coarse instant, no hold of that
	// time. A form that opens holds what holdsMoment found at a fine cut.
	if hm && !fm && openedOnly(t, at) {
		hm = false
	}
	if hm {
		return true, fm, true
	}
	return h, h && fineCut(m.res), true
}

// openedOnly reports whether t holds m at a coarse cut only as a misread
// of an escaped time: every instant of t as written that holds m changes
// once the escapes around it are read (escapeMoves: the date alone of a
// percent-encoded time, which opens to the whole time), and no instant of
// t with its escapes read holds m. An escape elsewhere in the text (a
// quote in a note, the "\/" a PHP encoder writes in a link) leaves an
// instant that holds m as it stands, and so the hold; a hold no instant as
// written gives is a form's that opens, and stands too. False where the
// text was not read.
func openedOnly(t *readerText, m moment) bool {
	text, ok := t.textNow()
	if !ok {
		return false
	}
	spans, ok := t.spansOf()
	if !ok {
		return false
	}
	holds := func(sp instant) bool {
		h, _ := byMoment(sp.moment(), m)
		return sp.keyed && h
	}
	moved := false
	for _, sp := range spans {
		if !holds(sp) {
			continue
		}
		if !escapeMoves(text, sp) {
			return false
		}
		moved = true
	}
	return moved && !slices.ContainsFunc(t.x.dc.spansOf(unescaped(text)), holds)
}

// escapeMoves reports whether reading the escapes around sp, an instant of
// text as written, changes it: the bytes escapeReach before and after it,
// their escapes read (unescaped), hold no instant of sp's own text where it
// stands (a percent-encoded time's date opens to the whole time).
func escapeMoves(text string, sp instant) bool {
	a, b := max(0, sp.start-escapeReach), min(len(text), sp.end+escapeReach)
	if !strings.ContainsAny(text[a:b], `%\`) {
		return false
	}
	own, at := text[sp.start:sp.end], len(unescaped(text[a:sp.start]))
	near := unescaped(text[a:b])
	return !slices.ContainsFunc(instantSpans(near), func(o instant) bool {
		return o.start == at && near[o.start:o.end] == own
	})
}

// escapeReach is how far around an instant escapeMoves reads: past the
// longest time a percent-encoder writes twice over.
const escapeReach = 128

// coarserOnly reports whether t, which holds w (moment m), holds it only
// as an instant of a coarser unit that m, cut to it, is (cutsEach's finer
// rule: a served date beside the application's own time of day). Such a
// place gives none of m's part below that unit, where its change lies, so
// it is no answer's source: bound to it, a replay would check the date
// alone and pass any time of that day. A wall clock of no zone is read in
// the application's zone too (inZone).
func (dc *decider) coarserOnly(t *readerText, w string, m moment) bool {
	in, ok := t.contains(w)
	return ok && !in && dc.namedBy(t, m, func(h, m moment) bool {
		return h.key == m.key || cuts(h, m, func(c cutRule) bool { return !c.finer })
	}) == 0
}

// cutOfFinerOnly reports whether t, which holds w (moment m), holds it only
// as m's cut of a finer instant: no word of it outside its instants finer
// than m is w, and no instant of it is m at m's unit or a coarser one, each
// instant judged at the finest unit its place shows in either run (other
// is the other run's text there, atFinest). A second read beside a
// UnixNano in a name: no variable can hold that text, and two reads of one
// coarse cut say nothing of each other. A finer instant at the first
// millisecond or nanosecond of m names m too, but only as that cut, the
// same as any other of m's finer instants, and so does one an encoder
// writes without its trailing zeros, which then shows m's own text in one
// run, and a number of seconds with its fraction, whose whole seconds are
// a word: only where neither run's text shows a unit finer than m at that
// place, as atFinest pairs them, is it m.
func (dc *decider) cutOfFinerOnly(t, other *readerText, w string, m moment) bool {
	h, ok := t.holdsWord(w)
	if !ok {
		return false
	}
	spans, read := dc.atFinest(t, other)
	return read && (!h || !heldOutside(t, spans, w, m)) &&
		dc.namedIn(spans, m, func(h, m moment) bool { return h.key == m.key && h.res >= m.res }) == 0
}

// atFinest is t's instants, each at the finest unit its place shows in
// either run: other is the other run's text at the place. An encoder that
// leaves out a fraction's trailing zeros (Go's RFC3339Nano, Java's
// Instant.toString, Python's isoformat, a JavaScript number) writes a time
// made at a second's first millisecond as a whole second, and the same
// field's time in the other run with its fraction. An instant's place is
// the JSON member it is the value of, named by its path (placesIn), its
// layout, and its text but for its fraction's digits (instantShape). Where
// both texts hold as many instants at a place, the k-th of each are one
// time's; where a member's count differs (a list that grew between runs),
// each takes the finest unit the other run shows at that member. An
// instant of no member (an element of a list of times, a field of its own)
// pairs only where both texts hold as many; any other is judged at its
// own text's unit, and so is one of no unit (a time-ordered UUID's). Each
// read of an instant is counted in dc.passes, each byte walked to place
// them in dc.walked, and the texts are read again only where the other run
// shows a unit finer than one of t's instants of its layout. read is false
// where keploy could not read t.
func (dc *decider) atFinest(t, other *readerText) (spans []instant, read bool) {
	spans, read = t.spansOf()
	if !read || other == nil || other == t {
		return spans, read
	}
	os, ok := other.spansOf()
	if !ok {
		return spans, true
	}
	finest := map[int]time.Duration{}
	for _, o := range os {
		if f, ok := finest[o.layout]; !ok || o.res < f {
			finest[o.layout] = o.res
		}
	}
	dc.passes += len(os) + len(spans)
	if !slices.ContainsFunc(spans, func(sp instant) bool {
		f, ok := finest[sp.layout]
		return ok && f < sp.res
	}) {
		return spans, true
	}
	text, fields, ok1 := dc.laidOut(t)
	var otext string
	var ofields []rawField
	var ok2 bool
	if ok1 {
		otext, ofields, ok2 = dc.laidOut(other)
	}
	if !ok1 || !ok2 {
		return spans, true
	}
	theirs, oat := map[instantPlace]placeUnits{}, dc.placesIn(otext, ofields, os)
	for j, o := range os {
		at := oat.of(j)
		u, ok := theirs[at]
		if !ok {
			u.finest = o.res
		}
		u.res, u.finest = append(u.res, o.res), min(u.finest, o.res)
		theirs[at] = u
	}
	ours, at := map[instantPlace]int{}, dc.placesIn(text, fields, spans)
	for i := range spans {
		ours[at.of(i)]++
	}
	kth, cloned := map[instantPlace]int{}, false
	for i, sp := range spans {
		p := at.of(i)
		u, ok := theirs[p]
		if !ok {
			continue
		}
		res := sp.res
		switch k := kth[p]; {
		case len(u.res) == ours[p]:
			res = u.res[k]
		case p.field != "":
			res = u.finest
		}
		kth[p]++
		if res >= sp.res {
			continue
		}
		if !cloned {
			spans, cloned = slices.Clone(spans), true
			dc.passes += len(spans)
		}
		spans[i].res = res
	}
	return spans, true
}

// laidOut is t's text and the fields it is laid out in (textFields): a
// recording's, a half's (textHalf, dc.laid, dc.laidNow), or none for a text
// handed whole, which is one field (a leaf's value); ok false where the
// recording was not read.
func (dc *decider) laidOut(t *readerText) (text string, fields []rawField, ok bool) {
	if text, fields, ok = t.read(); t.raw == nil {
		var kept bool
		if fields, kept = dc.laid[t]; !kept {
			fields = dc.laidNow[t]
		}
	}
	return text, fields, ok
}

// instantPlace is where an instant stands in its text, as atFinest pairs
// it with the other run's: the JSON member it is the value of, by its path
// ("" where none), its layout, and its text but for its fraction's digits.
type instantPlace struct {
	field, shape string
	layout       int
}

// placeUnits are the units of the instants a text holds at one place, in
// order, and the finest of them.
type placeUnits struct {
	res    []time.Duration
	finest time.Duration
}

// placed are a text's instants' places, in its instants' order, each read
// through of alone, which counts the read in n: a read the work's count
// sees, wherever it is made.
type placed struct {
	at []instantPlace
	n  *int
}

// of is the i-th instant's place, counted.
func (p placed) of(i int) instantPlace {
	*p.n++
	return p.at[i]
}

// placesIn are spans' places in text (spans in its order, as instantSpans
// gives them; fields: how text is laid out, none for one field), in one
// walk over the fields that hold them and spans. An instant's member is
// named by its path: the field's, its list indices dropped
// ("res.body", "MongoResponses[].Message.Sections[]"), then the keys of
// the JSON containers open where its value starts, from the field's root,
// an element of a list as one (`{"at":…,"items":[{"at":…}]}` holds "at"
// and "items[].at"), then its own (memberAt). Each step is joined by a
// byte no JSON key holds (stepKey, stepElem), so a key holding '.', '['
// or ']' ("a[]", "b.at") never spells another member's path. It is of no
// member ("") where no field holds it, where no container is open there,
// and where one of them is named by no key (fail closed: such an instant
// pairs only by count).
func (dc *decider) placesIn(text string, fields []rawField, spans []instant) placed {
	at := make([]instantPlace, len(spans))
	var w jsonWalk
	f, field, from, to, prefix := 0, -1, 0, len(text), ""
	for i, sp := range spans {
		dc.passes++
		at[i] = instantPlace{shape: instantShape(text[sp.start:sp.end]), layout: sp.layout}
		if len(fields) > 0 {
			for f < len(fields) && fields[f].end <= sp.start {
				f++
			}
			if f == len(fields) || fields[f].start > sp.start {
				continue
			}
			if f != field {
				field, from, to, prefix = f, fields[f].start, fields[f].end, listIndex.ReplaceAllString(fields[f].path, "[]")
				w = jsonWalk{from: from, at: from}
			}
		}
		dc.walked += w.to(text[:to], sp.start)
		name := memberAt(text[from:], sp.start-from)
		if top := len(w.open) - 1; name != "" && top >= 0 && !w.open[top].unnamed {
			at[i].field = prefix + stepKey + w.open[top].path + stepKey + name
		}
	}
	return placed{at: at, n: &dc.passes}
}

// listIndex is a list index in a field's path.
var listIndex = regexp.MustCompile(`\[[0-9]+\]`)

// stepKey and stepElem join a member's path (placesIn): a key, and an
// element of a list. JSON writes no control byte in a key, so no key's own
// text spells a step.
const stepKey, stepElem = "\x00", "\x01"

// jsonWalk reads a field's JSON structure, from its start (from) as far as
// at: the containers open there (open) and whether at stands in a string
// (str, esc: just after its backslash). A newline ends a string, as no
// JSON string holds one, so a quote in a text of another kind misleads the
// walk for that line alone.
type jsonWalk struct {
	from, at int
	open     []jsonOpen
	str, esc bool
}

// jsonOpen is a JSON container open in a walk: its path from the field's
// root, whether it is a list, and of an object, the key whose value is
// read now. unnamed: it, or a container it stands in, is the value of a
// member named by no key keploy reads.
type jsonOpen struct {
	path, key     string
	list, unnamed bool
}

// to walks text from w.at to end, and reports how many bytes it walked.
func (w *jsonWalk) to(text string, end int) (n int) {
	n = end - w.at
	for ; w.at < end; w.at++ {
		c, top := text[w.at], len(w.open)-1
		switch {
		case w.esc:
			w.esc = false
		case w.str:
			w.esc, w.str = c == '\\', c != '"' && c != '\n'
		case c == '"':
			w.str = true
		case top < 0 && (c == ':' || c == ','):
		case c == ':':
			w.open[top].key = keyBefore(text[w.from:w.at])
		case c == ',':
			w.open[top].key = ""
		case c == '{' || c == '[':
			o := jsonOpen{list: c == '['}
			if top >= 0 {
				p := w.open[top]
				o.path, o.unnamed = p.path+stepKey+p.key, p.unnamed || !p.list && p.key == ""
				if p.list {
					o.path = p.path + stepElem
				}
			}
			w.open = append(w.open, o)
		case (c == '}' || c == ']') && top >= 0:
			w.open = w.open[:top]
		}
	}
	return n
}

// keyBefore is the key a member's colon closes, the end of text: a quoted
// name, or a bare word (`{ SectionSingle msg: {…} }` names msg); "" where
// none stands there, or where the quoted name holds a quote (quotedName).
func keyBefore(text string) string {
	s := strings.TrimRight(text, " \t\r\n")
	if q, ok := strings.CutSuffix(s, `"`); ok {
		return quotedName(q)
	}
	return s[strings.LastIndexAny(s, " \t\r\n{}[],:\"")+1:]
}

// memberAt is the name of the JSON member whose value starts at start in
// text, read back over the value's quote and the colon (`"created_at":"…`
// is created_at); "" where no member's name stands just before it.
func memberAt(text string, start int) string {
	s, ok := strings.CutSuffix(strings.TrimRight(strings.TrimSuffix(text[:start], `"`), " "), ":")
	if !ok {
		return ""
	}
	if s, ok = strings.CutSuffix(strings.TrimRight(s, " "), `"`); !ok {
		return ""
	}
	return quotedName(s)
}

// quotedName is the name a quote opens at the end of s (its closing quote
// cut off); "" where the last quote in s is escaped: the name holds a
// quote, and read from it on it would spell another member's name
// (`"x\"at"` is no "at"), so its member is named by no key (fail closed).
func quotedName(s string) string {
	i := strings.LastIndexByte(s, '"')
	if i > 0 && s[i-1] == '\\' {
		return ""
	}
	return s[i+1:]
}

// instantShape is an instant's text with its fraction left out and each
// digit read as 0: two texts of one encoder's field, one written without
// its trailing zeros, read the same.
func instantShape(s string) string {
	s = fractionText.ReplaceAllString(s, "")
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return '0'
		}
		return r
	}, s)
}

// fractionText is a fraction as an instant's text writes it.
var fractionText = regexp.MustCompile(`[.,][0-9]+`)

// heldOutside reports whether t's text holds w as a word outside each of
// its instants finer than m (spans, each at the finest unit either run
// shows there): inside one, w is that finer instant's text cut to m (a
// time written without its trailing zeros, the whole seconds of a number
// of seconds with its fraction), which no variable held. A time-ordered
// UUID's time groups (no unit) are left out too: no instant's word stands
// in one. True where there is none, or where keploy could not read the
// text.
func heldOutside(t *readerText, spans []instant, w string, m moment) bool {
	text, ok := t.textNow()
	if !ok {
		return true
	}
	b, masked := []byte(text), false
	for _, sp := range spans {
		if sp.res < m.res {
			for j := sp.start; j < sp.end; j++ {
				b[j] = ' '
			}
			masked = true
		}
	}
	return !masked || heldIn(textForms(string(b)), w, 0, 1, 2)
}

// namedBy counts the instants of t that name m, or m in the application's
// zone (inZone), by the rule by; 1 where keploy could not read them.
func (dc *decider) namedBy(t *readerText, m moment, by func(h, m moment) bool) int {
	spans, read := t.spansOf()
	if !read {
		return 1
	}
	return dc.namedIn(spans, m, by)
}

// namedFiner reports whether an instant of t, at the finest unit its place
// shows in either run (atFinest), names m by the rule by.
func (dc *decider) namedFiner(t, other *readerText, m moment, by func(h, m moment) bool) bool {
	spans, read := dc.atFinest(t, other)
	return read && dc.namedIn(spans, m, by) > 0
}

// namedIn counts the instants of spans that name m, or m in the
// application's zone (inZone), by the rule by.
func (dc *decider) namedIn(spans []instant, m moment, by func(h, m moment) bool) int {
	ms := []moment{m}
	if z, ok := dc.inZone(m); ok {
		ms = append(ms, z)
	}
	n := 0
	for _, sp := range spans {
		for _, m := range ms {
			if sp.keyed && by(sp.moment(), m) {
				n++
			}
		}
	}
	return n
}

// instant decides one changed instant by the one order: a conflict at
// a fine cut (0), a pick (1), a given's text, one templated slot of the
// test case's request (2), an answer (3), a fine copy (4), a Clock its
// call's window holds (5), a coarse copy (6), a read since its test began
// (7), a client test's own value (8); else refused. A field keploy.yml
// declares a read of the clock is then decided by its rule (declaredClock).
func (dc *decider) instant(path string, at variables.Address, n int, c changedInstant) Decision {
	dc.readClock = false
	d := dc.instantLearned(path, at, n, c)
	src, declared := dc.source(path)
	declared = declared && src.Kind == SourceClock
	if declared {
		d = dc.declaredClock(path, at, n, c, src, d)
	}
	return dc.timeDefault(path, at, n, c, d, declared)
}

// instantLearned is instant as learning alone decides it.
func (dc *decider) instantLearned(path string, at variables.Address, n int, c changedInstant) Decision {
	defer dc.letGoHalves()
	r, l := c.r, c.l
	inScope := dc.inScope
	// A read of the application's clock that the call's own window holds in
	// both runs goes before a client test's request (readOverClients): no
	// place of the request is read for the moment, as main reads the call.
	earlier := dc.in.Earlier
	if c.rm != nil && c.lm != nil {
		earlier = readOverClients(earlier, dc.in.Window, []driftedInstant{{rec: *c.rm, live: *c.lm}}, dc.dr.zone)
	}
	overClients := len(earlier) < len(dc.in.Earlier)
	// untold: answers of the scope before the call, whose view in round F
	// keploy cannot tell, that hold F's moment and not R's.
	var hs []instantHold
	var untold []evPlace
	held := func(p evPlace) bool {
		hr, _, _ := dc.holds(p, p.t.rec, r, c.rm, c.byText)
		hl, _, _ := dc.holds(p, p.t.live, l, c.lm, c.byText)
		return hr || hl
	}
	for i, q := range dc.ps {
		// A moment a recording's request holds is what the application
		// sent there: a send, placed by when it was sent and decided by the
		// send rules (4, 5, 6), as an exchange's request leaf is, whenever
		// (or if ever) it was answered. Its answer is what the recording gave
		// the application, placed by when it was answered (halves).
		ps := []evPlace{q}
		if q.raw > 0 && held(q) {
			req, ans := dc.halves(i, q)
			ps = []evPlace{req, ans}
		}
		for _, p := range ps {
			if overClients && dc.clientCase(p.mock) {
				continue
			}
			hr, fr, okR := dc.holds(p, p.t.rec, r, c.rm, c.byText)
			hl, fl, okL := dc.holds(p, p.t.live, l, c.lm, c.byText)
			if !okR || !okL {
				return dc.refuse(path, at, n, r, l, RefusalUnread, dc.unreadWhy(), nil, Edit{Kind: EditReRecord}, dc.fieldNoise(path))
			}
			if hr || hl {
				hs = append(hs, instantHold{p: p, hr: hr, hl: hl, fr: fr, fl: fl, coarseR: hr && c.rm != nil && dc.coarserOnly(p.t.rec, r, *c.rm),
					rank: min(dc.rank(p.t.rec, r, c.rm, hr), dc.rank(p.t.live, l, c.lm, hl))})
			}
			if hr && !hl && len(c.aside) < maxLinePlaces && !slices.Contains(c.aside, p.name) {
				c.aside = append(c.aside, p.name)
			}
			if p.fUnstated && !hr && p.side == Response && !p.given && p.state == stateBefore && inScope(p) && (hl || dc.inAView(p, c)) {
				untold = append(untold, p)
			}
		}
	}
	// A place that holds a value by its exact text is named before one
	// that holds it by a cut, and a finer cut before a coarser one.
	sort.SliceStable(hs, func(i, j int) bool { return hs[i].rank < hs[j].rank })
	before := func(p evPlace) bool { return p.state == stateBefore && !p.fUnstated }
	request := func(p evPlace) bool { return p.given && !p.gave }
	// A copied text or number is a source by its text (3, 4, 6), one rule
	// across protocols: a variable holds one text, so a place that holds the
	// same moment only in another form (epoch milliseconds beside a date and
	// time) is no place to bind it. A typed instant is held by its moment.
	asText := func(t *readerText, w string) bool {
		held, _, _ := dc.holds(evPlace{gave: true}, t, w, nil, false)
		return held
	}
	byText := func(h instantHold) bool {
		return !c.text || asText(h.p.t.rec, r) && (h.p.fUnstated || asText(h.p.t.live, l))
	}

	// 4. and 6. A copy: requests of the call's scope or the start-up, sent
	// before the call in both runs, holding the same moment in both runs.
	// One that holds the other run's moment too (a range that grew in F to
	// hold the recorded moment and F's) did not move with the value: no
	// copy's source.
	copies := func(fine bool) []evPlace {
		return heldWhere(hs, func(h instantHold) bool {
			p := h.p
			if p.side != Request || p.given || !inScope(p) || !before(p) || !h.hr || !h.hl || (h.fr && h.fl) != fine || !byText(h) {
				return false
			}
			rf, _, _ := dc.holds(p, p.t.live, r, c.rm, c.byText)
			fr, _, _ := dc.holds(p, p.t.rec, l, c.lm, c.byText)
			return !rf && !fr
		})
	}

	// 0. Conflict: a place before in both runs that held R's value whole
	// at a fine cut (or by the text of a number that does not parse), or a
	// given that held its text, where round F gave another value and this
	// one did not follow. An answer whose view in round F still holds R's
	// value (keploy served it as recorded for the moment) gave F's
	// application R's value, which the call did not carry: where a send of
	// the scope shows the moment in both runs at a fine cut (a copy, 4),
	// the call followed that send, never the answer, so the answer is no
	// conflict. With no such send it stays one, as does an answer whose
	// view F moved to a third value.
	asRecorded := func(h instantHold) bool {
		held, _, _ := dc.holds(h.p, h.p.t.live, r, c.rm, c.byText)
		return h.p.side == Response && held
	}
	copied := len(copies(true)) > 0
	conflict := heldWhere(hs, func(h instantHold) bool {
		return before(h.p) && inScope(h.p) && h.hr && !h.hl && (h.fr || h.p.gave) && (!copied || !asRecorded(h))
	})
	if len(conflict) > 0 {
		p := conflict[0]
		return dc.refuse(path, at, n, r, l, RefusalConflict,
			strconv.Quote(r)+" stood whole at "+p.name+"; round F gave "+gaveAt(p, l)+" there and the value did not follow", names(conflict),
			Edit{Kind: EditFixApp}, dc.fieldNoise(path), Edit{Kind: EditGive, Name: givenName(path)})
	}
	// readsCopy: an answer that holds round F's moment by the copy's text
	// where a send of the scope shows that moment in both runs at a fine cut
	// (a copy, 4). The answer reads back what the application had sent (a
	// find of the row an insert of the scope wrote, an UPDATE that returns
	// the time it set), so the call followed the send, as at step 0: round F
	// giving such an answer before the call, though it was recorded after
	// it, makes it no pick and no second source. The bind at the send is
	// checked at replay, so a call whose moment is not the send's fails
	// closed there. Held by another text of a text's moment, or with no such
	// send, the answer stays a pick.
	readsCopy := func(h instantHold) bool {
		return copied && (!c.text || asText(h.p.t.live, l))
	}

	// 1. Pick: (i) round F's moment stands at an answer or the test case's
	// request as sent, which does not show it; (iii) the recorded moment
	// stands at an answer whose state in round F keploy cannot tell; (ii)
	// the recorded moment stands in the test case's request by a cut rule
	// that gives no moment F saw in it as sent. Never at a given.
	//
	// A place that holds round F's moment and not the recorded one only
	// because it reads back what a send of the scope wrote before it, a send
	// its own call learned as its read of the application's clock (clock),
	// is no pick (i) where the value is cut coarser than a millisecond and
	// the call's own window holds the read, or a client test's window since
	// its begin does (windowRead): the place holds the send's text as
	// recorded and as round F sent it, so the moment there is the
	// application's own earlier read of the clock, which met the call's in
	// one cut in round F alone (a record stamped at 09:38:01 when recorded,
	// read back before a call of 09:38:02; both at 09:39:04 in round F). A
	// replay reads the place back from the send's own read, as fresh as the
	// call's, so whichever the call took, the later steps decide the value
	// (a read of the clock its window holds, checked at replay), as with no
	// such place, and step 3's rule (an answer that only reads back a coarse
	// clock send is no source) reads the same place alike. A place that
	// shows the same text in both runs, a send learned as a plain value or
	// after the place, one of another test, a cut of a millisecond or finer
	// (two reads do not meet in one as the runs' timing falls), and a value
	// no such window holds stay picks.
	readIn := func(w *Window) bool { return w.holdsRead(*c.rm, *c.lm, dc.dr.zone) }
	windowRead := func() bool {
		return c.rm != nil && c.lm != nil && (readIn(dc.in.Window) || dc.dr.clientTests && readIn(dc.in.Window.sinceBegin()))
	}
	// Asked last of case (i), where the place does not show the recorded
	// moment unless it is unchanged (same), which reads back no new send.
	readsBack := func(p evPlace) bool {
		// windowRead holds only where both moments are known.
		if p.same() || !windowRead() || max(c.rm.res, c.lm.res) <= time.Millisecond {
			return false
		}
		return slices.ContainsFunc(dc.ps, func(s evPlace) bool {
			if !s.clock || !inScope(s) || !s.at.Before(p.at) {
				return false
			}
			inR, _, _ := dc.holds(p, p.t.rec, s.rec, nil, false)
			inF, _, _ := dc.holds(p, p.t.live, s.live, nil, false)
			return inR && inF
		})
	}
	for _, h := range hs {
		p := h.p
		if p.gave || p.side != Response && !request(p) {
			continue
		}
		untold := func(note string) Decision {
			return dc.refuse(path, at, n, r, l, RefusalPick,
				"the recorded moment stands in "+p.name+", whose state in round F keploy cannot tell ("+note+")", []string{p.name},
				Edit{Kind: EditReRecord}, dc.fieldNoise(path))
		}
		// A pick needs round F's moment at the call's own cut. A place that
		// holds it only as a finer time cut or rounded to it (a field's
		// whole $date of milliseconds, a UnixNano inside a name) gave the
		// application no value at that cut to pick, as at step 3: the later
		// steps read it, where the call's own window reads the clock,
		// checked at replay. One keploy cannot read is a pick (fail closed).
		// It is asked only where round F's text there holds the moment.
		finer := func(h, m moment) bool { return h.res < m.res && covers(h, m) }
		cut := func() bool {
			return c.lm != nil && dc.cutOfFinerOnly(p.t.live, p.t.rec, l, *c.lm) && dc.namedFiner(p.t.live, p.t.rec, *c.lm, finer)
		}
		switch {
		case before(p) && h.hl && (!h.hr || p.same()) && !cut() && !readsBack(p):
			why := "round F's moment stands unchanged in " + p.name
			if !p.same() {
				why = "round F's moment stands in " + p.name + ", which does not show it"
			}
			return dc.refuse(path, at, n, r, l, RefusalPick, why, []string{p.name}, dc.fieldNoise(path))
		// An answer recorded after the call that round F gave before it:
		// in round F the value may be a pick from it, as main reads it.
		// The recorded application never read it, so R's moment standing
		// there too (by another cut) says nothing. One that reads back a
		// copy's send (readsCopy) is no pick.
		case p.fGave && p.state == stateNotBefore && p.side == Response && h.hl && !cut() && !readsCopy(h):
			return dc.refuse(path, at, n, r, l, RefusalPick, "round F's moment stands in "+p.name+", which round F gave before the call but which was recorded after it", []string{p.name}, dc.fieldNoise(path))
		case p.side == Response && p.state == stateUnknown && (h.hr || h.hl):
			return untold(p.why)
		case h.hr && dc.swappable(p, c):
			return untold(swapNote)
		}
	}
	if c.rm != nil && c.lm != nil {
		// A client test's request is the client's own in each run, never
		// one keploy sent as recorded. A leaf of it that shows the value by
		// its text in both runs moved with the value: a slot (2), whatever
		// unit its digits end at (a UnixNano that ends in 0 beside one that
		// does not). One that holds the recorded moment only as a cut of a
		// finer instant (the UnixNano in a name the client made, beside the
		// second the application read from its own clock a moment later)
		// gave the application no value at that cut to pick, as in the loop
		// above. Neither is a place the recorded moment stands at by a rule
		// round F's does not follow: the later steps read the value, and
		// one no window holds stays refused, naming the place.
		followed := map[[2]string]bool{}
		for _, h := range hs {
			if p := h.p; h.hr && request(p) && dc.clientCase(p.mock) &&
				(h.hl && asText(p.t.rec, r) && asText(p.t.live, l) || dc.cutOfFinerOnly(p.t.rec, p.t.live, r, *c.rm)) {
				followed[[2]string{p.mock, p.path}] = true
			}
		}
		reqs := slices.DeleteFunc(slices.Clone(earlier), func(e Earlier) bool { return !e.Given || e.Gave || e.Unread != "" })
		for i, e := range reqs {
			if len(followed) > 0 && e.incoming {
				reqs[i].Seen = slices.DeleteFunc(slices.Clone(e.Seen), func(s Seen) bool { return followed[[2]string{e.Mock, s.Leaf.Path}] })
			}
		}
		if where, _ := dc.dr.requestHolds([]moment{*c.rm}, []moment{*c.lm}, reqs); where != "" {
			return dc.refuse(path, at, n, r, l, RefusalPick,
				"the recorded moment stands in the test case at "+where+", and no cut there gives round F's", []string{where}, dc.fieldNoise(path))
		}
	}

	// A given's text, and 2. one templated slot of the test case's request
	// that shows it in both runs: a Value bound there; two slots, refused.
	// A client test's request that holds the moment in both runs only as a
	// coarse cut of a finer instant (the UnixNano in a name, beside a
	// second) is no slot keploy can bind: a variable holds one text. Where
	// the call's own window holds the read, or the test's since its begin,
	// the value is that read (5, 7), as an answer's is at step 3 and as main
	// reads it; with no such read it stays a slot, which the adapter that
	// cannot bind another form refuses.
	cutSlot := func(p evPlace) bool {
		return c.rm != nil && c.lm != nil && dc.clientCase(p.mock) && dc.cutOfFinerOnly(p.t.rec, p.t.live, r, *c.rm) && dc.cutOfFinerOnly(p.t.live, p.t.rec, l, *c.lm) && windowRead()
	}
	gave := heldWhere(hs, func(h instantHold) bool { return before(h.p) && h.hr && h.hl && h.p.gave })
	slots, again := oneSource(oneSlotEach(heldWhere(hs, func(h instantHold) bool {
		return before(h.p) && h.hr && h.hl && request(h.p) && !h.p.same() && !cutSlot(h.p)
	})), c)
	if len(gave) > 0 {
		return dc.bound(path, at, n, c, gave, ": a value the test gave keploy, which the call sends as given in both runs")
	}
	switch len(slots) {
	case 0:
	case 1:
		return dc.bound(path, at, n, c, slots, ": the test case's request holds it, templated, in both runs"+again)
	default:
		return dc.refuse(path, at, n, r, l, RefusalTwoSources,
			"the same moment was sent at "+slots[0].name+" and "+slots[1].name+", two separate reads", names(slots), dc.fieldNoise(path))
	}

	// 3. Answer: an answer of the call's scope or the start-up, given
	// before the call in both runs, holds R's value, and F's text there
	// holds F's or may (fGaveIt).
	// fGaveIt: F's text at an answer holds F's moment, a view F served there
	// does, or F served it with no view told or read (fMay). Where F served
	// it changed and no view it gave holds F's moment, F's application took
	// its moment from elsewhere: the answer is no source, in or outside the
	// call's scope alike (an answer F never served is never before in F).
	fGaveIt := func(h instantHold) bool { return h.p.fMay || h.hl || dc.inAView(h.p, c) }
	answer := func(h instantHold) bool {
		p := h.p
		return p.side == Response && !p.given && inScope(p) && p.state == stateBefore && h.hr && !h.coarseR && fGaveIt(h) && byText(h)
	}
	answers := heldWhere(hs, answer)
	// An answer that holds it only as a coarse cut of a finer instant is no
	// source keploy can bind, and where the call's own window holds the
	// read, it is that read (5), as main reads it; so is a client test's
	// read since its begin (7).
	cutOnly := len(heldWhere(hs, func(h instantHold) bool {
		return answer(h) && !h.fr && c.rm != nil && dc.cutOfFinerOnly(h.p.t.rec, h.p.t.live, r, *c.rm)
	})) == len(answers)
	ownRead := cutOnly && windowRead()
	// An answer that only reads back what a send of the scope wrote, where
	// that send holds the moment at a cut coarser than a millisecond in both
	// runs and its own call learned it as its read of the clock (clockSrc,
	// as step 6 reads a send), is no source either: every such answer comes
	// after the send, so it holds the moment because the test wrote it there
	// a few milliseconds before, and two reads of one coarse cut are no copy
	// (step 6). Where the call's own window holds the read, or a client
	// test's window since its begin does, the moment goes on to steps 5, 7
	// and 6 as with no answer, each checked at replay; where neither does,
	// the value is a copy and the answer still binds it. A send at a
	// millisecond or finer still makes the answer a copy's (4), an answer no
	// such send came before still binds, and another test's answer is never
	// one of these.
	if !ownRead && len(answers) > 0 && c.rm != nil && c.lm != nil && max(c.rm.res, c.lm.res) > time.Millisecond && windowRead() {
		if coarse := copies(false); len(coarse) > 0 && earliest(coarse).clock && earliest(coarse).at.Before(earliest(answers).at) {
			ownRead = true
		}
	}
	if len(answers) > 0 && !ownRead {
		// An answer recorded after the call that round F gave the
		// application before it, holding F's moment, is a second source:
		// the bind would be a guess between the two; one that reads back a
		// copy's send (readsCopy) is none.
		// So is another test's answer given before the call whose view in
		// round F keploy cannot tell, holding R's moment apart from F's.
		late, why := dc.setSecondSource(c)
		for _, h := range hs {
			p := h.p
			switch {
			case late != "" || p.side != Response || p.given:
			case p.fGave && p.state == stateNotBefore && h.hl && !readsCopy(h):
				late, why = p.name, "round F's moment stands in "+p.name+", which round F gave before the call but which was recorded after it"
			case !inScope(p) && p.state == stateBefore && p.fUnstated && h.hr && !h.hl:
				late, why = p.name, "the recorded moment stands in "+p.name+", another test's answer given before the call, whose view in round F keploy cannot tell"
			}
		}
		if late != "" {
			return dc.refuse(path, at, n, r, l, RefusalUnknown, why+", beside "+answers[0].name+": two sources", []string{late, answers[0].name},
				Edit{Kind: EditReRecord}, dc.fieldNoise(path))
		}
		// A send before every such answer, at a fine cut, showing the same
		// moment: the answer reads back what the application wrote, and
		// the value is bound where a word's is, at its earliest place.
		if fine := copies(true); len(fine) > 0 && earliest(fine).at.Before(earliest(answers).at) {
			return dc.copied(path, at, n, c, fine, fineCopy)
		}
		return dc.bound(path, at, n, c, answers, ": an answer given before the call holds it: the dependency's value")
	}

	if fine := copies(true); len(fine) > 0 {
		return dc.copied(path, at, n, c, fine, fineCopy)
	}

	// An answer outside the call's scope that would bind it at step 3 were
	// it of the scope: a source outside the scope, refused naming the edit,
	// as a word's is; never a read of the clock. So is one main's echo of the
	// recordings outside the scope refuses (echoedElsewhere), and one round
	// F's view of a recording of the set holds (echoedOutside).
	// At a millisecond or coarser such an answer may hold the moment only
	// as two stamps the runs' timing put in one cut: where the call's own
	// window holds the read (5) it is that read, and else it is refused so.
	// One that holds it finer is a pick from it, refused here.
	var gaveOutside []string
	var gaveRefusal *Decision
	fine := false
	for _, h := range hs {
		if p := h.p; p.side == Response && !p.given && !inScope(p) && p.state == stateBefore && h.hr && fGaveIt(h) {
			gaveOutside, fine = append(gaveOutside, p.mock), fine || h.fr || h.fl
		}
	}
	if mock, ok := dc.echoedOutside(c, earlier); ok {
		gaveOutside, fine = append(gaveOutside, mock), fine || fineCut(c.rm.res) || fineCut(c.lm.res)
	}
	if len(gaveOutside) > 0 {
		d := dc.refuse(path, at, n, r, l, RefusalPick, "round F's moment, or the recorded one, stands in an answer round F gave before the call outside its scope", nil, Edit{Kind: EditReRecord}, dc.fieldNoise(path))
		if m := gaveOutside[0]; m != "" {
			d = dc.refuse(path, at, n, r, l, RefusalAnotherScope,
				"the moment stands in "+m+"'s answer, which round F gave before the call outside its scope: a copy or a pick from it, never a read of the clock; keploy binds only within a test and the start-up", []string{m},
				Edit{Kind: EditMoveToMocks, Mock: m}, Edit{Kind: EditMoveToStartup, Mock: m}, dc.fieldNoise(path))
		}
		if fine || len(untold) > 0 {
			return d
		}
		gaveRefusal = &d
	}

	// F may have given its moment at an answer whose view keploy cannot tell,
	// and no source above names it: the value may be a copy of that answer,
	// so no later step may read it as the clock or a coarse copy. Unknown,
	// as the word rule reads the same place (evidenceOf).
	if len(untold) > 0 {
		p := untold[0]
		return dc.refuse(path, at, n, r, l, RefusalUnknown,
			"keploy cannot tell what round F gave at "+p.name+" before this call (round F's moment may stand there, and the recorded one does not)", names(untold),
			Edit{Kind: EditReRecord}, dc.fieldNoise(path))
	}

	// 5. Clock: the call's own window holds it, as a plain read or plus an
	// offset. Read before a coarse copy (6): a read of now minus a debounce
	// beside an earlier send of the same second (a claim's dirtyAt <= now-2s
	// two seconds after the mark that set dirtyAt = now) is the call's own
	// read, and bound to the send it would go red when the two cross a cut.
	// Step 5 gives a Clock only where a replay can check it: where the
	// reading finds no offset, refuses the one it finds, or finds one this
	// protocol's replay does not compare, a coarse copy is still tried (a
	// millisecond read carried to a later send its window does not hold);
	// with none, an offset it does not compare is refused as one keploy
	// cannot check, except where a place of the scope held the moment
	// before the call only in another form: the value is as much a copy of
	// that place as a look-back, so it gets the last refusal, which names
	// the place, with the guard's reason beside it, and steps 7 and 8 do
	// not read it (read from the test's begin, a look-back would pass
	// dropped).
	zone := dc.dr.zone
	// otherForm: a place of the scope, before the call, that held the
	// moment only in another form (epoch nanoseconds in a name beside a
	// second), which no variable holds as this text.
	otherForm := func(h instantHold) bool {
		p := h.p
		return h.hr && (h.hl || p.fUnstated) && inScope(p) && p.state == stateBefore && !byText(h)
	}
	// cutWhy: the reading's own refusal of a cut, which keeps steps 7 and
	// 8 from reading the value; guardWhy: the offset guard's refusal of the
	// window plus an offset, which refuses that reading alone, so the value
	// is still read from the test's begin (7) and as its own value (8), as
	// main learned such a read since the test began.
	var off models.ClockOffset
	var cutWhy, guardWhy string
	inWindow := false
	if c.rm != nil && c.lm != nil {
		// The reading goes first: a wide window also holds a look-back as a
		// plain read, and learned plain it fails nearly every replay.
		off, cutWhy, inWindow = clockInWindow(path, c.ri, c.li, dc.in.Window, zone)
		if off.Since == models.SinceTestBegin {
			// A read plus an offset is read in its call's own window
			// alone, as main's learn step reads it: the reading found
			// this one only from its test's begin, where an offset fitted
			// to a window as wide as the test's span passes a time to live
			// changed by up to that span (concurrent_window_test.go). From the
			// begin only a plain read is taken (step 7).
			own := *dc.in.Window
			own.Rec.Began, own.F.Began = time.Time{}, time.Time{}
			off, cutWhy, inWindow = clockInWindow(path, c.ri, c.li, &own, zone)
		}
		if !inWindow && cutWhy == "" && dc.in.Window.holdsRead(*c.rm, *c.lm, zone) {
			off, cutWhy, inWindow = models.ClockOffset{Res: max(c.rm.res, c.lm.res)}, "", true
		} else if inWindow {
			switch g, uncompared := dc.offsetGuard(path, off, max(c.rm.res, c.lm.res), dc.in.Window); {
			case uncompared && copies(false) == nil && len(heldWhere(hs, otherForm)) > 0:
				off, cutWhy, inWindow = models.ClockOffset{}, g, false
			case uncompared && copies(false) == nil:
				if gaveRefusal != nil {
					return *gaveRefusal
				}
				return dc.heldRead(dc.refuse(path, at, n, r, l, RefusalCannotCheck, g, nil, dc.fieldNoise(path)))
			case g != "":
				off, cutWhy, guardWhy, inWindow = models.ClockOffset{}, "", g, false
			}
		}
	}
	// A look-back or a time to live whose moment a call of another test sent
	// before this call in both runs is that call's value carried: its offset
	// is the gap between the two calls, which a replay moves, so it is no
	// read of this call's clock. It is refused naming that call, as a source
	// outside the scope is (keploy binds only within a test and the
	// start-up); a plain read its own window holds stays a read. So is a
	// send that call's learn step learned as its own read of the clock plus
	// this one's offset at a finer cut (sameRead): two reads of one time to
	// live (a grant's expires_at at the millisecond, then a refresh token's
	// expiration at the second, both clock + 30d), which share this word's
	// cut only as the runs' timing falls. A copy of it shows the offset its
	// own read added only where the gap fits in this word's cut, and a
	// replay where it does not goes red.
	if inWindow && !off.Plain() {
		if carried := heldWhere(hs, func(h instantHold) bool {
			return h.hr && h.hl && h.p.otherTest && !dc.allScope && before(h.p) && !sameRead(h.p.offset, off, max(c.rm.res, c.lm.res))
		}); len(carried) > 0 {
			off, cutWhy, inWindow = models.ClockOffset{}, clockCarriedWhy(path, off, carried[0].name), false
		}
	}
	if inWindow {
		return dc.clock(path, at, n, c, off, "a read of the application's clock its call's window holds in both runs", copies(false) != nil)
	}
	if gaveRefusal != nil {
		return *gaveRefusal
	}

	// 6. Coarse copy: as 4, a millisecond or coarser; no window condition.
	// A send that is itself its call's read of the clock says nothing of
	// this one: two reads of one coarse cut, which share it only as the
	// runs' timing falls. It goes on to step 7, whose check goes red where it
	// was no read, and stays refused where 7 declines.
	// A cut of exactly a millisecond is no coarse cut: two reads do not
	// share one as the runs' timing falls, so such a send stays a source.
	// A typed instant's places that name its moment at its own cut come
	// first (a BSON date's place holds its raw milliseconds, not the text
	// its leaf writes), and with none it goes on to step 7 too; a text or
	// number is held by its text alone.
	coarse := copies(false)
	const copiedWhy = ": the same moment, at a millisecond or coarser, was sent before the call in both runs and its own window does not hold it: one read, copied"
	exact := func(h, m moment) bool { return h.key == m.key && h.res == m.res }
	sameText := slices.DeleteFunc(slices.Clone(coarse), func(p evPlace) bool {
		if !c.text && c.rm != nil && c.lm != nil {
			return dc.namedBy(p.t.rec, *c.rm, exact) == 0 || dc.namedBy(p.t.live, *c.lm, exact) == 0
		}
		return !asText(p.t.rec, r) || !asText(p.t.live, l)
	})
	clockSrc := func(src []evPlace) bool {
		return earliest(src).clock && (c.rm == nil || c.lm == nil || max(c.rm.res, c.lm.res) > time.Millisecond)
	}
	if len(sameText) > 0 && !clockSrc(sameText) {
		return dc.copied(path, at, n, c, sameText, copiedWhy)
	}

	// 7. Since begin (client tests): a plain read the window from each
	// run's test begin to the call holds (a read the application made
	// earlier in its test, before its call's window), checked from the
	// test's begin where the reading takes it (clockSinceBegin) and the
	// guards of step 5 keep it. Never a read plus an offset, whichever
	// reading is registered: fitted to a window as wide as the test's span,
	// it would pass a look-back or a time to live the application changed,
	// or dropped, within that span, so main's learn step takes only a plain
	// read there. A reading or guard that declines it leaves step 8 as it
	// was.
	if dc.dr.clientTests && c.rm != nil && c.lm != nil && cutWhy == "" {
		if wb := dc.in.Window.sinceBegin(); wb != nil && wb.holdsRead(*c.rm, *c.lm, zone) {
			enc := max(c.rm.res, c.lm.res)
			off := models.ClockOffset{Res: enc}
			if ln, sok := clockSinceBegin(Learned{Kind: KindClockOffset, Offset: &off}); sok && ln.Offset != nil {
				switch g, uncompared := dc.offsetGuard(path, *ln.Offset, enc, wb); {
				case uncompared:
					return dc.heldRead(dc.refuse(path, at, n, r, l, RefusalCannotCheck, g, nil, dc.fieldNoise(path)))
				case g == "":
					return dc.clock(path, at, n, c, *ln.Offset, "a read of the application's clock made since its test began in both runs, before its call's window", false)
				}
			}
		}
	}

	if len(coarse) > 0 && (!clockSrc(coarse) || twoClockReads(coarse)) {
		return dc.copied(path, at, n, c, coarse, copiedWhy)
	}

	// 8. The test's own value (client tests): no earlier place holds it in
	// either run, its answer does not echo it, its cut is not ambiguous,
	// and, where the runs know their test's begin, it was made before it.
	// An instant its protocol types (a BSON date) in a call that carries
	// the application's clock is never one: main's learn step binds a
	// test's own value only in a text (the harness's "[E2E] app-<UnixNano>"),
	// and learns a typed instant there only as a read of that clock.
	late, typed := "", false
	if dc.dr.clientTests && len(hs) == 0 && cutWhy == "" && dc.instantEchoed(c) == "" {
		if typed = !c.text && dc.in.Window != nil; !typed {
			if late = madeSinceBegin(dc.in.Window, dc.in.RecBegan, c, zone); late == "" {
				d := dc.learned(path, at, n, r, l, OutcomeFresh, "the test's own value: no place before the call holds it; bound at its first call"+answeredAt(c.echo, c.rec), nil)
				d.Echo = c.echo
				return d
			}
		}
	}
	why := "a time its call's window does not hold in both runs, that no answer gave and no request of this scope sent"
	if dc.in.Window == nil {
		// The adapter read no app clock for the call: name that cause.
		why = "a time no answer gave and no request of this scope sent; the call carries no app clock, so no window of its own can hold it"
	}
	if cutWhy != "" || guardWhy != "" {
		why += " (" + cutWhy + guardWhy + ")"
	}
	if late != "" {
		why += "; nor the test's own value, made before it began: " + late
	}
	if typed {
		why += "; nor the test's own value: its protocol types it as one instant, which in a call that carries the application's clock keploy learns only as a read of that clock"
	}
	// The places whose recorded text held the recorded value and that are
	// no source in round F are named with why (main named the recording
	// round F never served): a person reads where the value came from. A
	// recording's two halves are one place, named once.
	var outside, sentIn, heldAt, inOtherForm []string
	for _, h := range hs {
		switch p := h.p; {
		case otherForm(h) && len(inOtherForm) < maxLinePlaces:
			inOtherForm = append(inOtherForm, p.name)
		case h.hr && h.hl && p.otherTest && !dc.allScope:
			if len(outside) < maxLinePlaces && !slices.Contains(outside, p.name) {
				outside = append(outside, p.name)
				sentIn = append(sentIn, p.name+" in "+scopeName(p))
			}
		case h.hr && !h.hl && len(heldAt) < maxLinePlaces && !slices.Contains(heldAt, p.name):
			heldAt = append(heldAt, p.name)
			switch p.state {
			case stateNotBefore:
				why += "; the recorded value stood at " + p.name + ", which round F did not give or send before this call"
			case stateUnknown:
				why += "; the recorded value stood at " + p.name + ", whose state in round F keploy cannot tell: " + p.why
			default:
				why += "; the recorded value stood at " + p.name + ", where round F's value is another"
			}
		}
	}
	if len(outside) > 0 {
		why += " [but " + strings.Join(sentIn, ", ") + " did]"
	}
	if len(inOtherForm) > 0 {
		why += "; " + strings.Join(inOtherForm, ", ") + " held the moment only in another form, which no variable holds as this text"
	}
	return dc.refuse(path, at, n, r, l, RefusalNoClockRead, why, append(append(outside, heldAt...), inOtherForm...), dc.fieldNoise(path), Edit{Kind: EditReRecord})
}

// madeSinceBegin is why c, a changed instant of a client test's call that
// no earlier place holds, may have been made after its test began in a
// run (step 8 of the instant order: the test's own value is one made before the begin;
// one made later is a read of the clock, which steps 5 and 7 learn where a
// window holds it, plus an offset, or a value the test gave). Each run's
// begin is its window's; with no window from the begin (a call that
// carries no app clock), the recording's is recBegan and round F's is
// unknown. "" where no run knows its begin (the test's own value, as a
// test of no begin makes it), where c is no moment (a number matched by
// its text), or where each run's moment that knows its begin, its whole
// cut, lies before it, read as UTC and, for a wall clock of no zone, in
// the application's zone too. A time of day names no day, so no begin.
func madeSinceBegin(w *Window, recBegan time.Time, c changedInstant, zone *time.Location) string {
	if c.rm == nil || c.lm == nil {
		return ""
	}
	type run struct {
		name  string
		began time.Time
		m     moment
	}
	runs := []run{{"the recorded one", recBegan, *c.rm}}
	if wb := w.sinceBegin(); wb != nil {
		runs = []run{{"the recorded one", wb.Rec.From, *c.rm}, {"round F's", wb.F.From, *c.lm}}
	} else if recBegan.IsZero() {
		return ""
	}
	for _, run := range runs {
		if run.m.key.day {
			return run.name + " is a time of day, which names no day"
		}
		at := []time.Time{time.Unix(0, run.m.key.at)}
		if !run.m.zoned && zone != nil {
			at = append(at, wallIn(run.m.key.at, zone))
		}
		if slices.ContainsFunc(at, func(t time.Time) bool { return t.Add(max(run.m.res, time.Nanosecond)).After(run.began) }) {
			return run.name + " lies past its test's begin"
		}
	}
	return ""
}

// offsetGuard is main's refusal of a read plus an offset a window w holds
// (typed.go's clockOffset), whichever reading found the offset: one keploy
// cannot narrow to one value, one a window meets a wall time shown twice in
// (the application may resolve it to its first showing, an hour early), and
// one the adapter's replay does not compare (uncompared: keploy's limit,
// refused as one it cannot check). An offset its reading wrote no line for
// (main's reading, which leaves its unit to its caller) is refused too
// where it reads in a second or finer, as main refuses it: an application
// that cuts its value to that unit makes a time to live changed by one look
// the same, and a replay holds nothing of it. A reading that writes its
// offset's line has judged its unit itself. "" where none holds.
func (dc *decider) offsetGuard(path string, off models.ClockOffset, enc time.Duration, w *Window) (why string, uncompared bool) {
	if off.Min == 0 && off.Max == 0 {
		return "", false
	}
	switch {
	case off.Why == "" && off.Unit() <= time.Second:
		return clockOffsetFineWhy(path, off), false
	case !w.offsets:
		return clockOffsetUncomparedWhy(path, off), true
	case !off.Narrow():
		return clockOffsetWideWhy(path, off, enc), false
	}
	return dc.dr.offsetFallBackWhy(path, off, enc, w), false
}

// rank is how closely t names w (its moment m): 0 by its exact text,
// else the unit of the finest cut that gives it; the most where held is
// false or no cut does.
func (dc *decider) rank(t *readerText, w string, m *moment, held bool) int64 {
	const none = int64(1) << 62
	if !held {
		return none
	}
	if h, _, _ := dc.holds(evPlace{gave: true}, t, w, nil, false); h {
		return 0
	}
	if m == nil {
		return none
	}
	spans, ok := t.spansOf()
	if !ok {
		return none
	}
	best := none
	for _, sp := range spans {
		if !sp.keyed {
			continue
		}
		h := sp.moment()
		if h.key == m.key {
			return 1
		}
		cuts(h, *m, func(c cutRule) bool {
			best = min(best, c.unit)
			return false
		})
	}
	return best
}

// swappable reports whether p is a connection's start-up keploy may swap
// for another whose field holding the recorded value differs (Raw's
// swapHolds): its state in round F keploy cannot tell. A recording that is
// no start-up is no swap's, and is not loaded to ask.
func (dc *decider) swappable(p evPlace, c changedInstant) bool {
	if p.raw == 0 {
		return false
	}
	x := dc.in.Raw[p.raw-1]
	return x.startUp && x.swapHolds(x.payload().text, dc.in.Raw, func(own string) bool {
		h, _, _ := dc.holds(evPlace{}, dc.x.text(own), c.r, c.rm, c.byText)
		return h
	})
}

// clock is c learned as a read of the application's clock with its check
// off, the rule naming the window that holds it: refused where the call's
// own recorded answer holds it (replay would serve that answer's recorded
// moment), its two runs write it in different layouts, or it stands in a
// text where no replay checks it as one word (clockWordOf). A read that is
// one word of a longer text carries where (models.ClockOffset.Word), for
// every adapter alike.
func (dc *decider) clock(path string, at variables.Address, n int, c changedInstant, off models.ClockOffset, rule string, copied bool) Decision {
	r, l := c.r, c.l
	if dc.noPayload.why != "" && !copied {
		// Beside an exchange keploy holds no payload of, the value may have
		// been built on its answer: main's offset rule refuses a read of
		// the clock there, and binds one a place before the call holds as
		// a coarse copy (copied), which the order reads after the clock.
		return dc.noPayloadRefusal(path, at, n, r, l)
	}
	if echo := dc.instantEchoed(c); echo != "" {
		return dc.heldRead(dc.cannotCheck(path, at, n, r, l, "keploy cannot check a clock read its own call's recorded answer holds ("+echo+"): it would serve that answer's recorded moment"))
	}
	if c.text {
		lay := models.ClockOffset{Layout: clockTextLayout(r)}.MeetLayout(clockTextLayout(l))
		apart := lay == "" || lay == models.LayoutApart
		if c.noWord != "" {
			if form, formF := dc.claimForm(path, c); form != "" {
				// The time default checks a time in a claim of a token the
				// application signed by its form alone, at that claim: its
				// claim's forms say how a run writes it, so one its two runs
				// wrote in two layouts names none (a Layout of "" meets any).
				if apart {
					lay = ""
				}
				off.Layout, off.Form, off.FormF, off.Claim = lay, form, formF, c.claim
				rule = "a time in the " + c.claim + " claim of a JWT, checked by its form alone (" + off.Forms() + ", keploy.yml's times setting at its default), the rest of the token checked as learned; learned as " + rule
				d := dc.learned(path, at, n, r, l, OutcomeClock, rule, c.aside)
				off.Why = d.Line
				d.Clock = &off
				return d
			}
		}
		if apart {
			return dc.cannotCheck(path, at, n, r, l, "its two runs write it in different layouts")
		}
		if c.noWord != "" {
			return dc.heldRead(dc.cannotCheck(path, at, n, r, l, "a replay checks a clock read inside a text only as one word of the text as recorded, and "+c.noWord))
		}
		off.Layout, off.Word = lay, c.word
	} else if off.Layout == "" {
		off.Layout = models.LayoutTyped
	}
	if !off.Plain() {
		// The window holds the read; the value is the read plus this.
		rule += ", stored as " + models.ClockOffset{Min: off.Min, Max: off.Max, Res: off.Res}.String()
	}
	if len(c.aside) > 0 {
		// A reading's own line (an offset's) names the places set aside too.
		aside := "; set aside: " + strings.Join(c.aside, ", ") + " held the recorded value and round F's value does not stand there"
		rule += aside
		if off.Why != "" {
			off.Why += aside
		}
	}
	d := dc.learned(path, at, n, r, l, OutcomeClock, rule, c.aside)
	if off.Why == "" {
		off.Why = d.Line
	}
	d.Clock = &off
	return d
}

// bound is a Value bound at the earliest of src, rule following that
// place's name; From says Run where it is the start-up's or an earlier
// test's (the run's variable), Given (Client) where it is the test case's
// request (a client test's), and Clock where its call's learn step learned
// it as a plain read of the clock (valueByForm reads it).
func (dc *decider) bound(path string, at variables.Address, n int, c changedInstant, src []evPlace, rule string) Decision {
	first := earliest(src)
	if first.trailed {
		rule += ", sent in " + dc.in.Trails.Test + ", which this call trails"
	}
	fromAt, name, places := dc.fromAt(first, c.r, c.l), first.name, names(src)
	if claim := ClaimName(fromAt); claim != "" {
		// Bound inside a token, the line names the claim that holds it,
		// and so does its place in the line's places.
		name += ", its JWT's " + claim + " claim"
		for i, p := range places {
			if p == first.name {
				places[i] += " (its JWT's " + claim + " claim)"
			}
		}
	}
	d := dc.learned(path, at, n, c.r, c.l, OutcomeValue, "bound to "+name+rule+answeredAt(c.valueEcho, c.rec), places)
	d.From = Place{Mock: first.mock, Side: first.side, Path: first.path, Live: c.l, Run: first.startUp || first.otherTest || first.trailed, InStartUp: first.startUp,
		Given: first.given, Client: dc.clientCase(first.mock), Clock: first.clock, Offset: first.offset}
	d.FromAt = fromAt
	d.Echo = c.valueEcho
	return d
}

// sentInTurnRule is the rule of a Value bound at another call's request
// sent in the other order in each run (sentInTurn, Decision.EitherOrder).
const sentInTurnRule = ", another call of the test that sent the value too: the recording sent it before this call, round F not before it, so the two are sends of one value the application held, in either order; bound at whichever comes first and checked at the other"

// sentInTurn reports whether p, a place that leaves the word w unknown, is
// the request of another call of the call's own test that sent the call's
// whole value too (rec, live: the call's leaf in each run): a recording the
// recorded application sent before the call, at a time keploy reads, that
// round F had given the application none of before the call: served only
// after the call reached keploy (Raw.servedAfter), or not served by the
// call's read at all (a sibling still in its own learn step, or one round F
// sends later). The two are one state: whether a sibling's serve lands
// before the call's read or after it hangs on how long each waited for its
// learn step, which grows with load (Order.ArrivedAt), so the rule reads
// them alike. A request gives the application nothing, so neither call is
// the other's source, and round F gave nothing there before the call: the
// two are sends of one value the application held, which the runs made in
// another order (Decision.EitherOrder). Only a recording whose request
// alone holds the recorded word (its answer gave the application none of
// it) and the recorded value whole, as recorded or, served, with a view
// whose request alone holds round F's word and value whole, in the call's
// own test as its scope names it (Input.ScopeOf: not the start-up, another
// test, a test the call trails, or a recording of a scope keploy was not
// told): any other place stays unknown (fail closed), and so does one round
// F served before the call reached keploy, which shows what it gave there.
// A recording the scope names the call's own test is neither the start-up's
// nor another test's (inScope), so the scope alone rules those out; a
// recording of the test the call trails can still be of that scope, so it
// is ruled out by name.
func (dc *decider) sentInTurn(p evPlace, w readWord, rec, live string) bool {
	if p.raw == 0 || p.trailed || dc.in.ScopeOf(p.mock) != ScopeTest(dc.in.Test) {
		return false
	}
	r := dc.in.Raw[p.raw-1]
	if r.noTime || !r.at.Before(dc.in.At) || r.served && !r.servedAfter {
		return false
	}
	sentOnly := func(t *readerText, word, whole string) bool {
		at, ok := t.fieldsWith(word)
		return ok && len(at) > 0 && !slices.ContainsFunc(at, func(f fieldAt) bool { return !requestPath(f.path) }) && holdsWhole(t, whole)
	}
	return sentOnly(p.t.rec, w.Rec, rec) && (p.same() || sentOnly(p.t.live, w.Live, live))
}

// earliest is the first of src to happen, src's first of those at one time.
func earliest(src []evPlace) evPlace {
	return slices.MinFunc(src, func(a, b evPlace) int { return a.at.Compare(b.at) })
}

// answeredAt is the end of a word's rule that names where its call's answer
// reads it back (the answer rewritten at replay is part of what keploy
// learned), rec the request leaf's recorded text; "" where it reads none.
func answeredAt(echo []variables.Address, rec string) string {
	if len(echo) == 0 {
		return ""
	}
	return "; answered at " + addrList(echo) + ", where the call's answer holds " + strconv.Quote(rec) + " whole as recorded"
}

// fineCopy is the rule of a value bound as a copy at a fine cut.
const fineCopy = ": the same moment, at a cut finer than a millisecond, was sent before the call in both runs: one read, copied"

// twoClockReads reports whether the earliest of src and a later one were
// each learned as their call's read of the clock.
func twoClockReads(src []evPlace) bool {
	first := earliest(src)
	return first.clock && slices.ContainsFunc(src, func(p evPlace) bool { return p.clock && p.name != first.name })
}

// copied is a Value bound at the earliest of src, a copy's sources; a later
// one its own call learned as a read of the clock is a second, separate
// read, and the word is refused naming both.
func (dc *decider) copied(path string, at variables.Address, n int, c changedInstant, src []evPlace, rule string) Decision {
	first := earliest(src)
	if i := slices.IndexFunc(src, func(p evPlace) bool { return p.clock && p.name != first.name }); i >= 0 {
		d := dc.refuse(path, at, n, c.r, c.l, RefusalTwoSources,
			"the same moment was sent at "+first.name+" and "+src[i].name+", two separate reads", []string{first.name, src[i].name}, dc.fieldNoise(path))
		if first.clock {
			// Each source its own call's read of the clock: a copy of a read.
			d = dc.heldRead(d)
		}
		return d
	}
	return dc.bound(path, at, n, c, src, rule)
}

// instantEchoed is the first leaf of the call's recorded answer that echoes
// the recorded instant of the request leaf: holds its text, or an instant
// that echoes its moment (echoesMoment) and that no source holds
// (heldElsewhere); "" where none does. An HTTP answer's Date header is no
// echo: it is the server's clock at its answer, never the stored form of
// what the application sent. What heldElsewhere answers is kept for each
// moment, cut, text and form the answer asks of it, so an answer of many
// instants of one second asks it once.
func (dc *decider) instantEchoed(c changedInstant) string {
	type ask struct {
		key      instantKey
		unit     time.Duration
		text     string
		declared bool
	}
	held := map[ask]bool{}
	for _, a := range dc.in.Resp {
		if httpDateHeader(a.Path) {
			continue
		}
		typed := a.Type == Instant
		if t := dc.x.text(a.Value); textHoldsInstant(t, c.r, false) || c.rm != nil && dc.echoesMoment(t, *c.rm, func(e moment, unit time.Duration, w string, declared bool) bool {
			if typed {
				w = ""
			}
			k := ask{e.key, unit, w, declared}
			h, ok := held[k]
			if !ok {
				h = dc.heldElsewhere(e, *c.rm, unit, w, declared)
				held[k] = h
			}
			return h
		}) {
			return a.Path
		}
	}
	return ""
}

// httpDateHeader reports whether path is an HTTP answer's Date header.
func httpDateHeader(path string) bool {
	name, ok := strings.CutPrefix(path, "header.")
	return ok && strings.EqualFold(name, "Date")
}

// textHoldsInstant reports whether t holds w, an instant's text, as its
// words (holds' reading of a text); declared: in t as written or in one of
// its declared forms (formsOf: decoded, case-folded, an encoded run's
// text), w being read in such a form of another text.
func textHoldsInstant(t *readerText, w string, declared bool) bool {
	h, ok := t.holdsWord(w)
	if ok && !h && len(variables.Words(w)) > 1 {
		h, ok = t.contains(w)
		if ok && !h && declared {
			s, _ := t.textNow()
			h = strings.Contains(t.formsOf(s), w)
		}
	}
	return ok && h
}

// echoesMoment reports whether an instant of t echoes m, a value's moment,
// or m read in the run's zone (inZone): the instant is m, or m cut,
// truncated or rounded at one unit (cuts' coarser rule, m held, the
// instant the value), and elsewhere, asked with the finest such unit (0 of
// m itself), the instant's text and whether it was read in a declared form
// (the text decoded or case-folded, an encoded run's text: elsewhere then
// reads the source in its forms too), does not explain it. A database stores
// what a call sent, or that cut to what it keeps: a second of a read of
// milliseconds (a DATETIME), a day of it (a date kept as its midnight). An
// instant that m is a cut of is another moment, finer: a row's created_at
// of 597 ms that m, a read of 600, rounds at 10 ms. The unit an instant is
// written at does not say whether it is m's stored form (the read's second
// as Unix seconds may be a sign-in time the request sets beside the read),
// so an instant a source holds is that source's value (heldElsewhere),
// which its own binding binds. t's instants are read as holdsMoment reads
// them, its escapes read and its declared forms too; one as written that
// its escapes change (openedOnly: a percent-encoded time's date alone) is
// none. A text with no escapes opens to itself, read once, as written.
func (dc *decider) echoesMoment(t *readerText, m moment, elsewhere func(a moment, unit time.Duration, text string, declared bool) bool) bool {
	text, ok := t.textNow()
	if !ok {
		return false
	}
	spans, ok := t.spansOf()
	if !ok {
		return false
	}
	ms := []moment{m}
	if z, ok := dc.inZone(m); ok {
		ms = append(ms, z)
	}
	echoes := func(in string, declared bool) func(sp instant) bool {
		return func(sp instant) bool {
			if !sp.keyed {
				return false
			}
			a := sp.moment()
			unit, echo := time.Duration(math.MaxInt64), false
			for _, m := range ms {
				if a.key == m.key {
					unit, echo = 0, true
					continue
				}
				cuts(m, a, func(c cutRule) bool {
					if c.finer {
						return false
					}
					unit, echo = min(unit, time.Duration(c.unit)), true
					return true
				})
			}
			return echo && !elsewhere(a, unit, in[sp.start:sp.end], declared)
		}
	}
	x := t.x.dc
	open := unescaped(text)
	extra := x.rawExtra(text)
	return slices.ContainsFunc(spans, func(sp instant) bool { return echoes(text, false)(sp) && !escapeMoves(text, sp) }) ||
		open != text && slices.ContainsFunc(x.spansOf(open), echoes(open, true)) || slices.ContainsFunc(x.spansOf(extra), echoes(extra, true))
}

// heldElsewhere reports whether a, an instant of the call's recorded answer
// that echoes the read (moment read) at unit (echoesMoment), is held by a
// source: a place the application sent before the call that holds a and
// not the read, so the answer holds that place's value (a sign-in time the
// request sets, a created_at an earlier insert wrote), which its own
// binding binds, and not the read's stored form. A source is, by
// construction, one that could have made the value:
//   - a place the application sent: a leaf of the call's request, or of a
//     request of its scope sent before it (an exchange's, a recording's
//     request half, a client test's own request), never an answer or a
//     header a dependency gave (an HTTP Date is the server's own clock), nor
//     a value the test gave keploy;
//   - one that holds a as the answer writes it, a variable holding one text
//     (text, "" of a typed instant, held by its moment), read in the form
//     the answer's instant was read in: as written, or, where declared (the
//     answer decoded or case-folded, an encoded run's text), as written and
//     in its own declared forms, its instants and its text alike (an ISO
//     time stored as sent is held case-folded too, a percent-encoded one
//     by its plain source);
//   - a value made anew each run: in round F it holds another moment, written
//     the same way. One the same in both runs, or that F wrote otherwise,
//     binds nothing, so a replay would serve the answer's recorded moment,
//     the hazard an echo is refused for;
//   - not another read of the application's clock that its call learned at
//     a cut no finer than unit (evPlace.offset): a and the read's cut are
//     then two reads that cut alike, two sources. A leaf of the call's own
//     request that is such a read is refused itself, as an echo of its own
//     moment, which the answer holds.
//
// A place that holds the read itself is the read: a request that sets the
// read and filters by it, answered with what it set, is still the read's
// echo, as is the leaf the read stands in. The read is looked for in the
// place as written and in its declared forms, however the answer's instant
// was read: a place that holds the read in base64 beside its second holds
// the read.
func (dc *decider) heldElsewhere(a, read moment, unit time.Duration, text string, declared bool) bool {
	dc.asked++
	holds := func(t *readerText) bool {
		return dc.holdsMomentOf(t, a, declared) && !dc.holdsMomentOf(t, read, true) && (text == "" || textHoldsInstant(t, text, declared))
	}
	source := func(rec, live *readerText) bool { return holds(rec) && dc.movedOn(rec, live, a, declared) }
	for path, l := range dc.in.Rec.Leaves {
		if source(dc.x.text(l.Value), dc.x.text(dc.in.Live.Leaves[path].Value)) {
			return true
		}
	}
	for i, p := range dc.ps {
		if !dc.inScope(p) || p.gave {
			continue
		}
		if p.raw > 0 {
			if !holds(p.t.rec) {
				continue
			}
			p, _ = dc.halves(i, p)
		}
		if p.side == Request && p.state == stateBefore && !clockCut(p.offset, unit) && source(p.t.rec, p.t.live) {
			return true
		}
	}
	return false
}

// movedOn reports whether live, a place's text in round F, holds another
// moment than a where rec, its recorded text, holds a, written in a layout
// rec writes a in: a value made anew each run, in one form. A layout's
// instants are all keyed or none (a time-ordered UUID's), and one that
// names a is keyed, so an instant of live in its layout is too. declared:
// both texts are read in their declared forms too, and an instant of rec
// pairs only with one of live read the same way (kindSpans): as written
// with as written, a form with that form. A place plain in the recording
// and in base64 in round F is not one value written anew in one form.
func (dc *decider) movedOn(rec, live *readerText, a moment, declared bool) bool {
	if dc.holdsMomentOf(live, a, declared) {
		return false
	}
	rs, ls := kindSpans(rec, declared), kindSpans(live, declared)
	for k, spans := range rs {
		if slices.ContainsFunc(spans, func(r instant) bool {
			return dc.namedIn([]instant{r}, a, func(h, m moment) bool { return h.key == m.key }) > 0 &&
				slices.ContainsFunc(ls[k], func(l instant) bool { return l.layout == r.layout })
		}) {
			return true
		}
	}
	return false
}

// kindSpans are t's instants by the way they are read: as written first,
// and, where declared, those of each of its declared forms at that form's
// place (decoded.kinds); none of a text keploy could not read.
func kindSpans(t *readerText, declared bool) (out [1 + formKinds][]instant) {
	spans, ok := t.spansOf()
	out[0] = spans
	if !declared || !ok {
		return out
	}
	s, _ := t.textNow()
	x := t.x.dc
	d := derivedOfText(x, textDecoded, s, func(own string) decoded { return decodeForms(own) }, func(d decoded) int64 { return d.size() })
	for k, form := range d.kinds(s) {
		if form != "" {
			out[1+k] = x.spansOf(form)
		}
	}
	return out
}

// clockCut reports whether o, the read of the clock a place's call learned
// it as (evPlace.offset), is one cut no finer than unit: nil where it
// learned none, and one of no Res is taken for one.
func clockCut(o *models.ClockOffset, unit time.Duration) bool {
	return o != nil && (o.Res <= 0 || o.Res >= unit)
}

// inScope reports whether p is a place of the call's scope: its own test's,
// the start-up's, or one the test gave (every place under ModeAll).
func (dc *decider) inScope(p evPlace) bool { return dc.allScope || !p.otherTest || p.given }

// holdsMomentOf reports whether an instant of t, as written (declared: or
// in a declared form, formSpans), is m itself, or m in the application's
// zone (inZone). A text keploy could not read gives no instants, and holds
// none.
func (dc *decider) holdsMomentOf(t *readerText, m moment, declared bool) bool {
	return dc.namedIn(formSpans(t, declared), m, func(h, m moment) bool { return h.key == m.key }) > 0
}

// formSpans are t's instants as written, and, where declared, those of its
// declared forms (formsOf: decoded, case-folded, an encoded run's text)
// too; none of a text keploy could not read.
func formSpans(t *readerText, declared bool) []instant {
	spans, ok := t.spansOf()
	if !declared || !ok {
		return spans
	}
	s, _ := t.textNow()
	return slices.Concat(spans, t.x.dc.spansOf(t.formsOf(s)))
}

// readBack is where the call's own answer reads back w, a word of the
// request leaf at path (recorded rec, at at's word, through no decode
// step): the same word of every answer leaf whose recorded text is rec
// whole (a find by a key its cursor echoes), and every key of the answer's
// objects that is rec whole (a lookup answered under its id), a place
// keploy cannot name: refused. Every other answer text that
// holds the word, longer or shorter (a count, a date whose minute it is),
// is never rewritten: a word that short may stand there by chance, and a
// rewrite could make a test pass on an answer the dependency never gave. A
// fresh word such a text holds where nothing reads it back is refused
// (answerHoldsWord), as main refuses it. One rule for a Value and a fresh
// word; withWhole: a fresh word's whole value too (a Value's is the
// adapter's echo, as main answers it). unnamed is the path of the first
// such leaf keploy cannot name to the adapter.
func (dc *decider) readBack(path string, at variables.Address, w readWord, rec string, withWhole bool) (echo []variables.Address, unnamed string) {
	if at.Word < 0 || len(at.Steps) > 0 || w.Rec == rec && !withWhole {
		return nil, ""
	}
	return dc.wholeEchoes(path, at, rec)
}

// wholeEchoes are the answer leaves whose recorded text is rec, the request
// leaf's at path, whole (main's WholeEcho), each at at's steps and word,
// and the keys of the answer's objects that are rec whole; unnamed as
// readBack gives it.
func (dc *decider) wholeEchoes(path string, at variables.Address, rec string) (echo []variables.Address, unnamed string) {
	r := dc.in.Rec.Leaves[path]
	for _, l := range dc.in.Resp {
		// An answer scalar of any type holding the whole text reads it back
		// (a number sent as text, answered as a number), as main's WholeEcho.
		scalar := l.Type == String || l.Type == Number || l.Type == Bool
		if l.Value == rec && (l.Type == r.Type || scalar) || l.Type == Keys && slices.Contains(strings.Split(l.Value, "\n"), rec) {
			a := dc.in.AddressOf(l, true)
			if a.Place.Part == 0 && unnamed == "" {
				unnamed = l.Path
			}
			if len(at.Steps) > 0 {
				a.Steps = at.Steps
			}
			a.Word = at.Word
			echo = append(echo, a)
		}
	}
	return echo, unnamed
}

// answerHoldsWord is the first leaf of the call's recorded answer that
// holds w, a changed word's recorded text, as a word in any reading and
// declared form, in its case or folded; "" where none does. A leaf its
// protocol types as one instant has no words, as main reads it (echoedPart).
// A word made anew that the answer holds where nothing reads the request
// back may be the dependency's echo of it inside a longer value (an id in
// a GraphQL argument, an OData filter, a header the answer counts): served
// as recorded, the application would get the recorded word back, so the
// word is refused, never learned fresh. Each ask is a lookup of the call's
// answer index (answerIndex).
func (dc *decider) answerHoldsWord(w string) string {
	ai := dc.answerIndex()
	ai.asks++
	first, vs := -1, []string{w}
	if folded := strings.ToLower(w); folded != w {
		vs = append(vs, folded)
	}
	for _, v := range vs {
		i, ok := ai.first[v]
		if !ok && ai.cands != nil && !ai.cands[v] && aWord(v) {
			ai.scans++
			i, ok = dc.answerScan(v, ai.past)
		}
		if ok && (first < 0 || i < first) {
			first = i
		}
	}
	if first < 0 {
		return ""
	}
	return dc.in.Resp[first].Path
}

// answerWords is the call's answer index: each word of every leaf of its
// recorded answer but an instant, in every reading and declared form (as
// holdsWord reads a text), mapped to the first leaf that holds it (its
// index in Resp). It is built once per call, at the first word that asks,
// in one pass over each answer text, its words counted on the reader's
// budget (claim) at most answerClaimBytes at a time. From the leaf past
// on, where they pass the budget, it maps only the call's own words
// (cands); a word asked that is none of them is answered by a scan of
// those leaves (scans). reads counts the answer texts read, asks the words
// asked, dropped the words mapped and let go at the claim that failed:
// what keeps a call's work linear in its words and its answer.
type answerWords struct {
	first                             map[string]int
	cands                             map[string]bool
	past, reads, asks, scans, dropped int
}

// answerClaimBytes is the most of an answer text's words answerIndex maps
// before it claims them, so a text past the budget is never mapped whole.
const answerClaimBytes = 64 << 10

// answerIndex is the call's answer index, built at its first ask.
func (dc *decider) answerIndex() *answerWords {
	if dc.answer != nil {
		return dc.answer
	}
	ai := &answerWords{first: map[string]int{}, past: len(dc.in.Resp)}
	dc.answer = ai
	for i, a := range dc.in.Resp {
		if a.Type == Instant {
			continue
		}
		ai.reads++
		var added []string
		n := int64(0)
		formWords(a.Value, func(w string) {
			if _, ok := ai.first[w]; ok || ai.cands != nil && !ai.cands[w] {
				return
			}
			ai.first[w] = i
			if ai.cands == nil {
				added = append(added, w)
				if n += wordBytes + int64(len(w)); n >= answerClaimBytes {
					ai.settle(dc, i, added, n)
					added, n = added[:0], 0
				}
			}
		})
		ai.settle(dc, i, added, n)
	}
	return ai
}

// settle claims the n bytes of the words added at leaf i since the last
// claim. Where the claim fails it lets go of those that are none of the
// call's words (callWords) and maps only the call's words from leaf i on.
func (ai *answerWords) settle(dc *decider, i int, added []string, n int64) {
	if ai.cands != nil || n == 0 || dc.x.claim(n) {
		return
	}
	ai.cands, ai.past = dc.callWords(), i
	for _, w := range added {
		if !ai.cands[w] {
			delete(ai.first, w)
			ai.dropped++
		}
	}
}

// callWords are the words of the call's changed request leaves as
// recorded, in every reading and declared form (textForms folds the case of
// each form it decodes): the words answerHoldsWord is asked. A word asked
// that is none of them (a word of a deeper decode) costs a scan past the
// budget, never a wrong answer.
func (dc *decider) callWords() map[string]bool {
	c := map[string]bool{}
	for _, p := range dc.in.Drift {
		formWords(dc.in.Rec.Leaves[p].Value, func(w string) { c[w] = true })
	}
	return c
}

// answerScan is the first leaf of the answer from from on, but an instant,
// that holds w as a word in any reading and declared form.
func (dc *decider) answerScan(w string, from int) (int, bool) {
	for i := from; i < len(dc.in.Resp); i++ {
		if a := dc.in.Resp[i]; a.Type != Instant && heldIn(textForms(a.Value), w, 0, 1, 2) {
			return i, true
		}
	}
	return 0, false
}

// formWords calls f with each word of s in every declared form and reading
// (textForms, readings), as rawTokens reads them, keeping none: each
// stretch of bytes the reading says continue a word, read in place.
func formWords(s string, f func(string)) {
	for _, form := range textForms(s) {
		for _, word := range readings {
			for i := 0; i < len(form); i++ {
				if !word(form[i]) {
					continue
				}
				j := i + 1
				for j < len(form) && word(form[j]) {
					j++
				}
				f(form[i:j])
				i = j
			}
		}
	}
}

// aWord reports whether w can be a word of a text in some reading: idByte,
// the widest, continues each of its bytes (heldBy). No text holds any other
// w, so answerHoldsWord never scans for one.
func aWord(w string) bool {
	for i := 0; i < len(w); i++ {
		if !idByte(w[i]) {
			return false
		}
	}
	return w != ""
}

// unanswerable refuses a word whose answer places (echo, unnamed: as
// readBack gives them) the adapter cannot answer: at a word, where it
// cannot answer one, or at a place keploy cannot name to it.
func (dc *decider) unanswerable(path string, bare variables.Address, rec, live string, echo []variables.Address, unnamed string) (Decision, bool) {
	switch {
	case len(echo) > 0 && !dc.in.Caps.AnswerAtWord:
		return dc.cannotCheck(path, bare, -1, rec, live, "the adapter cannot answer a word inside its answer yet"), true
	case unnamed != "":
		return dc.cannotCheck(path, bare, -1, rec, live, "its recorded answer holds it at "+unnamed+", a place keploy cannot name to the adapter yet"), true
	}
	return Decision{}, false
}

// addrList is as, each as its address prints, comma-separated.
func addrList(as []variables.Address) string {
	s := make([]string, len(as))
	for i, a := range as {
		s[i] = a.String()
	}
	return strings.Join(s, ", ")
}

// fromAt is the address of the word v (f in round F) at p, a Value's bind
// place: the earlier exchange's leaf as its adapter names it (an answer's as
// an answer's), at v's word where v is one of its words, else the whole
// leaf; no place (word -1) for a recording keploy read whole. A client
// test's request (clientCase) whose leaf's own words do not hold v but
// whose JWT does, in a claim that holds v and f at one word in the two runs
// (claimWord: a token the application minted with an id in it, sent back
// to it), is addressed at that word of the claim (|jwt.<claim>#<n>): a
// replay decodes the claim in the request that pairs with the test case,
// binds the variable to it, and compares each later use with it. A call's
// request is never addressed at a claim: its replay puts the recorded
// values back at its places, and cannot write a claim into a signed token;
// its leaf is named whole, as before. A value that is the JWT's signature
// in both runs (sigAt) is addressed at the signature (|jwtsig), first: a
// replay reads it there whole, whatever its count of words. A value of more
// than one word that a longer text holds as one of its tokens in both runs,
// at one index (tokenStepAt: an id in a consent URL, a key after "Bearer "), is
// addressed at that token (|token.<n>): a replay reads it there whole,
// wherever the next run's '-' and '_' fall, where the text whole would hold
// the id equal to the whole URL; a random token (randomToken) so even where
// it is one word of the text in both runs (an id of letters and digits,
// whose next run may hold a '-'). A token step of the URL-safe cut
// (|urltoken.<n>) reads an id in a URL's path, and one through |pct a value
// behind escapes; a word the escapes hide from the text's own words is read
// through |pct at its word (pctWord), never the text whole. A Cookie or
// Set-Cookie header holds a token as one pair's value, beside other pairs
// (refresh_token=<JWT>; refresh_token_staging=<JWT>), which no JWT step reads
// whole: its signature and its claims are read in the first pair that holds
// them (cookieToken), at that pair's step (|cookie.<name>|jwtsig,
// |cookie.<name>|jwt.<claim>#<n>), each where the whole header's would be.
// Words and claims are read in the text the address's own steps open (an
// adapter's PlaceOf may name a decode step), so the claim's step follows
// them; a leaf they do not open has no word or claim there: it is named
// whole, as before.
func (dc *decider) fromAt(p evPlace, v, f string) variables.Address {
	if p.leaf.Path == "" {
		return variables.Address{Word: -1}
	}
	a := dc.in.AddressOf(p.leaf, p.side == Response)
	rec, _ := a.WordOf(p.leaf.Value)
	live, _ := a.WordOf(p.live)
	if sigAt(rec, live, v, f) {
		a.Steps, a.Word = append(slices.Clip(a.Steps), variables.StepJWTSig), -1
		return a
	}
	if st, _, ok := cookieToken(a, rec, live, v, f, false); ok {
		a.Steps, a.Word = append(slices.Clip(a.Steps), st...), -1
		return a
	}
	steps, byToken := tokenStepAt(rec, live, v, f)
	if byToken && randomToken(v, f) {
		// A random token that is one word in both runs (an id of letters
		// and digits alone) is still read at its token: the next run's id
		// may hold a '-' or '_', which a word would cut.
		a.Steps, a.Word = append(slices.Clip(a.Steps), steps...), -1
		return a
	}
	for i, w := range variables.Words(rec) {
		if rec[w[0]:w[1]] == v {
			a.Word = i
			return a
		}
	}
	if n, ok := pctWord(rec, v); ok {
		a.Steps, a.Word = append(slices.Clip(a.Steps), variables.StepPct), n
		return a
	}
	if byToken {
		a.Steps, a.Word = append(slices.Clip(a.Steps), steps...), -1
		return a
	}
	if !dc.clientCase(p.mock) {
		return a
	}
	if st, n, ok := claimWord(rec, live, v, f); ok {
		a.Steps, a.Word = append(slices.Clip(a.Steps), st), n
	} else if st, n, ok := cookieToken(a, rec, live, v, f, true); ok {
		a.Steps, a.Word = append(slices.Clip(a.Steps), st...), n
	}
	return a
}

// tokenStepAt is the steps to the token that v and f, a value as recorded
// and as round F saw it, are in rec and live, two runs of a longer text: a
// token of one index n in both, cut by the first of tokenCuts that holds
// it so (|token.<n>, the base64 alphabets together; else |urltoken.<n>,
// the URL-safe one alone, whose '/' ends a token of a URL's path:
// "/v1/requests/<id>"), the first such token of rec that is v, where
// live's token of that index is f. A text that holds escapes ("%2B" of a
// standard key passed through url.QueryEscape) is read so too through
// |pct, after its own text: |pct alone where the decoded texts are v and f
// whole. ok is false where rec and live are v and f whole (the place is
// named whole), or no cut holds such a token: past the last token of
// either text, no token of that index is in both. Each cut reads rec and
// live once each, their n-th tokens side by side, each from the end of the
// one before, so a leaf of any length costs a pass for each cut (and one
// to decode it), never a pass for each token.
func tokenStepAt(rec, live, v, f string) ([]variables.Step, bool) {
	if rec == v && live == f {
		return nil, false
	}
	if st, ok := cutTokenAt(rec, live, v, f); ok {
		return []variables.Step{st}, true
	}
	if !strings.Contains(rec, "%") && !strings.Contains(live, "%") {
		return nil, false
	}
	r, rok := variables.Decode(variables.StepPct, rec)
	l, lok := variables.Decode(variables.StepPct, live)
	switch {
	case !rok || !lok:
		return nil, false
	case r == v && l == f:
		return []variables.Step{variables.StepPct}, true
	}
	if st, ok := cutTokenAt(r, l, v, f); ok {
		return []variables.Step{variables.StepPct, st}, true
	}
	return nil, false
}

// pctWord is n where v is the n-th word of rec percent-decoded, rec a text
// that holds escapes and whose own words do not hold v ("9Lk" of
// "code=q3Zx%2B9Lk", whose own word there is "2B9Lk"): a replay reads that
// word through |pct, where the text whole holds no word equal to v. ok is
// false where rec holds no escape, does not decode, or its decoded words do
// not hold v.
func pctWord(rec, v string) (int, bool) {
	if !strings.Contains(rec, "%") {
		return 0, false
	}
	r, ok := variables.Decode(variables.StepPct, rec)
	if !ok {
		return 0, false
	}
	for i, w := range variables.Words(r) {
		if r[w[0]:w[1]] == v {
			return i, true
		}
	}
	return 0, false
}

// cutTokenAt is the token step of the first of tokenCuts at which v and f
// are rec's and live's token of one index (tokenStepAt).
func cutTokenAt(rec, live, v, f string) (variables.Step, bool) {
	for _, c := range tokenCuts {
		r, l := 0, 0
		for n := 0; ; n++ {
			rs, re, rok := c.next(rec, r, len(rec))
			ls, le, lok := c.next(live, l, len(live))
			if !rok || !lok {
				break
			}
			if rec[rs:re] == v && live[ls:le] == f {
				return c.step(n), true
			}
			r, l = re, le
		}
	}
	return "", false
}

// tokenCuts are the cuts tokenStepAt reads a text's tokens by, in turn,
// each the one a replay's step of it reads: the base64 alphabets together
// (variables.NextToken, |token.<n>), then the URL-safe one alone
// (variables.NextURLToken, |urltoken.<n>).
var tokenCuts = []struct {
	step func(int) variables.Step
	next func(s string, from, to int) (int, int, bool)
}{
	{variables.Token, variables.NextToken},
	{variables.URLToken, variables.NextURLToken},
}

// cookieToken is the steps from a, a Cookie or Set-Cookie header's own text
// (rec and live, two runs of it), to the part of a JWT that holds v in rec
// and f in live, where one of its pairs holds that token as its value: the
// pair's step (cookie.<name>), then jwtsig where v and f are the token's
// signatures (sigAt), at word -1; with claims, the claim's step instead
// where a claim holds them (claimWord), at its word n. The pairs are
// cookieSpans' (named once in both runs, in one order, whose values
// differ), the first that holds them in rec's order: a replay reads that
// pair by name. ok is false where a is no such header's text, or no pair
// holds them so.
func cookieToken(a variables.Address, rec, live, v, f string, claims bool) ([]variables.Step, int, bool) {
	if !cookiePlace(a) {
		return nil, 0, false
	}
	spans, _ := cookieSpans(rec, live)
	for _, s := range spans {
		pair := s.Parts[0]
		if !claims {
			if sigAt(pair.Rec, pair.Live, v, f) {
				return []variables.Step{pair.Step, variables.StepJWTSig}, -1, true
			}
			continue
		}
		if st, n, ok := claimWord(pair.Rec, pair.Live, v, f); ok {
			return []variables.Step{pair.Step, st}, n, true
		}
	}
	return nil, 0, false
}

// sigAt reports whether v and f, a value as recorded and as round F saw
// it, are the signatures of rec and live, two runs of one JWT
// (variables.StepJWTSig): the place is then addressed at its signature,
// whose words a replay could not count on (decider.signature), whatever
// its words are.
func sigAt(rec, live, v, f string) bool {
	r, rok := variables.Decode(variables.StepJWTSig, rec)
	l, lok := variables.Decode(variables.StepJWTSig, live)
	return rok && lok && r == v && l == f
}

// ClaimName is the JWT claim a bind place reads (its last such step), for a
// learned line: "" where it reads none. An adapter reads it to refuse a
// claim place that is no client test's request (Place.Claim).
func ClaimName(at variables.Address) string {
	claim := ""
	for _, st := range at.Steps {
		if c, ok := strings.CutPrefix(string(st), string(variables.JWTClaim(""))); ok {
			claim = c
		}
	}
	return claim
}

// claimWord is the claim of the JWTs rec and live, two runs of one leaf,
// that holds v in rec and f in live at one word n, as a step: the first
// such claim by name (jwtSpan reads the claims that differ, in that order).
// Where no claim holds them at a word, the first claim that is v in rec and
// f in live whole (an id of several words, a UUID in a sub claim), at word
// -1. A random token (randomToken) a claim is whole in both runs is read
// there whole first, even where it is one word of it in both runs (an id of
// letters and digits, whose next run may hold a '-' or '_', which a word
// would cut), as fromAt reads one at its token. ok is false where either is
// no JWT (jwtSpan then opens no claim), or no claim holds them so: the leaf
// is then named whole, as before.
func claimWord(rec, live, v, f string) (variables.Step, int, bool) {
	span, _ := jwtSpan(rec, live)
	if randomToken(v, f) {
		for _, part := range span.Parts {
			if part.Rec == v && part.Live == f {
				return part.Step, -1, true
			}
		}
	}
	for _, part := range span.Parts {
		for n, w := range variables.Words(part.Rec) {
			if part.Rec[w[0]:w[1]] != v {
				continue
			}
			if lw, ok := (variables.Address{Word: n}).WordOf(part.Live); ok && lw == f {
				return part.Step, n, true
			}
		}
	}
	for _, part := range span.Parts {
		if part.Rec == v && part.Live == f {
			return part.Step, -1, true
		}
	}
	return "", 0, false
}

// claimAt is Place.Claim of p, a leaf at the variable place p.Var, recorded
// rec and seen live in record's full round, that holds the recorded value v
// and the value f F saw inside a longer value: p.Var, at the word of the
// claim that holds them (claimWord), or in a Cookie header at the claim of
// the token a pair holds (cookieToken: |cookie.<name>|jwt.<claim>#<n>);
// "" where none does, p.Var is no place (""), or p is no client test's
// request (Client): a replay reads a claim
// only in the request that pairs with a client test case, and cannot write
// one into a call's signed token. A place that already names a step or a
// word is none: its claim would stand in text it decodes.
func claimAt(p Place, rec, live, v, f string) string {
	if !p.Client {
		return ""
	}
	a, err := variables.ParseAddress(p.Var)
	if err != nil || !a.Whole() {
		return ""
	}
	if st, n, ok := claimWord(rec, live, v, f); ok {
		a.Steps, a.Word = []variables.Step{st}, n
		return a.String()
	}
	st, n, ok := cookieToken(a, rec, live, v, f, true)
	if !ok {
		return ""
	}
	a.Steps, a.Word = st, n
	return a.String()
}

// wordBindAt is Place.Word of p, a leaf at the variable place p.Var,
// recorded rec and seen live in record's full round, that held the value
// (v as recorded, f as F saw it) inside a longer value: p.Var at the one
// word rec changed by (ChangedWord), where v changed by one word alone too,
// and the leaf's word is v's, rec's recorded and live's F's (p.Var itself
// where rec and live are that word whole). "" where rec is v whole (no
// longer value: the place binds whole), either changed by more than one word
// or by a byte between words, the words differ, p.Var is no place or names a
// word or a step already, or p is no client test's request (Client): a
// replay binds a variable at a word only of the request that pairs with a
// client test case, and reads a call's request only to put its recorded
// values back.
func wordBindAt(p Place, rec, live, v, f string) string {
	if !p.Client || rec == v {
		return ""
	}
	a, err := variables.ParseAddress(p.Var)
	if err != nil || !a.Whole() {
		return ""
	}
	k, ok := ChangedWord(v, f)
	if !ok {
		return ""
	}
	n, ok := ChangedWord(rec, live)
	if !ok {
		return ""
	}
	vw, fw := wordOf(v, k), wordOf(f, k)
	if wordOf(rec, n) != vw || wordOf(live, n) != fw {
		return ""
	}
	// rec is the word whole only where live is F's word whole too:
	// ChangedWord keeps every byte but the word's alike.
	if rec != vw {
		a.Word = n
	}
	return a.String()
}

// ChangedWord is the index of the one word (variables.Words) by which live
// differs from rec: live is rec with that word written anew, every other
// byte alike. ok is false where the two are alike, or differ in more than
// one word, in their count of words, or in a byte between words.
func ChangedWord(rec, live string) (n int, ok bool) {
	rw, lw := variables.Words(rec), variables.Words(live)
	if len(rw) != len(lw) {
		return -1, false
	}
	n = -1
	for i := range rw {
		if rec[rw[i][0]:rw[i][1]] == live[lw[i][0]:lw[i][1]] {
			continue
		}
		if n >= 0 {
			return -1, false
		}
		n = i
	}
	if n < 0 || rec[:rw[n][0]] != live[:lw[n][0]] || rec[rw[n][1]:] != live[lw[n][1]:] {
		return -1, false
	}
	return n, true
}

// wordOf is the n-th word of s (variables.Words), "" where s has none.
func wordOf(s string, n int) string {
	w, _ := variables.Address{Word: n}.WordOf(s)
	return w
}

// ClaimHolding is the name of the claim of the JWT text holds (a token, or
// "Bearer <token>") whose value holds v whole or at a word, the first such
// claim by name: "" where text holds no JWT, v is "", or no claim holds v.
// A refusal reads it to name the claim it found no place to bind at.
func ClaimHolding(text, v string) string {
	_, claims, ok := jwtClaims(text)
	if !ok || v == "" {
		return ""
	}
	for _, n := range slices.Sorted(maps.Keys(claims)) {
		c, ok := variables.Decode(variables.StepJSON, string(claims[n]))
		if !ok {
			continue
		}
		if c == v {
			return n
		}
		for _, w := range variables.Words(c) {
			if c[w[0]:w[1]] == v {
				return n
			}
		}
	}
	return ""
}

// ClaimRefused is why the value v, sent earlier by mock at path inside a
// claim of the JWT there (rec as recorded, live in record's full round),
// binds no variable there, the claim named (ClaimHolding): "" where rec
// holds v in no claim. A call's request (client false) is no place to bind
// a claim at: a replay reads a claim only in the request that pairs with a
// client test case, and cannot write one into a token the call sends. A
// client test's request is none where round F's text there holds no JWT,
// or no claim of its token holds round F's value where the recording's
// claim held v (at that word, or whole: claimWord).
func ClaimRefused(mock, path, rec, live, v string, client bool) string {
	claim := ClaimHolding(rec, v)
	if claim == "" {
		return ""
	}
	why := mock + " sent this value in the " + claim + " claim of the JWT at " + path
	if !client {
		return why + ", a call's request: keploy binds a variable at a JWT claim only in a client test's request, which a replay reads, never in a token a call sends, which a replay cannot write a claim into"
	}
	if _, _, ok := jwtClaims(live); !ok {
		return why + ", and this run's text there holds no JWT: no variable can bind it there"
	}
	return why + ", and no claim of this run's token holds this run's value where the recording's claim held it, at that word or whole: no variable can bind it there"
}

// BindAt is where a variable binds at p, and the value F saw there: its
// variable place (Var) and Live, where p held the value whole; the word of
// its JWT claim (Claim) and that word of Live, where p is a client test's
// request (Client) that held the value inside the claim; else the word of
// it that held the value's one changed word (Word) and that word of Live.
// ok is false where p has no variable place, held the value inside a
// longer value otherwise (Claim and Word "", which name no place), or names
// a claim or a word of a call's request, which a replay cannot write a
// claim into, nor bind a word at: no place a replay can bind at.
func (p Place) BindAt() (at, live string, ok bool) {
	switch {
	case p.Var == "":
		return "", "", false
	case p.Whole:
		return p.Var, p.Live, true
	case !p.Client:
		return "", "", false
	}
	at = cmp.Or(p.Claim, p.Word)
	a, err := variables.ParseAddress(at)
	if err != nil {
		return "", "", false
	}
	w, ok := a.WordOf(p.Live)
	if !ok {
		return "", "", false
	}
	return at, w, true
}

// learned is a decided word and its line.
func (dc *decider) learned(path string, at variables.Address, n int, r, f string, o WordOutcome, rule string, places []string) Decision {
	at.Word = n
	from := ""
	if dc.allScope {
		from = dc.in.Landed.String()
	}
	return Decision{At: at, Recorded: r, Full: f, Outcome: o, Line: learnedLine(learnedWord{
		Test: dc.in.Test, Mock: dc.in.Mock, Field: path, Word: n, Recorded: r, Full: f,
		Outcome: o, Rule: rule, Places: places, From: from, Noise: noiseName(dc.in, path),
	})}
}

// unreadWhy is why a call is refused where keploy read a recording of it
// once and could not read it again (a lazy Raw whose payload the set's
// store let go, and whose load then failed): the recordings, by name, so
// the line names the place and its cause, never a budget it did not pass.
func (dc *decider) unreadWhy() string {
	var names []string
	for _, p := range dc.ps {
		for _, t := range append([]*readerText{p.t.rec, p.t.live}, p.t.views...) {
			if n := unreadName(p.mock, Earlier{}); t.rawUnread && !slices.Contains(names, n) {
				names = append(names, n)
			}
		}
	}
	return "keploy could not read " + cmp.Or(strings.Join(names, ", "), "a recording of the test") +
		" again, an earlier place of the call it read before, so it cannot tell whether what changed came from there"
}

// refuse is a refused word and its line.
func (dc *decider) refuse(path string, at variables.Address, n int, r, f string, kind RefusalKind, why string, places []string, edits ...Edit) Decision {
	at.Word = n
	rf := &Refusal{Kind: kind, Test: dc.in.Test, Mock: dc.in.Mock, Field: path, Word: n, Recorded: r, Full: f, Why: why, Places: places, Edits: onceEach(edits)}
	hideSecrets(rf)
	return Decision{At: at, Recorded: r, Full: f, Outcome: OutcomeRefused, Refusal: rf, Line: RefusalLine(*rf)}
}

// noPayloadRefusal refuses a value no place binds beside an exchange keploy
// holds no payload of (payloadless), naming it, with main's reason.
func (dc *decider) noPayloadRefusal(path string, at variables.Address, n int, r, f string) Decision {
	return dc.refuse(path, at, n, r, f, RefusalUnread, dc.noPayload.why, []string{dc.noPayload.mock},
		Edit{Kind: EditReRecord}, dc.fieldNoise(path))
}

// cannotCheck refuses a field keploy cannot decide yet: its limit.
func (dc *decider) cannotCheck(path string, at variables.Address, n int, r, f, why string) Decision {
	return dc.refuse(path, at, n, r, f, RefusalCannotCheck, why, nil, dc.fieldNoise(path))
}

func (dc *decider) fieldNoise(path string) Edit {
	return noiseEdit(dc.in.Mock, noiseName(dc.in, path))
}

// noiseEdit is noise at the field keploy.yml's noise names name, for a
// word of it too, as keploy.yml names no word; a re-record where no
// keploy.yml noise names it (noiseAt).
func noiseEdit(mock, name string) Edit {
	if _, _, ok := noiseAt(name); !ok {
		return Edit{Kind: EditReRecord}
	}
	return Edit{Kind: EditNoiseAtField, Mock: mock, Field: name, Word: -1}
}

// noiseName is the field at path as keploy.yml's noise names it
// (fieldName: an array's element as "[]", a value's encoding left out): its
// recorded leaf's name, else its leaf's in record's full round, else path.
func noiseName(in Input, path string) string {
	if l, ok := in.Rec.Leaves[path]; ok {
		return fieldName(l)
	}
	if l, ok := in.Live.Leaves[path]; ok {
		return fieldName(l)
	}
	return path
}

// onceEach is es with each edit named once, at its first place.
func onceEach(es []Edit) []Edit {
	out := es[:0:0]
	for _, e := range es {
		if !slices.Contains(out, e) {
			out = append(out, e)
		}
	}
	return out
}

// names are ps' line names, first three.
func names(ps []evPlace) []string {
	var out []string
	for _, p := range ps[:min(len(ps), maxLinePlaces)] {
		out = append(out, p.name)
	}
	return out
}

// gaveAt is what round F gave at p, in a line's words, its value quoted
// (a line quotes every value it names: hidden).
func gaveAt(p evPlace, f string) string {
	if p.same() {
		return "the same text"
	}
	if l := p.liveText(); len(l) <= 40 && !strings.HasPrefix(l, withoutMark) {
		return strconv.Quote(l)
	}
	return withoutMark + strconv.Quote(f)
}

// scopeName names p's scope outside the call for a line.
func scopeName(p evPlace) string {
	return cmp.Or(p.scope, "an earlier test or between tests")
}

// givenName is the name a give edit gives the value at path: a part of the
// url by its whole path (url.path.2, url.query.page), as a path segment's
// index alone names nothing a person reads; any other field by its last key.
func givenName(path string) string {
	if strings.HasPrefix(path, "url.") {
		return path
	}
	if i := strings.LastIndexAny(path, ".]"); i >= 0 && i+1 < len(path) {
		return path[i+1:]
	}
	return path
}
