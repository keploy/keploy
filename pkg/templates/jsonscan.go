package templates

import (
	"bytes"
	"encoding/json"
	"math/big"
	"regexp"
	"sort"
	"strconv"
	"strings"
)

// LeafKind is the JSON type of a leaf.
type LeafKind int

// The kinds of leaf a JSON document holds. LeafBare is a placeholder written
// outside any JSON string ({"n":{{int .n}}}), the form older templatize wrote
// for numbers; it makes the document invalid JSON until it is rendered.
const (
	LeafString LeafKind = iota
	LeafNumber
	LeafBool
	LeafNull
	LeafBare
)

// PathSeg is one step of a leaf's path: an object key, or an array index.
type PathSeg struct {
	Key     string
	Index   int
	IsIndex bool
}

// Leaf is one scalar of a JSON document, located by path and by byte span.
type Leaf struct {
	Path []PathSeg
	// Start and End are the leaf's byte span; a string's includes its quotes.
	Start, End int
	Kind       LeafKind
	// InArray marks a leaf with an array among its ancestors.
	InArray bool
	// Shadowed marks a leaf that is not the document's value at its path: a
	// key on its path occurs again later in the same object, and a reader
	// keeps a repeated key's LAST value (encoding/json, JSON.parse and
	// json.loads all do). {"id":"a","id":{"x":1}} has no value "a" at id.
	Shadowed bool
}

// Raw is the leaf's text in doc.
func (l Leaf) Raw(doc string) string { return doc[l.Start:l.End] }

// Value is the leaf's value as text: a string's decoded content, a number or
// literal as written, a bare placeholder as written.
func (l Leaf) Value(doc string) string {
	raw := l.Raw(doc)
	if l.Kind != LeafString {
		return raw
	}
	var s string
	if err := json.Unmarshal([]byte(raw), &s); err != nil {
		return raw[1 : len(raw)-1]
	}
	return s
}

// PathKey is the path in one comparable string.
func (l Leaf) PathKey() string { return pathKey(l.Path) }

// DisplayPath is the path as a person reads it: items[0].id.
func (l Leaf) DisplayPath() string {
	var b strings.Builder
	for i, s := range l.Path {
		if s.IsIndex {
			b.WriteString("[" + strconv.Itoa(s.Index) + "]")
			continue
		}
		if i > 0 {
			b.WriteByte('.')
		}
		b.WriteString(s.Key)
	}
	return b.String()
}

// Under is the leaf's place under prefix, as a person reads it:
// body.items[0].id, and body[4] (not body.[4]) for an element of a top-level
// array.
func (l Leaf) Under(prefix string) string {
	d := l.DisplayPath()
	if d == "" || strings.HasPrefix(d, "[") {
		return prefix + d
	}
	return prefix + "." + d
}

func pathKey(p []PathSeg) string {
	var b strings.Builder
	for _, s := range p {
		if s.IsIndex {
			b.WriteString("\x00i" + strconv.Itoa(s.Index))
		} else {
			b.WriteString("\x00k" + s.Key)
		}
	}
	return b.String()
}

// JSONLeaves returns the scalars of doc in document order, or false when doc
// is not one JSON document. A placeholder written outside a JSON string is
// read as a scalar, so a body of an older templatize still scans.
func JSONLeaves(doc string) ([]Leaf, bool) {
	s, ok := scanJSON(doc)
	if !ok {
		return nil, false
	}
	return s.leaves, true
}

// JSONPlaces returns the scalars and the object keys of doc, each in document
// order, from one read; false when doc is not one JSON document.
func JSONPlaces(doc string) ([]Leaf, []ObjectKey, bool) {
	d, ok := parseJSON(doc)
	if !ok {
		return nil, nil, false
	}
	return d.leaves, d.keys, true
}

// IsNumber reports whether s is one JSON number and nothing else.
func IsNumber(s string) bool {
	return s != "" && numberRe.FindString(s) == s
}

