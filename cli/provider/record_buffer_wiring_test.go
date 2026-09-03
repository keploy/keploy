package provider

import (
	"go/ast"
	"go/parser"
	"go/token"
	"strconv"
	"testing"
)

// Every record-buffer flag that is REGISTERED must also be RESOLVED.
//
// --half-close-grace shipped registered, aliased in flagNameMapping, and
// forwarded to the agent's argv — and read by nobody, because no
// resolveRecordBuffer* call was ever added for it. Every test passed:
// registration tests only prove the flag parses.
//
// In docker record mode that gap is total. pkg/platform/docker/util.go
// says it outright — the host's keploy.yml is not bind-mounted into the
// agent container, so argv is the only propagation channel. The
// orchestrator appended `--half-close-grace -1s`, the containerised
// agent parsed it and threw it away, and ran the 10s default. An
// operator who disabled half-close got it enabled, with no way to tell
// from outside.
//
// There is no runtime seam to assert this through: the resolve calls
// live inside ValidateFlags, behind validation that a bare harness
// cannot satisfy. So the pin is on the source, in the same spirit as
// relay_config_test.go's pin on the relay.Config literal. It proves the
// wiring, not the behaviour.
func TestEveryRecordBufferFlagIsResolved(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "cmd.go", nil, 0)
	if err != nil {
		t.Fatalf("parse cmd.go: %v", err)
	}

	// Flags registered against Record.RecordBuffer.*, and flags passed to
	// a resolveRecordBuffer* helper.
	registered := map[string]bool{}
	resolved := map[string]bool{}

	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || len(call.Args) == 0 {
			return true
		}
		sel, ok := call.Fun.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		name := func(i int) (string, bool) {
			lit, ok := call.Args[i].(*ast.BasicLit)
			if !ok || lit.Kind != token.STRING {
				return "", false
			}
			v, err := strconv.Unquote(lit.Value)
			return v, err == nil
		}

		switch {
		// cmd.Flags().Duration("x", c.cfg.Record.RecordBuffer.Y, ...)
		case len(call.Args) >= 2 && isRecordBufferSelector(call.Args[1]):
			if f, ok := name(0); ok {
				registered[f] = true
			}
		// c.resolveRecordBufferXxx(cmd, "x", "ENV", &c.cfg.Record.RecordBuffer.Y)
		case len(sel.Sel.Name) > len("resolveRecordBuffer") &&
			sel.Sel.Name[:len("resolveRecordBuffer")] == "resolveRecordBuffer":
			if f, ok := name(1); ok {
				resolved[f] = true
			}
		}
		return true
	})

	if len(registered) == 0 {
		t.Fatal("found no record-buffer flag registrations in cmd.go; this pin has stopped " +
			"looking at anything and would pass no matter what")
	}

	for flag := range registered {
		if !resolved[flag] {
			t.Errorf("--%s is registered against config.Record.RecordBuffer but never passed to "+
				"a resolveRecordBuffer* helper. It parses, it is forwarded to the agent's argv, "+
				"and nothing reads it into config — a knob that silently does nothing. In docker "+
				"record mode argv is the ONLY propagation channel, so the operator's value is "+
				"lost entirely.", flag)
		}
	}
}

// isRecordBufferSelector reports whether an expression is a selector
// rooted at ...Record.RecordBuffer, e.g.
// c.cfg.Record.RecordBuffer.HalfCloseGrace.
func isRecordBufferSelector(e ast.Expr) bool {
	sel, ok := e.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	inner, ok := sel.X.(*ast.SelectorExpr)
	return ok && inner.Sel.Name == "RecordBuffer"
}
