package learn

import "strings"

// RequestWords is a filter of the words of a client test's request whose
// body keploy could not read whole (Tracker.Incoming's unread: a body
// larger than the ingress reads, one of an encoding a recording keeps as its
// client sent it): each word of letters and digits of the body, as the
// application reads it (decoded), in each form the search reads a text in
// (textForms: as written, its escapes and quoted-printable read, its runs of
// the base64 alphabet decoded, each case-folded), read as it streams past a
// window of requestWordsWindow bytes at a time and never kept (a Bloom
// filter, wordFilter, of a fixed size, requestWordsBytes). It holds every
// word of those forms, and a word that was not, about once in a hundred for
// a body of up to about a hundred thousand distinct words and forms of
// them, more often for a larger one: a word it holds that was not written
// costs a refusal (the request may be the source of the value), never a
// value learned from a request that holds it. A nil RequestWords, and one
// that could not read its text in those forms (undecoded), may hold any
// word.
type RequestWords struct {
	f wordFilter
	// buf is the text written and not yet read: less than a window, which
	// ends at a place no form runs across (requestWordsCut).
	buf []byte
	// n is the bytes written.
	n int64
	// instant says a form of the text held a token shaped as an instant
	// (instantShaped), which a value's instant in another layout may be
	// written from (mayHold).
	instant bool
	// undecoded says the text held a window with no place to cut it, a run
	// of a form longer than a window (a file sent as one base64 string), or
	// more than requestWordsMaxBytes: its forms were not all read, and it
	// may hold any word.
	undecoded bool
}

const (
	// requestWordsBytes is the size of every request's filter: 1 Mi bits,
	// ten for each of about a hundred thousand words and forms of them.
	requestWordsBytes = 128 << 10
	// requestWordsWindow is the most of a text read at once (textForms).
	requestWordsWindow = 64 << 10
	// requestWordsMaxBytes is the most of a text read: past it the filter
	// may hold any word.
	requestWordsMaxBytes = 64 << 20
)

// NewRequestWords is an empty filter of a request's words.
func NewRequestWords() *RequestWords {
	return &RequestWords{f: make(wordFilter, requestWordsBytes/8)}
}

// Write adds the words of p, a piece of the text in the order it comes: a
// word or a form p ends inside of goes on in the next Write (End ends it).
// It never fails.
func (w *RequestWords) Write(p []byte) (int, error) {
	n := len(p)
	w.n += int64(n)
	if w.n > requestWordsMaxBytes {
		w.undecoded, w.buf = true, nil
	}
	for len(p) > 0 && !w.undecoded {
		k := min(requestWordsWindow-len(w.buf), len(p))
		w.buf = append(w.buf, p[:k]...)
		p = p[k:]
		if len(w.buf) == requestWordsWindow {
			cut := requestWordsCut(w.buf)
			if cut <= 0 {
				w.undecoded, w.buf = true, nil
				break
			}
			w.read(string(w.buf[:cut]))
			w.buf = append(w.buf[:0], w.buf[cut:]...)
		}
	}
	return n, nil
}

// requestWordsCut is the last place in b, after its first byte, before a
// byte no form runs across (a space or a printable mark that is no part of
// a word, of a run of the base64 alphabet, of an escape or of a
// quoted-printable one; never a line break, which wrapped base64 runs
// across): 0 where there is none. A dot ends every run (nextB64Run), a
// JWT's parts too, so a cut before one splits none. The byte before it may
// start an escape: '%' and '=' go on in a hex digit, a line break or a
// backslash, and '\' in a letter, '/', a backslash or '"', the only one of
// these it cuts before, and "\"" reads as a space (unescaped), a
// separator, so each side reads as it does whole.
//
// It never cuts before a ':', so no cut hides an instant (instantShaped):
// a cut before the one ':' of a time of day to the minute ("10" | ":00")
// leaves neither side one, and every other instant a cut may split (at a
// space, ',' or '.', the only bytes it cuts before that an instant holds)
// leaves one whole on a side: an ISO 8601 date before its time, a time of
// day ("10:00:41") in an RFC 1123 one, the seconds of an epoch before its
// fraction, a time of day before its fraction.
func requestWordsCut(b []byte) int {
	for i := len(b) - 1; i > 0; i-- {
		if cutBefore(b[i]) {
			return i
		}
	}
	return 0
}

