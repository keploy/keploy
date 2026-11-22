package replay

import (
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// reporterHooks implements the optional TestCaseReporter and records what it
// was handed.
type reporterHooks struct {
	TestHooks
	calls []string
	err   error
}

func (h *reporterHooks) AfterTestCaseRun(_ context.Context, testSetID string, tc *models.TestCase, result *models.TestResult) error {
	name := ""
	if tc != nil {
		name = tc.Name
	}
	status := ""
	if result != nil {
		status = string(result.Status)
	}
	h.calls = append(h.calls, testSetID+"/"+name+"="+status)
	return h.err
}

func TestTheReporterIsOptional(t *testing.T) {
	// plainHooks (test_set_orderer_test.go) implements TestHooks and NOT this
	// extension: the assertion must miss it, which is what lets every existing
	// implementation stay untouched.
	if _, ok := interface{}(&plainHooks{}).(TestCaseReporter); ok {
		t.Fatal("a hooks implementation without AfterTestCaseRun satisfies TestCaseReporter; the extension is not optional")
	}
	if _, ok := interface{}(&reporterHooks{}).(TestCaseReporter); !ok {
		t.Fatal("a hooks implementation WITH AfterTestCaseRun does not satisfy TestCaseReporter")
	}
}

// The two tests that used to sit here -- one asserting the double recorded what
// it was handed, one asserting the double returned its own error -- exercised
// only the double: a mutation disabling the hook call in RunTestSet left both
// green. What they claimed to pin is now pinned against the real engine, in
// testcase_reporter_runtestset_test.go:
//
//	TestRunTestSetReportsEveryRecordedVerdict   what is handed over, and once each
//	TestAFailingReporterDoesNotFailTheRun       the "logs and carries on" contract
//
// TestTheReporterIsOptional stays here because it is about the TYPE: no engine
// can tell you whether an implementation without AfterTestCaseRun satisfies the
// interface.

// A VERDICT IS REPORTED AS IT SETTLES, NOT AT THE END OF THE SET.
//
// Every verdict used to be collected and drained from a defer, so the hook
// fired for the whole set at once, after RunTestSet returned, in MAP order.
// Anything following it live -- enterprise's progress file, k8s-proxy's
// per-case stream to the UI -- showed nothing for the length of the set and
// then everything in a burst, in an order that was not the run's.
//
// The reporting happens inside closures in RunTestSet, so the assertion is on
// the source: what regressed is WHERE the call sits, not what it computes.
func TestVerdictsAreReportedAsTheySettle(t *testing.T) {
	decl := runTestSetDecl(t)

	var reportsInline, guardsRetry bool
	ast.Inspect(decl, func(n ast.Node) bool {
		asg, ok := n.(*ast.AssignStmt)
		if !ok {
			return true
		}
		for i, lhs := range asg.Lhs {
			id, ok := lhs.(*ast.Ident)
			if !ok || id.Name != "recordVerdict" || i >= len(asg.Rhs) {
				continue
			}
			ast.Inspect(asg.Rhs[i], func(m ast.Node) bool {
				switch v := m.(type) {
				case *ast.CallExpr:
					if fn, ok := v.Fun.(*ast.Ident); ok && fn.Name == "reportVerdict" {
						reportsInline = true
					}
				case *ast.UnaryExpr:
					// `!r.config.RetryPassing`
					if sel, ok := v.X.(*ast.SelectorExpr); ok && sel.Sel.Name == "RetryPassing" {
						guardsRetry = true
					}
				}
				return true
			})
		}
		return true
	})
	if !reportsInline {
		t.Fatal("recordVerdict does not report the verdict as it settles; the hook fires for the whole set at once after RunTestSet returns, so a live consumer sees nothing during the set and then a burst in map order")
	}
	if !guardsRetry {
		t.Fatal("recordVerdict reports without checking RetryPassing; that cycle re-runs passing cases, so each would be reported as many times as it ran instead of once with its last verdict")
	}
}

// AND THE DRAIN MUST NOT USE A CANCELLED CONTEXT.
//
// The drain runs from a defer registered BEFORE the teardown defer that calls
// runTestSetCtxCancel, and defers run last-in-first-out -- so by the time it
// runs the run context is already cancelled. Harmless for today's two
// implementations, which ignore ctx, and a trap for the next one.
func TestTheVerdictDrainUsesALiveContext(t *testing.T) {
	decl := runTestSetDecl(t)

	// The DRAIN's own defer, not merely somewhere in RunTestSet: WithoutCancel
	// is used elsewhere in this function, so a looser check passes while the
	// drain goes back to the cancelled context.
	var withoutCancel bool
	ast.Inspect(decl, func(n ast.Node) bool {
		d, ok := n.(*ast.DeferStmt)
		if !ok {
			return true
		}
		var drains, strips bool
		ast.Inspect(d, func(m ast.Node) bool {
			call, ok := m.(*ast.CallExpr)
			if !ok {
				return true
			}
			switch fn := call.Fun.(type) {
			case *ast.Ident:
				if fn.Name == "reportVerdict" {
					drains = true
				}
			case *ast.SelectorExpr:
				if fn.Sel.Name == "WithoutCancel" {
					strips = true
				}
			}
			return true
		})
		if drains && strips {
			withoutCancel = true
		}
		return true
	})
	if !withoutCancel {
		t.Fatal("the deferred verdict drain passes the run context as-is; runTestSetCtxCancel has already fired by then, so every late verdict reaches the hook on a dead context")
	}
}

// runTestSetDecl parses replay.go and returns RunTestSet, for the two
// assertions above that have to read where a call sits rather than what it
// returns.
func runTestSetDecl(t *testing.T) *ast.FuncDecl {
	t.Helper()
	src, err := os.ReadFile("replay.go")
	if err != nil {
		t.Fatalf("reading replay.go: %v", err)
	}
	f, err := parser.ParseFile(token.NewFileSet(), "replay.go", src, 0)
	if err != nil {
		t.Fatalf("parsing replay.go: %v", err)
	}
	for _, d := range f.Decls {
		if fn, ok := d.(*ast.FuncDecl); ok && fn.Name.Name == "RunTestSet" {
			return fn
		}
	}
	t.Fatal("RunTestSet is gone or was renamed")
	return nil
}
