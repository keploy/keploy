// Package variables is the replay side of a test's baked variables: the places
// of a dependency call that hold a variable's value, and the values a replay
// binds to them.
//
// A variable is a value the application makes anew on every run (an order id
// it mints, a token it signs) that flows from the place where it first appears
// into later dependency calls. The learn step writes it on the recorded mocks,
// which keep their recorded bytes (models.MockVariables):
//
//   - bind: the request place where the value first appears. Its live value is
//     the variable's value for the rest of the scope; nothing else is asserted
//     about it.
//   - use: a request place that must hold the variable's value. Any other
//     value is a mismatch: the call is not answered and its test fails, with
//     both values as evidence.
//   - answer: a response place that holds the variable's value. It is served
//     with the value the variable has this run.
//
// A variable is never inferred from what a value looks like: only the places
// written on the mocks are followed.
package variables

import (
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/templates"
)

// Part is the part of a call a place is in.
type Part int

// The parts a place can name. A place is always one whole value: a JSON
// scalar, one path segment, one query value, one header value.
const (
	// PartBody is a scalar of a JSON body, named by its path.
	PartBody Part = iota + 1
	// PartPath is one segment of the URL path, counted from 0 after the
	// leading slash, read decoded.
	PartPath
	// PartQuery is the value of one query parameter, read decoded.
	PartQuery
	// PartHeader is the whole value of one header.
	PartHeader
)

// Place is where in a call a variable's value is.
type Place struct {
	Part Part
	// Path is the JSON path of a PartBody place.
	Path []templates.PathSeg
	// Index is the segment of a PartPath place.
	Index int
	// Name is the parameter of a PartQuery place, or the header of a
	// PartHeader place.
	Name string
}

// The prefixes of the place grammar.
const (
	bodyPrefix   = "body"
	pathPrefix   = "url.path["
	queryPrefix  = "url.query."
	headerPrefix = "header."
)

// ErrPlace marks a place that is not in the grammar.
var ErrPlace = errors.New("not a variable place")

// Parse reads a place:
//
//	body.items[0].id      a scalar of a JSON body (["a.b"] for a key holding . [ ] or ")
//	url.path[2]           the third segment of the URL path
//	url.query.page        the value of the query parameter page
//	header.X-Request-Id   the value of a header (any case)
//
// A query or header name writes '|' and '#', which a word address
// (ParseAddress) reads as its own, as \| and \#, and a '\' before one of
// them, before another '\' or at its end as \\ (escapeName); any other
// '\' is itself. A JSON key holding '|' or '#' is quoted, as one holding '.'.
// A name written before the escape reads here as it did, but for a "\|",
// "\#" or "\\" in it: no header name holds one ('\' is not a token byte),
// and a query name that did now reads with that '\' dropped. A place of a
// mock's variables block is read with ReadPlace: as Parse reads it where the
// recording holds it, else as a word address (ParseAddress, which reads a
// name ending in #<digits> as a word of it).
func Parse(s string) (Place, error) {
	switch {
	case s == bodyPrefix:
		return Place{}, fmt.Errorf("%w: %q names the whole body; name one of its scalars (body.<path>)", ErrPlace, s)
	case strings.HasPrefix(s, bodyPrefix+".") || strings.HasPrefix(s, bodyPrefix+"["):
		path, err := parseJSONPath(s[len(bodyPrefix):])
		if err != nil {
			return Place{}, fmt.Errorf("%w: %q: %v", ErrPlace, s, err)
		}
		return Place{Part: PartBody, Path: path}, nil
	case strings.HasPrefix(s, pathPrefix) && strings.HasSuffix(s, "]"):
		n, err := strconv.Atoi(s[len(pathPrefix) : len(s)-1])
		if err != nil || n < 0 || strconv.Itoa(n) != s[len(pathPrefix):len(s)-1] {
			return Place{}, fmt.Errorf("%w: %q: the segment is a number from 0", ErrPlace, s)
		}
		return Place{Part: PartPath, Index: n}, nil
	case strings.HasPrefix(s, queryPrefix) && len(s) > len(queryPrefix):
		return Place{Part: PartQuery, Name: unescapeName(s[len(queryPrefix):])}, nil
	case strings.HasPrefix(s, headerPrefix) && len(s) > len(headerPrefix):
		return Place{Part: PartHeader, Name: unescapeName(s[len(headerPrefix):])}, nil
	}
	return Place{}, fmt.Errorf("%w: %q (want body.<path>, url.path[<n>], url.query.<name> or header.<name>)", ErrPlace, s)
}

