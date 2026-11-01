package learn

import (
	"cmp"
	"slices"
	"sort"
	"strconv"
	"strings"
	"sync"
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
		return elsewhere(in, ds, fields)
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
	in.Earlier, in.Raw = timedEarlier(in.Earlier, in.At), timedRaw(in.Raw, in.At)
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
				nil, dc.wordNoise(fields[i], d.At.Word))
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
	// dr is the call's drift cache, or an empty one.
	dr *DriftCache
	// set: the recordings the set read once (Input.Given); op: what each
	// recording outside the call's scope holds (outside), read once.
	set []GivenPlaces
	op  *offsetPlaces
	// encoded is the leaf's inDecodedForm, read once and only where a word
	// of it would be learned fresh.
	encoded func() (string, *evPlace)
}

// decideWords decides every changed word of in, and the field each
// decision is of; allScope reads every earlier place of the run as the
// call's scope (Mode Elsewhere).
func decideWords(in Input, allScope bool) ([]Decision, []string) {
	ps, unread := placesOf(in)
	set, setNotRead := setUnread(in)
	unread = cmp.Or(unread, setNotRead)
	x := newReaderIndex(in.Drifts)
	defer x.release() // what the index keeps goes back to the set's budget
	dc := &decider{in: in, ps: withTexts(x, ps), x: x, allScope: allScope, set: set, dr: in.Drifts}
	if dc.dr == nil {
		dc.dr = NewDriftCache()
	}
	var out []Decision
	var fields []string // fields[i] is the field out[i] decides
	for _, path := range in.Drift {
		out = append(out, dc.leaf(path, unread)...)
		for len(fields) < len(out) {
			fields = append(fields, path)
		}
	}
	dc.oneValue(out, fields)
	if dc.x.unread() {
		// A run past its budget: every change of the call is refused.
		for i, d := range out {
			out[i] = dc.refuse(fields[i], d.At, d.At.Word, d.Recorded, d.Full, RefusalUnread,
				"keploy could not read the run's values past their budget, which came before this call", nil,
				Edit{Kind: EditReRecord}, dc.fieldNoise(fields[i]))
		}
	}
	return out, fields
}

// oneValue makes one new value the call sent at several places one
// variable: bound at its first place, made anew each run, and used at the
// others, each use naming the first in its line, so a replay that sends
// the places apart misses. A value recorded at several places that round F
// sent as two different new values is two reads keploy cannot tell apart:
// each such place is refused, naming the first.
func (dc *decider) oneValue(out []Decision, fields []string) {
	first := map[string]int{}
	name := func(i int) string {
		if w := out[i].At.Word; w >= 0 {
			return dc.in.Mock + " " + fields[i] + " word " + strconv.Itoa(w)
		}
		return dc.in.Mock + " " + fields[i]
	}
	for i, d := range out {
		j, ok := first[d.Recorded]
		switch {
		case d.Outcome != OutcomeFresh:
		case !ok:
			first[d.Recorded] = i
		case out[j].Outcome == OutcomeFresh && out[j].Full == d.Full:
			out[i] = dc.learned(fields[i], d.At, d.At.Word, d.Recorded, d.Full, OutcomeValue,
				"used the value bound at "+name(j)+", made anew in this call and sent at both places in both runs", []string{name(j)})
			out[i].From = Place{Mock: dc.in.Mock, Side: Request, Path: fields[j], Live: d.Full, Whole: out[j].At.Word < 0}
			out[i].FromAt = out[j].At
		default:
			why := "the recorded value stands at " + name(j) + " and " + name(i) + ", and round F sent " + strconv.Quote(out[j].Full) + " and " + strconv.Quote(d.Full) + " there: two values, and keploy cannot tell which one the recording's is"
			for _, k := range []int{j, i} {
				if out[k].Outcome == OutcomeFresh {
					out[k] = dc.refuse(fields[k], out[k].At, out[k].At.Word, out[k].Recorded, out[k].Full, RefusalTwoSources, why,
						[]string{name(j), name(i)}, dc.wordNoise(fields[k], out[k].At.Word))
				}
			}
		}
	}
}

