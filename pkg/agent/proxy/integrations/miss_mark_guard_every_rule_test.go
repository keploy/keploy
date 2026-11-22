package integrations

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strings"
	"testing"
)

// A function breaking every rule it can at once reports each of them, so no rule can hide
// another: a waiver of the hiding rule would then pass the hidden one.
func TestEveryRuleAFunctionBreaksIsReportedTogether(t *testing.T) {
	for _, c := range []struct{ name, body, want string }{
		{"every rule but ErrNoMockMatched's at once",
			"report := build()\nif ok {\n\tintegrations.StampMark(report, mark)\n}\nsend(models.NewMockMismatchError(err, report))\nsend(models.NewMockMismatchError(err, other))\nsend(models.NewMockMismatchError(err, nil))\nsend(models.ParserError{Err: err})\nreturn models.NewMockMismatchError(err, &models.MockMismatchReport{})",
			strings.Join([]string{ruleNilReport, ruleNoReport, ruleInline, ruleCondition, ruleNoRound}, "; ")},
		{"every rule but a built report's at once",
			"send(models.NewMockMismatchError(err, nil))\nsend(models.ParserError{Err: err})\nsend(models.NewMockMismatchError(err, &models.MockMismatchReport{}))\nreturn fmt.Errorf(\"%w: x\", models.ErrNoMockMatched)",
			strings.Join([]string{ruleNilReport, ruleNoReport, ruleInline, ruleNoMatched}, "; ")},
	} {
		f, err := parser.ParseFile(token.NewFileSet(), "shape.go", "package p\n\nfunc f() {\n"+c.body+"\n}\n", parser.SkipObjectResolution)
		if err != nil {
			t.Fatalf("%s: %v", c.name, err)
		}
		if got := strings.Join(missUnstamped(f.Decls[0].(*ast.FuncDecl).Body, "models", "integrations"), "; "); got != c.want {
			t.Errorf("%s: the guard says %q, want %q", c.name, got, c.want)
		}
	}
}