// ObjectKey is one object key of a JSON document: Path is the path of the
// value it names (its own name last), and Start and End span the key's JSON
// string, quotes included.
type ObjectKey struct {
	Path       []PathSeg
	Start, End int
	InArray    bool
	// Shadowed marks a key that a reader never sees: it, or a key on its
	// path, occurs again later in the same object (see Leaf.Shadowed).
	Shadowed bool
}

// Raw is the key's JSON string in doc.
func (k ObjectKey) Raw(doc string) string { return doc[k.Start:k.End] }

// DisplayPath is the path as a person reads it: items[0].id.
func (k ObjectKey) DisplayPath() string { return Leaf{Path: k.Path}.DisplayPath() }

// jsonDoc is one JSON document read once: its scalars and its object keys,
// each in document order.
type jsonDoc struct {
	leaves []Leaf
	keys   []ObjectKey
}

// keysTemplated reports whether an object key of doc holds a placeholder.
func (d *jsonDoc) keysTemplated(doc string) bool {
	for _, k := range d.keys {
		if Contains(k.Raw(doc)) {
			return true
		}
	}
	return false
}

// parseAnswer reads what an app answered as JSON exactly when encoding/json
// does (json.Valid, as the comparison reads it): a placeholder in an answer
// is the app's text, never a value written outside a string.
func parseAnswer(doc string) (*jsonDoc, bool) {
	if !json.Valid([]byte(doc)) {
		return nil, false
	}
	return parseJSON(doc)
}

// parseJSON reads doc in one pass, or false when it is not one JSON document.
// Without a placeholder, doc is one exactly when json.Valid says so; a
// placeholder written where a JSON value goes ({"n":{{int .n}}}) is read as
// that value.
func parseJSON(doc string) (*jsonDoc, bool) {
	s, ok := scanJSON(doc)
	if !ok {
		return nil, false
	}
	return &jsonDoc{leaves: s.leaves, keys: s.keys}, true
}

func scanJSON(doc string) (*scanner, bool) {
	s := &scanner{doc: doc}
	s.ws()
	if s.i >= len(s.doc) {
		return nil, false
	}
	if !s.value(nil, false) {
		return nil, false
	}
	s.ws()
	if s.i != len(s.doc) {
		return nil, false
	}
	// A document that is one placeholder and nothing else is text, not JSON:
	// {{string .id}} alone stands for the value as written (usr_1), never
	// for a JSON string ("usr_1"). Only inside a JSON document is a
	// placeholder outside quotes a whole JSON value ({"n":{{int .n}}}).
	if len(s.leaves) == 1 && s.leaves[0].Kind == LeafBare && len(s.leaves[0].Path) == 0 {
		return nil, false
	}
	s.markShadowed()
	return s, true
}

type scanner struct {
	doc    string
	i      int
	leaves []Leaf
	keys   []ObjectKey
	depth  int
	// replaced is the byte span, key to the end of its value, of each object
	// member a later member of the same object and name replaces.
	replaced []byteSpan
}

type byteSpan struct{ start, end int }

// markShadowed marks every leaf and key that lies inside a replaced member
// (Leaf.Shadowed). Members nest or are disjoint, so the replaced spans merge
// into disjoint sorted spans, and each place is found by binary search.
func (s *scanner) markShadowed() {
	if len(s.replaced) == 0 {
		return
	}
	sort.Slice(s.replaced, func(i, j int) bool { return s.replaced[i].start < s.replaced[j].start })
	merged := s.replaced[:1]
	for _, sp := range s.replaced[1:] {
		if last := &merged[len(merged)-1]; sp.start < last.end {
			if sp.end > last.end {
				last.end = sp.end
			}
			continue
		}
		merged = append(merged, sp)
	}
	inside := func(start, end int) bool {
		i := sort.Search(len(merged), func(i int) bool { return merged[i].end >= end })
		return i < len(merged) && merged[i].start <= start
	}
	for i := range s.leaves {
		s.leaves[i].Shadowed = inside(s.leaves[i].Start, s.leaves[i].End)
	}
	for i := range s.keys {
		s.keys[i].Shadowed = inside(s.keys[i].Start, s.keys[i].End)
	}
}

