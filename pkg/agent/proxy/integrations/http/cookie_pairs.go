package http

import (
	"slices"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/matcher/learn"
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