// cutBefore says whether a text may be cut before c (requestWordsCut).
func cutBefore(c byte) bool {
	return c < 0x80 && !isB64Byte(c) && !strings.ContainsRune("=\\%:", rune(c)) && (c == ' ' || c == '\t' || c > ' ' && c < 0x7f)
}

// read adds the words of each form of text, one window (textForms), and
// notes a form shaped as an instant (instantShaped).
func (w *RequestWords) read(text string) {
	for _, form := range textForms(text) {
		eachWord(form, w.f.add)
		if !w.instant && instantShaped(form) {
			w.instant = true
		}
	}
}

// instantMinDigits is the fewest digits of a run read as an instant: Unix
// seconds since 1973 (nine digits) and every longer epoch.
const instantMinDigits = 9

// instantShaped says whether s holds a token shaped as an instant: a run of
// instantMinDigits digits or more, or a match of a layout keploy reads an
// instant in (clockLayouts: an RFC 1123 or ISO 8601 date or time, a compact
// one, a time-bearing UUID, an epoch, a time of day), whether or not its
// reader reads it (month 13 is a match). Random bytes hold one about once
// in a hundred megabytes: the likeliest, a time of day, is two digits, ':'
// and two digits.
func instantShaped(s string) bool {
	run := 0
	for i := 0; i < len(s); i++ {
		if !isDigit(s[i]) {
			run = 0
		} else if run++; run >= instantMinDigits {
			return true
		}
	}
	for _, m := range scanInstants(s) {
		if len(m) > 0 {
			return true
		}
	}
	return false
}

// WriteText adds the words of s as a text of its own: a word that s begins
// or ends with is not joined to the words written before or after it.
func (w *RequestWords) WriteText(s string) {
	w.End()
	_, _ = w.Write([]byte(s))
	w.End()
}

// End reads the text written and not yet read, ending the word or form
// being read.
func (w *RequestWords) End() {
	if len(w.buf) > 0 { // nil once undecoded
		w.read(string(w.buf))
	}
	w.buf = nil
}

// MayHoldAny reports whether w may hold any word: it is nil, or it could
// not read its text in every form (undecoded). A writer may stop writing
// to one that does.
func (w *RequestWords) MayHoldAny() bool { return w == nil || w.undecoded }

// valueChange is a value's change from its recorded run to record's full
// round, as a request's words are asked for it (RequestWords.mayHold).
type valueChange struct {
	// moments says the value holds an instant in either run: it is an
	// instant leaf, or a form of its text holds a moment in a layout keploy
	// reads (apartFromMoments).
	moments bool
	// moved are the words that changed (movedOf) and are no part of an
	// instant the value holds.
	moved []string
	// marks says the value changed outside its instants where no word of
	// record's full round did: a mark between two words, or a word it held
	// as recorded and no longer does.
	marks bool
}

// changeOf is the change of a value from r to l (valueChange): what moved
// in it apart from the instants it holds, each read once for the test where
// dc keeps it (spansOf). An instant leaf is an instant whole: nothing of it
// is apart from one.
func (dc *DriftCache) changeOf(r, l Leaf) valueChange {
	rest := func(x Leaf) ([]string, bool) {
		if x.Type == Instant {
			return []string{""}, true
		}
		return dc.apartFromMoments(x.Value)
	}
	rec, rm := rest(r)
	live, lm := rest(l)
	c := valueChange{moments: rm || lm, moved: movedOf(rec, live)}
	c.marks = len(c.moved) == 0 && strings.Trim(rec[0], " ") != strings.Trim(live[0], " ")
	return c
}

