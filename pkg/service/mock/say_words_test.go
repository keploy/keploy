package mock

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var unshowable = regexp.MustCompile(`(?i)\b(packs?|windows?|windowed|posture|residency|isolation|isolated|flows?|mappings?|pool|consumed|harness|tier|parser|mitm|may|might|probably|maybe)\b|not at fault|did not prove|a typo|not sure|check the logs above|logs above`)

func literals(e ast.Expr) []string {
	var out []string
	ast.Inspect(e, func(n ast.Node) bool {
		if lit, ok := n.(*ast.BasicLit); ok && lit.Kind == token.STRING {
			if s, err := strconv.Unquote(lit.Value); err == nil {
				out = append(out, s)
			}
		}
		return true
	})
	return out
}

func marks(args []ast.Expr, name string) bool {
	for _, a := range args {
		if sel, ok := a.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
			if x, ok := sel.X.(*ast.Ident); ok && x.Name == "log" {
				return true
			}
		}
	}
	return false
}

func TestLinesAMockRunShowSayOnlyFactsInPlainWords(t *testing.T) {
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	checked := 0
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		fset := token.NewFileSet()
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		ast.Inspect(f, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			var msg ast.Expr
			args := call.Args
			switch sel.Sel.Name {
			case "Warn", "Error":
				if len(args) > 0 {
					msg = args[0]
				}
			case "Info":
				if len(args) > 0 && marks(args, "Show") {
					msg = args[0]
				}
			case "LogError":
				if len(args) > 2 {
					msg = args[2]
				}
			}
			if msg == nil || marks(args, "Hide") {
				return true
			}
			texts := literals(msg)
			for _, a := range args {
				if c, ok := a.(*ast.CallExpr); ok && len(c.Args) == 2 {
					if k, ok := c.Args[0].(*ast.BasicLit); ok && k.Value == `"next_step"` {
						texts = append(texts, literals(c.Args[1])...)
					}
				}
			}
			for _, s := range texts {
				checked++
				s = strings.ReplaceAll(s, "mappings.yaml", "")
				if w := unshowable.FindString(s); w != "" {
					t.Errorf("%s: %q shows %q", fset.Position(call.Pos()), s, w)
				}
			}
			return true
		})
	}
	if checked < 30 {
		t.Fatalf("only %d messages checked", checked)
	}
}
