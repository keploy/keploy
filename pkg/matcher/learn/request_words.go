package learn

// RequestWords is a filter of the words of a client test's request whose
// body keploy could not read whole (Tracker.Incoming's unread: a body
// larger than the ingress reads, one of an encoding a recording keeps as its
// client sent it): each word of letters and digits of the body, as the
// application reads it (decoded), and each in lower case, as the search
// reads a text's case-folded form beside it (textForms), read as it
// streams past and never kept
// (a Bloom filter, wordFilter, of a fixed size, requestWordsBytes). It holds
// every word written to it, and a word that was not, about once in a
// hundred for a body of up to about a hundred thousand distinct words and
// lower-case forms of them, more often for a larger one: a word it holds that was not written costs a
// refusal (the request may be the source of the value), never a value
// learned from a request that holds it. A nil RequestWords may hold any
// word.
type RequestWords struct {
	f wordFilter
	// h is the FNV-1a state (filterHash) of the word being read, and hl of
	// it in lower case; in whether one is.
	h, hl uint64
	in    bool
}

// requestWordsBytes is the size of every request's filter: 1 Mi bits, ten
// for each of about a hundred thousand words and lower-case forms.
const requestWordsBytes = 128 << 10

// NewRequestWords is an empty filter of a request's words.
func NewRequestWords() *RequestWords {
	return &RequestWords{f: make(wordFilter, requestWordsBytes/8)}
}

// Write adds each word of p, a piece of the text in the order it comes: a
// word p ends without a byte that is no letter or digit goes on in the next
// Write (End ends it). It never fails.
func (w *RequestWords) Write(p []byte) (int, error) {
	for _, c := range p {
		if !isWord(c) {
			w.End()
			continue
		}
		if !w.in {
			w.h, w.hl, w.in = fnvOffset, fnvOffset, true
		}
		w.h ^= uint64(c)
		w.h *= fnvPrime
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		w.hl ^= uint64(c)
		w.hl *= fnvPrime
	}
	return len(p), nil
}

// WriteText adds each word of s as a text of its own: a word that s begins
// or ends with is not joined to the words written before or after it.
func (w *RequestWords) WriteText(s string) {
	w.End()
	_, _ = w.Write([]byte(s))
	w.End()
}

// End ends the word being read, if any, adding it.
func (w *RequestWords) End() {
	if w.in {
		set := func(word int, bit uint64) bool { w.f[word] |= bit; return true }
		w.f.bitsOf(spreadHash(w.h), set)
		w.f.bitsOf(spreadHash(w.hl), set)
		w.in = false
	}
}

// MayHold reports whether the request may hold v as a token (ContainsToken):
// each of v's runs of letters and digits (mayHold). A v of no letter or
// digit, and any v of a nil RequestWords, may.
func (w *RequestWords) MayHold(v string) bool {
	if w == nil {
		return true
	}
	return mayHold(w.f, wordsBy(v, isWord))
}
