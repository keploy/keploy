package learn

import (
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

// LeafPath is the exact leaf path (Leaf.Path) of a place as Place.String
// writes it (variables.Parse), the name learned noise is stored under. ok is
// false for a place that does not parse. A place of a mock's variables block
// is read with VariableLeafPath, which reads a word address there.
func LeafPath(place string) (string, bool) {
	p, err := variables.Parse(place)
	if err != nil {
		return "", false
	}
	return placeLeafPath(p)
}

// VariableLeafPath is the leaf path of place, a place of m's variables block
// (a bind, a use or an answer of models.MockVariable), read as m's replay
// reads it (variables.ReadPlace against m's recorded request,
// variables.InRequest): what the place's noise is dropped by when the place
// becomes a variable's (MergeLearned), and what the learn step excuses at a
// recording's variable (HTTP's variablePlaces). A place the recording holds
// as written names its own leaf, a legacy name that ends in #<digits> or
// holds '|' included; a word address, or a decode step, names the leaf of
// the field it reads a word of: the word is a part of that leaf's value,
// and no leaf is named "<field>#<n>". A mock with no recorded HTTP request
// (another protocol's, whose replay reads its own recording) holds no place
// here, so its places read as word addresses, as Place.String and its
// learn step write them. ok is false for a place that reads neither way.
func VariableLeafPath(m *models.Mock, place string) (string, bool) {
	var req *models.HTTPReq
	if m != nil {
		req = m.Spec.HTTPReq
	}
	a, err := variables.ReadPlace(place, variables.InRequest(req))
	if err != nil {
		return "", false
	}
	return placeLeafPath(a.Place)
}

// placeLeafPath is the exact leaf path (Leaf.Path) of p.
func placeLeafPath(p variables.Place) (string, bool) {
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
// digits, '_' and '-'; "var-" before one a run's name would begin with.
func varName(place string) string {
	p, err := variables.Parse(place)
	base := ""
	if err == nil {
		switch p.Part {
		case variables.PartPath:
			base = "path" + strconv.Itoa(p.Index)
		case variables.PartQuery, variables.PartHeader:
			base = p.Name
		case variables.PartBody:
			for i := len(p.Path) - 1; i >= 0 && base == ""; i-- {
				if !p.Path[i].IsIndex && !strings.HasPrefix(p.Path[i].Key, "$") {
					base = p.Path[i].Key
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
	// A run's name (models.RunVariable) is the set's, which a replay keeps
	// past the test: a test's own variable never takes it.
	if models.RunVariable(base) {
		base = "var-" + base
	}
	return base
}
