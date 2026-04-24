// Package notimestampinparser implements a go/analysis analyzer that forbids
// calls to time.Now / time.Since / time.Until inside parser "record-path" files.
//
// Rationale (PLAN.md invariant I5): parsers in the V2 proxy architecture must
// source ReqTimestampMock and ResTimestampMock from fakeconn.Chunk.ReadAt /
// WrittenAt rather than calling time.Now() themselves. A parser that calls
// time.Now() during record captures scheduler/decoder latency instead of the
// actual wire event time, which is a subtle correctness bug.
//
// Scope (files the analyzer inspects):
//   - pkg/agent/proxy/integrations/**/recorder/*.go (excluding *_test.go)
//   - pkg/agent/proxy/integrations/**/encode*.go    (excluding *_test.go)
//
// Allowlist (files/lines the analyzer skips within scope):
//   - *_test.go under recorder/                    — tests are fine
//   - record_legacy*.go under recorder/            — legacy path predates the rule
//   - any call site with the magic comment `// allow:time.Now` on the line
//     immediately above                            — log/telemetry opt-out
package notimestampinparser

import (
	"go/ast"
	"go/token"
	"path/filepath"
	"strings"

	"golang.org/x/tools/go/analysis"
)

// Analyzer is the go/analysis analyzer exported for driver programs and tests.
var Analyzer = &analysis.Analyzer{
	Name: "notimestampinparser",
	Doc:  "forbids time.Now/time.Since/time.Until inside parser record-path files; timestamps must come from fakeconn.Chunk.ReadAt/WrittenAt",
	Run:  run,
}

// forbiddenSelectors lists the time-package selectors we refuse inside scope.
// time.Since and time.Until are included because they call time.Now internally.
var forbiddenSelectors = map[string]bool{
	"Now":   true,
	"Since": true,
	"Until": true,
}

// suppressionComment is the magic marker that, when placed on the line directly
// above a call site, exempts that single call from the rule.
const suppressionComment = "// allow:time.Now"

func run(pass *analysis.Pass) (any, error) {
	for _, file := range pass.Files {
		tokFile := pass.Fset.File(file.FileStart)
		if tokFile == nil {
			continue
		}
		filename := tokFile.Name()
		if !inScope(filename) {
			continue
		}
		if fileAllowlisted(filename) {
			continue
		}

		// Build a quick map of comments keyed by the line they end on, so we
		// can detect suppression comments on the line immediately above a
		// call. We use the comment's End position (last line of the comment)
		// so that multi-line block-comment forms still work correctly.
		suppressLines := collectSuppressLines(tokFile, file.Comments)

		ast.Inspect(file, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			ident, ok := sel.X.(*ast.Ident)
			if !ok {
				return true
			}
			if ident.Name != "time" {
				return true
			}
			if !forbiddenSelectors[sel.Sel.Name] {
				return true
			}
			// Defensive: if the "time" identifier has an Obj (i.e. a local
			// variable named time shadows the package), skip — there's no
			// way it refers to the stdlib package.
			if ident.Obj != nil {
				return true
			}

			pos := pass.Fset.Position(sel.Pos())
			if suppressLines[pos.Line] {
				return true
			}
			pass.Reportf(sel.Pos(),
				"time.%s is forbidden in parser record-path files (invariant I5); derive timestamps from fakeconn.Chunk.ReadAt/WrittenAt instead. Use `// allow:time.Now` on the preceding line to suppress for log/telemetry sites.",
				sel.Sel.Name)
			return true
		})
	}
	return nil, nil
}

// inScope reports whether filename falls within the analyzer's scope:
//   - any .go file under a "recorder/" directory (excluding _test.go), or
//   - any encode*.go file (excluding _test.go).
//
// The check is path-substring based so it works for both real production
// paths (pkg/agent/proxy/integrations/<proto>/recorder/foo.go) and testdata
// paths used by analysistest (testdata/src/<pkg>/recorder/foo.go).
func inScope(filename string) bool {
	base := filepath.Base(filename)
	if !strings.HasSuffix(base, ".go") {
		return false
	}
	if strings.HasSuffix(base, "_test.go") {
		return false
	}
	// Normalise to forward slashes so Windows-style separators don't defeat
	// the substring match. (Cheap; no-op on POSIX.)
	norm := filepath.ToSlash(filename)
	if strings.Contains(norm, "/recorder/") {
		return true
	}
	if strings.HasPrefix(base, "encode") {
		return true
	}
	return false
}

// fileAllowlisted reports whether the file, though in scope, is exempt from
// the rule entirely (legacy record paths).
func fileAllowlisted(filename string) bool {
	base := filepath.Base(filename)
	return strings.HasPrefix(base, "record_legacy")
}

// collectSuppressLines returns the set of source lines L for which the line
// immediately above L contains the suppression marker "// allow:time.Now".
//
// Using the comment's End line ensures that a block or line comment that
// spans N lines still correctly exempts the call on the line directly after
// its last line.
func collectSuppressLines(tf *token.File, groups []*ast.CommentGroup) map[int]bool {
	out := make(map[int]bool)
	for _, g := range groups {
		for _, c := range g.List {
			raw := c.Text
			text := strings.TrimSpace(raw)
			var inner string
			switch {
			case strings.HasPrefix(text, "//"):
				// Line comment: `// allow:time.Now ...`
				inner = strings.TrimSpace(strings.TrimPrefix(text, "//"))
			case strings.HasPrefix(text, "/*") && strings.HasSuffix(text, "*/"):
				// Block comment: `/* allow:time.Now ... */` (single-
				// or multi-line; ast.Comment.Text includes the outer
				// delimiters). Strip both delimiters before matching.
				inner = strings.TrimSpace(text[2 : len(text)-2])
			default:
				continue
			}
			// Accept any marker-prefixed form (tolerate trailing
			// context like "allow:time.Now  -- boot-time splash log").
			if !strings.HasPrefix(inner, "allow:time.Now") {
				continue
			}
			endLine := tf.Position(c.End()).Line
			out[endLine+1] = true
		}
	}
	_ = suppressionComment // keep the documented constant referenced.
	return out
}