// String is the place as Parse reads it.
func (p Place) String() string {
	switch p.Part {
	case PartBody:
		return bodyPrefix + formatJSONPath(p.Path)
	case PartPath:
		return pathPrefix + strconv.Itoa(p.Index) + "]"
	case PartQuery:
		return queryPrefix + escapeName(p.Name)
	case PartHeader:
		return headerPrefix + escapeName(p.Name)
	}
	return ""
}

// escapeName writes a name so that a '|' or '#' in it never ends it in a
// word address: \| and \#, and \\ for a '\' before one of them, before
// another '\' or at the end. A name with none of them is itself.
func escapeName(n string) string {
	if !strings.ContainsAny(n, `|#\`) {
		return n
	}
	var b strings.Builder
	for i := 0; i < len(n); i++ {
		if c := n[i]; c == '|' || c == '#' || c == '\\' && (i+1 == len(n) || strings.IndexByte(`|#\`, n[i+1]) >= 0) {
			b.WriteByte('\\')
		}
		b.WriteByte(n[i])
	}
	return b.String()
}

// unescapeName reads what escapeName wrote: \|, \# and \\ are the byte
// after the '\'; any other '\' is itself.
func unescapeName(s string) string {
	if !strings.Contains(s, `\`) {
		return s
	}
	var b strings.Builder
	for i := 0; i < len(s); i++ {
		if s[i] == '\\' && i+1 < len(s) && strings.IndexByte(`|#\`, s[i+1]) >= 0 {
			i++
		}
		b.WriteByte(s[i])
	}
	return b.String()
}

// BodyPlace is the place of a scalar of a JSON body.
func BodyPlace(path []templates.PathSeg) Place {
	return Place{Part: PartBody, Path: append([]templates.PathSeg(nil), path...)}
}

// parseJSONPath reads .key, [n] and ["key"] steps, at least one.
func parseJSONPath(s string) ([]templates.PathSeg, error) {
	var out []templates.PathSeg
	for i := 0; i < len(s); {
		switch s[i] {
		case '.':
			j := i + 1
			for j < len(s) && s[j] != '.' && s[j] != '[' {
				j++
			}
			key := s[i+1 : j]
			if key == "" || strings.ContainsAny(key, `]"`) {
				return nil, fmt.Errorf("an empty or unquoted key at byte %d (write [\"...\"] for a key holding . [ ] or \")", i)
			}
			out = append(out, templates.PathSeg{Key: key})
			i = j
		case '[':
			end := strings.IndexByte(s[i:], ']')
			if i+1 < len(s) && s[i+1] == '"' {
				var key string
				dec := json.NewDecoder(strings.NewReader(s[i+1:]))
				if err := dec.Decode(&key); err != nil {
					return nil, fmt.Errorf("a quoted key at byte %d: %v", i, err)
				}
				end = i + 1 + int(dec.InputOffset())
				if end >= len(s) || s[end] != ']' {
					return nil, fmt.Errorf("a quoted key at byte %d is not closed by ]", i)
				}
				out = append(out, templates.PathSeg{Key: key})
				i = end + 1
				continue
			}
			if end < 0 {
				return nil, fmt.Errorf("[ at byte %d is not closed", i)
			}
			raw := s[i+1 : i+end]
			n, err := strconv.Atoi(raw)
			if err != nil || n < 0 || strconv.Itoa(n) != raw {
				return nil, fmt.Errorf("[%s] at byte %d is not an index (a number from 0) or a quoted key", raw, i)
			}
			out = append(out, templates.PathSeg{Index: n, IsIndex: true})
			i += end + 1
		default:
			return nil, fmt.Errorf("byte %d: want . or [", i)
		}
	}
	if len(out) == 0 {
		return nil, errors.New("no path")
	}
	return out, nil
}

// formatJSONPath writes a path as parseJSONPath reads it.
func formatJSONPath(path []templates.PathSeg) string {
	var b strings.Builder
	for _, s := range path {
		switch {
		case s.IsIndex:
			b.WriteString("[" + strconv.Itoa(s.Index) + "]")
		case s.Key == "" || strings.ContainsAny(s.Key, `.[]"|#`):
			q, _ := json.Marshal(s.Key)
			b.WriteString("[" + string(q) + "]")
		default:
			b.WriteString("." + s.Key)
		}
	}
	return b.String()
}

// samePath reports whether two JSON paths are the same steps.
func samePath(a, b []templates.PathSeg) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}
