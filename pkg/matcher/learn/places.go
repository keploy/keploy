package learn

import (
	"slices"
	"strconv"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/templates"
	"go.keploy.io/server/v3/pkg/variables"
)

// VarPlace is the leaf's place in the grammar a mock's variables are written
// in (variables.Parse): body.<json path>, url.path[<n>], url.query.<name>,
// header.<name>. ok is false for a leaf no variable place can name: a field
// of a form or multipart body, a JSON key no path names alone (Unnamed), a
// whole body that is not JSON. jsonBody says whether the leaf's request (or
// answer) has a JSON body.
func VarPlace(l Leaf, jsonBody bool) (string, bool) {
	if l.Unnamed {
		return "", false
	}
	switch {
	case strings.HasPrefix(l.Path, "url.path."):
		n, err := strconv.Atoi(strings.TrimPrefix(l.Path, "url.path."))
		if err != nil || n < 0 {
			return "", false
		}
		return variables.Place{Part: variables.PartPath, Index: n}.String(), true
	case strings.HasPrefix(l.Path, "url.query."):
		return variables.Place{Part: variables.PartQuery, Name: strings.TrimPrefix(l.Path, "url.query.")}.String(), true
	case strings.HasPrefix(l.Path, "header."):
		return variables.Place{Part: variables.PartHeader, Name: strings.TrimPrefix(l.Path, "header.")}.String(), true
	case jsonBody && (strings.HasPrefix(l.Path, "body.") || strings.HasPrefix(l.Path, "body[")):
		path, ok := jsonSegs(strings.TrimPrefix(l.Path, "body"))
		if !ok {
			return "", false
		}
		return variables.BodyPlace(path).String(), true
	}
	return "", false
}

// LeafPath is the exact leaf path (Leaf.Path) of a variable place, the name
// learned noise is stored under: what a place's noise is dropped by when the
// place becomes a variable's. ok is false for a place that does not parse.
func LeafPath(place string) (string, bool) {
	p, err := variables.Parse(place)
	if err != nil {
		return "", false
	}
	switch p.Part {
	case variables.PartPath:
		return "url.path." + strconv.Itoa(p.Index), true
	case variables.PartQuery:
		return "url.query." + p.Name, true
	case variables.PartHeader:
		return "header." + strings.ToLower(p.Name), true
	case variables.PartBody:
		var b strings.Builder
		b.WriteString("body")
		for _, s := range p.Path {
			if s.IsIndex {
				b.WriteString("[" + strconv.Itoa(s.Index) + "]")
				continue
			}
			b.WriteString("." + s.Key)
		}
		return b.String(), true
	}
	return "", false
}

// jsonSegs reads a JSON leaf path after its root (".a.b[0].c"), as
// jsonLeaves writes it.
func jsonSegs(s string) ([]templates.PathSeg, bool) {
	var out []templates.PathSeg
	for i := 0; i < len(s); {
		switch s[i] {
		case '.':
			j := i + 1
			for j < len(s) && s[j] != '.' && s[j] != '[' {
				j++
			}
			if j == i+1 {
				return nil, false
			}
			out = append(out, templates.PathSeg{Key: s[i+1 : j]})
			i = j
		case '[':
			end := strings.IndexByte(s[i:], ']')
			if end < 0 {
				return nil, false
			}
			n, err := strconv.Atoi(s[i+1 : i+end])
			if err != nil || n < 0 {
				return nil, false
			}
			out = append(out, templates.PathSeg{Index: n, IsIndex: true})
			i += end + 1
		default:
			return nil, false
		}
	}
	return out, len(out) > 0
}

// varName is the name a variable first found at place gets: the place's last
// key, header or query name, or "path<n>" for a URL segment, in letters,
// digits, '_' and '-'. A name is never made of a key that may be a value the
// app sent (keyMayBeValue): a body place holding one such key, or such a
// query name, is named "var". The whole body place is, not only that key: a
// MongoDB place joins its keys with '.' as they are, so the keys either side
// of a dotted key's {} part may be the rest of that key (alice.smith@example.com
// is alice, smith@example and com). An operator key ($date, $oid) and an
// array position (documents.0, as MongoDB writes it) name nothing: the key
// above them names the variable. The one key of a recording's echo place
// (echoKeyOf) is keploy's words, the recording's name, and names it.
func varName(mock, place string) string {
	p, err := variables.Parse(place)
	base := ""
	switch {
	case err != nil:
	case len(p.Path) == 1 && echoKeyOf(mock, p.Path[0].Key):
		base = p.Path[0].Key
	default:
		switch p.Part {
		case variables.PartPath:
			base = "path" + strconv.Itoa(p.Index)
		case variables.PartQuery:
			if models.KeyShape(p.Name) == p.Name {
				base = p.Name
			}
		case variables.PartHeader:
			base = p.Name
		case variables.PartBody:
			if slices.ContainsFunc(p.Path, func(s templates.PathSeg) bool { return !s.IsIndex && keyMayBeValue(s.Key) }) {
				break
			}
			for i := len(p.Path) - 1; i >= 0 && base == ""; i-- {
				if k := p.Path[i].Key; !p.Path[i].IsIndex && !strings.HasPrefix(k, "$") && !allDigits(k) {
					base = k
				}
			}
		}
	}
	base = strings.Map(func(r rune) rune {
		if r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '_' || r == '-' {
			return r
		}
		return -1
	}, base)
	if base == "" {
		base = "var"
	}
	return base
}

// echoKeyOf reports whether key is the key a protocol with no place of the
// variables grammar (a Postgres bind) names the echo of recording mock bound
// at a position by: the recording's name, then "-bind" and the position from
// 1 ("mock-3-bind1"). The name is spelled in the runes a variable's name
// keeps: letters, digits and '-' as they are, '_' as "__", any other rune as
// '_', its code in hex and '_' ("mock.3" is "mock_2e_3"). Named after its
// recording, an echo's name is the same however a test's calls are learned;
// the key holds no part of a call, so it is no value the app sent.
func echoKeyOf(mock, key string) bool {
	if mock == "" {
		return false
	}
	var spelled strings.Builder
	for _, r := range mock {
		switch {
		case r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || r == '-':
			spelled.WriteRune(r)
		case r == '_':
			spelled.WriteString("__")
		default:
			spelled.WriteString("_" + strconv.FormatInt(int64(r), 16) + "_")
		}
	}
	pos, ok := strings.CutPrefix(key, spelled.String()+"-bind")
	return ok && allDigits(pos) && pos[0] != '0'
}

// keyMayBeValue reports whether a body key may be a value the app sent: one
// of its dot-separated parts is not kept by models.FieldShape (an email
// address, an ID with a digit in it). An operator key FieldShape keeps ($set,
// $oid) is no value, and neither is a key of digits alone: MongoDB writes an
// array position so, and varName never names a variable by one.
func keyMayBeValue(k string) bool {
	if allDigits(k) {
		return false
	}
	for _, part := range strings.Split(k, ".") {
		if models.FieldShape(part) != part {
			return true
		}
	}
	return false
}

// allDigits reports whether s is one or more of the digits 0-9.
func allDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}
