package recorder

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// contextReads are the read helpers that take a context and return its error
// before they read. A read of a session's stream handed the recording's
// context returns at the stop before it reads what the relay captured, and
// that exchange is lost with nothing recording it.
var contextReads = map[string]bool{
	"ReadPacketBuffer":        true,
	"ReadPacketBufferChecked": true,
	"ReadPacketStream":        true,
	"ReadRequiredBytes":       true,
	"AppendRequiredBytes":     true,
	"ReadBytes":               true,
}

// TestEveryStreamReadTakesNoContext pins that the recorder reads its streams
// only through readPacket and readPacketChecked, which take no context: a call
// of a context-taking read helper anywhere else in the recorder's code could
// be handed the recording's context and end on the stop.
func TestEveryStreamReadTakesNoContext(t *testing.T) {
	fset := token.NewFileSet()
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatal(err)
	}
	parsed := 0
	wrapped := map[string]int{}
	for _, e := range entries {
		name := e.Name()
		if !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatal(err)
		}
		parsed++
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
				if !ok || !contextReads[sel.Sel.Name] {
					return true
				}
				if fn.Recv == nil && (fn.Name.Name == "readPacket" || fn.Name.Name == "readPacketChecked") {
					wrapped[fn.Name.Name]++
					return true
				}
				t.Errorf("%s: %s calls %s, which takes a context; read the session's streams with readPacket or readPacketChecked",
					fset.Position(call.Pos()), fn.Name.Name, sel.Sel.Name)
				return true
			})
		}
	}
	if parsed == 0 {
		t.Fatal("parsed no source file of the recorder")
	}
	if wrapped["readPacket"] != 1 || wrapped["readPacketChecked"] != 1 {
		t.Fatalf("readPacket and readPacketChecked each wrap one context-taking read, got %v", wrapped)
	}
}
