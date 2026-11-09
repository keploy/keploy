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
	// digit says a form of the text held a digit, which an instant in any
	// layout holds (MayHoldChange).
	digit bool
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
// byte no form runs across (a space or a mark that is no part of a word, a
// run of the base64 alphabet, an escape, a quoted-printable one, a line
// break of wrapped base64, a JWT's dot or a path's slash): 0 where there is
// none. The byte before it may start an escape: '%' and '=' go on in a hex
// digit, a line break or a backslash, and '\' in a letter, '/', a backslash
// or '"', the only one of these it cuts before, and "\"" reads as a space
// (unescaped), a separator, so each side reads as it does whole.
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
	return c < 0x80 && !isB64Byte(c) && !strings.ContainsRune("=\\%.\r\n", rune(c)) && (c == ' ' || c == '\t' || c > ' ' && c < 0x7f)
}

// read adds the words of each form of text, one window (textForms).
func (w *RequestWords) read(text string) {
	for _, form := range textForms(text) {
		eachWord(form, func(word string) {
			w.f.add(word)
			if !w.digit && strings.ContainsAny(word, "0123456789") {
				w.digit = true
			}
		})
	}
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

// MayHoldChange reports whether the request may be the source of a value's
// change from rec, as recorded, to live, as record's full round saw it:
// where the value holds an instant (moments: an instant leaf, or a text
// that holds a moment in a layout keploy reads), in any layout, where a
// form of the request held a digit; else where it may hold a word of what
// changed (movedOf), as written or case-folded, and where no word
// changed. A nil or undecoded RequestWords may hold any change.
func (w *RequestWords) MayHoldChange(rec, live string, moments bool) bool {
	if w == nil || w.undecoded {
		return true
	}
	if moments {
		return w.digit
	}
	moved := movedOf(rec, live)
	if len(moved) == 0 {
		return true
	}
	for _, m := range moved {
		if w.f.has(m) || w.f.has(strings.ToLower(m)) {
			return true
		}
	}
	return false
}

// movedOf are the words of live, a value as record's full round saw
// it, that changed from rec, as recorded: those between the words both
// begin and end with (movedWords), and each word of a form of live
// (textForms) no form of rec holds.
func movedOf(rec, live string) []string {
	_, out := movedWords(wordsBy(rec, isWord), wordsBy(live, isWord))
	held := map[string]bool{}
	for _, f := range textForms(rec) {
		eachWord(f, func(w string) { held[w] = true })
	}
	for _, f := range textForms(live) {
		eachWord(f, func(w string) {
			if !held[w] {
				out = append(out, w)
			}
		})
	}
	return out
}

// momentsIn says whether a value's change from r to l is of an instant: an
// instant leaf, or a text either run of which holds a moment in a layout
// keploy reads (heldIn), which a request may hold in another layout.
func (dc *DriftCache) momentsIn(r, l Leaf) bool {
	if r.Type == Instant || l.Type == Instant {
		return true
	}
	for _, s := range []string{r.Value, l.Value} {
		var h heldMoments
		if dc.heldIn(&h, s, dc.rawExtra(s)); len(h.list) > 0 {
			return true
		}
	}
	return false
}

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
// hold the change (MayHoldChange) is no source of it, and the value is read
// as it is where that request never came.
func (dc *DriftCache) requestUnreadOf(served []Earlier, r, l Leaf) string {
	moments, read := false, false
	for _, e := range served {
		if e.unreadWords != nil {
			if !read {
				moments, read = dc.momentsIn(r, l), true
			}
			if !e.unreadWords.MayHoldChange(r.Value, l.Value, moments) {
				continue
			}
		}
		if why := requestUnread([]Earlier{e}); why != "" {
			return why
		}
	}
	return ""
}
