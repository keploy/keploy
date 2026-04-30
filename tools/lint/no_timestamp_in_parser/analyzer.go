// Package notimestampinparser implements a go/analysis analyzer that forbids
// calls to time.Now / time.Since / time.Until inside parser "record-path" files.
//
// Rationale (PLAN.md invariant I5): parsers in the V2 proxy architecture must
// source ReqTimestampMock and ResTimestampMock from fakeconn.Chunk.ReadAt /
// WrittenAt rather than calling time.Now() themselves. A parser that calls
// time.Now() during record captures scheduler/decoder latency instead of the
// actual wire event time, which is a subtle correctness bug.
//
// Scope (files the analyzer inspects) — V2 record-path files ONLY:
//   - any *_v2.go file (record_v2.go, encode_v2.go, query_v2.go, etc.)
//   - any .go file under a recorder_v2/ directory (reserved for future
//     parsers that split V2 logic into a subpackage)
//
// The legacy encode.go / record.go files in pkg/agent/proxy/integrations/
// are deliberately out of scope — they predate the V2 chunk-timestamp
// contract and use time.Now() extensively. Retrofitting the rule there
// would produce a flood of false positives and conflict with the
// documented pre-V2 behaviour.
//
// Allowlist (files/lines the analyzer skips within scope):
//   - *_test.go                                    — tests are fine
//   - record_legacy*.go                            — legacy path predates the rule
//   - any call site with the magic comment `// allow:time.Now` on the line
//     immediately above                            — log/telemetry opt-out
package notimestampinparser

import (
	"go/ast"
	"go/token"
	"go/types"
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
			if !forbiddenSelectors[sel.Sel.Name] {
				return true
			}
			if !refersToStdlibTimePackage(pass, ident) {
				return true
			}

			pos := pass.Fset.Position(sel.Pos())
			if suppressLines[pos.Line] {
				return true
			}
			pass.Reportf(sel.Pos(),
				"time.%s is forbidden in parser record-path files (invariant I5); derive timestamps from fakeconn.Chunk.ReadAt/WrittenAt instead. Use `%s` on the preceding line to suppress for log/telemetry sites.",
				sel.Sel.Name, suppressionComment)
			return true
		})
	}
	return nil, nil
}

// inScope reports whether filename falls within the analyzer's scope.
// The rule only applies to V2 record-path files; legacy encode.go / record.go
// files continue to use time.Now() (that behaviour is documented in PLAN.md
// as the pre-V2 anti-pattern the new architecture replaces, not something
// to be retrofitted).
//
// Two matchers (intentionally directory-agnostic — the rule is named
// by file-naming convention, not location, so every repo that hosts
// V2 parsers in the same naming scheme — keploy, keploy/integrations,
// keploy/enterprise — gets consistent enforcement without each repo
// re-declaring its own integration tree):
//   - any *_v2.go file (record_v2.go, encode_v2.go, query_v2.go, …)
//   - any .go file under a recorder_v2/ directory (reserved for
//     parsers that want to split V2 logic into a subpackage).
//
// The older "any encode*.go" pattern was too broad — legacy encode.go files
// in integrations/generic and integrations/http use time.Now() legitimately
// and must not be flagged. Narrowing to *_v2.go scopes the rule precisely
// to the files that adopted the chunk-timestamp contract.
//
// Matching is path-substring based so analysistest's testdata/src/<pkg>/
// layout works the same as real production paths.
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
	if strings.HasSuffix(base, "_v2.go") {
		return true
	}
	if strings.Contains(norm, "/recorder_v2/") {
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

// refersToStdlibTimePackage reports whether the identifier in a
// selector expression like ident.Now resolves to the stdlib "time"
// package. Uses pass.TypesInfo so aliased imports
// (`import stdtime "time"`) and renamed copies are all handled
// correctly; falls back to a bare name match if TypesInfo is
// unavailable for any reason (e.g. testdata without full type-check).
func refersToStdlibTimePackage(pass *analysis.Pass, ident *ast.Ident) bool {
	if pass.TypesInfo != nil {
		if obj, ok := pass.TypesInfo.Uses[ident]; ok {
			if pkgName, ok := obj.(*types.PkgName); ok {
				return pkgName.Imported().Path() == "time"
			}
			// A non-PkgName Use (local var, function, etc.) means
			// the identifier isn't a package reference.
			return false
		}
	}
	// No type info or no Uses entry — fall back to the name match
	// the original implementation used. This keeps analysistest
	// working in constrained testdata environments.
	if ident.Name != "time" {
		return false
	}
	// ident.Obj != nil implies a local variable named "time" shadows
	// the package; in that case the selector is not referring to
	// the stdlib.
	return ident.Obj == nil
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
			// Derive the marker body from the documented constant so
			// the matcher and the user-facing error message stay a
			// single source of truth.
			marker := strings.TrimSpace(strings.TrimPrefix(suppressionComment, "//"))
			if !strings.HasPrefix(inner, marker) {
				continue
			}
			endLine := tf.Position(c.End()).Line
			out[endLine+1] = true
		}
	}
	return out
}
