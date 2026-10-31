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
	// PartHeader is the whole value of one header, or one claim of the JSON
	// Web Token it holds (Place.Claim).
	PartHeader
)

// JWTMark is what a header place's name gains for a claim of the JSON Web
// Token the header holds ("<scheme> <token>" or the token alone):
// header.<name>{jwt}.<claim path>. The claim is read decoded, as a JSON
// scalar of the token's claims; a replay checks it as any place, and puts the
// recorded claim back into the token's claims, whose signature the matcher
// compares by its form alone (a signature changes with every claim, and
// keploy holds no signer's key).
const JWTMark = "{jwt}"

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
	// Claim is the JSON path of a claim of the JSON Web Token a PartHeader
	// place's header holds (JWTMark), nil for the header's whole value.
	Claim []templates.PathSeg
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
//	header.Authorization{jwt}.sub
//	                      a claim of the JSON Web Token the header holds
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
		return Place{Part: PartQuery, Name: s[len(queryPrefix):]}, nil
	case strings.HasPrefix(s, headerPrefix) && len(s) > len(headerPrefix):
		name := s[len(headerPrefix):]
		i := strings.Index(name, JWTMark)
		if i < 0 {
			return Place{Part: PartHeader, Name: name}, nil
		}
		if i == 0 {
			return Place{}, fmt.Errorf("%w: %q names no header before %s", ErrPlace, s, JWTMark)
		}
		claim, err := parseJSONPath(name[i+len(JWTMark):])
		if err != nil {
			return Place{}, fmt.Errorf("%w: %q: the claim after %s: %v", ErrPlace, s, JWTMark, err)
		}
		return Place{Part: PartHeader, Name: name[:i], Claim: claim}, nil
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
		return queryPrefix + p.Name
	case PartHeader:
		if len(p.Claim) > 0 {
			return headerPrefix + p.Name + JWTMark + formatJSONPath(p.Claim)
		}
		return headerPrefix + p.Name
	}
	return ""
}

// Key is the place as String writes it with a header's name in lower case,
// as it is read in any case: two places of one request are one where their
// keys are.
func (p Place) Key() string {
	if p.Part == PartHeader {
		p.Name = strings.ToLower(p.Name)
	}
	return p.String()
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
		case s.Key == "" || strings.ContainsAny(s.Key, `.[]"`):
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
