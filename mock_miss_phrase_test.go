package main

import (
	"fmt"
	"go/ast"
	"go/parser"
	"go/token"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"testing"
)

// missPhrase is what users and CI scripts grep keploy's output for to fail a
// run on a mock miss. keploy's own lines must use it only for an actual miss.
const missPhrase = "mock miss"

// missLiteral is one string literal in keploy's non-test Go source: the file
// (slash-separated, from the module root), the top-level function or method
// it sits in ("" at package level) and the literal's text.
type missLiteral struct {
	file, fn, text string
}

// missLine says which miss a registered literal reports, and how many times
// the literal is in its function.
type missLine struct {
	n   int
	why string
}

// missLines is the registry of the lines that report an actual miss: every
// string literal in keploy's source that contains missPhrase, in any case,
// must be one of them, as many times as registered.
//
// A line that is not a miss (a rule stated at start, an egress knob, a
// warning about a degraded state, help text, the config template) must say
// what it means without the phrase. A registered line that is reworded or
// removed fails the row too, so a miss line keeps the wording users grep for.
var missLines = map[missLiteral]missLine{
	{"pkg/agent/proxy/dns.go", "(*Proxy).resolveUncachedDNSResponse", "DNS mock miss resolved via upstream forward"}:                                                                                                                {1, "mocking on, no DNS mock answered, an in-network or opted-in resolver did"},
	{"pkg/agent/proxy/dns.go", "(*Proxy).resolveUncachedDNSResponse", "DNS mock miss: upstream NODATA (empty NOERROR), relaying as valid negative answer (a genuinely unrecorded record of this type would also relay empty here)"}: {1, "mocking on, no DNS mock answered, upstream said NODATA"},
	{"pkg/agent/proxy/dns.go", "(*Proxy).resolveUncachedDNSResponse", "DNS mock miss: upstream NODATA, relaying (already logged for this name/qtype)"}:                                                                              {1, "the same miss, deduplicated"},
	{"pkg/agent/proxy/dns.go", "(*Proxy).resolveUncachedDNSResponse", "DNS mock miss: upstream returned negative answer; falling back to synthetic DNS response"}:                                                                   {1, "mocking on, no DNS mock answered, upstream could not answer (SERVFAIL, REFUSED, ...)"},
	{"pkg/agent/proxy/dns.go", "(*Proxy).resolveUncachedDNSResponse", "DNS mock miss + upstream forward failed; falling back to synthetic response"}:                                                                                {1, "mocking on, no DNS mock answered, and the forward got no answer"},
	{"pkg/agent/proxy/dns.go", "(*Proxy).resolveUncachedDNSResponse", "mock miss"}:                                                                                                                                                  {1, "mocking on, no DNS mock answered: the reported miss"},
	{"pkg/agent/proxy/integrations/http/http.go", "(*HTTP).MockOutgoing", "mock miss — 502 already sent to client"}:                                                                                                                 {1, "no HTTP mock matched the request"},
	{"pkg/agent/proxy/integrations/mysql/replayer/match.go", "matchCommandWith", "mock miss"}:                                                                                                                                       {2, "no MySQL mock matched the command"},
}