// maxDepth bounds the recursion on a hostile body: the nesting
// encoding/json accepts (its maxNestingDepth), and no more.
const maxDepth = 10000

var numberRe = regexp.MustCompile(`^-?(?:0|[1-9][0-9]*)(?:\.[0-9]+)?(?:[eE][+-]?[0-9]+)?`)

func (s *scanner) ws() {
	for s.i < len(s.doc) {
		switch s.doc[s.i] {
		case ' ', '\t', '\n', '\r':
			s.i++
		default:
			return
		}
	}
}

func (s *scanner) value(path []PathSeg, inArray bool) bool {
	if s.i >= len(s.doc) {
		return false
	}
	switch c := s.doc[s.i]; {
	case c == '{':
		if m := anchoredRe.FindStringIndex(s.doc[s.i:]); m != nil {
			s.leaf(path, inArray, s.i+m[1], LeafBare)
			return true
		}
		return s.object(path, inArray)
	case c == '[':
		return s.array(path)
	case c == '"':
		end, ok := stringEnd(s.doc, s.i)
		if !ok {
			return false
		}
		s.leaf(path, inArray, end, LeafString)
		return true
	case c == '-' || (c >= '0' && c <= '9'):
		m := numberRe.FindStringIndex(s.doc[s.i:])
		if m == nil {
			return false
		}
		s.leaf(path, inArray, s.i+m[1], LeafNumber)
		return true
	case strings.HasPrefix(s.doc[s.i:], "true"):
		s.leaf(path, inArray, s.i+4, LeafBool)
		return true
	case strings.HasPrefix(s.doc[s.i:], "false"):
		s.leaf(path, inArray, s.i+5, LeafBool)
		return true
	case strings.HasPrefix(s.doc[s.i:], "null"):
		s.leaf(path, inArray, s.i+4, LeafNull)
		return true
	}
	return false
}

func (s *scanner) leaf(path []PathSeg, inArray bool, end int, kind LeafKind) {
	p := make([]PathSeg, len(path))
	copy(p, path)
	s.leaves = append(s.leaves, Leaf{Path: p, Start: s.i, End: end, Kind: kind, InArray: inArray})
	s.i = end
}

func (s *scanner) object(path []PathSeg, inArray bool) bool {
	if s.depth++; s.depth > maxDepth {
		return false
	}
	defer func() { s.depth-- }()
	s.i++ // {
	s.ws()
	if s.i < len(s.doc) && s.doc[s.i] == '}' {
		s.i++
		return true
	}
	// members is each name's latest member in this object, key to value.
	members := map[string]byteSpan{}
	for {
		s.ws()
		if s.i >= len(s.doc) || s.doc[s.i] != '"' {
			return false
		}
		keyStart := s.i
		end, ok := stringEnd(s.doc, s.i)
		if !ok {
			return false
		}
		var key string
		if err := json.Unmarshal([]byte(s.doc[s.i:end]), &key); err != nil {
			return false
		}
		kp := make([]PathSeg, len(path), len(path)+1)
		copy(kp, path)
		s.keys = append(s.keys, ObjectKey{Path: append(kp, PathSeg{Key: key}), Start: s.i, End: end, InArray: inArray})
		s.i = end
		s.ws()
		if s.i >= len(s.doc) || s.doc[s.i] != ':' {
			return false
		}
		s.i++
		s.ws()
		if !s.value(append(path, PathSeg{Key: key}), inArray) {
			return false
		}
		// A later member of the same name replaces this object's earlier one.
		if prev, dup := members[key]; dup {
			s.replaced = append(s.replaced, prev)
		}
		members[key] = byteSpan{keyStart, s.i}
		s.ws()
		if s.i >= len(s.doc) {
			return false
		}
		switch s.doc[s.i] {
		case ',':
			s.i++
		case '}':
			s.i++
			return true
		default:
			return false
		}
	}
}

