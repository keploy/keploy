package http

import (
	"crypto/sha256"
	"encoding/hex"
	"maps"
	"net/url"
	"slices"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/variables"
)

// cookieLeaf is the learn step's path of the Cookie header (learnNoise.shape
// names a header by its lowercased name).
const cookieLeaf = "header.cookie"

// maxNamedPairs is how many of a Cookie header's pairs a refusal names; the
// rest are counted.
const maxNamedPairs = 5

// withCookiePairs is why, the learn step's refusal of the leaf rec (live in
// this run), with the pairs of a Cookie header that differ named after the
// leaf's path: a jar holds a session token beside pairs the page sets
// itself, and "header.cookie changed" alone does not say which of them the
// call sent otherwise. It names pairs, never a value. why is returned as it
// is for any other leaf, and where no pair differs (only the bytes between
// pairs did).
func withCookiePairs(why string, rec, live learn.Leaf) string {
	if rec.Path != cookieLeaf {
		return why
	}
	pairs := cookiePairsDiffer(rec.Value, live.Value)
	if pairs == "" {
		return why
	}
	if !strings.Contains(why, rec.Path) {
		return why + " (" + rec.Path + ": " + pairs + ")"
	}
	return strings.Replace(why, rec.Path, rec.Path+" ("+pairs+")", 1)
}

// cookiePairsDiffer says, by name, how the pairs of live, a Cookie header's
// text in this run, differ from those of rec, its recorded text: a pair not
// sent in this run, one that holds another value (a name sent more than
// once differs where its values, in order, do), one sent in this run only,
// in the recorded order and then the live one; that its parts with no name
// differ (unnamedParts: no value is said); or that the same pairs come in
// another order. "" where the pairs and those parts are the same, in the
// same order.
func cookiePairsDiffer(rec, live string) string {
	rn, rv := cookieJar(rec)
	ln, lv := cookieJar(live)
	var parts []string
	n := 0
	say := func(name, how string) {
		n++
		if len(parts) < maxNamedPairs {
			parts = append(parts, "pair "+pairName(name)+" "+how)
		}
	}
	for _, name := range rn {
		switch l, ok := lv[name]; {
		case !ok:
			say(name, "is not sent in this run")
		case !slices.Equal(rv[name], l):
			say(name, "holds another value")
		}
	}
	for _, name := range ln {
		if _, ok := rv[name]; !ok {
			say(name, "is sent in this run only")
		}
	}
	if !slices.Equal(unnamedParts(rec), unnamedParts(live)) {
		n++
		if len(parts) < maxNamedPairs {
			parts = append(parts, "its parts with no name differ")
		}
	}
	if n == 0 {
		if strings.Join(rn, ";") != strings.Join(ln, ";") {
			return "its pairs are sent in another order"
		}
		return ""
	}
	if n > len(parts) {
		parts = append(parts, strconv.Itoa(n-len(parts))+" more pairs differ")
	}
	return strings.Join(parts, "; ")
}

// cookieJar is the pairs of a Cookie header's text, as variables' cookie
// step reads them (split at ';', the name trimmed, the value up to its
// trailing blanks): each name once, in the order it first stands, and each
// name's values in order. A part with no '=' is no pair (unnamedParts).
func cookieJar(text string) ([]string, map[string][]string) {
	var names []string
	values := map[string][]string{}
	for _, pair := range strings.Split(text, ";") {
		name, v, ok := strings.Cut(pair, "=")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if _, seen := values[name]; !seen {
			names = append(names, name)
		}
		values[name] = append(values[name], strings.TrimRight(v, " \t"))
	}
	return names, values
}

// unnamedParts is the parts of a Cookie header's text with no '=', each
// trimmed, in order: a cookie set with no name, which a browser sends as its
// value alone. They are no pair (cookieJar), so no pair rule excuses them:
// they are compared as recorded. A part of blanks alone is the bytes between
// pairs, not a part.
func unnamedParts(text string) []string {
	var out []string
	for _, part := range strings.Split(text, ";") {
		if p := strings.TrimSpace(part); p != "" && !strings.Contains(p, "=") {
			out = append(out, p)
		}
	}
	return out
}

// pairName is a cookie's name as a refusal writes it: as sent where every
// byte is a visible ASCII one other than '"', else quoted.
func pairName(name string) string {
	if name == "" {
		return strconv.Quote(name)
	}
	for i := 0; i < len(name); i++ {
		if c := name[i]; c <= ' ' || c >= 0x7f || c == '"' {
			return strconv.Quote(name)
		}
	}
	return name
}

