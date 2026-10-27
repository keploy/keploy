package learn

import (
	"container/list"
	"strings"
	"sync"
)

// textCache keeps, for one test (DriftCache), what the learn step derives
// of a text a rule reads more than once: its other declared forms
// (rawExtra), its instants (spansOf), its words (rawTokens), and a
// payload's text with its answer's clock reads blanked, with its words
// (searched, readTokens). It is keyed by the text, and keeps each text it
// is keyed by as a copy of its own: a field's text is a part of its
// payload's, so a key that was the field's own would keep the whole
// payload in memory for the test, whatever the store of payloads let go
// (bodyStore). What is derived of a text is read from that copy, so it
// keeps no other text either.
//
// What it keeps is within textBudget bytes (about: each copy's letters and
// what is derived of it), the text read least recently let go first, and a
// text it let go is derived again where a rule next reads it. A test's rules
// read every field of each earlier payload they search, so without a budget
// it kept a copy of all of them at once (612 MiB over 256 recordings of
// 1 MiB). What a text larger than the whole budget derives is handed to its
// reader and kept by none, letting go of nothing. A payload that is all of
// its recording's (Raw.id) is read through the set's cache instead
// (setDerived), by the recording, never by its text.
type textCache struct {
	mu     sync.Mutex
	budget int64
	used   int64
	order  *list.List // of *textEntry, the most recently read first
	by     map[textKey]*textEntry
}

// textKind is which of a text's derived forms an entry keeps.
type textKind uint8

const (
	textExtra textKind = iota
	textSpans
	textTokens
	textBlanked
	textBlankTok
)

type textKey struct {
	kind textKind
	text string
}

type textEntry struct {
	key  textKey
	val  any
	size int64
	at   *list.Element
}

// textBudget is the bytes a test's textCache keeps.
var textBudget int64 = 16 << 20

// instantBytes is about what an instant takes in a slice of them.
const instantBytes = 96

func newTextCache() *textCache {
	return &textCache{budget: textBudget, order: list.New(), by: map[textKey]*textEntry{}}
}

// get is the kept value of k, marked read most recently.
func (c *textCache) get(k textKey) (any, bool) {
	if c == nil {
		return nil, false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	e := c.by[k]
	if e == nil {
		return nil, false
	}
	c.order.MoveToFront(e.at)
	return e.val, true
}

// put keeps v, derived of k, of size bytes (k's letters counted here),
// unless another read kept k first, then lets go of the texts read least
// recently until the cache is within its budget. A value past the whole
// budget is kept by none and lets go of nothing.
func (c *textCache) put(k textKey, v any, size int64) {
	if c == nil {
		return
	}
	size += int64(len(k.text)) + headerBytes
	c.mu.Lock()
	defer c.mu.Unlock()
	if size > c.budget || c.by[k] != nil {
		return
	}
	e := &textEntry{key: k, val: v, size: size}
	e.at = c.order.PushFront(e)
	c.by[k] = e
	c.used += size
	for c.used > c.budget {
		last := c.order.Back()
		old := last.Value.(*textEntry)
		c.order.Remove(last)
		delete(c.by, old.key)
		c.used -= old.size
	}
}

// keptBytes is the bytes the cache counts as kept, for the rows.
func (c *textCache) keptBytes() int64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.used
}

// derivedOfText is derive(text), kept in dc's text cache by kind and a copy
// of text (textCache): derive reads the copy, so what it keeps holds no part
// of a longer text that text was cut from. size is about the bytes v holds
// past the copy's. A nil cache derives it now.
func derivedOfText[V any](dc *DriftCache, kind textKind, text string, derive func(own string) V, size func(V) int64) V {
	if dc == nil {
		return derive(text)
	}
	k := textKey{kind, text}
	if v, ok := dc.text.get(k); ok {
		return v.(V)
	}
	k.text = strings.Clone(text)
	v := derive(k.text)
	dc.text.put(k, v, size(v))
	return v
}

func stringBytes(s string) int64 { return int64(len(s)) }

func spansBytes(s []instant) int64 { return headerBytes + instantBytes*int64(len(s)) }

func tokensBytes(t []map[string]bool) int64 {
	n := int64(headerBytes)
	for _, words := range t {
		n += wordsBytes(words)
	}
	return n
}

func pairBytes(p [2]string) int64 { return int64(len(p[0]) + len(p[1])) }
