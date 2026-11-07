package mocknoise

import "strings"

// jsonScalars calls f with the path and the JSON text of each scalar of doc
// (a string with its quotes, a number, true, false or null), in the order doc
// holds them. doc must be valid JSON (json.Valid). The path is the one
// matcher.Flatten gives: the keys from the root to the scalar joined by '.',
// where an array adds no key; a key is named by its text between its quotes,
// escapes and all. f must not keep path past the call.
//
// Nothing is copied, decoded or kept: a document costs one pass over its text
// and a path as deep as its deepest key, however large it is, where a
// json.Unmarshal and a Flatten of it allocate about forty times its size.
func jsonScalars(doc string, f func(path []byte, text string)) {
	w := jsonWalker{doc: doc, f: f}
	w.value(0)
}

type jsonWalker struct {
	doc  string
	i    int
	path []byte
	f    func(path []byte, text string)
}

// value walks the value at w.i, which is keys keys deep in the document.
func (w *jsonWalker) value(keys int) {
	w.space()
	start := w.i
	switch w.doc[w.i] {
	case '{':
		w.i++
		n := len(w.path)
		for w.more('}') {
			key := w.str()
			w.space()
			w.i++ // the colon
			if keys > 0 {
				w.path = append(w.path, '.')
			}
			w.path = append(w.path, key...)
			w.value(keys + 1)
			w.path = w.path[:n]
		}
	case '[':
		w.i++
		for w.more(']') {
			w.value(keys)
		}
	case '"':
		w.str()
		w.f(w.path, w.doc[start:w.i])
	default: // a number, true, false or null
		for w.i < len(w.doc) && !strings.ContainsRune(" \t\r\n,]}", rune(w.doc[w.i])) {
			w.i++
		}
		w.f(w.path, w.doc[start:w.i])
	}
}

// more steps over the space and the comma after a member or an element, and
// the space before the next, and reports whether one follows; at the end of
// the object or array it steps over the closing bracket.
func (w *jsonWalker) more(end byte) bool {
	w.space()
	if w.doc[w.i] == ',' {
		w.i++
		w.space()
	}
	if w.doc[w.i] == end {
		w.i++
		return false
	}
	return true
}

// space steps over the space at w.i. In a valid document a value or a closing
// bracket follows every space walked, so it never runs past the end.
func (w *jsonWalker) space() {
	for strings.IndexByte(" \t\r\n", w.doc[w.i]) >= 0 {
		w.i++
	}
}

// str steps over the string at w.i and returns its text between the quotes.
func (w *jsonWalker) str() string {
	w.i++
	start := w.i
	for w.doc[w.i] != '"' {
		if w.doc[w.i] == '\\' {
			w.i++
		}
		w.i++
	}
	w.i++
	return w.doc[start : w.i-1]
}

// jsonStringText returns a string scalar's text between its quotes (jsonScalars'
// text), or "" for a number, true, false or null.
func jsonStringText(text string) string {
	if !strings.HasPrefix(text, `"`) {
		return ""
	}
	return text[1 : len(text)-1]
}