// cookieSetByPage begins the learned-noise key (MockSpec.ReqBodyNoise) of a
// pair of a recorded Cookie header that the page set while its test ran:
// "header.cookie|set-by-page:" then the pair's name. A replay's call may go
// without that pair (the page had not set it yet when the mocks answered
// sooner than the real services did), and only without it: a value it
// sends there is compared as recorded, as is every other pair. The key
// names no leaf, so no learned entry of a whole leaf covers it or is
// covered by it (learnedCovers).
const cookieSetByPage = cookieLeaf + "|set-by-page:"

// cookieSetByRace begins the learned-noise key of a pair of a recorded
// Cookie header whose value two or more answers of its test set in a race
// (racedValues): "header.cookie|set-by-race:" then the pair's name, holding
// the sha256 digest of each value the race set (cookieDigest), never a
// value itself. The jar keeps whichever racing answer the browser read
// last, which differs run to run; a replay's call may send the pair with
// any one of those values, and only with one of them: every other pair is
// compared as recorded.
const cookieSetByRace = cookieLeaf + "|set-by-race:"

// cookieDigest is how a set-by-race key holds a value: its sha256, in hex.
func cookieDigest(v string) string {
	sum := sha256.Sum256([]byte(v))
	return hex.EncodeToString(sum[:])
}

// pairsAsLearned reports whether live, a Cookie header's text in this run,
// is rec, its recorded text, with pairs the mock learned excused and
// otherwise as recorded, pair by pair: a pair the page sets (cookieSetByPage)
// gone, named once in rec; a pair a race set (cookieSetByRace) sent once with
// a value whose digest the key holds, sent once in rec. Every other pair is
// sent with its recorded values, in the recorded order, and live sends no
// pair rec lacks; its parts with no name (unnamedParts) are rec's, in order.
// false where no pair is excused.
func pairsAsLearned(learned map[string][]string, rec, live string) bool {
	if !slices.Equal(unnamedParts(rec), unnamedParts(live)) {
		return false
	}
	rn, rv := cookieJar(rec)
	ln, lv := cookieJar(live)
	var kept []string
	excused := false
	for _, name := range rn {
		l, sent := lv[name]
		switch {
		case !sent:
			if _, page := learned[cookieSetByPage+name]; !page || len(rv[name]) != 1 {
				return false
			}
			excused = true
			continue
		case !slices.Equal(rv[name], l):
			digests, raced := learned[cookieSetByRace+name]
			if !raced || len(rv[name]) != 1 || len(l) != 1 || !slices.Contains(digests, cookieDigest(l[0])) {
				return false
			}
			excused = true
		}
		kept = append(kept, name)
	}
	return excused && slices.Equal(kept, ln)
}

// cookiePairsLearned is what a call paired with m, an HTTP mock (its request
// recorded), learns pair by pair of its Cookie header, rec as recorded and
// live in this run, from m's earlier recordings (those the learn step reads
// as m's earlier places): noise at the key of each pair live does not send
// that the page itself set while the test ran (cookieSetByPage: pageSet), and
// at the key of each pair live sends with another value where answers of
// the test set both values in a race (cookieSetByRace: racedValues). rest is
// rec with those pairs as live sends them (a pair gone dropped, a raced pair
// at its live value): the learn step reads it as the header's recorded text,
// so every other pair that differs is learned, or refused, by the header's
// own rule. nil and rec where no pair is excused.
func cookiePairsLearned(m *models.Mock, rec, live string, earlier []*models.Mock) ([]learn.Learned, string) {
	rn, rv := cookieJar(rec)
	_, lv := cookieJar(live)
	var gone, changed []string
	for _, name := range rn {
		l, sent := lv[name]
		switch {
		case !sent && len(rv[name]) == 1:
			gone = append(gone, name)
		case sent && len(rv[name]) == 1 && len(l) == 1 && rv[name][0] != l[0]:
			changed = append(changed, name)
		}
	}
	if len(gone) == 0 && len(changed) == 0 {
		// Nothing a pair's rule reads: no recording before m is read.
		return nil, rec
	}
	var out []learn.Learned
	drop := map[string]bool{}
	to := map[string]string{}
	for _, name := range pageSet(m, gone, earlier) {
		key := cookieSetByPage + name
		l := learn.Leaf{Path: key, Noise: key, Type: learn.String}
		out = append(out, learn.Learned{Kind: learn.KindNoise, Recorded: l, Live: l})
		drop[name] = true
	}
	for _, name := range changed {
		values := racedValues(m, name, rv[name][0], lv[name][0], earlier)
		if values == nil {
			continue
		}
		digests := make([]string, 0, len(values))
		for _, v := range values {
			digests = appendNew(digests, cookieDigest(v))
		}
		slices.Sort(digests)
		key := cookieSetByRace + name
		l := learn.Leaf{Path: key, Noise: key, Type: learn.String, Value: strings.Join(digests, ",")}
		out = append(out, learn.Learned{Kind: learn.KindNoise, Recorded: l, Live: l})
		to[name] = lv[name][0]
	}
	if len(out) == 0 {
		return nil, rec
	}
	parts := strings.Split(rec, ";")
	kept := parts[:0]
	for _, part := range parts {
		name, _, ok := strings.Cut(part, "=")
		name = strings.TrimSpace(name)
		v, raced := to[name]
		switch {
		case ok && drop[name]:
			continue
		case ok && raced:
			part = part[:strings.Index(part, "=")+1] + v
		}
		kept = append(kept, part)
	}
	return out, strings.TrimLeft(strings.Join(kept, ";"), " \t")
}