// apartFromMoments are the forms of text (textForms, the first as written)
// with each instant a form holds (spansOf, a keyed one: a time-ordered
// UUID's time groups name no moment) cut out of it, one space in the place
// of it and of the spaces around it (so a value with an instant more reads
// as it does without it), and whether a form held one.
func (dc *DriftCache) apartFromMoments(text string) (forms []string, held bool) {
	forms = append(forms, textForms(text)...)
	for i, f := range forms {
		var b strings.Builder
		at := 0
		for _, sp := range dc.spansOf(f) {
			if !sp.keyed {
				continue
			}
			b.WriteString(strings.TrimRight(f[at:sp.start], " "))
			b.WriteByte(' ')
			for at = sp.end; at < len(f) && f[at] == ' '; at++ {
			}
			held = true
		}
		if b.Len() > 0 {
			forms[i] = b.String() + f[at:]
		}
	}
	return forms, held
}

// MayHoldChange reports whether the request may be the source of the change
// of a value that is no instant leaf from rec, as recorded, to live, as
// record's full round saw it (mayHold of its change).
func (w *RequestWords) MayHoldChange(rec, live string) bool {
	return w.mayHold((*DriftCache)(nil).changeOf(Leaf{Value: rec}, Leaf{Value: live}))
}

// mayHold reports whether the request may be the source of a value's change
// c: where the value holds an instant (c.moments), in any layout, and a
// form of the request held a token shaped as an instant (instantShaped);
// where it may hold a word of what changed apart from the value's instants
// (c.moved), as written or case-folded; and where the value changed outside
// its instants with no word gained (c.marks), a change no word tells apart
// from one the request made. So a read of the clock beside a word the
// request holds is its own where that word did not change, and the
// request's where it did. A nil or undecoded RequestWords
// may hold any change. A value the application made from the request's
// words by an offset from its clock ("in 5 days") has that read of the
// clock as its source, not the request.
func (w *RequestWords) mayHold(c valueChange) bool {
	if w == nil || w.undecoded {
		return true
	}
	if c.moments && w.instant {
		return true
	}
	for _, m := range c.moved {
		if w.f.has(m) || w.f.has(strings.ToLower(m)) {
			return true
		}
	}
	return c.marks
}

// movedOf are the words of live, the forms of a value as record's full
// round saw it (textForms, the first as written), that changed from rec,
// the forms of it as recorded: those between the words both begin and end
// with as written (movedWords), and each word of a form of live no form of
// rec holds.
func movedOf(rec, live []string) []string {
	_, out := movedWords(wordsBy(rec[0], isWord), wordsBy(live[0], isWord))
	held := map[string]bool{}
	for _, f := range rec {
		eachWord(f, func(w string) { held[w] = true })
	}
	for _, f := range live {
		eachWord(f, func(w string) {
			if !held[w] {
				out = append(out, w)
			}
		})
	}
	return out
}

// Words are the words of the place's request keploy could not read that it
// keeps (unreadWords; Tracker.IncomingWords): nil where it keeps none.
func (e Earlier) Words() *RequestWords { return e.unreadWords }

// unreadWordsIn says whether one of served is a client test's request keploy
// could not read that keeps its words (Earlier.unreadWords).
func unreadWordsIn(served []Earlier) bool {
	for _, e := range served {
		if e.unreadWords != nil {
			return true
		}
	}
	return false
}

// requestUnreadOf is requestUnread of served for one value's change from r
// to l: a client test's request keploy could not read whose words cannot
// hold the change (mayHold) is no source of it, and the value is read
// as it is where that request never came.
func (dc *DriftCache) requestUnreadOf(served []Earlier, r, l Leaf) string {
	var c valueChange
	read := false
	for _, e := range served {
		if e.unreadWords != nil {
			if !read {
				c, read = dc.changeOf(r, l), true
			}
			if !e.unreadWords.mayHold(c) {
				continue
			}
		}
		if why := requestUnread([]Earlier{e}); why != "" {
			return why
		}
	}
	return ""
}
