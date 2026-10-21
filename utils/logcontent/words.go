package logcontent

import (
	"errors"
	"sort"
	"strings"
	"sync"
	"syscall"
	"unicode"
)

// An error keploy makes is a chain: fmt.Errorf("failed to run test set %s:
// %w", id, err) around fmt.Errorf("failed to decode the mock: %w", err)
// around a parser's error, which quotes what it could not read. With the
// switch off, Error logs the chain by its kind and its text's length, and by
// its shape: its text read against the constant formats keploy's code makes
// errors with and the constant texts it makes them of (errorFormats and
// errorTexts, which the guard in tests/logcontent writes from keploy's
// source), each value a format formats written {}, but the error it wraps
// (%w), whose text is read so in turn. Only those formats' and texts' own
// words, and {}, are written, so whatever fills a value is not, the
// application's content included; and a text, or the part of one, that no
// format of keploy's makes is {}: "failed to run test set {}: failed to
// decode the mock: {}". A chain whose outermost text no format makes has no
// shape.
//
// So a line keeps keploy's own words for what failed where, which is what a
// user reads a failed run's console for, and drops only what the words were
// about. The words are those of the keploy that wrote the table: an error a
// format added since makes {} until it is written again.

// shapeDepth bounds how many errors of a chain a shape reads, and
// shapeBudget how many texts one shape reads against the formats at all: a
// line's encoding costs a bounded time, whatever its error.
const (
	shapeDepth  = 32
	shapeBudget = 256
)

// errorShape is err's shape, or "" when keploy's words make none of it; text
// is err's text, which a line has formatted once already.
func errorShape(err error, text string) (shape string) {
	// A line is never lost to its shape: an error's Error that panics as
	// the chain is read leaves the line without one.
	defer func() {
		if recover() != nil {
			shape = ""
		}
	}()
	if err == nil {
		return ""
	}
	sh := shaper{w: loadWords(), budget: shapeBudget}
	shape, _ = sh.of(err, text, 0)
	return shape
}

// textShape is text's shape, a text no error carries (a panic's string), or
// "" when keploy's words make none of it.
func textShape(text string) (shape string) {
	defer func() {
		if recover() != nil {
			shape = ""
		}
	}()
	sh := shaper{w: loadWords(), budget: shapeBudget}
	if s, ok := sh.of(nil, text, 0); ok {
		return s
	}
	return ""
}

// panicErrorShape is err's shape (errorShape), its text read once here: an
// Error that panics as it is read has none.
func panicErrorShape(err error) (shape string) {
	defer func() {
		if recover() != nil {
			shape = ""
		}
	}()
	return errorShape(err, err.Error())
}

// shaper reads one error's shape: keploy's words, and what is left of the
// texts it may read against them.
type shaper struct {
	w      *words
	budget int
}

// shapeOf is the shape of text, err's text (err nil for a text a format
// formatted with %s or %v, which no Unwrap reaches), and false when no format
// or text of keploy's makes it. A format of no words of its own ("%w: %s")
// makes a shape only through what it wraps or formats last: one that writes no
// letter says nothing, and is none.
func (sh *shaper) of(err error, text string, depth int) (string, bool) {
	if depth >= shapeDepth || sh.budget <= 0 {
		return "", false
	}
	sh.budget--
	w := sh.w
	if w.texts[text] {
		return text, true
	}
	if err != nil {
		// The standard library's sentinels and the kernel's errnos are text
		// the application did not write either.
		for _, s := range sentinels {
			if err == s.err {
				return text, true
			}
		}
		if _, ok := err.(syscall.Errno); ok {
			return text, true
		}
	}
	var inner error
	innerText := ""
	if err != nil {
		if inner = errors.Unwrap(err); inner != nil {
			innerText = inner.Error()
		}
	}
	for k := len(w.firstLens) - 1; k >= 0; k-- {
		n := w.firstLens[k]
		if n > len(text) {
			continue
		}
		for _, f := range w.byFirst[text[:n]] {
			// A text a format formatted is read against the formats of
			// words of their own: one of none would read it whole again.
			if err == nil && f.weak {
				continue
			}
			if s, ok := sh.shape(f, text, inner, innerText, depth); ok {
				return s, true
			}
		}
	}
	return "", false
}

// shape is text's shape as f makes it, wrapping inner (whose text is
// innerText) at its %w, if f makes text.
func (sh *shaper) shape(f errorFormat, text string, inner error, innerText string, depth int) (string, bool) {
	if f.wrap < 0 {
		h, ok := holes(text, f.chunks)
		if !ok {
			return "", false
		}
		return f.worded(sh.render(f, h, "", depth))
	}
	if inner == nil {
		return "", false
	}
	// The text is what is before the error it wraps, its text, and what is
	// after; the error's text may be found in it more than once.
	for at := strings.Index(text, innerText); at >= 0; {
		before, okB := holes(text[:at], f.chunks[:f.wrap+1])
		after, okA := holes(text[at+len(innerText):], f.chunks[f.wrap+1:])
		if okB && okA {
			in, ok := sh.of(inner, innerText, depth+1)
			if !ok {
				in = "{}"
			}
			return f.worded(sh.render(f, append(append(before, ""), after...), in, depth))
		}
		if at >= len(text) {
			break
		}
		next := strings.Index(text[at+1:], innerText)
		if next < 0 {
			break
		}
		at += 1 + next
	}
	return "", false
}