// pairedWithoutCookie is res, the learn step's pairing of live, a call's
// shape, read again where the call sends no Cookie header and missed: as a
// call that sends the header with no pair (an empty list of pairs). A page
// that sets its only pairs itself sends none before it set them, and a
// mock that answers sooner than its service did lets it call first. It is
// that pairing, and the shape it read, only where the call then pairs
// (learn.Paired) with a recording whose Cookie header is what drifted; the
// caller still holds every recorded pair to the page's rule
// (cookiePairsLearned) before it takes it.
func pairedWithoutCookie(live learn.Shape, cands []learn.Candidate, res learn.Result) (learn.Result, learn.Shape, bool) {
	if _, sent := live.Leaves[cookieLeaf]; sent || res.Outcome != learn.Miss {
		return res, live, false
	}
	s := live
	s.Leaves = make(map[string]learn.Leaf, len(live.Leaves)+1)
	maps.Copy(s.Leaves, live.Leaves)
	s.Leaves[cookieLeaf] = learn.Leaf{Path: cookieLeaf, Noise: cookieLeaf, Type: learn.String}
	again := learn.Pair(s, cands)
	if again.Outcome != learn.Paired || !slices.Contains(again.Diff.Drift, cookieLeaf) {
		return res, live, false
	}
	return again, s, true
}

// learnedNoise is what the mock's learned noise holds at a KindNoise leaf r
// learned: the digests of a set-by-race key (cookieSetByRace), nothing at
// any other.
func learnedNoise(r learn.Leaf) []string {
	if !strings.HasPrefix(r.Path, cookieSetByRace) || r.Value == "" {
		return []string{}
	}
	return strings.Split(r.Value, ",")
}

// pairwiseKept reports whether learned, what the learn step learned of a
// call whose Cookie header it read with pairs excused (cookiePairsLearned),
// learned the header's other pairs as a replay compares them pair by pair:
// as noise, refused (unaccounted), or as the use of a value the test gave
// (given) at one or more of its pairs (givenFor's steps), which a replay
// writes at each such pair of the recorded header before it compares the
// pairs one by one (pairsAsLearned). A variable, an echo or a read of the
// clock is checked against the header's whole text, and so is a given the
// header's text holds whole: no pair holds it.
func pairwiseKept(learned []learn.Learned, given []variables.Binding) bool {
	for _, l := range learned {
		if l.Recorded.Path != cookieLeaf || l.Kind == learn.KindNoise || l.Kind == learn.KindUnaccounted {
			continue
		}
		if _, steps, ok := givenFor(l.Recorded, l.Live, given); ok && len(steps) > 0 {
			continue
		}
		return false
	}
	return true
}

// learnedKeys is the learned noise a mock holds for learned, its leaves
// learned as noise (learnedNoise).
func learnedKeys(learned []learn.Learned) map[string][]string {
	out := make(map[string][]string, len(learned))
	for _, l := range learned {
		out[l.Recorded.Path] = learnedNoise(l.Recorded)
	}
	return out
}

