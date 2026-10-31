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
		name := strings.TrimPrefix(l.Path, "header.")
		if token, ok := ClaimOf(l.Path); ok {
			// A claim of the token the header holds, whole or as one
			// cookie's value (JWT.Leaves, HeaderJWTs).
			claim, ok := jsonSegs(l.Path[len(token)+len(JWTMark):])
			header := strings.TrimPrefix(TokenHeader(token), "header.")
			if !ok || header == "" || len(claim) == 0 {
				return "", false
			}
			cookie := ""
			if len(token) > len("header."+header) {
				cookie = token[len("header."+header)+1 : len(token)-1]
			}
			return variables.Place{Part: variables.PartHeader, Name: header, Claim: claim, Cookie: cookie}.String(), true
		}
		if cookie, ok := CookieOf(l.Path); ok {
			// One cookie's value of a Cookie header (CookieLeaves).
			return variables.Place{Part: variables.PartHeader, Name: variables.CookieHeader, Cookie: cookie}.String(), true
		}
		if strings.ContainsRune(name, '{') {
			// A token's place, or a part of a header, named with no claim
			// ("header.authorization{jwt}"): a header's name holds no '{'.
			return "", false
		}
		return variables.Place{Part: variables.PartHeader, Name: name}.String(), true
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
		if len(p.Claim) > 0 {
			cookie := ""
			if p.Cookie != "" {
				cookie = "{" + p.Cookie + "}"
			}
			return "header." + strings.ToLower(p.Name) + cookie + JWTMark + leafSegs(p.Claim), true
		}
		if p.Cookie != "" {
			return "header." + strings.ToLower(p.Name) + "{" + p.Cookie + "}", true
		}
		return "header." + strings.ToLower(p.Name), true
	case variables.PartBody:
		return "body" + leafSegs(p.Path), true
	}
	return "", false
}

// leafSegs writes a JSON path as a leaf's path holds it after its root
// (jsonSegs reads it).
func leafSegs(path []templates.PathSeg) string {
	var b strings.Builder
	for _, s := range path {
		if s.IsIndex {
			b.WriteString("[" + strconv.Itoa(s.Index) + "]")
			continue
		}
		b.WriteString("." + s.Key)
	}
	return b.String()
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
			if p.Cookie != "" {
				// A cookie, or its token's claim, by the cookie's name.
				base = p.Cookie
			}
			// A token's claim is named by its claim's key.
			for i := len(p.Claim) - 1; i >= 0; i-- {
				if !p.Claim[i].IsIndex {
					base = p.Claim[i].Key
					break
				}
			}
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
