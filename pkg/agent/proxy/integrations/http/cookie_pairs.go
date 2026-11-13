package http

import (
	"net/url"
	"slices"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
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
// in the recorded order and then the live one; or that the same pairs come
// in another order. "" where the pairs are the same, in the same order.
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
// name's values in order. A part with no '=' is no pair.
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

// pairsGone are the pairs of rec, a Cookie header's recorded text, that
// live, its text in this run, does not send; ok is true where those are its
// only difference: every other pair is sent once, in the recorded order,
// with its recorded value, live sends no pair rec lacks, and each pair gone
// is named once in rec. ok is false where no pair is gone.
func pairsGone(rec, live string) (gone []string, ok bool) {
	rn, rv := cookieJar(rec)
	ln, lv := cookieJar(live)
	var kept []string
	for _, name := range rn {
		if _, sent := lv[name]; sent {
			kept = append(kept, name)
			continue
		}
		if len(rv[name]) != 1 {
			return nil, false
		}
		gone = append(gone, name)
	}
	if len(gone) == 0 || !slices.Equal(kept, ln) {
		return nil, false
	}
	for _, name := range ln {
		if !slices.Equal(rv[name], lv[name]) {
			return nil, false
		}
	}
	return gone, true
}

// goneAsLearned reports whether live, a Cookie header's text in this run,
// is rec, its recorded text, without pairs the mock learned the page sets
// while its test runs (cookieSetByPage), and otherwise as recorded.
func goneAsLearned(learned map[string][]string, rec, live string) bool {
	gone, ok := pairsGone(rec, live)
	if !ok {
		return false
	}
	for _, name := range gone {
		if _, set := learned[cookieSetByPage+name]; !set {
			return false
		}
	}
	return true
}

// pagePairsGone are the learned noise of a call paired with m, an HTTP
// mock (its request recorded), whose Cookie header rec, as recorded, lost pairs in this run (live:
// pairsGone) that the page itself set while the test ran: noise at each
// such pair's key (cookieSetByPage). A pair is the page's where m's earlier
// recordings (earlier: those the learn step reads as m's earlier places)
// show both that a call to m's host recorded before m went without it, so
// the test began without it, and that no answer recorded before m set it
// (Set-Cookie), so no service did: a session cookie a service sets, and
// one the test began with, stay compared, and a call that goes without
// them is refused. An answer that cannot be read back counts as one that
// set it. nil where any pair gone is not the page's: the call's Cookie
// header is then learned, or refused, as before.
func pagePairsGone(m *models.Mock, rec, live string, earlier []*models.Mock) []learn.Learned {
	gone, ok := pairsGone(rec, live)
	if !ok {
		// Nothing gone: no recording before m is read.
		return nil
	}
	at, host := m.Spec.ReqTimestampMock, hostOf(m.Spec.HTTPReq)
	without := make(map[string]bool, len(gone))
	for _, b := range earlier {
		if b == nil || b.Spec.HTTPReq == nil || !b.Spec.ReqTimestampMock.Before(at) {
			continue
		}
		answered, err := withResponse(b)
		if err != nil || answered.Spec.HTTPResp == nil {
			return nil
		}
		set := headerOf(answered.Spec.HTTPResp.Header, "Set-Cookie")
		for _, name := range gone {
			if strings.Contains(set, name+"=") {
				return nil
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
	out := make([]learn.Learned, 0, len(gone))
	for _, name := range gone {
		if !without[name] {
			return nil
		}
		key := cookieSetByPage + name
		l := learn.Leaf{Path: key, Noise: key, Type: learn.String}
		out = append(out, learn.Learned{Kind: learn.KindNoise, Recorded: l, Live: l})
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