// pageSet are those of gone, pairs of m's recorded Cookie header a call in
// this run went without, that the page itself set while the test ran. A
// pair is the page's where m's earlier recordings show both that a call to
// m's host recorded before m went without it, so the test began without it,
// and that no answer recorded before m set it (Set-Cookie), so no service
// did: a session cookie a service sets, and one the test began with, stay
// compared, and a call that goes without them is refused. An answer that
// cannot be read back counts as one that set every pair gone.
func pageSet(m *models.Mock, gone []string, earlier []*models.Mock) []string {
	if len(gone) == 0 {
		return nil
	}
	at, host := m.Spec.ReqTimestampMock, hostOf(m.Spec.HTTPReq)
	without := make(map[string]bool, len(gone))
	set := make(map[string]bool, len(gone))
	for _, b := range earlier {
		if b == nil || b.Spec.HTTPReq == nil || !b.Spec.ReqTimestampMock.Before(at) {
			continue
		}
		answered, err := withResponse(b)
		if err != nil || answered.Spec.HTTPResp == nil {
			return nil
		}
		text := headerOf(answered.Spec.HTTPResp.Header, "Set-Cookie")
		for _, name := range gone {
			if strings.Contains(text, name+"=") {
				set[name] = true
			}
		}
		if hostOf(b.Spec.HTTPReq) != host {
			continue
		}
		_, sent := cookieJar(headerOf(b.Spec.HTTPReq.Header, "Cookie"))
		for _, name := range gone {
			if _, ok := sent[name]; !ok {
				without[name] = true
			}
		}
	}
	var out []string
	for _, name := range gone {
		if without[name] && !set[name] {
			out = append(out, name)
		}
	}
	return out
}

// racedValues are the values of the pair name that answers of m's test
// recorded before m set in a race, where one of them set rv, the pair's
// value in m's recorded Cookie header, and another lv, its value in this
// run: answers (Set-Cookie of name; the last such cookie of an answer) to
// requests that each sent the pair once, all with one earlier value. The
// browser keeps whichever of them it read last, so the pair holds any one
// of their values. nil where no such race set both: a value no answer of
// the test set, one a single answer set, two answers to requests that sent
// other values (a rotation, not a race). An answer that cannot be read back
// is no answer of a race.
func racedValues(m *models.Mock, name, rv, lv string, earlier []*models.Mock) []string {
	type answer struct{ mock, value string }
	at := m.Spec.ReqTimestampMock
	races := map[string][]answer{}
	for _, b := range earlier {
		if b == nil || b.Spec.HTTPReq == nil || !b.Spec.ReqTimestampMock.Before(at) {
			continue
		}
		answered, err := withResponse(b)
		if err != nil || answered.Spec.HTTPResp == nil {
			continue
		}
		values := setCookieValues(headerOf(answered.Spec.HTTPResp.Header, "Set-Cookie"), name)
		_, sent := cookieJar(headerOf(b.Spec.HTTPReq.Header, "Cookie"))
		if len(values) == 0 || len(sent[name]) != 1 {
			continue
		}
		from := sent[name][0]
		races[from] = append(races[from], answer{b.Name, values[len(values)-1]})
	}
	for _, race := range races {
		setRec, setLive := "", ""
		for _, a := range race {
			switch a.value {
			case rv:
				setRec = a.mock
			case lv:
				setLive = a.mock
			}
		}
		if setRec == "" || setLive == "" || setRec == setLive {
			continue
		}
		var out []string
		for _, a := range race {
			out = appendNew(out, a.value)
		}
		return out
	}
	return nil
}

// setCookieValues are the values text, a recorded Set-Cookie header (its
// lines joined with ","), sets for the cookie name, in order: each line's
// first pair whose name is name. A line starts the text or follows a ','
// (and blanks); its value runs to the next ';' or ',', which a cookie's
// value never holds (RFC 6265 §4.1.1).
func setCookieValues(text, name string) []string {
	var out []string
	for i := 0; i < len(text); {
		at := strings.Index(text[i:], name+"=")
		if at < 0 {
			break
		}
		at += i
		i = at + len(name) + 1
		if strings.TrimRight(text[:at], " \t") != "" && !strings.HasSuffix(strings.TrimRight(text[:at], " \t"), ",") {
			continue
		}
		end := strings.IndexAny(text[i:], ";,")
		if end < 0 {
			end = len(text) - i
		}
		out = append(out, strings.TrimSpace(text[i:i+end]))
	}
	return out
}

// hostOf is the host (and port) a recorded request went to: its URL's, or,
// where the URL names none, its Host header's.
func hostOf(r *models.HTTPReq) string {
	if u, err := url.Parse(r.URL); err == nil && u.Host != "" {
		return strings.ToLower(u.Host)
	}
	return strings.ToLower(headerOf(r.Header, "Host"))
}

// headerOf is the value of the header name in a recorded header set, its
// name in any case.
func headerOf(h map[string]string, name string) string {
	for k, v := range h {
		if strings.EqualFold(k, name) {
			return v
		}
	}
	return ""
}