// worded is s, a shape f rendered, and whether it is one: a format of words of
// its own always says something, one of none only when what it wraps or
// formats does.
func (f errorFormat) worded(s string) (string, bool) {
	if !f.weak || hasLetter([]string{s}) {
		return s, true
	}
	return "", false
}

// render writes f's words with its values between them: the error it wraps
// (%w) as inner, its last value, when the format ends with it and formats it
// as a text (%s, %v), as that text's own shape, and every other value {}.
func (sh *shaper) render(f errorFormat, h []string, inner string, depth int) string {
	var b strings.Builder
	for i, c := range f.chunks {
		b.WriteString(c)
		if i == len(f.chunks)-1 {
			break
		}
		switch {
		case i == f.wrap:
			b.WriteString(inner)
		case i == len(f.chunks)-2 && f.chunks[i+1] == "" && (f.verbs[i] == 's' || f.verbs[i] == 'v'):
			if s, ok := sh.of(nil, h[i], depth+1); ok {
				b.WriteString(s)
			} else {
				b.WriteString("{}")
			}
		default:
			b.WriteString("{}")
		}
	}
	return b.String()
}

// holes are the values s holds between chunks' words, when s is chunks'
// words with anything between each two: it starts with the first, ends with
// the last, and holds the others in order between, each placed leftmost. A
// value may hold anything, so the leftmost place each middle chunk is found
// in is as good as any.
func holes(s string, chunks []string) ([]string, bool) {
	if len(chunks) == 1 {
		return nil, s == chunks[0]
	}
	first, last := chunks[0], chunks[len(chunks)-1]
	if len(s) < len(first)+len(last) || !strings.HasPrefix(s, first) || !strings.HasSuffix(s, last) {
		return nil, false
	}
	mid := s[len(first) : len(s)-len(last)]
	out := make([]string, 0, len(chunks)-1)
	for _, c := range chunks[1 : len(chunks)-1] {
		i := strings.Index(mid, c)
		if i < 0 {
			return nil, false
		}
		out = append(out, mid[:i])
		mid = mid[i+len(c):]
	}
	return append(out, mid), true
}

// errorFormat is one of keploy's error formats, read: its words, with a value
// between each two (the verb that formats it, verbs), and which of the values
// is the error it wraps (%w), -1 for none; weak: it has no words of its own
// ("%w: %s"), so it makes a shape only through what it holds.
type errorFormat struct {
	chunks []string
	verbs  []byte
	wrap   int
	weak   bool
}

// parseFormat reads a format of fmt's into its words and its values: each
// verb, with its flags, width and precision, is a value; %% is a word's %.
// One with more than one %w, or with an argument index or a * (a value
// read for a width), is none of keploy's shapes.
func parseFormat(s string) (errorFormat, bool) {
	f := errorFormat{wrap: -1}
	var cur strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] != '%' {
			cur.WriteByte(s[i])
			continue
		}
		i++
		for i < len(s) && strings.IndexByte("+-# 0123456789.", s[i]) >= 0 {
			i++
		}
		if i >= len(s) {
			return f, false
		}
		switch s[i] {
		case '%':
			cur.WriteByte('%')
			continue
		case '[', '*':
			return f, false
		case 'w':
			if f.wrap >= 0 {
				return f, false
			}
			f.wrap = len(f.chunks)
		}
		f.chunks = append(f.chunks, cur.String())
		f.verbs = append(f.verbs, s[i])
		cur.Reset()
	}
	f.chunks = append(f.chunks, cur.String())
	return f, true
}

// words are errorFormats and errorTexts, read once, when a line first needs
// a shape: the formats by their first word, and the lengths those first
// words have, shortest first, which a text is read against longest first.
type words struct {
	texts     map[string]bool
	byFirst   map[string][]errorFormat
	firstLens []int
}

var (
	wordsOnce sync.Once
	wordsRead words
)

func loadWords() *words {
	wordsOnce.Do(func() { wordsRead = readWords(errorFormats, errorTexts) })
	return &wordsRead
}

func readWords(formats, texts []string) words {
	w := words{texts: make(map[string]bool, len(texts)), byFirst: map[string][]errorFormat{}}
	for _, t := range texts {
		w.texts[t] = true
	}
	lens := map[int]bool{}
	for _, s := range formats {
		f, ok := parseFormat(s)
		if !ok {
			continue
		}
		f.weak = !hasLetter(f.chunks) // "%w: %s": says only what it holds says
		w.byFirst[f.chunks[0]] = append(w.byFirst[f.chunks[0]], f)
		lens[len(f.chunks[0])] = true
	}
	for n := range lens {
		w.firstLens = append(w.firstLens, n)
	}
	sort.Ints(w.firstLens)
	// The formats that say most come first: of two a text matches, the one
	// with more words, and so fewer values, is the one that made it, or
	// says no more than it.
	for k, fs := range w.byFirst {
		sort.SliceStable(fs, func(i, j int) bool { return wordLen(fs[i]) > wordLen(fs[j]) })
		w.byFirst[k] = fs
	}
	return w
}

// hasLetter: one of chunks holds a letter.
func hasLetter(chunks []string) bool {
	for _, c := range chunks {
		for _, r := range c {
			if unicode.IsLetter(r) {
				return true
			}
		}
	}
	return false
}

func wordLen(f errorFormat) int {
	n := 0
	for _, c := range f.chunks {
		n += len(c)
	}
	return n
}
