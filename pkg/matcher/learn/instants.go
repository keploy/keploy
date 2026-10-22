package learn

import (
	"strings"
	"time"
)

// instant is an instant a text holds in a clock layout: where it stands
// (from, to), what it says (at) to the resolution it says it at (res), and
// whether it is a time of day with no date (day).
type instant struct {
	from, to int
	at       time.Time
	res      time.Duration
	day      bool
}

// instantsOf are the instants s holds in clockLayouts, at ASCII word
// boundaries, each place read by the first layout that reads it, as clockIn
// reads them. A UUID is no instant here: its groups are an id's words.
func instantsOf(s string) []instant {
	if s == "" {
		return nil
	}
	taken := make([]bool, len(s))
	var out []instant
	for _, lay := range clockLayouts {
		if lay.timeWord != nil {
			continue
		}
		for _, m := range clockMatches(lay, s, taken) {
			t, res, ok := lay.read(s[m[0]:m[1]])
			if !ok {
				continue
			}
			for i := m[0]; i < m[1]; i++ {
				taken[i] = true
			}
			out = append(out, instant{from: m[0], to: m[1], at: t.UTC(), res: res, day: lay.wraps})
		}
	}
	return out
}

// instantPiece is the instants of s that the word t stands strictly inside,
// wherever s holds it as a token (tokenAt): the minute "18" of
// "14:18:04.274Z", the date "2026-10-22T14" of an id reading. ok is false
// when s does not hold t, or holds it once outside them, or t is a whole
// instant itself: a value of its own, searched as one.
func instantPiece(s, t string, ins []instant) ([]instant, bool) {
	if t == "" || len(ins) == 0 {
		return nil, false
	}
	var out []instant
	found := false
	for i := 0; i <= len(s)-len(t); {
		j := strings.Index(s[i:], t)
		if j < 0 {
			break
		}
		at := i + j
		i = at + 1
		if !tokenAt(s, t, at) {
			continue
		}
		found = true
		in := false
		for _, x := range ins {
			if x.from <= at && at+len(t) <= x.to && x.to-x.from > len(t) {
				out, in = append(out, x), true
				break
			}
		}
		if !in {
			return nil, false
		}
	}
	return out, found
}

// holdsInstant reports whether have holds one of want: the same instant, to
// the coarser of their two resolutions (a time read to the millisecond is
// the one a header writes to the second), a time of day compared modulo a
// day. A piece of an instant (a minute, a second) is no value of its own: it
// came from another text only where that text holds its instant.
func holdsInstant(want, have []instant) bool {
	for _, w := range want {
		for _, h := range have {
			if sameInstant(w, h) {
				return true
			}
		}
	}
	return false
}

func sameInstant(a, b instant) bool {
	res := a.res
	if b.res > res {
		res = b.res
	}
	if res <= 0 {
		res = time.Nanosecond
	}
	if a.day || b.day {
		day := 24 * time.Hour
		of := func(t time.Time) time.Duration {
			return (time.Duration(t.Hour())*time.Hour + time.Duration(t.Minute())*time.Minute + time.Duration(t.Second())*time.Second + time.Duration(t.Nanosecond())) % day
		}
		x, y := of(a.at), of(b.at)
		return x-x%res == y-y%res
	}
	return a.at.Truncate(res).Equal(b.at.Truncate(res))
}

// instantReach is how far from a word an instant holding it can reach: no
// clock layout reads more than this many bytes.
const instantReach = 64

// pieceAt reports whether text holds t, as a token, at a place it can have
// come from (or gone to): inside an instant equal to one of want
// (holdsInstant), or, when loose, outside every instant of text (a number of
// its own). A place inside another instant (the minute of a Date header, the
// seconds of another time) is another clock read's piece, never one. Each
// place t stands is read on its own, the instant around it read within
// instantReach and at the full text's word boundaries, so a long payload is
// never read whole for one word.
func pieceAt(text, t string, want []instant, loose bool) bool {
	for i := 0; i <= len(text)-len(t); {
		j := strings.Index(text[i:], t)
		if j < 0 {
			return false
		}
		at := i + j
		i = at + 1
		if !tokenAt(text, t, at) {
			continue
		}
		lo, hi := at-instantReach, at+len(t)+instantReach
		if lo < 0 {
			lo = 0
		}
		if hi > len(text) {
			hi = len(text)
		}
		inside := false
		for _, x := range instantsOf(text[lo:hi]) {
			from, to := lo+x.from, lo+x.to
			if from > at || at+len(t) > to {
				continue
			}
			// A read the window cut out of a longer word is none.
			if from > 0 && isWord(text[from-1]) && isWord(text[from]) || to < len(text) && isWord(text[to-1]) && isWord(text[to]) {
				continue
			}
			if holdsInstant(want, []instant{x}) {
				return true
			}
			inside = true
		}
		if loose && !inside {
			return true
		}
	}
	return false
}

// pieceNotIn reports whether t, a word of what the drifted value s changed
// (ins: its instants), is a piece of an instant of s (instantPiece) that
// none of texts holds where it can have come from: an instant changes whole,
// so a piece of one came from that instant, written anywhere at any
// precision, or from the clock, never from another clock read (a header's
// minute) nor from a number that happens to equal it ("26.04" of a
// User-Agent). The source of a drifted value is searched so.
func pieceNotIn(s, t string, ins []instant, texts ...string) bool {
	return pieceNot(s, t, ins, false, texts)
}

// pieceNotEchoed is pieceNotIn for the call's own answer, which may be
// derived from a part of the time it was sent (a sequence by the second,
// "seq 05"): there a number of its own holding the piece counts, and only
// another clock read's piece does not.
func pieceNotEchoed(s, t string, ins []instant, texts ...string) bool {
	return pieceNot(s, t, ins, true, texts)
}

func pieceNot(s, t string, ins []instant, loose bool, texts []string) bool {
	piece, ok := instantPiece(s, t, ins)
	if !ok {
		return false
	}
	for _, text := range texts {
		if pieceAt(text, t, piece, loose) {
			return false
		}
	}
	return true
}