// TestMockMissMeansAMiss reads every string literal in keploy's non-test Go
// source and fails on one that contains missPhrase, in any case, and is not a
// registered miss line, and on a registered miss line that is gone.
//
// Limits: it reads literals one by one, so a phrase built at run time ("mock "
// + "miss", a format verb, a value from a file) is not seen, and a literal is
// keyed by where it is written, so a constant holding the phrase that a
// non-miss line logs elsewhere passes; no source in the repo does either.
// Comments are not output and are not read. Test files,
// testdata, hidden directories (.github's sample apps) and nested modules are
// not keploy's own output and are skipped. Whether a registered line is logged
// only on a miss is held by the rows of its own package (for DNS,
// TestDNSForwardLinesSayMockMissOnlyOnAMiss).
func TestMockMissMeansAMiss(t *testing.T) {
	found := map[missLiteral][]token.Position{}
	fset := token.NewFileSet()
	err := filepath.WalkDir(".", func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		if d.IsDir() {
			if path == "." {
				return nil
			}
			name := d.Name()
			// Hidden directories (.github's sample apps), testdata, vendored and
			// node code, and nested modules are not keploy's own output.
			if strings.HasPrefix(name, ".") || name == "testdata" || name == "vendor" || name == "node_modules" {
				return filepath.SkipDir
			}
			if _, statErr := os.Stat(filepath.Join(path, "go.mod")); statErr == nil {
				return filepath.SkipDir
			}
			return nil
		}
		if !strings.HasSuffix(path, ".go") || strings.HasSuffix(path, "_test.go") {
			return nil
		}
		f, err := parser.ParseFile(fset, path, nil, parser.SkipObjectResolution)
		if err != nil {
			return err
		}
		file := filepath.ToSlash(path)
		for _, decl := range f.Decls {
			fn := ""
			if fd, ok := decl.(*ast.FuncDecl); ok {
				fn = funcName(fd)
			}
			ast.Inspect(decl, func(n ast.Node) bool {
				lit, ok := n.(*ast.BasicLit)
				if !ok || lit.Kind != token.STRING {
					return true
				}
				text, err := strconv.Unquote(lit.Value)
				if err != nil {
					t.Errorf("%s: cannot unquote %s: %v", fset.Position(lit.Pos()), lit.Value, err)
					return true
				}
				if strings.Contains(strings.ToLower(text), missPhrase) {
					key := missLiteral{file, fn, text}
					found[key] = append(found[key], fset.Position(lit.Pos()))
				}
				return true
			})
		}
		return nil
	})
	if err != nil {
		t.Fatalf("walk the module's source: %v", err)
	}

	var extra, stale []string
	for lit, at := range found {
		if want := missLines[lit].n; len(at) > want {
			// Which copy is the new one is not known: name them all.
			where := make([]string, len(at))
			for i, pos := range at {
				where[i] = pos.String()
			}
			extra = append(extra, fmt.Sprintf("%s in %s: %s, %d found, %d registered", strings.Join(where, ", "), lit.fn, snippet(lit.text), len(at), want))
		}
	}
	for lit, reg := range missLines {
		if got := len(found[lit]); got < reg.n {
			stale = append(stale, fmt.Sprintf("%s in %s: %s, %d found, %d registered", lit.file, lit.fn, snippet(lit.text), got, reg.n))
		}
	}
	sort.Strings(extra)
	sort.Strings(stale)
	for _, e := range extra {
		t.Errorf("%q in a line that is not a registered miss: %s\n\tsay what the line means without the phrase, or, if it reports an actual miss, add it to missLines", missPhrase, e)
	}
	for _, s := range stale {
		t.Errorf("registered miss line no longer in the source: %s\n\ta reported miss keeps the wording users grep for; update missLines only if the miss itself moved", s)
	}
}

// snippet quotes text, cut to the words around missPhrase when it is long
// (the config template is one literal of a few hundred lines).
func snippet(text string) string {
	const room = 80
	if len(text) <= 2*room {
		return strconv.Quote(text)
	}
	i := strings.Index(strings.ToLower(text), missPhrase)
	from, to := max(i-room, 0), min(i+len(missPhrase)+room, len(text))
	return "..." + strconv.Quote(text[from:to]) + "..."
}

// funcName names a top-level function, or a method as "(*T).m" / "T.m".
func funcName(fd *ast.FuncDecl) string {
	if fd.Recv == nil || len(fd.Recv.List) == 0 {
		return fd.Name.Name
	}
	recv := fd.Recv.List[0].Type
	star := ""
	if se, ok := recv.(*ast.StarExpr); ok {
		star, recv = "*", se.X
	}
	if ix, ok := recv.(*ast.IndexExpr); ok {
		recv = ix.X
	}
	if ix, ok := recv.(*ast.IndexListExpr); ok {
		recv = ix.X
	}
	name := "?"
	if id, ok := recv.(*ast.Ident); ok {
		name = id.Name
	}
	if star != "" {
		return "(*" + name + ")." + fd.Name.Name
	}
	return name + "." + fd.Name.Name
}