func (s *scanner) array(path []PathSeg) bool {
	if s.depth++; s.depth > maxDepth {
		return false
	}
	defer func() { s.depth-- }()
	s.i++ // [
	s.ws()
	if s.i < len(s.doc) && s.doc[s.i] == ']' {
		s.i++
		return true
	}
	for idx := 0; ; idx++ {
		s.ws()
		if !s.value(append(path, PathSeg{Index: idx, IsIndex: true}), true) {
			return false
		}
		s.ws()
		if s.i >= len(s.doc) {
			return false
		}
		switch s.doc[s.i] {
		case ',':
			s.i++
		case ']':
			s.i++
			return true
		default:
			return false
		}
	}
}

// stringEnd returns the index just past the JSON string starting at doc[i],
// or false where encoding/json rejects the string: a byte below 0x20
// unescaped (a TAB, a newline), an escape other than \" \\ \/ \b \f \n \r \t
// or \u and four hex digits, or no closing quote. As encoding/json, it does
// not reject a byte that is not UTF-8.
func stringEnd(doc string, i int) (int, bool) {
	for j := i + 1; j < len(doc); j++ {
		c := doc[j]
		switch {
		case c == '"':
			return j + 1, true
		case c < 0x20:
			return 0, false
		case c == '\\':
			if j+1 >= len(doc) {
				return 0, false
			}
			switch doc[j+1] {
			case '"', '\\', '/', 'b', 'f', 'n', 'r', 't':
				j++
			case 'u':
				if j+6 > len(doc) || !isHex4(doc[j+2:j+6]) {
					return 0, false
				}
				j += 5
			default:
				return 0, false
			}
		}
	}
	return 0, false
}

func isHex4(s string) bool {
	for i := 0; i < len(s); i++ {
		c := s[i]
		if !('0' <= c && c <= '9' || 'a' <= c && c <= 'f' || 'A' <= c && c <= 'F') {
			return false
		}
	}
	return true
}

// jsonEscape is s as it goes between the quotes of a JSON string, with no
// HTML escaping: what was recorded as <a&b> stays readable.
func jsonEscape(s string) string {
	var b bytes.Buffer
	enc := json.NewEncoder(&b)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(s) // a string always encodes
	out := strings.TrimSuffix(b.String(), "\n")
	return out[1 : len(out)-1]
}

// leafIndex finds a document's leaves by path: only a leaf that is the
// document's value at its path (not Shadowed). Of a key an object repeats,
// the last is the one at its path, its whole value: encoding/json, JSON.parse
// and json.loads all keep the last value of a repeated key, so in
// {"id":"a","id":{"x":1}} no scalar is at id.
func leafIndex(leaves []Leaf) map[string]Leaf {
	idx := make(map[string]Leaf, len(leaves))
	for _, l := range leaves {
		if !l.Shadowed {
			idx[l.PathKey()] = l
		}
	}
	return idx
}

// jsonValueEqual reports whether two JSON scalars written as raw text are the
// same value: strings by content, numbers by their exact value (1.50 is 1.5,
// 1e2 is 100, and no digit of 123456789012345678901 or 0.1000000000000000000001
// is lost), a string never a number.
func jsonValueEqual(a, b string) bool {
	if a == b {
		return true
	}
	var av, bv interface{}
	da := json.NewDecoder(strings.NewReader(a))
	da.UseNumber()
	db := json.NewDecoder(strings.NewReader(b))
	db.UseNumber()
	if da.Decode(&av) != nil || db.Decode(&bv) != nil {
		return false
	}
	an, aNum := av.(json.Number)
	bn, bNum := bv.(json.Number)
	if aNum && bNum {
		x, okA := new(big.Rat).SetString(an.String())
		y, okB := new(big.Rat).SetString(bn.String())
		return okA && okB && x.Cmp(y) == 0
	}
	return av == bv
}
