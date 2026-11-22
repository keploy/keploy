package proxy

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"strings"
	"testing"
)

// Every function that can start a LEGACY recording must also consult
// shouldRecordViaSupervisor.
//
// This has silently gone wrong twice, the same way both times. A dispatch
// site calls RecordOutgoing directly with a hand-built RecordSession; the
// parser's RecordOutgoing sees V2 == nil and falls to recordLegacy; and the
// parser records legacy on the default configuration even though IsV2()
// returns true. keploy#4526 found it on the generic catch-all — "the V2
// migration silently skipped the widest code path in the proxy" — and
// deliberately left the MySQL probe branch for later. Nothing failed in
// between.
//
// WHAT THIS CATCHES: a NEW dispatch site added in a new function with no
// gate at all — the shape both regressions actually took.
//
// WHAT IT DOES NOT CATCH, and must not be trusted for: whether an existing
// gate still WORKS. An earlier version of this pin counted call sites and
// was green against `if false && shouldRecordViaSupervisor(...)` and
// against inserting a `!` — it never looked for the predicate at all,
// because shouldRecordViaSupervisor(x) is an *ast.Ident call, not a
// SelectorExpr, and was invisible to its walker by construction. What
// still defeats it is a decoy: an ungated dispatch in a function that
// mentions shouldRecordViaSupervisor for some other reason satisfies the
// count. Those live behaviours belong to the table test over
// shouldRecordViaSupervisor and to
// TestMySQLProbeBranchRoutesByShouldRecordViaSupervisor, which hand a
// parser to the real dispatcher and assert which surface it receives.
// exemptFn is the V2 adapter itself; see the scan loop.
const exemptFn = "recordViaSupervisor"

// callCounts returns how many times each interesting function is called
// anywhere inside n, counting BOTH pkg.Fn()/x.Fn() selector calls and
// bare Fn() identifier calls. The identifier case is the one an earlier
// version of this pin missed entirely.
func callCounts(n ast.Node) (legacy, gate int) {
	ast.Inspect(n, func(inner ast.Node) bool {
		call, ok := inner.(*ast.CallExpr)
		if !ok {
			return true
		}
		var name string
		switch fn := call.Fun.(type) {
		case *ast.SelectorExpr:
			name = fn.Sel.Name
		case *ast.Ident:
			name = fn.Name
		}
		switch name {
		case "RecordOutgoing":
			legacy++
		case "shouldRecordViaSupervisor":
			gate++
		}
		return true
	})
	return legacy, gate
}

func TestEveryLegacyDispatchFunctionConsultsTheGate(t *testing.T) {
	t.Parallel()

	fset := token.NewFileSet()

	// Scan EVERY non-test file in the package, not just proxy.go. Both
	// prior regressions happened to live in proxy.go; the next one need
	// not, and a pin that names one file silently stops covering the
	// package the moment a dispatch moves.
	entries, err := os.ReadDir(".")
	if err != nil {
		t.Fatalf("read package dir: %v", err)
	}
	var scanned []string
	var totalLegacy, totalGate int
	exemptSeen := false

	for _, e := range entries {
		name := e.Name()
		if e.IsDir() || !strings.HasSuffix(name, ".go") || strings.HasSuffix(name, "_test.go") {
			continue
		}
		file, err := parser.ParseFile(fset, name, nil, 0)
		if err != nil {
			t.Fatalf("parse %s: %v", name, err)
		}
		scanned = append(scanned, name)

		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			// recordViaSupervisor is the V2 adapter itself: its
			// RecordOutgoing call IS the supervised dispatch, so it has
			// legacy=1 gate=0 by construction. Exempt by name, and assert
			// below that the name still resolves so the allowlist cannot
			// rot into a hole.
			if fn.Name.Name == exemptFn {
				exemptSeen = true
				continue
			}
			legacy, gate := callCounts(fn.Body)
			totalLegacy += legacy
			totalGate += gate
			if legacy > 0 && gate < legacy {
				t.Errorf("%s has %d legacy RecordOutgoing dispatch site(s) but consults "+
					"shouldRecordViaSupervisor only %d time(s) (%s). Every legacy dispatch needs "+
					"the gate in front of it, or that parser records legacy on the default "+
					"configuration even though IsV2() is true — silently, because an ungated site "+
					"is indistinguishable from a gated one whose parser is not V2, and every "+
					"runtime test keeps passing. This is the defect keploy#4526 found on the "+
					"generic catch-all and the one the MySQL probe branch carried after it.",
					fn.Name.Name, legacy, gate, fset.Position(fn.Pos()))
			}
		}
	}

	if !exemptSeen {
		t.Errorf("the %q exemption matched no function in %v — either it was renamed (update "+
			"exemptFn) or removed (drop the exemption). An allowlist entry that resolves to "+
			"nothing is a hole in this pin.", exemptFn, scanned)
	}

	// Guard against the pin quietly matching nothing — the failure mode that
	// makes a source-scanning test worthless.
	if totalLegacy == 0 || totalGate == 0 {
		t.Fatalf("found %d RecordOutgoing and %d shouldRecordViaSupervisor call sites across "+
			"%v; this pin has stopped looking at anything and would pass no matter what",
			totalLegacy, totalGate, scanned)
	}
}
