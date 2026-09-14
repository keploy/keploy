package replay

import (
	"go/ast"
	"go/parser"
	"go/token"
	"sort"
	"strings"
	"testing"
)

// The consumer half of the wiring pins.
//
// TestRunTestSetWiring (depresult_wiring_test.go) explains at length why the
// wiring of a 3000-line function nothing executes has to be read out of the
// source, and carries the argument-list rows. THIS file pins the shapes an
// identifier set cannot express: a `case models.CONSUMER:` arm inside a
// `switch testCase.Kind`, a struct-literal field, and an arm in a different
// function altogether.
//
// Every check here was derived from a MUTATION THAT SURVIVED. Deleting any one
// of the four consumer arms below left the whole suite green — the consumer
// unit tests exercise pure functions in isolation and nothing asserted that
// the replay loop calls them. A test whose failure mode is "the feature is
// silently disconnected" is exactly the class this file exists for.

// funcInFile returns the named top-level func declaration from file.
func funcInFile(t *testing.T, file, name string) (*token.FileSet, *ast.FuncDecl) {
	t.Helper()
	fset := token.NewFileSet()
	f, err := parser.ParseFile(fset, file, nil, 0)
	if err != nil {
		t.Fatalf("parse %s: %v", file, err)
	}
	for _, decl := range f.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != name {
			continue
		}
		return fset, fn
	}
	t.Fatalf("%s not found in %s", name, file)
	return nil, nil
}

// kindSwitchArm finds the `switch testCase.Kind` statements in fn and returns,
// for each one that carries a `models.CONSUMER` case, the identifier set of
// that case's body. A switch with no CONSUMER arm contributes nothing, which
// is what makes deleting an arm a failure here.
func kindSwitchArms(t *testing.T, fn *ast.FuncDecl, tag string) []map[string]bool {
	t.Helper()
	var out []map[string]bool
	ast.Inspect(fn, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok || sw.Tag == nil {
			return true
		}
		sel, ok := sw.Tag.(*ast.SelectorExpr)
		if !ok {
			return true
		}
		recv, ok := sel.X.(*ast.Ident)
		if !ok || recv.Name+"."+sel.Sel.Name != tag {
			return true
		}
		for _, stmt := range sw.Body.List {
			clause, ok := stmt.(*ast.CaseClause)
			if !ok {
				continue
			}
			consumer := false
			for _, expr := range clause.List {
				if printedIs(expr, "models.CONSUMER") {
					consumer = true
				}
			}
			if !consumer {
				continue
			}
			idents := map[string]bool{}
			for _, s := range clause.Body {
				ast.Inspect(s, func(n ast.Node) bool {
					if id, ok := n.(*ast.Ident); ok {
						idents[id.Name] = true
					}
					return true
				})
			}
			out = append(out, idents)
		}
		return true
	})
	return out
}

func printedIs(expr ast.Expr, want string) bool {
	sel, ok := expr.(*ast.SelectorExpr)
	if !ok {
		return false
	}
	pkg, ok := sel.X.(*ast.Ident)
	if !ok {
		return false
	}
	return pkg.Name+"."+sel.Sel.Name == want
}

// TestConsumerKindArmsExist pins that each `switch testCase.Kind` a consumer
// test has to pass through still has its arm, identified by something only
// that arm does.
func TestConsumerKindArmsExist(t *testing.T) {
	_, runTestSet := runTestSetSource(t)
	_, createFailed := funcInFile(t, "replay.go", "CreateFailedTestResult")

	tests := []struct {
		name string
		fn   *ast.FuncDecl
		want []string
		why  string
	}{
		{
			name: "the record window is read off the consumer spec",
			fn:   runTestSet,
			want: []string{"reqTime", "respTime", "testCase", "RecordWindow"},
			why: "Without this arm reqTime/respTime stay at their zero values and models.BaseTime is " +
				"sent to the agent as every consumer test's window, which for the timestamp fallback " +
				"selects every mock ever recorded.",
		},
		{
			name: "the judge is reached through the loop's Kind switch",
			fn:   runTestSet,
			want: []string{"consumerRes", "ConsumerResult", "CompareEffects", "testPass", "testResult"},
			why: "This is the arm that runs the judge at all. Renaming it to a Kind that never matches " +
				"leaves testPass at its default and every consumer test green.",
		},
		{
			name: "the verdict is turned into a persisted result",
			fn:   runTestSet,
			want: []string{"testCaseResult", "TestResult", "TestCaseID", "Consumer", "Info", "testStatus"},
			why: "Without this arm testCaseResult stays nil, the loop takes its 'test case result is " +
				"nil' branch, and no verdict reaches the report, JUnit or --format json.",
		},
		{
			name: "a failed simulation still produces a named consumer result",
			fn:   createFailed,
			want: []string{"CompareEffects", "ConsumerResult", "Refusal", "errorMessage"},
			why: "Without this arm a consumer test whose simulation failed outright is persisted with " +
				"an empty result: red, but naming nothing an agent loop can act on.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			arms := kindSwitchArms(t, tt.fn, "testCase.Kind")
			var seen []string
			for _, idents := range arms {
				missing := false
				for _, w := range tt.want {
					if !idents[w] {
						missing = true
						break
					}
				}
				if !missing {
					return
				}
				seen = append(seen, strings.Join(sortedKeys(idents), " "))
			}
			t.Fatalf("no `case models.CONSUMER:` arm in %s carries all of %v.\nfound these arms instead:\n\t%s\n\nWHY THIS MATTERS: %s",
				tt.fn.Name.Name, tt.want, strings.Join(seen, "\n\t"), tt.why)
		})
	}
}