// leaf decides the changed words of the drifted leaf at path.
func (dc *decider) leaf(path, unread string) []Decision {
	rec, live := dc.in.Rec.Leaves[path], dc.in.Live.Leaves[path]
	at := dc.in.AddressOf(rec, false)
	switch {
	case unread != "":
		return []Decision{dc.refuse(path, at, -1, rec.Value, live.Value, RefusalUnread,
			"keploy could not read "+unread+", which came before this call", nil,
			Edit{Kind: EditReRecord}, dc.fieldNoise(path))}
	case rec.Unnamed || live.Unnamed:
		return []Decision{dc.cannotCheck(path, at, -1, rec.Value, live.Value, "no path names this field alone")}
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
	dc.encoded = sync.OnceValues(func() (string, *evPlace) { return dc.inDecodedForm(rds) })
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
	if d, ok := dc.oneToken(path, bare, rec, live, rd); ok {
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
	done := map[*instantWord]bool{}
	for _, w := range rd.Words {
		switch {
		case w.Instant != nil:
			if done[w.Instant] {
				continue
			}
			done[w.Instant] = true
			out = append(out, dc.textInstant(path, bare, rec, live, rd, w))
		case w.RecIdx < 0 || w.LiveIdx < 0:
			return append(out, dc.unpaired(path, bare, rec, live, rd)), true
		default:
			out = append(out, dc.word(path, bare, rec, live, rd.Rec, w, pieceOfAnId(rd, w)))
		}
	}
	return out, false
}

// tokenBits is the fewest bits (wordBits) the changed part of a URL-safe
// base64 token carries in each run for keploy to read it as one random
// value, about eleven characters: a short id whose separator changed
// ("order-17_x" to "order-42-x") is no such token.
const tokenBits = 64

// oneToken decides a field that is one URL-safe base64 token in each run
// and whose '-' and '_' moved. They are letters of that alphabet there, not
// separators, so the runs' words do not pair and replay could never write
// them: the token is one value, read as a word whole. A word of 32 bits or
// more that an earlier place holds, where no place shows the whole token,
// may be carried inside it, so it is left to the word rules. ok is false
// where the field is no such token.
func (dc *decider) oneToken(path string, bare variables.Address, rec, live string, rd textReading) (Decision, bool) {
	paired := !rd.ChangedShape && len(rd.Separators) == 0 && !slices.ContainsFunc(rd.Words, func(w readWord) bool { return w.RecIdx < 0 || w.LiveIdx < 0 })
	if paired || len(rd.Instants) > 0 || len(bare.Steps) > 0 || !randomToken(rec, live) {
		return Decision{}, false
	}
	whole := readWord{At: bare, Rec: rec, Live: live, Tokens: []wholeToken{{Kind: tokenB64, Rec: rec, Live: live}}}
	shown := slices.ContainsFunc(dc.ps, func(p evPlace) bool { return tokenShown(p, whole) })
	for _, w := range rd.Words {
		for _, p := range dc.ps {
			if !shown && (wordBits(w.Rec) >= chanceBits && evHolds(p.t.rec, w.Rec) || wordBits(w.Live) >= chanceBits && evHolds(p.t.live, w.Live)) {
				return Decision{}, false
			}
		}
	}
	return dc.word(path, bare, rec, live, rec, whole, false), true
}

// randomToken reports whether rec and live are one value of one generator:
// a run of the URL-safe base64 alphabet of one length in each run, whose
// changed part, past what they share at each end, carries tokenBits in both.
// Text keploy can name is no such value: hex groups (a UUID's, with or
// without a word beside them) and a run that decodes to text (a name of a
// script keploy does not split into words, inside base64 JSON).
func randomToken(rec, live string) bool {
	const urlSafe = "-_0123456789abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ"
	if len(rec) != len(live) || strings.Trim(rec, urlSafe) != "" || strings.Trim(live, urlSafe) != "" {
		return false
	}
	hexGroups := func(s string) bool { return strings.Trim(strings.ToLower(s), "-_0123456789abcdef") == "" }
	if hexGroups(rec) || hexGroups(live) {
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
	return wordBits(r) >= tokenBits && wordBits(l) >= tokenBits && b64Text(rec, nil) == "" && b64Text(live, nil) == ""
}

// unpaired decides a text where a word was added or dropped: refused whole
// as unknown where a place keploy reads holds a word of what changed (it
// may be a source of part of it), else as a changed shape.
func (dc *decider) unpaired(path string, at variables.Address, rec, live string, rd textReading) Decision {
	var held []evPlace
	for _, p := range dc.ps {
		if slices.ContainsFunc(rd.Words, func(w readWord) bool {
			return w.Rec != "" && evHolds(p.t.rec, w.Rec) || w.Live != "" && evHolds(p.t.live, w.Live)
		}) {
			held = append(held, p)
		}
	}
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

// pieceOfAnId reports whether w is under the floor and another changed word of
// its text carries chance bits in both runs ("k" of "k-Rq7Lm2Xv9Tp4Wn8Zc"): the
// text is one value, and w a piece of it.
func pieceOfAnId(rd textReading, w readWord) bool {
	return wordBits(w.Rec) < unreadFloorBits && wordBits(w.Live) < unreadFloorBits && slices.ContainsFunc(rd.Words, func(o readWord) bool {
		return wordBits(o.Rec) >= chanceBits && wordBits(o.Live) >= chanceBits
	})
}

// neverServed reports whether the set's read saw no serve of mock.
func (dc *decider) neverServed(mock string) bool {
	return slices.ContainsFunc(dc.set, func(g GivenPlaces) bool { return g.mock == mock && g.first == 0 })
}

// word decides one changed word that is no instant (any other word), at
// its address (w.At) in text, the text the leaf at bare opens to. piece:
// pieceOfAnId.
func (dc *decider) word(path string, bare variables.Address, rec, live, text string, w readWord, piece bool) Decision {
	at, n := w.At, w.At.Word
	ps := append([]evPlace(nil), dc.ps...)
	cand := map[string]bool{}
	// moved: why a place that shows the word shows it at another field.
	// agree: every candidate shows R's word in R and F's in F at one field.
	// Each then gives the same pair, so serve order, which tells which
	// candidate the application had last, cannot change what is bound, and
	// replay checks the bound value at the earliest place.
	moved := make([]string, len(ps))
	agree := true
	for i, p := range ps {
		ev, _ := evidenceAt(p, w.Rec, w.Live)
		if ev == evNone {
			continue
		}
		cand[p.mock] = true
		if ev == evShown {
			moved[i] = movedField(p, w.Rec, w.Live)
		}
		agree = agree && ev == evShown && moved[i] == ""
	}
	if !agree {
		orderUnknown(dc.in, ps, cand)
	}
	swapUnknown(dc.in, ps, w)
	// aside: places that held R's word where F's did not follow, too
	// little to be a conflict; a fresh word's line names them. unserved:
	// recordings round F never served, which say nothing of a piece.
	var src, outside, conflict, unknown, aside, unserved []evPlace
	for i, p := range ps {
		ev, read := evidenceAt(p, w.Rec, w.Live)
		if piece && ev == evUnknown && p.fUnstated && !p.fGave {
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
			if p.gave || countsByText(p, w) || (dc.allScope || !p.otherTest) && heldWhole(p, w, text) {
				conflict = append(conflict, p)
			} else {
				aside = append(aside, p)
			}
		case evUnknown:
			unknown = append(unknown, p)
		}
	}
	for _, g := range dc.outside().words {
		p := evPlace{name: g.mock, mock: g.mock, otherTest: true, scope: "another test, or after this call", at: g.at}
		ev, changed := setEvidence(g, w.Rec, w.Live, dc.in.At)
		if changed {
			p.live = withoutMark + w.Live
		}
		switch ev {
		case evShown:
			outside = append(outside, p)
		case evUnknown:
			if piece && dc.neverServed(g.mock) {
				unserved = append(unserved, p)
				break
			}
			p.why = setUnknownWhy(g, dc.in.At)
			unknown = append(unknown, p)
		case evDisproven:
			if !listsHold(g.answerLists, w.Rec) {
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
			Edit{Kind: EditFixApp}, dc.fieldNoise(path), Edit{Kind: EditGive, Name: lastSegment(path)})
	case len(unknown) > 0:
		p := unknown[0]
		why := p.why
		switch {
		case why == "" && (p.fMay || len(p.views) > 0):
			why = "round F served it and keploy cannot tell what it gave there"
		case why == "":
			why = "both values hold it, or only part of a word"
		}
		return dc.refuse(path, at, n, w.Rec, w.Live, RefusalUnknown,
			"keploy cannot tell what round F gave at "+p.name+" before this call ("+why+")", names(unknown),
			Edit{Kind: EditReRecord}, dc.wordNoise(path, n))
	case len(outside) > 0:
		p := outside[0]
		return dc.refuse(path, at, n, w.Rec, w.Live, RefusalAnotherScope,
			"the value "+p.name+" sent in "+scopeName(p)+"; keploy binds only within a test and the start-up", names(outside),
			Edit{Kind: EditMoveToMocks, Mock: p.mock}, Edit{Kind: EditMoveToStartup, Mock: p.mock}, dc.wordNoise(path, n))
	}
	if why, p := dc.encoded(); p != nil {
		if p.otherTest && !dc.allScope {
			return dc.refuse(path, at, n, w.Rec, w.Live, RefusalAnotherScope, why+", sent in "+scopeName(*p)+"; keploy binds only within a test and the start-up", []string{p.name},
				Edit{Kind: EditMoveToMocks, Mock: p.mock}, Edit{Kind: EditMoveToStartup, Mock: p.mock}, dc.fieldNoise(path))
		}
		return dc.refuse(path, at, n, w.Rec, w.Live, RefusalUnknown, why, []string{p.name}, Edit{Kind: EditReRecord}, dc.fieldNoise(path))
	}
	echo, no := dc.answered(path, bare, w, rec, live, true)
	if no != nil {
		return *no
	}
	rule := "no place before the call shows it"
	if len(aside) > 0 {
		p := aside[0]
		rule += " (" + p.name + " held " + strconv.Quote(w.Rec) + " and round F gave " + gaveAt(p, w.Live) + " there: set aside, as a copy moves with its source)"
	}
	if len(unserved) > 0 {
		rule += " (round F never served " + unserved[0].name + ", and a piece of a longer value under " + strconv.Itoa(unreadFloorBits) + " bits is no evidence at such a place)"
	}
	d := dc.learned(path, at, n, w.Rec, w.Live, OutcomeFresh, rule+"; bound at its first call"+answeredAt(echo, rec), names(append(aside, unserved...)))
	d.Echo = echo
	return d
}

// inDecodedForm is why no word of a leaf may be learned fresh, and the place
// that says so: an earlier place holds a word of 32 bits
// or more in each run, and only the decoded forms of the leaf's texts hold
// it. That is an id carried inside an encoding the reader opens no step for
// (base64 wrapped between other text, base32, 0x hex, a signed message):
// keploy reads such forms at a place but binds no word in them, so the
// words of the encoded text would go unchecked at their first call. A place
// that may hold them in round F (evidenceAt) refuses as well: one rule
// across forms. It binds nothing.
func (dc *decider) inDecodedForm(rds []textReading) (string, *evPlace) {
	for _, rd := range rds {
		rs, fs := decodedOnly(dc.in.Drifts, rd.Rec, rd.Live), decodedOnly(dc.in.Drifts, rd.Live, rd.Rec)
		for _, p := range dc.ps {
			for _, r := range rs {
				if !evHolds(p.t.rec, r) {
					continue
				}
				for _, f := range fs {
					if ev, read := evidenceAt(p, r, f); read && (ev == evShown || ev == evUnknown) {
						return p.name + " held " + strconv.Quote(r) + " and " + strconv.Quote(f) + " may be round F's there: this value carries it in a decoded form, where keploy binds no word", &p
					}
				}
			}
		}
	}
	return "", nil
}

// decodedOnly are the words of 32 bits or more that s's declared forms hold
// and s does not, in any case, in no form of other, sorted so a word as it
// stands comes before its lower-cased form.
func decodedOnly(dc *DriftCache, s, other string) []string {
	var out []string
	plain, others := tokensOfForms([]string{strings.ToLower(s)}), dc.rawTokens(other)
	for i, set := range dc.rawTokens(s) {
		for w := range set {
			if wordBits(w) >= 32 && !plain[i][strings.ToLower(w)] && !others[i][w] {
				out = append(out, w)
			}
		}
	}
	slices.Sort(out)
	return slices.Compact(out)
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
	if iw.Rec != nil && iw.Live != nil && !iw.ByText {
		c.rm, c.lm = iw.Rec, iw.Live
		c.ri = typedInstant{at: time.Unix(0, iw.Rec.key.at).UTC(), res: iw.Rec.res, names: namesMoment}
		c.li = typedInstant{at: time.Unix(0, iw.Live.key.at).UTC(), res: iw.Live.res, names: namesMoment}
	}
	c.echo, c.unnamed = dc.readBack(path, w.At, w, rec, true)
	c.rec = rec
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
	p              evPlace
	hr, hl, fr, fl bool
	// coarseR: R's text holds R's value only as an instant of a coarser
	// unit (a day, an hour) that the value, cut to it, is (coarserOnly).
	coarseR bool
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
	byText bool
	// text: an instant in a text, whose layout the Clock keeps; word, where
	// it stands in the leaf's recorded text (clockWordOf); noWord, why a
	// Clock there is no check a replay makes.
	text   bool
	word   models.ClockWord
	noWord string
	// echo is where the call's answer reads the word back (readBack, whole
	// values too), valueEcho the same for a Value (the text whole is the
	// adapter's echo), unnamed the first such place keploy cannot name, rec
	// the leaf's recorded text.
	echo, valueEcho []variables.Address
	unnamed, rec    string
	// aside names the earlier places that held R's value where F's does
	// not stand: a Clock's line says it set them aside.
	aside []string
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
		return word, "the text around it changed too"
	}
	return models.ClockWordOf(rec, rs, re), ""
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
	if !ok {
		return false, false, false
	}
	// An instant of several words (a date and a time) stands in a text
	// as those words in order.
	if !h && len(variables.Words(w)) > 1 {
		text, ok := t.textNow()
		if !ok {
			return false, false, false
		}
		h = strings.Contains(text, w)
	}
	if m == nil || p.gave {
		return h, h && byText, true
	}
	hm, fm, ok := t.holdsMoment(*m)
	if !ok {
		return false, false, false
	}
	if z, zok := dc.inZone(*m); !hm && zok {
		if hm, fm, ok = t.holdsMoment(z); !ok {
			return false, false, false
		}
	}
	// A text with escapes is read as it opens: as written, the date alone
	// of a percent-encoded time reads as a coarse instant, no hold of that
	// time. A form that opens holds what holdsMoment found at a fine cut.
	if hm && !fm {
		if text, ok := t.textNow(); ok && decodeForms(text).hasDec {
			hm = false
		}
	}
	if hm {
		return true, fm, true
	}
	return h, h && fineCut(m.res), true
}

// coarserOnly reports whether t, which holds w (moment m), holds it only
// as an instant of a coarser unit that m, cut to it, is (cutsEach's finer
// rule: a served date beside the application's own time of day). Such a
// place gives none of m's part below that unit, where its change lies, so
// it is no answer's source: bound to it, a replay would check the date
// alone and pass any time of that day. A wall clock of no zone is read in
// the application's zone too (inZone).
func (dc *decider) coarserOnly(t *readerText, w string, m moment) bool {
	text, ok := t.textNow()
	spans, read := t.spansOf()
	if !ok || !read || strings.Contains(text, w) {
		return false
	}
	ms := []moment{m}
	if z, ok := dc.inZone(m); ok {
		ms = append(ms, z)
	}
	for _, sp := range spans {
		for _, m := range ms {
			if h := sp.moment(); sp.keyed && (h.key == m.key || cuts(h, m, func(c cutRule) bool { return !c.finer })) {
				return false
			}
		}
	}
	return true
}

// instant decides one changed instant by the one order: a conflict at
// a fine cut (0), a pick (1), a given's text, one templated slot of the
// test case's request (2), an answer (3), a fine copy (4), a Clock its
// call's window holds (5), a coarse copy (6), a read since its test began
// (7), a client test's own value (8); else refused.
func (dc *decider) instant(path string, at variables.Address, n int, c changedInstant) Decision {
	r, l := c.r, c.l
	inScope := func(p evPlace) bool { return dc.allScope || !p.otherTest || p.given }
	// untold: answers of the scope before the call, whose view in round F
	// keploy cannot tell, that hold F's moment and not R's.
	var hs []instantHold
	var untold []evPlace
	for _, p := range dc.ps {
		hr, fr, okR := dc.holds(p, p.t.rec, r, c.rm, c.byText)
		hl, fl, okL := dc.holds(p, p.t.live, l, c.lm, c.byText)
		if !okR || !okL {
			return dc.refuse(path, at, n, r, l, RefusalUnread, "keploy could not read the run's values past their budget, which came before this call", nil, Edit{Kind: EditReRecord}, dc.fieldNoise(path))
		}
		if hr || hl {
			hs = append(hs, instantHold{p: p, hr: hr, hl: hl, fr: fr, fl: fl, coarseR: hr && c.rm != nil && dc.coarserOnly(p.t.rec, r, *c.rm),
				rank: min(dc.rank(p.t.rec, r, c.rm, hr), dc.rank(p.t.live, l, c.lm, hl))})
		}
		if hr && !hl && len(c.aside) < maxLinePlaces {
			c.aside = append(c.aside, p.name)
		}
		if p.fUnstated && !hr && p.side == Response && !p.given && p.state == stateBefore && inScope(p) && (hl || dc.inAView(p, c)) {
			untold = append(untold, p)
		}
	}
	// A place that holds a value by its exact text is named before one
	// that holds it by a cut, and a finer cut before a coarser one.
	sort.SliceStable(hs, func(i, j int) bool { return hs[i].rank < hs[j].rank })
	before := func(p evPlace) bool { return p.state == stateBefore && !p.fUnstated }
	request := func(p evPlace) bool { return p.given && !p.gave }

	// 0. Conflict: a place before in both runs that held R's value whole
	// at a fine cut (or by the text of a number that does not parse), or a
	// given that held its text, where round F gave another value and this
	// one did not follow.
	conflict := heldWhere(hs, func(h instantHold) bool { return before(h.p) && inScope(h.p) && h.hr && !h.hl && (h.fr || h.p.gave) })
	if len(conflict) > 0 {
		p := conflict[0]
		return dc.refuse(path, at, n, r, l, RefusalConflict,
			strconv.Quote(r)+" stood whole at "+p.name+"; round F gave "+gaveAt(p, l)+" there and the value did not follow", names(conflict),
			Edit{Kind: EditFixApp}, dc.fieldNoise(path), Edit{Kind: EditGive, Name: lastSegment(path)})
	}

	// 1. Pick: (i) round F's moment stands at an answer or the test case's
	// request as sent, which does not show it; (iii) the recorded moment
	// stands at an answer whose state in round F keploy cannot tell; (ii)
	// the recorded moment stands in the test case's request by a cut rule
	// that gives no moment F saw in it as sent. Never at a given.
	for _, h := range hs {
		p := h.p
		if p.gave || p.side != Response && !request(p) {
			continue
		}
		untold := func(note string) Decision {
			return dc.refuse(path, at, n, r, l, RefusalPick,
				"the recorded moment stands in "+p.name+", whose state in round F keploy cannot tell ("+note+")", []string{p.name},
				Edit{Kind: EditReRecord}, dc.wordNoise(path, n))
		}
		switch {
		case before(p) && h.hl && (!h.hr || p.same()):
			why := "round F's moment stands unchanged in " + p.name
			if !p.same() {
				why = "round F's moment stands in " + p.name + ", which does not show it"
			}
			return dc.refuse(path, at, n, r, l, RefusalPick, why, []string{p.name}, dc.wordNoise(path, n))
		case p.side == Response && p.state == stateUnknown && (h.hr || h.hl):
			return untold(p.why)
		case h.hr && dc.swappable(p, c):
			return untold(swapNote)
		}
	}
	if c.rm != nil && c.lm != nil {
		var reqs []Earlier
		for _, e := range dc.in.Earlier {
			if e.Given && !e.Gave && e.Unread == "" {
				reqs = append(reqs, e)
			}
		}
		if where, _ := dc.dr.requestHolds([]moment{*c.rm}, []moment{*c.lm}, reqs); where != "" {
			return dc.refuse(path, at, n, r, l, RefusalPick,
				"the recorded moment stands in the test case at "+where+", and no cut there gives round F's", []string{where}, dc.wordNoise(path, n))
		}
	}

	// A given's text, and 2. one templated slot of the test case's request
	// that shows it in both runs: a Value bound there; two slots, refused.
	gave := heldWhere(hs, func(h instantHold) bool { return before(h.p) && h.hr && h.hl && h.p.gave })
	slots := heldWhere(hs, func(h instantHold) bool { return before(h.p) && h.hr && h.hl && request(h.p) && !h.p.same() })
	if len(gave) > 0 {
		return dc.bound(path, at, n, c, gave, ": a value the test gave keploy, which the call sends as given in both runs")
	}
	switch len(slots) {
	case 0:
	case 1:
		return dc.bound(path, at, n, c, slots, ": the test case's request holds it, templated, in both runs")
	default:
		return dc.refuse(path, at, n, r, l, RefusalTwoSources,
			"the same moment was sent at "+slots[0].name+" and "+slots[1].name+", two separate reads", names(slots), dc.wordNoise(path, n))
	}

	// 3. Answer: an answer of the call's scope or the start-up, given
	// before the call in both runs, holds R's value, and F's text there
	// holds F's or is not stated.
	answers := heldWhere(hs, func(h instantHold) bool {
		p := h.p
		return p.side == Response && !p.given && inScope(p) && p.state == stateBefore && h.hr && !h.coarseR && (p.fUnstated || h.hl)
	})
	if len(answers) > 0 {
		// An answer recorded after the call that round F gave the
		// application before it, holding F's moment, is a second source:
		// the bind would be a guess between the two.
		// So is another test's answer given before the call whose view in
		// round F keploy cannot tell, holding R's moment apart from F's.
		late, why := dc.setSecondSource(c)
		for _, h := range hs {
			p := h.p
			switch {
			case late != "" || p.side != Response || p.given:
			case p.fGave && p.state == stateNotBefore && h.hl:
				late, why = p.name, "round F's moment stands in "+p.name+", which round F gave before the call but which was recorded after it"
			case !inScope(p) && p.state == stateBefore && p.fUnstated && h.hr && !h.hl:
				late, why = p.name, "the recorded moment stands in "+p.name+", another test's answer given before the call, whose view in round F keploy cannot tell"
			}
		}
		if late != "" {
			return dc.refuse(path, at, n, r, l, RefusalUnknown, why+", beside "+answers[0].name+": two sources", []string{late, answers[0].name},
				Edit{Kind: EditReRecord}, dc.wordNoise(path, n))
		}
		return dc.bound(path, at, n, c, answers, ": an answer given before the call holds it: the dependency's value")
	}

	// 4. and 6. A copy: requests of the call's scope or the start-up, sent
	// before the call in both runs, holding the same moment in both runs.
	// One that holds the other run's moment too (a range that grew in F to
	// hold the recorded moment and F's) did not move with the value: no
	// copy's source.
	copies := func(fine bool) []evPlace {
		return heldWhere(hs, func(h instantHold) bool {
			p := h.p
			if p.side != Request || p.given || !inScope(p) || !before(p) || !h.hr || !h.hl || (h.fr && h.fl) != fine {
				return false
			}
			rf, _, _ := dc.holds(p, p.t.live, r, c.rm, c.byText)
			fr, _, _ := dc.holds(p, p.t.rec, l, c.lm, c.byText)
			return !rf && !fr
		})
	}
	if fine := copies(true); len(fine) > 0 {
		return dc.copied(path, at, n, c, fine, ": the same moment, at a cut finer than a millisecond, was sent before the call in both runs: one read, copied")
	}

	// An answer outside the call's scope that would bind it at step 3 were
	// it of the scope: a source outside the scope, refused naming the edit,
	// as a word's is; never a read of the clock. So is one main's echo of the
	// recordings outside the scope refuses (echoedElsewhere), and one round
	// F's view of a recording of the set holds (echoedOutside).
	var gaveOutside []string
	for _, h := range hs {
		if p := h.p; p.side == Response && !p.given && !inScope(p) && p.state == stateBefore && h.hr && (p.fUnstated || h.hl) {
			gaveOutside = append(gaveOutside, p.mock)
		}
	}
	if mock, ok := dc.echoedOutside(c); ok {
		gaveOutside = append(gaveOutside, mock)
	}
	if len(gaveOutside) > 0 {
		if gaveOutside[0] == "" {
			return dc.refuse(path, at, n, r, l, RefusalPick, "round F's moment, or the recorded one, stands in an answer round F gave before the call outside its scope", nil, Edit{Kind: EditReRecord}, dc.wordNoise(path, n))
		}
		m := gaveOutside[0]
		return dc.refuse(path, at, n, r, l, RefusalAnotherScope,
			"the moment stands in "+m+"'s answer, which round F gave before the call outside its scope: a copy or a pick from it, never a read of the clock; keploy binds only within a test and the start-up", []string{m},
			Edit{Kind: EditMoveToMocks, Mock: m}, Edit{Kind: EditMoveToStartup, Mock: m}, dc.wordNoise(path, n))
	}

	// F may have given its moment at an answer whose view keploy cannot tell,
	// and no source above names it: the value may be a copy of that answer,
	// so no later step may read it as the clock or a coarse copy. Unknown,
	// as the word rule reads the same place (evidenceAt).
	if len(untold) > 0 {
		p := untold[0]
		return dc.refuse(path, at, n, r, l, RefusalUnknown,
			"keploy cannot tell what round F gave at "+p.name+" before this call (round F's moment may stand there, and the recorded one does not)", names(untold),
			Edit{Kind: EditReRecord}, dc.wordNoise(path, n))
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
	// cannot check.
	zone := dc.dr.zone
	// cutWhy: the reading's own refusal of a cut, which keeps steps 7 and
	// 8 from reading the value; guardWhy: the offset guard's refusal of the
	// window plus an offset, which refuses that reading alone, so the value
	// is still read from the test's begin (7) and as its own value (8), as
	// main learned such a read since the test began.
	var off models.ClockOffset
	var cutWhy, guardWhy string
	inWindow := false
	if c.rm != nil && c.lm != nil {
		if dc.in.Window.holdsRead(*c.rm, *c.lm, zone) {
			off, inWindow = models.ClockOffset{Res: max(c.rm.res, c.lm.res)}, true
		} else {
			off, cutWhy, inWindow = clockInWindow(path, c.ri, c.li, dc.in.Window, zone)
			if inWindow {
				if g, uncompared := dc.offsetGuard(path, off, max(c.rm.res, c.lm.res), dc.in.Window); uncompared && copies(false) == nil {
					return dc.refuse(path, at, n, r, l, RefusalCannotCheck, g, nil, dc.fieldNoise(path))
				} else if g != "" {
					off, cutWhy, guardWhy, inWindow = models.ClockOffset{}, "", g, false
				}
			}
		}
	}
	if inWindow {
		return dc.clock(path, at, n, c, off, "a read of the application's clock its call's window holds in both runs")
	}

	// 6. Coarse copy: as 4, a millisecond or coarser; no window condition.
	if coarse := copies(false); len(coarse) > 0 {
		return dc.copied(path, at, n, c, coarse, ": the same moment, at a millisecond or coarser, was sent before the call in both runs and its own window does not hold it: one read, copied")
	}

	// 7. Since begin (client tests): a read the window from each run's
	// test begin to the call holds, plain or plus an offset (a time to live
	// added to a read the application made earlier in its test, before its
	// call's window), checked from the test's begin where the reading takes
	// it (clockSinceBegin) and the guards of step 5 keep it. A look-back is
	// never checked from the begin, whichever reading is registered: it
	// would pass one the application shortened, or dropped, within the
	// test's span. A reading or guard that declines it leaves step 8 as it
	// was.
	if dc.dr.clientTests && c.rm != nil && c.lm != nil && cutWhy == "" {
		if wb := dc.in.Window.sinceBegin(); wb != nil {
			enc := max(c.rm.res, c.lm.res)
			off, ok := models.ClockOffset{Res: enc}, wb.holdsRead(*c.rm, *c.lm, zone)
			if !ok {
				off, _, ok = clockInWindow(path, c.ri, c.li, wb, zone)
				ok = ok && off.Min > 0
			}
			if ln, sok := clockSinceBegin(Learned{Kind: KindClockOffset, Offset: &off}); ok && sok && ln.Offset != nil {
				switch g, uncompared := dc.offsetGuard(path, *ln.Offset, enc, wb); {
				case uncompared:
					return dc.refuse(path, at, n, r, l, RefusalCannotCheck, g, nil, dc.fieldNoise(path))
				case g == "":
					return dc.clock(path, at, n, c, *ln.Offset, "a read of the application's clock made since its test began in both runs, before its call's window")
				}
			}
		}
	}

	// 8. The test's own value (client tests): no earlier place holds it in
	// either run, its answer does not echo it, its cut is not ambiguous,
	// and, where the runs know their test's begin, it was made before it.
	late := ""
	if dc.dr.clientTests && len(hs) == 0 && cutWhy == "" && dc.instantEchoed(c) == "" {
		if late = madeSinceBegin(dc.in.Window, dc.in.RecBegan, c, zone); late == "" {
			d := dc.learned(path, at, n, r, l, OutcomeFresh, "the test's own value: no place before the call holds it; bound at its first call"+answeredAt(c.echo, c.rec), nil)
			d.Echo = c.echo
			return d
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
	// The places whose recorded text held the recorded value and that are
	// no source in round F are named with why (main named the recording
	// round F never served): a person reads where the value came from.
	var outside, sentIn, heldAt []string
	for _, h := range hs {
		switch p := h.p; {
		case h.hr && h.hl && p.otherTest && !dc.allScope:
			if len(outside) < maxLinePlaces {
				outside = append(outside, p.name)
				sentIn = append(sentIn, p.name+" in "+scopeName(p))
			}
		case h.hr && !h.hl && len(heldAt) < maxLinePlaces:
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
	return dc.refuse(path, at, n, r, l, RefusalNoClockRead, why, append(outside, heldAt...), dc.wordNoise(path, n), Edit{Kind: EditReRecord})
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
		for _, t := range at {
			if t.Add(max(run.m.res, time.Nanosecond)).After(run.began) {
				return run.name + " lies past its test's begin"
			}
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
// swapHolds): its state in round F keploy cannot tell.
func (dc *decider) swappable(p evPlace, c changedInstant) bool {
	if p.raw == 0 {
		return false
	}
	x := dc.in.Raw[p.raw-1]
	return x.swapHolds(x.payload().text, dc.in.Raw, func(own string) bool {
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
func (dc *decider) clock(path string, at variables.Address, n int, c changedInstant, off models.ClockOffset, rule string) Decision {
	r, l := c.r, c.l
	if echo := dc.instantEchoed(c); echo != "" {
		return dc.cannotCheck(path, at, n, r, l, "keploy cannot check a clock read its own call's recorded answer holds ("+echo+"): it would serve that answer's recorded moment")
	}
	if c.text {
		lay := models.ClockOffset{Layout: clockTextLayout(r)}.MeetLayout(clockTextLayout(l))
		if lay == "" || lay == models.LayoutApart {
			return dc.cannotCheck(path, at, n, r, l, "its two runs write it in different layouts")
		}
		if c.noWord != "" {
			return dc.cannotCheck(path, at, n, r, l, "a replay checks a clock read inside a text only as one word of the text as recorded, and "+c.noWord)
		}
		off.Layout, off.Word = lay, c.word
	}
	if !off.Plain() {
		// The window holds the read; the value is the read plus this.
		rule += ", stored as " + models.ClockOffset{Min: off.Min, Max: off.Max, Res: off.Res}.String()
	}
	if len(c.aside) > 0 {
		rule += "; set aside: " + strings.Join(c.aside, ", ") + " held the recorded value and round F's value does not stand there"
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
// test's (the run's variable).
func (dc *decider) bound(path string, at variables.Address, n int, c changedInstant, src []evPlace, rule string) Decision {
	first := earliest(src)
	d := dc.learned(path, at, n, c.r, c.l, OutcomeValue, "bound to "+first.name+rule+answeredAt(c.valueEcho, c.rec), names(src))
	d.From = Place{Mock: first.mock, Side: first.side, Path: first.path, Live: c.l, Run: first.startUp || first.otherTest, InStartUp: first.startUp}
	d.FromAt = dc.fromAt(first, c.r)
	d.Echo = c.valueEcho
	return d
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

// copied is a Value bound at the earliest of src, a copy's sources; a later
// one its own call learned as a read of the clock is a second, separate
// read, and the word is refused naming both.
func (dc *decider) copied(path string, at variables.Address, n int, c changedInstant, src []evPlace, rule string) Decision {
	first := earliest(src)
	for _, p := range src {
		if p.clock && p.name != first.name {
			return dc.refuse(path, at, n, c.r, c.l, RefusalTwoSources,
				"the same moment was sent at "+first.name+" and "+p.name+", two separate reads", []string{first.name, p.name}, dc.wordNoise(path, n))
		}
	}
	return dc.bound(path, at, n, c, src, rule)
}

// instantEchoed is the first leaf of the call's recorded answer that holds
// the recorded instant, by its text or its moment; "" where none does.
func (dc *decider) instantEchoed(c changedInstant) string {
	for _, a := range dc.in.Resp {
		p := evPlace{}
		if h, _, _ := dc.holds(p, dc.x.text(a.Value), c.r, c.rm, c.byText); h {
			return a.Path
		}
	}
	return ""
}

// readBack is where the call's own answer reads back w, a word of the
// request leaf at path (recorded rec, at at's word, through no decode
// step): the same word of every answer leaf whose recorded text is rec
// whole (a find by a key its cursor echoes), and every key of the answer's
// objects that is rec whole (a lookup answered under its id), a place
// keploy cannot name: refused. Every other answer text that
// holds the word, longer or shorter (a count, a date whose minute it is),
// is served as recorded: a word that short may stand there by chance, and
// a test that reads it goes red at replay, never green by a rewrite. One
// rule for a Value and a fresh word; withWhole: a fresh word's whole value
// too (a Value's is the adapter's echo, as main answers it). unnamed is the
// path of the first such leaf keploy cannot name to the adapter.
func (dc *decider) readBack(path string, at variables.Address, w readWord, rec string, withWhole bool) (echo []variables.Address, unnamed string) {
	r := dc.in.Rec.Leaves[path]
	if at.Word < 0 || len(at.Steps) > 0 || w.Rec == rec && !withWhole {
		return nil, ""
	}
	for _, l := range dc.in.Resp {
		// An answer scalar of any type holding the whole text reads it back
		// (a number sent as text, answered as a number), as main's WholeEcho.
		scalar := l.Type == String || l.Type == Number || l.Type == Bool
		if l.Value == rec && (l.Type == r.Type || scalar) || l.Type == Keys && slices.Contains(strings.Split(l.Value, "\n"), rec) {
			a := dc.in.AddressOf(l, true)
			if a.Place.Part == 0 && unnamed == "" {
				unnamed = l.Path
			}
			a.Word = at.Word
			echo = append(echo, a)
		}
	}
	return echo, unnamed
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

// fromAt is the address of the word v at p, a Value's bind place: the
// earlier exchange's leaf as its adapter names it (an answer's as an
// answer's), at v's word where v is one of its words, else the whole leaf;
// no place (word -1) for a recording keploy read whole.
func (dc *decider) fromAt(p evPlace, v string) variables.Address {
	if p.leaf.Path == "" {
		return variables.Address{Word: -1}
	}
	a := dc.in.AddressOf(p.leaf, p.side == Response)
	for i, w := range variables.Words(p.leaf.Value) {
		if p.leaf.Value[w[0]:w[1]] == v {
			a.Word = i
			break
		}
	}
	return a
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
		Outcome: o, Rule: rule, Places: places, From: from,
	})}
}

// refuse is a refused word and its line.
func (dc *decider) refuse(path string, at variables.Address, n int, r, f string, kind RefusalKind, why string, places []string, edits ...Edit) Decision {
	at.Word = n
	rf := &Refusal{Kind: kind, Test: dc.in.Test, Mock: dc.in.Mock, Field: path, Word: n, Recorded: r, Full: f, Why: why, Places: places, Edits: edits}
	hideSecrets(rf)
	return Decision{At: at, Recorded: r, Full: f, Outcome: OutcomeRefused, Refusal: rf, Line: RefusalLine(*rf)}
}

// cannotCheck refuses a field keploy cannot decide yet: its limit.
func (dc *decider) cannotCheck(path string, at variables.Address, n int, r, f, why string) Decision {
	return dc.refuse(path, at, n, r, f, RefusalCannotCheck, why, nil, dc.fieldNoise(path))
}

func (dc *decider) fieldNoise(path string) Edit { return noiseEdit(dc.in.Mock, path, -1) }

func (dc *decider) wordNoise(path string, n int) Edit { return noiseEdit(dc.in.Mock, path, n) }

// noiseEdit is noise at word n of field, or at the field where n is -1.
func noiseEdit(mock, field string, n int) Edit {
	if n < 0 {
		return Edit{Kind: EditNoiseAtField, Mock: mock, Field: field, Word: -1}
	}
	return Edit{Kind: EditNoiseAtWord, Mock: mock, Field: field, Word: n}
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

// lastSegment is path's last key, a given's name.
func lastSegment(path string) string {
	if i := strings.LastIndexAny(path, ".]"); i >= 0 && i+1 < len(path) {
		return path[i+1:]
	}
	return path
}
