package mock

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"
)

var unshowable = regexp.MustCompile(`(?i)\b(packs?|windows?|windowed|posture|residency|isolation|isolated|flows?|mappings?|pool|consumed|harness|tier|parser|mitm|may|might|probably|maybe|start-ups?|cases?)\b|not at fault|did not prove|a typo|not sure|logs above`)

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
		found := false
		ast.Inspect(a, func(n ast.Node) bool {
			if sel, ok := n.(*ast.SelectorExpr); ok && sel.Sel.Name == name {
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "log" {
					found = true
				}
			}
			if c, ok := n.(*ast.CallExpr); ok && name == "Show" {
				if sel, ok := c.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "show" {
					found = true
				}
			}
			return true
		})
		if found {
			return true
		}
	}
	return false
}

func mockOnly(cond ast.Expr) bool {
	found := false
	ast.Inspect(cond, func(n ast.Node) bool {
		switch t := n.(type) {
		case *ast.SelectorExpr:
			if t.Sel.Name == "mock" || t.Sel.Name == "MockMode" {
				found = true
			}
		}
		return true
	})
	return found
}

func assigned(body ast.Node, name string) []string {
	var out []string
	ast.Inspect(body, func(n ast.Node) bool {
		if as, ok := n.(*ast.AssignStmt); ok {
			for i, l := range as.Lhs {
				if id, ok := l.(*ast.Ident); ok && id.Name == name && i < len(as.Rhs) {
					out = append(out, literals(as.Rhs[i])...)
				}
			}
		}
		return true
	})
	return out
}

func shown(fset *token.FileSet, f *ast.File, all bool, check func(token.Position, string)) {
	var inMock []ast.Node
	ast.Inspect(f, func(n ast.Node) bool {
		if s, ok := n.(*ast.IfStmt); ok && mockOnly(s.Cond) {
			inMock = append(inMock, s.Body)
		}
		return true
	})
	within := func(n ast.Node) bool {
		for _, b := range inMock {
			if n.Pos() >= b.Pos() && n.End() <= b.End() {
				return true
			}
		}
		return false
	}
	for _, d := range f.Decls {
		fn, ok := d.(*ast.FuncDecl)
		if !ok || fn.Body == nil {
			continue
		}
		ast.Inspect(fn.Body, func(n ast.Node) bool {
			call, ok := n.(*ast.CallExpr)
			if !ok {
				return true
			}
			sel, ok := call.Fun.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			args := call.Args
			var msg ast.Expr
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
			if msg == nil || marks(args, "Hide") || !all && !marks(args, "Show") && !within(call) {
				return true
			}
			texts := literals(msg)
			for _, a := range args {
				c, ok := a.(*ast.CallExpr)
				if !ok || len(c.Args) != 2 {
					continue
				}
				if k, ok := c.Args[0].(*ast.BasicLit); ok && k.Value == `"next_step"` {
					texts = append(texts, literals(c.Args[1])...)
					if id, ok := c.Args[1].(*ast.Ident); ok {
						texts = append(texts, assigned(fn.Body, id.Name)...)
					}
				}
			}
			for _, s := range texts {
				check(fset.Position(call.Pos()), strings.ReplaceAll(s, "mappings.yaml", ""))
			}
			return true
		})
	}
}

func TestLinesAMockRunShowSayOnlyFactsInPlainWords(t *testing.T) {
	checked := 0
	for dir, all := range map[string]bool{
		".":                     true,
		"../../platform/http":   false,
		"../../agent/routes":    false,
		"../../agent/proxy":     false,
		"../../service/agent":   false,
		"../../../utils":        false,
		"../../../cli":          false,
		"../../../cli/provider": false,
	} {
		entries, err := os.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range entries {
			name := e.Name()
			if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
				continue
			}
			fset := token.NewFileSet()
			f, err := parser.ParseFile(fset, filepath.Join(dir, name), nil, 0)
			if err != nil {
				t.Fatal(err)
			}
			shown(fset, f, all, func(at token.Position, s string) {
				checked++
				if w := unshowable.FindString(s); w != "" {
					t.Errorf("%s: %q shows %q", at, s, w)
				}
			})
		}
	}
	if checked < 60 {
		t.Fatalf("only %d messages checked", checked)
	}
}