// Backdate anchors generated TLS certificates. Read literally off
// testCases[0].HTTPReq.Timestamp it is the ZERO TIME for any test case that is
// not HTTP, so a consumer set anchored its certificates on wall-clock now
// rather than on the recording — and it panics outright on a nil first
// element. backdateFor is Kind-aware and nil-safe. TestBackdateFor covers the
// function; this covers that RunTestSet uses it, at BOTH construction sites.
func TestBackdateIsAssignedFromBackdateFor(t *testing.T) {
	_, fn := runTestSetSource(t)

	var got []string
	ast.Inspect(fn, func(n ast.Node) bool {
		kv, ok := n.(*ast.KeyValueExpr)
		if !ok {
			return true
		}
		key, ok := kv.Key.(*ast.Ident)
		if !ok || key.Name != "Backdate" {
			return true
		}
		got = append(got, strings.Join(sortedKeys(exprIdents([]ast.Expr{kv.Value})), " "))
		return true
	})

	sort.Strings(got)
	want := []string{"backdateFor testCases", "backdateFor testCases"}
	if strings.Join(got, " | ") != strings.Join(want, " | ") {
		t.Fatalf("the Backdate fields in RunTestSet are assigned from %v, want %v.\n\n"+
			"WHY THIS MATTERS: there are two models.OutgoingOptions construction sites — the "+
			"docker-compose branch and the native one — and reverting either to "+
			"testCases[0].HTTPReq.Timestamp leaves the other one covered and the suite green, "+
			"while that branch's consumer sets go back to a wall-clock-anchored certificate "+
			"and a panic on a nil first test case.", got, want)
	}
}

// THE UNARMED-CONSUMER-SET REFUSAL MUST STOP THE SET, not merely be called.
//
// It used to be called from inside two mocking-strategy branches which between
// them do not cover `!r.instrument && cmdType == DockerCompose`; a consumer set
// there was never refused for any reason. It is now one call on the path every
// branch converges on, and this pins BOTH halves that make it a refusal: it
// takes all four values the decision is made of, and its error returns
// TestSetStatusFailed rather than being logged and walked past.
//
// Dropping the `return` — the shape that made drainConsumerGate's verdict
// mutation-vacuous one file over — would leave a consumer suite running with
// the whole recorded pool resident, which is a red suite that is a lie or a
// green one for the same reason.
func TestTheUnarmedConsumerSetRefusalStopsTheSet(t *testing.T) {
	fset, fn := runTestSetSource(t)

	var found []*ast.IfStmt
	ast.Inspect(fn, func(n ast.Node) bool {
		stmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		assign, ok := stmt.Init.(*ast.AssignStmt)
		if !ok || len(assign.Rhs) != 1 {
			return true
		}
		call, ok := assign.Rhs[0].(*ast.CallExpr)
		if !ok || calleeName(call) != "r.refuseUnmappedConsumerSet" {
			return true
		}
		got := sortedKeys(exprIdents(call.Args))
		want := []string{"isMappingEnabled", "testCases", "testSetID", "useMappingBased"}
		if strings.Join(got, " ") != strings.Join(want, " ") {
			t.Errorf("refuseUnmappedConsumerSet is called with %v, want %v.\n\n"+
				"WHY THIS MATTERS: dropping isMappingEnabled or useMappingBased makes the refusal "+
				"decide on a subset of the conjuncts that decide whether the per-test mapping is "+
				"actually armed; the missing one is then a consumer suite replayed against the "+
				"whole recorded pool.", got, want)
		}
		found = append(found, stmt)
		return true
	})

	if len(found) != 1 {
		t.Fatalf("expected exactly one `if err := r.refuseUnmappedConsumerSet(...); err != nil` in RunTestSet, found %d.\n\n"+
			"WHY THIS MATTERS: zero means a consumer set with an unarmed mapping runs anyway. More "+
			"than one means the decision is duplicated per branch, which is how the "+
			"`!r.instrument && cmdType == DockerCompose` combination came to reach no call at all.", len(found))
	}
	const want = "{ return models.TestSetStatusFailed, err }"
	if got := printNode(t, fset, found[0].Body); got != want {
		t.Fatalf("the refusal now does %s, want %s.\n\n"+
			"WHY THIS MATTERS: logging the refusal and continuing runs exactly the configuration the "+
			"refusal exists to prevent — every test asserting some other test's message — and exits 0.",
			got, want)
	}
}
