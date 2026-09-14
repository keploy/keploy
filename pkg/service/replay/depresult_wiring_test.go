package replay

import (
	"go/ast"
	"go/parser"
	"go/printer"
	"go/token"
	"os"
	"sort"
	"strings"
	"testing"
)

// RunTestSet is ~2000 lines long and no test executes it: standing up its
// eight collaborator interfaces (TestDB, MockDB, MappingDB, ReportDB,
// TestSetConfig, Telemetry, Instrumentation, TestHooks) plus the simulate loop
// is a harness this slice does not justify. The consequence measured by a
// reviewer is that the DepResult writer could be replaced with a discard and
// the whole suite stayed green.
//
// The BEHAVIOUR of every extracted seam is table-tested above
// (resolveTestOutcome, resolveTestStatus, attachDepResults, buildDepResults,
// shouldEmitFailureLogs, recordUnexercised). What is still unpinned is the
// WIRING: that RunTestSet actually calls them, with the arguments that make
// them mean what the tests say they mean.
//
// So this pins the wiring by reading the source, at TWO levels of strictness,
// because neither alone is right for every site:
//
//   - IDENTIFIER SETS (TestRunTestSetWiring below) for argument lists. An
//     earlier version compared the printed expression everywhere and produced a
//     false failure during review when a behaviour-preserving argument rewrite
//     changed the text. Comparing identifier sets lets a semantically neutral
//     refactor through, while dropping an argument, hardcoding one to a
//     literal, or swapping in a different variable still fails.
//   - VERBATIM PRINTED EXPRESSIONS (TestSafetyPredicatesArePinnedVerbatim,
//     TestDepWriterGateIsPinnedVerbatim, TestOutcomeDrivesTheRunVerdict,
//     TestMockLookupCarriesATarget) for the handful of sites where the
//     STRUCTURE is the safety property. An identifier set cannot see an
//     operator flip: rewriting the five-conjunct `depAssertionValid` as
//     `... && hasExpectedMocks || instrumentConsumedFetchErr == nil` keeps every
//     identifier, makes the predicate unconditionally true in --base-path runs,
//     and left the whole package green.
//
// WHAT THIS APPROACH STILL CANNOT CATCH, stated so the next reviewer does not
// have to re-derive it: a semantic wiring bug that keeps the same identifiers
// and the same structure — passing a testCaseResult that is not the object
// InsertTestCaseResult persists, or the rows not surviving into the report
// file. The runtime proof for those is the e2e script in
// .github/workflows/test_workflow_scripts/golang/mock_mismatch, which asserts
// on the persisted report YAML, the process exit code and a per-test NDJSON
// delta rather than on log greps. That split — behaviour of every extracted
// seam in unit tests here, wiring by AST, end-to-end truth in the e2e — is the
// deliberate coverage story for this slice.
func runTestSetSource(t *testing.T) (*token.FileSet, *ast.FuncDecl) {
	t.Helper()
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "replay.go", nil, 0)
	if err != nil {
		t.Fatalf("parse replay.go: %v", err)
	}
	for _, decl := range file.Decls {
		fn, ok := decl.(*ast.FuncDecl)
		if !ok || fn.Name.Name != "RunTestSet" || fn.Recv == nil {
			continue
		}
		return fset, fn
	}
	t.Fatal("RunTestSet not found in replay.go")
	return nil, nil
}

// calleeName renders a call's callee as `foo` or `recv.foo`.
func calleeName(call *ast.CallExpr) string {
	switch fun := call.Fun.(type) {
	case *ast.Ident:
		return fun.Name
	case *ast.SelectorExpr:
		if recv, ok := fun.X.(*ast.Ident); ok {
			return recv.Name + "." + fun.Sel.Name
		}
		return fun.Sel.Name
	}
	return ""
}

// exprIdents collects every identifier appearing anywhere in the expressions,
// including the field names of selector chains (`r.config.Test.StrictFailure`
// contributes r, config, Test and StrictFailure). A literal contributes
// nothing, which is what makes `depAssertFail -> false` a failure.
func exprIdents(exprs []ast.Expr) map[string]bool {
	out := map[string]bool{}
	for _, e := range exprs {
		ast.Inspect(e, func(n ast.Node) bool {
			if ident, ok := n.(*ast.Ident); ok {
				out[ident.Name] = true
			}
			return true
		})
	}
	return out
}

// callArgIdents returns, for every call to `name` in fn, the identifier set of
// its arguments.
func callArgIdents(fn *ast.FuncDecl, name string) []map[string]bool {
	var out []map[string]bool
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || calleeName(call) != name {
			return true
		}
		out = append(out, exprIdents(call.Args))
		return true
	})
	return out
}

// assignRHSIdents returns, for every `name := rhs` / `name = rhs` in fn, the
// identifier set of the right-hand side.
func assignRHSIdents(fn *ast.FuncDecl, name string) []map[string]bool {
	var out []map[string]bool
	ast.Inspect(fn, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) == 0 {
			return true
		}
		ident, ok := assign.Lhs[0].(*ast.Ident)
		if !ok || ident.Name != name {
			return true
		}
		out = append(out, exprIdents(assign.Rhs))
		return true
	})
	return out
}

// printNode renders a node back to source on one line, so a test can pin the
// STRUCTURE of an expression and not just the identifiers in it.
func printNode(t *testing.T, fset *token.FileSet, n ast.Node) string {
	t.Helper()
	var sb strings.Builder
	if err := printer.Fprint(&sb, fset, n); err != nil {
		t.Fatalf("print node: %v", err)
	}
	return strings.Join(strings.Fields(sb.String()), " ")
}

func sortedKeys(m map[string]bool) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func TestRunTestSetWiring(t *testing.T) {
	_, fn := runTestSetSource(t)

	tests := []struct {
		name string
		// Exactly one of these two is used.
		call       string
		assignment string
		want       []string
		why        string
	}{
		{
			name:       "the dependency-assertion predicate still requires instrument mode",
			assignment: "depAssertionValid",
			want:       []string{"instrument", "useMappingBased", "isMappingEnabled", "hasExpectedMocks", "instrumentConsumedFetchErr"},
			why: "Dropping r.instrument would emit dependency rows in --base-path / remote-agent runs, " +
				"where the per-test mock mapping is never armed on the agent and 'which mock this request " +
				"consumed' is decided across all tests. Every row would be a false MISSING.",
		},
		{
			name:       "mockSetMismatch is computed under that same predicate",
			assignment: "mockSetMismatch",
			want:       []string{"isMockSubset", "filteredMockNames", "filteredExpectedNames"},
			why:        "The verdict signal and the rows that explain it must come from identical inputs.",
		},
		{
			name:       "the response diff is un-suppressed through the tested predicate",
			assignment: "emitFailureLogs",
			want:       []string{"shouldEmitFailureLogs", "mockSetMismatch", "AssertDependencies", "neverDemotable"},
			why: "Reverting this to `!mockSetMismatch` hands the user a test that --assert-dependencies " +
				"just marked FAILED with its response diff suppressed as 're-record noise'. It must be " +
				"the BY-KIND bit, not effectMockMissing: a consumer test whose mock set diverged only " +
				"on coordination traffic still has a verdict that came from the judge, and suppressing " +
				"its rows leaves a FAILED consumer test with no categories, no summary and no findings.",
		},
		{
			name:       "the verdict goes through the single seam, carrying the raw response result and all three knobs",
			assignment: "outcome",
			want: []string{"resolveTestOutcome", "testPass", "mockSetMismatch",
				"SchemaNoiseStrict", "AssertDependencies", "StrictFailure", "neverDemotable", "effectMockMissing"},
			why: "resolveTestOutcome is where --assert-dependencies turns a PASSED-and-green test into a " +
				"FAILED-and-red one, and where a consumer test's verdict is kept out of the OBSOLETE " +
				"demotion. BOTH trailing bits must reach it: neverDemotable stops a judge-FAILED " +
				"consumer test being graded obsolete, effectMockMissing promotes a judge-PASSED one " +
				"whose effect mock went unconsumed. Passing the same value for both, or a literal " +
				"false for either, compiles and stays green while reopening one of the two holes.",
		},
		{
			name:       "the resolved status is what gets persisted",
			assignment: "testStatus",
			want:       []string{"outcome", "Status"},
			why:        "Recomputing the status inline would let it drift from the verdict the seam decided.",
		},
		{
			name:       "the rows are built from the mapped expectation and the per-test consumed set",
			assignment: "depAssert",
			want: []string{"buildDepResults", "expectedMocks", "perTestConsumed", "depAssertionValid",
				"perTestConsumedKnown", "reusableMockNames", "mockKindByName", "mockLookup"},
			why: "This is the FIRST WRITER of models.Result.DepResult (design v2 §2, §7 slice 4). " +
				"Deleting it makes the whole slice a no-op.",
		},
		{
			name: "the rows are attached to the persisted result",
			call: "attachDepResults",
			want: []string{"testCaseResult", "testStatus", "depAssert", "AssertDependencies"},
			why: "Without this call the rows are computed and thrown away: nothing reaches the report, " +
				"JUnit, --format json, or the DEPENDENCY_MISSING category.",
		},
		{
			name: "the per-test-set set the summary reads is actually filled",
			call: "recordUnexercised",
			want: []string{"depMissingTests", "testCase", "Name", "testStatus", "depLevel"},
			why: "The summary Warn is the only WARN-level surface for the knob-off case. Never inserting " +
				"into the map it reads makes that summary a silent no-op even though the call to it survives.",
		},
		{
			name: "the inert-knob warning is still emitted, and is told whether anything was eligible",
			call: "r.warnDependencyAssertionInert",
			want: []string{"testSetID", "useMappingBased", "isMappingEnabled", "instrumentConsumedFetchErr", "noEligibleDeps", "depAssertionInertWarned"},
			why: "Without it, --assert-dependencies against an unmapped or mapping-disabled test set is a " +
				"green run for an assertion that never executed. noEligibleDeps is the fifth reason and the " +
				"one an ordinary recording hits: every mapped dependency filtered out as session/connection " +
				"tier, so the assertion reports NOT CHECKED and the user is told why. Dropping the argument " +
				"leaves those runs with a report full of `dependencies_checked: false` and no explanation. " +
				"instrumentConsumedFetchErr is the fourth reason: when the per-test consumed fetch fails the " +
				"assertion cannot run either, and dropping it makes the run either say nothing at all or " +
				"blame the recording's tier for a transport error.",
		},
		{
			name:       "the consumer judge is what decides a consumer test",
			assignment: "testPass",
			want:       []string{"r", "CompareEffects", "testCase", "consumerRes", "testSetID", "emitFailureLogs", "consumerDep"},
			why: "CompareEffects IS the slice: the pairing, the lanes, the payload diff, the count " +
				"assertion and every refusal. Replacing this assignment with `true, &models.Result{}` " +
				"compiles, leaves every consumer unit test green — they exercise pure functions in " +
				"isolation — and passes every consumer test in the suite. Passing anything other than " +
				"the agent's own result for THIS test judges one test's effects against another's spec.",
		},
		{
			name:       "the consumer judge is told whether the sync path's dependency assertion ran",
			assignment: "consumerDep",
			want:       []string{"newConsumerDepAssertion", "hasExpectedMocks", "depAssertionValid", "filteredExpectedNames", "mockLookup"},
			why: "A consume-and-write-to-a-database consumer test asserts NOTHING on its own: its whole " +
				"claim is the sync path's deps[i] presence rows, and spec.SideEffects is a record-time " +
				"count nothing at replay turns into an assertion. A literal consumerDepAssertion{Ran: " +
				"true} here compiles and reports such a test PASSED with zero assertions executed " +
				"whenever it has no usable mapping — design §5's false-pass row 0. mockLookup is " +
				"load-bearing: without it the predicate degenerates to 'the mapping is non-empty', " +
				"which the test's OWN TRIGGER satisfies.",
		},
		{
			name: "the delivery gate is returned to boot at the start of the set",
			call: "r.resetConsumerGate",
			want: []string{"runTestSetCtx", "testSetID", "testCases"},
			why: "--keep-app-alive reuses one application process across test sets, so a gate left armed " +
				"by an interrupted set leaks onto this set's first test. It takes testCases because " +
				"containsConsumerTest is what keeps an HTTP-only suite from making an agent round trip " +
				"per test set; the hook it used to live in has no test cases in scope.",
		},
		{
			name: "the delivery gate is drained at the end of the set, not only at the start",
			call: "r.drainConsumerGate",
			want: []string{"runTestSetCtx", "testSetID", "testCases"},
			why: "An effect that lands after a test's grace fails the NEXT test as an extra. The last " +
				"test of the last set has no next test, and the reset in BeforeTestSetReplay runs " +
				"before a set, never after one — so without this call an N+1 emission at the very end " +
				"of a run produces no row, no log line and no non-zero exit.",
		},
		{
			name: "the per-test-set summary of unexercised dependencies is still emitted",
			call: "r.warnUnexercisedDependencies",
			want: []string{"testSetID", "depMissingTests"},
			why:  "It is the only WARN-level surface for the knob-off case; the per-test line is Debug.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []map[string]bool
			if tt.assignment != "" {
				got = assignRHSIdents(fn, tt.assignment)
			} else {
				got = callArgIdents(fn, tt.call)
			}

			var seen []string
			for _, idents := range got {
				missing := make([]string, 0, len(tt.want))
				for _, w := range tt.want {
					if !idents[w] {
						missing = append(missing, w)
					}
				}
				if len(missing) == 0 {
					return
				}
				seen = append(seen, strings.Join(sortedKeys(idents), " "))
			}
			target := tt.call
			if tt.assignment != "" {
				target = tt.assignment + " := ..."
			}
			t.Fatalf("RunTestSet's %s no longer passes all of %v.\nfound these identifier sets instead:\n\t%s\n\nWHY THIS MATTERS: %s",
				target, tt.want, strings.Join(seen, "\n\t"), tt.why)
		})
	}
}

// The deferred streaming path must NOT write DepResult: its expected-mock list
// is an un-tier-filtered slice built before the run, so reusable/startup mocks
// would show up as per-test dependencies and go "missing" at random.
func TestStreamingPathDoesNotWriteDepResults(t *testing.T) {
	_, fn := runTestSetSource(t)
	if calls := callArgIdents(fn, "buildDepResults"); len(calls) != 1 {
		t.Fatalf("expected exactly one buildDepResults call in RunTestSet, found %d", len(calls))
	}
	if calls := callArgIdents(fn, "attachDepResults"); len(calls) != 1 {
		t.Fatalf("expected exactly one attachDepResults call in RunTestSet, found %d", len(calls))
	}
}

// ancestorsOf returns the node chain from fn down to the innermost node whose
// position range contains pos, outermost first.
func ancestorsOf(fn *ast.FuncDecl, pos token.Pos) []ast.Node {
	var chain []ast.Node
	ast.Inspect(fn, func(n ast.Node) bool {
		if n == nil {
			return false
		}
		if pos < n.Pos() || pos >= n.End() {
			return false
		}
		chain = append(chain, n)
		return true
	})
	return chain
}

// posOfCall returns the position of the single call to name inside fn.
func posOfCall(t *testing.T, fn *ast.FuncDecl, name string) token.Pos {
	t.Helper()
	var found []token.Pos
	ast.Inspect(fn, func(n ast.Node) bool {
		if call, ok := n.(*ast.CallExpr); ok && calleeName(call) == name {
			found = append(found, call.Pos())
		}
		return true
	})
	if len(found) != 1 {
		t.Fatalf("expected exactly one call to %s in RunTestSet, found %d", name, len(found))
	}
	return found[0]
}

// posOfAssign returns the position of the single `name = rhs` / `name := rhs`
// statement inside fn whose right-hand side mentions rhsIdent. rhsIdent
// disambiguates variables the 3000-line function assigns from more than one
// code path — testStatus is also set by the deferred streaming path, which
// deliberately writes no DepResult at all.
func posOfAssign(t *testing.T, fn *ast.FuncDecl, name, rhsIdent string) token.Pos {
	t.Helper()
	var found []token.Pos
	ast.Inspect(fn, func(n ast.Node) bool {
		assign, ok := n.(*ast.AssignStmt)
		if !ok || len(assign.Lhs) == 0 {
			return true
		}
		ident, ok := assign.Lhs[0].(*ast.Ident)
		if !ok || ident.Name != name {
			return true
		}
		if rhsIdent != "" && !exprIdents(assign.Rhs)[rhsIdent] {
			return true
		}
		found = append(found, assign.Pos())
		return true
	})
	if len(found) != 1 {
		t.Fatalf("expected exactly one assignment to %s (rhs mentioning %q) in RunTestSet, found %d",
			name, rhsIdent, len(found))
	}
	return found[0]
}

// TestRunTestSetWiring above pins WHAT each call is passed. It does not pin
// that the calls RUN: a reviewer wrapped the whole writer block in
// `if depAssertionValid && false { ... }`, keeping every call and every
// argument byte-identical, and the package stayed green. Nothing in the repo
// executes RunTestSet, so reachability has to be read out of the source too.
//
// This pins the ENCLOSING CONDITIONS as an exact list. Adding any guard around
// the writer — a `&& false`, a new flag, a duplicate of an existing check —
// changes the list and fails here with the reason attached. Removing one of
// the two nil guards fails too, which is deliberate: they are what keep the
// writer from dereferencing a nil result.
func TestDepWriterIsReachable(t *testing.T) {
	_, fn := runTestSetSource(t)
	pos := posOfAssign(t, fn, "depAssert", "buildDepResults")

	var conds []string
	for _, n := range ancestorsOf(fn, pos) {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok {
			continue
		}
		// An ancestor if whose ELSE branch contains the writer is a different
		// (and much stranger) shape; record it distinctly rather than
		// pretending the condition guards the writer.
		if ifStmt.Else != nil && pos >= ifStmt.Else.Pos() && pos < ifStmt.Else.End() {
			conds = append(conds, "else:"+strings.Join(sortedKeys(exprIdents([]ast.Expr{ifStmt.Cond})), " "))
			continue
		}
		conds = append(conds, strings.Join(sortedKeys(exprIdents([]ast.Expr{ifStmt.Cond})), " "))
	}

	want := []string{"nil testResult", "nil testCaseResult"}
	if strings.Join(conds, " | ") != strings.Join(want, " | ") {
		t.Fatalf("the DepResult writer's enclosing conditions are %v, want %v.\n\n"+
			"WHY THIS MATTERS: the wiring test above compares the arguments each call is passed, "+
			"not whether the block runs. Wrapping the block in `if depAssertionValid && false` "+
			"kept every call and argument identical and left the whole package green. The only "+
			"conditions allowed to guard the writer are the two nil checks that protect the "+
			"result pointers; gating it on anything else (including the assertion predicate, "+
			"which buildDepResults already takes as an argument) makes the rows conditional on "+
			"something no test can see.", conds, want)
	}
}

// The DEPENDENCY_MISSING label attachDepResults applies is keyed off the
// PERSISTED status, so the writer has to run after the verdict is folded into
// testStatus. Reordering the block above `testStatus = outcome.Status` would
// label the test from a stale status — an OBSOLETE test that the knob just
// promoted to FAILED would be labelled from the pre-promotion value — while
// leaving every argument and every enclosing condition untouched.
func TestDepWriterRunsAfterTheStatusIsResolved(t *testing.T) {
	_, fn := runTestSetSource(t)
	statusAt := posOfAssign(t, fn, "testStatus", "outcome")

	// Every reader of the resolved status has to come after the assignment.
	// Checking only attachDepResults is not enough: moving the assignment DOWN
	// to just above the writer keeps that one ordering intact while feeding
	// the per-status counters (currentSuccess / currentObsolete /
	// currentFailures) and the persisted testStatus a stale value.
	readers := map[string]token.Pos{
		"attachDepResults(...)":   posOfCall(t, fn, "attachDepResults"),
		"switch testStatus {...}": posOfStatusSwitch(t, fn),
	}
	for what, at := range readers {
		if statusAt >= at {
			t.Fatalf("`testStatus = outcome.Status` (pos %d) does not precede %s (pos %d).\n\n"+
				"WHY THIS MATTERS: attachDepResults applies the DEPENDENCY_MISSING label from the "+
				"PERSISTED status, and the switch below drives the per-status counters. A test the "+
				"--assert-dependencies promotion just moved from OBSOLETE to FAILED would be "+
				"labelled and counted from its pre-promotion value, with every call argument and "+
				"every enclosing condition left untouched.", statusAt, what, at)
		}
	}
}

// posOfStatusSwitch returns the position of `switch testStatus { ... }`.
func posOfStatusSwitch(t *testing.T, fn *ast.FuncDecl) token.Pos {
	t.Helper()
	var found []token.Pos
	ast.Inspect(fn, func(n ast.Node) bool {
		sw, ok := n.(*ast.SwitchStmt)
		if !ok {
			return true
		}
		if ident, ok := sw.Tag.(*ast.Ident); ok && ident.Name == "testStatus" {
			found = append(found, sw.Pos())
		}
		return true
	})
	if len(found) != 1 {
		t.Fatalf("expected exactly one `switch testStatus` in RunTestSet, found %d", len(found))
	}
	return found[0]
}

// The third invisible mutation: an unconditional `continue` / `return` /
// `break` inserted above the writer in its own block leaves the arguments, the
// enclosing conditions and the ordering all intact.
func TestNothingUnconditionallySkipsTheDepWriter(t *testing.T) {
	_, fn := runTestSetSource(t)
	pos := posOfAssign(t, fn, "depAssert", "buildDepResults")

	chain := ancestorsOf(fn, pos)
	var block *ast.BlockStmt
	for _, n := range chain {
		if b, ok := n.(*ast.BlockStmt); ok {
			block = b
		}
	}
	if block == nil {
		t.Fatal("could not find the block statement holding the DepResult writer")
	}
	for _, stmt := range block.List {
		if stmt.Pos() >= pos {
			break
		}
		switch s := stmt.(type) {
		case *ast.BranchStmt:
			t.Fatalf("an unconditional %s at position %d precedes the DepResult writer in its own block; "+
				"the writer is unreachable", s.Tok, s.Pos())
		case *ast.ReturnStmt:
			t.Fatalf("an unconditional return at position %d precedes the DepResult writer in its own block; "+
				"the writer is unreachable", s.Pos())
		}
	}
}

// nonDemotable must be resolved FROM THE TEST CASE'S KIND, not from a config
// knob and not from a literal. The identifier-set test above proves it reaches
// resolveTestOutcome and shouldEmitFailureLogs; this proves it means what it
// says. `nonDemotable := false` would satisfy every other test in this file
// and would restore, for every consumer test, exactly the silent OBSOLETE
// demotion that routes "the worker stopped producing" to a green run.
func TestNonDemotableIsResolvedFromTheTestCaseKind(t *testing.T) {
	fset, fn := runTestSetSource(t)

	// TWO ASSIGNMENTS, TWO DIFFERENT CLAIMS, AND THEY MAY NOT BE THE SAME
	// EXPRESSION. Collapsing them — either direction — reopens a hole:
	// giving the promotion the by-Kind bit fails every clean consumer test
	// whose coordination mock went unconsumed; giving the demotion veto the
	// narrow bit persists a judge-FAILED consumer test as OBSOLETE with the
	// run exiting 0.
	pins := []struct {
		assignment string
		want       string
		why        string
	}{
		{
			assignment: "neverDemotable",
			want:       "neverDemotableKind(testCase.Kind)",
			why: "This is the whole of the consumer contract's first rule: a consumer test the JUDGE " +
				"failed may never be graded OBSOLETE, whatever mock happened to go unconsumed. A " +
				"literal false here compiles, keeps every identifier the wiring test looks for, and " +
				"silently demotes every failing consumer test — which does not fail the test set and " +
				"does not change the exit code. Narrowing it to 'an effect mock went unconsumed' does " +
				"the same thing for the trigger-only and coordination-only mock sets.",
		},
		{
			assignment: "effectMockMissing",
			want:       "missingEffectMockPromotes(testCase.Kind, filteredExpectedNames, filteredMockNames, mockLookup)",
			why: "This is the PROMOTION: a consumer test whose effects compared clean but whose effect " +
				"mock was never consumed. It must stay narrow — promoting on coordination traffic a " +
				"healthy client skips is a false RED — and it must stay computed, because a literal " +
				"false restores the silent green for 'the worker stopped producing'.",
		},
	}

	for _, pin := range pins {
		t.Run(pin.assignment, func(t *testing.T) {
			var got []string
			ast.Inspect(fn, func(n ast.Node) bool {
				assign, ok := n.(*ast.AssignStmt)
				if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
					return true
				}
				ident, ok := assign.Lhs[0].(*ast.Ident)
				if !ok || ident.Name != pin.assignment {
					return true
				}
				got = append(got, printNode(t, fset, assign.Rhs[0]))
				return true
			})
			for _, g := range got {
				if g == pin.want {
					return
				}
			}
			t.Fatalf("no assignment to %s has the pinned right-hand side.\nfound: %v\nwant:  %q\n\nWHY THIS MATTERS: %s",
				pin.assignment, got, pin.want, pin.why)
		})
	}
}

// TestRunTestSetWiring compares identifier SETS, which is deliberately tolerant
// of a neutral argument rewrite — and therefore blind to a rewrite of the
// BOOLEAN ALGEBRA. Two mutations proved it:
//
//	depAssertionValid := ... && hasExpectedMocks || instrumentConsumedFetchErr == nil
//	buildDepResults(..., depAssertionValid || perTestConsumedKnown, ...)
//
// Both kept every identifier and left the whole package green. The first is the
// worse one: && binds tighter than ||, so the predicate reads
// (A && B && C && D) || (err == nil), and in --base-path / remote-agent runs
// instrumentConsumedFetchErr is always nil — the predicate becomes
// unconditionally TRUE, mockSetMismatch is computed against a mapping that was
// never armed, healthy tests are demoted to OBSOLETE and every dependency is
// reported MISSING.
//
// A five-conjunct safety predicate is worth pinning verbatim.
func TestSafetyPredicatesArePinnedVerbatim(t *testing.T) {
	fset, fn := runTestSetSource(t)

	tests := []struct {
		name       string
		assignment string
		want       string
		why        string
	}{
		{
			name:       "the dependency-assertion predicate is a pure conjunction",
			assignment: "depAssertionValid",
			want: "r.instrument && useMappingBased && isMappingEnabled && hasExpectedMocks && " +
				"instrumentConsumedFetchErr == nil",
			why: "Every conjunct removes a mode in which 'which mock this request consumed' is not " +
				"attributable to one test. Turning any && into || makes the predicate true in modes " +
				"where the per-test mapping was never armed: healthy tests get demoted to OBSOLETE " +
				"and every dependency is reported MISSING.",
		},
		{
			name:       "the resolved verdict is folded back into testPass, not recomputed",
			assignment: "testPass",
			want:       "outcome.Status == models.TestStatusPassed",
			why: "This is how the --assert-dependencies promotion reaches the 'result' log line and " +
				"every later reader. Recomputing it from the raw response result silently drops the " +
				"promotion.",
		},
		{
			name:       "the no-eligible-dependency signal means EVERY mapped entry was filtered out",
			assignment: "noEligibleDeps",
			want:       "len(expectedMocks) > 0 && len(filteredExpectedNames) == 0",
			why: "It must be computed from the same two lists the assertion uses, or the warning claims " +
				"something buildDepResults disagrees with. Turning the && into || fires it on every test " +
				"that maps a dependency (training users to ignore the line); dropping the len(expectedMocks) " +
				"conjunct fires it on every test that makes no outgoing calls at all, which is normal.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var got []string
			ast.Inspect(fn, func(n ast.Node) bool {
				assign, ok := n.(*ast.AssignStmt)
				if !ok || len(assign.Lhs) != 1 || len(assign.Rhs) != 1 {
					return true
				}
				ident, ok := assign.Lhs[0].(*ast.Ident)
				if !ok || ident.Name != tt.assignment {
					return true
				}
				got = append(got, printNode(t, fset, assign.Rhs[0]))
				return true
			})
			for _, g := range got {
				if g == tt.want {
					return
				}
			}
			t.Fatalf("no assignment to %s has the pinned right-hand side.\nfound: %v\nwant:  %q\n\n"+
				"WHY THIS MATTERS: %s\n\nIf the rewrite is deliberate, update this literal — that is "+
				"the review step an identifier-set comparison cannot force.",
				tt.assignment, got, tt.want, tt.why)
		})
	}
}

// The `valid` argument buildDepResults takes is the same class of predicate and
// is pinned the same way: `depAssertionValid || perTestConsumedKnown` keeps
// both identifiers and inverts the meaning.
func TestDepWriterGateIsPinnedVerbatim(t *testing.T) {
	fset, fn := runTestSetSource(t)

	const want = "depAssertionValid && perTestConsumedKnown"
	var got []string
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || calleeName(call) != "buildDepResults" {
			return true
		}
		// buildDepResults(expected, consumed, valid, reusable, kinds, lookup)
		if len(call.Args) < 3 {
			t.Fatalf("buildDepResults is called with %d arguments", len(call.Args))
		}
		got = append(got, printNode(t, fset, call.Args[2]))
		return true
	})

	for _, g := range got {
		if g == want {
			return
		}
	}
	t.Fatalf("buildDepResults' `valid` argument is %v, want %q.\n\n"+
		"WHY THIS MATTERS: this gate must be a CONJUNCTION. Turning it into `||` makes the writer "+
		"emit rows in modes where the per-test mock mapping was never armed on the agent, so every "+
		"dependency reads as MISSING for a healthy run.", got, want)
}

// posOfIf returns the single *ast.IfStmt inside fn whose condition prints
// exactly cond.
func posOfIf(t *testing.T, fset *token.FileSet, fn *ast.FuncDecl, cond string) *ast.IfStmt {
	t.Helper()
	var found []*ast.IfStmt
	ast.Inspect(fn, func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok {
			return true
		}
		if printNode(t, fset, ifStmt.Cond) == cond {
			found = append(found, ifStmt)
		}
		return true
	})
	if len(found) != 1 {
		t.Fatalf("expected exactly one `if %s` in RunTestSet, found %d", cond, len(found))
	}
	return found[0]
}

// THE LAST MUTATION-VACUOUS LINE IN THE CHAIN. Replacing
// `if outcome.FailsTestSet {` with `if false && outcome.FailsTestSet {` left the
// whole package green: --assert-dependencies still wrote FAILED into the report
// but the run exited 0 — precisely the false-green the slice exists to close.
//
// That single line is the entire path from the slice's headline promotion to
// the user-visible outcome: RunTestSet returns testSetStatus -> testRunResult ->
// `utils.ErrCode = 1` -> the process exit code. TestRunTestSetWiring pins ten
// other seams and omitted this one.
//
// The second row is the same class with a smaller blast radius: neutering the
// RecordMismatch guard silently empties the end-of-run mock-mismatch report.
//
// The third row is the consumer twin of the first, on the suffix of a run
// nothing else watches: `_ = r.drainConsumerGate(...)` left the whole suite
// green while a trailing effect after the last test of the last set produced
// an ERROR line and exit 0.
func TestOutcomeDrivesTheRunVerdict(t *testing.T) {
	fset, fn := runTestSetSource(t)

	tests := []struct {
		name string
		cond string
		want string
		why  string
	}{
		{
			name: "FailsTestSet is what makes the run red and the exit code non-zero",
			cond: "outcome.FailsTestSet",
			want: "{ testSetStatus = models.TestSetStatusFailed }",
			why: "RunTestSet returns testSetStatus, testRunResult turns it into utils.ErrCode = 1, " +
				"and that is the process exit code. Without this assignment --assert-dependencies " +
				"writes FAILED into the report and the run still exits 0 — the exact false-green " +
				"this slice exists to close.",
		},
		{
			name: "RecordMismatch is what fills the end-of-run mock-mismatch report",
			cond: "outcome.RecordMismatch",
			want: "{ r.mockMismatchFailures.AddFailure(testSetID, testCase.Name, filteredExpectedNames, filteredMockNames) }",
			why: "It is the only per-test-set record of which expected mocks diverged; an empty " +
				"store renders an end-of-run report that says nothing diverged.",
		},
		{
			name: "a trailing effect after the last test of the set makes the run red",
			cond: "r.drainConsumerGate(runTestSetCtx, testSetID, testCases)",
			want: "{ testSetStatus = models.TestSetStatusFailed }",
			why: "drainConsumerGate itself bites (TestDrainConsumerGate), but the line that CONSUMES " +
				"its verdict did not: rewriting it as `_ = r.drainConsumerGate(...)` left the WHOLE " +
				"suite green, because the only other pin on it (the r.drainConsumerGate row in " +
				"TestRunTestSetWiring) asserts the call exists with the right arguments and says " +
				"nothing about the result being used. This is the sole place an over-production " +
				"after the LAST test of the LAST set can be seen — the reset in BeforeTestSetReplay " +
				"runs before a set, never after one — so discarding it reopens the N+1 regression " +
				"with a loud ERROR line and exit 0.",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ifStmt := posOfIf(t, fset, fn, tt.cond)
			if ifStmt.Else != nil {
				t.Fatalf("`if %s` grew an else branch; this guard is meant to be a plain one-way gate", tt.cond)
			}
			if got := printNode(t, fset, ifStmt.Body); got != tt.want {
				t.Fatalf("`if %s` now does %s, want %s.\n\nWHY THIS MATTERS: %s",
					tt.cond, got, tt.want, tt.why)
			}
		})
	}
}

// The mock display lookup is what gives a dependency row a human-meaningful
// target. mockTargetFromSpec is thoroughly table-tested, but its SINGLE
// production call site was not pinned: replacing `target: mockTargetFromSpec(mock)`
// with `target: ""` left the package green, and every dependency row would
// degrade to `deps[0] postgres (presence)` — five calls to one service
// distinguishable only by an unstable index, which is the finding this lookup
// field exists to fix.
func TestMockLookupCarriesATarget(t *testing.T) {
	fset, fn := runTestSetSource(t)

	var found []string
	ast.Inspect(fn, func(n ast.Node) bool {
		lit, ok := n.(*ast.CompositeLit)
		if !ok {
			return true
		}
		ident, ok := lit.Type.(*ast.Ident)
		if !ok || ident.Name != "mockDisplayInfo" {
			return true
		}
		found = append(found, printNode(t, fset, lit))
		return true
	})

	if len(found) != 1 {
		t.Fatalf("expected exactly one mockDisplayInfo literal in RunTestSet, found %d: %v", len(found), found)
	}
	// `role` and `kind` are pinned alongside `target` because they carry the
	// consumer contract's non-demotion: without them every unconsumed
	// per-test mock looks like an effect mock, and a per-test coordination
	// call the client legitimately skipped fails a clean consumer test with a
	// message about the worker's production.
	const want = "mockDisplayInfo{ summary: models.MockSummaryFromSpec(mock), protocol: string(mock.Kind), target: mockTargetFromSpec(mock), kind: mock.Kind, role: mock.Spec.Metadata[models.MetaKeyRole], }"
	if found[0] != want {
		t.Fatalf("the mock display lookup is now %s, want %s.\n\n"+
			"WHY THIS MATTERS: `target` is the only human-meaningful destination a dependency row "+
			"can carry — neither models.MockEntry (the mapping side) nor models.MockState (the "+
			"consumed side) records one. Dropping it collapses five outgoing calls to the same "+
			"service into rows that differ only by index. `kind`/`role` are what scope the "+
			"CONSUMER non-demotion to effect mocks; dropping them makes it fire on every "+
			"unconsumed coordination mock.", found[0], want)
	}
}

// THE ELIGIBILITY HALF OF THE INERT-KNOB WARNING.
//
// buildDepResults now reports NOT-CHECKED when the recording maps dependencies
// to a test and every one of them is filtered out (DNS, or reusable
// session/connection tier — which is what models.Mock.DeriveLifetime makes of
// untagged HTTP/Postgres/MySQL egress, so it is the ORDINARY case). That is the
// honest answer, but on its own it is an unexplained `dependencies_checked:
// false` on every test of the run, indistinguishable from a --base-path run.
//
// The explanation is the fifth inert reason, and it can only be produced if the
// main replay loop tells the warner what it observed. TestRunTestSetWiring pins
// that the argument is passed; this pins that the MAIN LOOP is the caller that
// passes a computed value and that the streaming pass — which has no filtered
// expectation list — passes a literal false rather than something that would
// make it claim a tier problem it never measured.
func TestMainLoopReportsEligibilityToTheWarner(t *testing.T) {
	fset, fn := runTestSetSource(t)

	const warner = "r.warnDependencyAssertionInert"
	// (testSetID, useMappingBased, isMappingEnabled, consumedFetchFailed,
	//  deferredStreaming, noEligibleDeps, warned)
	const eligibilityArg = 5

	var args []string
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || calleeName(call) != warner {
			return true
		}
		if len(call.Args) <= eligibilityArg {
			t.Fatalf("%s takes %d argument(s); noEligibleDeps is argument %d", warner, len(call.Args), eligibilityArg+1)
		}
		args = append(args, printNode(t, fset, call.Args[eligibilityArg]))
		return true
	})

	sort.Strings(args)
	want := []string{"false", "noEligibleDeps"}
	if strings.Join(args, " | ") != strings.Join(want, " | ") {
		t.Fatalf("%s is passed noEligibleDeps=%v across its call sites, want %v.\n\n"+
			"WHY THIS MATTERS: exactly one call site (the main replay loop) can observe that every mapped "+
			"dependency of a test was filtered out; passing a literal false there deletes the only "+
			"explanation the user gets for a run where every test reports dependencies_checked=false. "+
			"The Phase-2 streaming pass has no filtered expectation list, so passing anything but false "+
			"there makes it claim a tier problem it never measured.", warner, args, want)
	}
}

// M-1: THE STREAMING HALF OF THE INERT-KNOB WARNING.
//
// RunTestSet splits SSE/chunked test cases out of the main replay loop and
// replays them in a Phase-2 pass that deliberately writes no DepResult and
// never calls resolveTestOutcome, so --assert-dependencies cannot promote
// anything there. The warning that says so lives in that pass and nowhere
// else: TestRunTestSetWiring's "the inert-knob warning is still emitted" row
// is satisfied by the Phase-1 call alone, so deleting the Phase-2 one restores
// exactly the silent green this slice exists to close — a CI run that points
// --assert-dependencies at a set of streaming tests, gets no dependency
// assertion at all, and exits 0.
//
// The BEHAVIOUR of the warner (scope-accurate message, one line per test set,
// silence when the knob is off) is table-tested in
// TestWarnDependencyAssertionInert. This pins that the streaming pass reaches
// it, and reaches it with deferredStreaming set — passing a bare `false` there
// would type-check, keep every identifier, and warn about nothing.
func TestStreamingPassWarnsTheAssertionDidNotRun(t *testing.T) {
	fset, fn := runTestSetSource(t)

	const warner = "r.warnDependencyAssertionInert"
	// deferredStreaming is the 5th parameter: (testSetID, useMappingBased,
	// isMappingEnabled, consumedFetchFailed, deferredStreaming, noEligibleDeps,
	// warned).
	const streamingArg = 4

	calls := callArgIdents(fn, warner)
	if len(calls) != 2 {
		t.Fatalf("RunTestSet calls %s %d time(s), want 2 (the main replay loop and the deferred streaming pass).\n\n"+
			"WHY THIS MATTERS: the streaming pass runs no dependency assertion at all. Without its own "+
			"call, --assert-dependencies over a set of SSE/chunked tests is silently inert and exits 0.",
			warner, len(calls))
	}

	// The Phase-2 block: `if ... len(streamingTests) > 0 { for ... range streamingTests { ... } }`.
	var inStreamingBlock []*ast.CallExpr
	ast.Inspect(fn, func(n ast.Node) bool {
		ifStmt, ok := n.(*ast.IfStmt)
		if !ok || ifStmt.Cond == nil || !exprIdents([]ast.Expr{ifStmt.Cond})["streamingTests"] {
			return true
		}
		rangesOverStreamingTests := false
		ast.Inspect(ifStmt.Body, func(m ast.Node) bool {
			rng, ok := m.(*ast.RangeStmt)
			if ok && exprIdents([]ast.Expr{rng.X})["streamingTests"] {
				rangesOverStreamingTests = true
			}
			return true
		})
		if !rangesOverStreamingTests {
			return true
		}
		ast.Inspect(ifStmt.Body, func(m ast.Node) bool {
			if call, ok := m.(*ast.CallExpr); ok && calleeName(call) == warner {
				inStreamingBlock = append(inStreamingBlock, call)
			}
			return true
		})
		return true
	})

	if len(inStreamingBlock) != 1 {
		t.Fatalf("the deferred-streaming block contains %d call(s) to %s, want exactly 1.\n\n"+
			"WHY THIS MATTERS: cli/provider/cmd.go's --assert-dependencies help promises one warning per "+
			"test set whenever the assertion cannot run. A streaming test set with no warning makes that "+
			"promise false and hands CI a green run for an assertion that never executed.",
			len(inStreamingBlock), warner)
	}

	call := inStreamingBlock[0]
	if len(call.Args) <= streamingArg {
		t.Fatalf("%s in the streaming block takes %d argument(s); deferredStreaming is argument %d",
			warner, len(call.Args), streamingArg+1)
	}
	if got := printNode(t, fset, call.Args[streamingArg]); got != "true" {
		t.Fatalf("the streaming pass passes deferredStreaming=%s, want true.\n\n"+
			"WHY THIS MATTERS: reaching that block means len(streamingTests) > 0. Anything but a "+
			"literal true makes the call reachable but inert — dependencyAssertionInertReason returns "+
			"\"\" for a healthy test set, so the warning silently never fires.", got)
	}

	// The other half: the main replay loop only ever sees the non-streaming
	// bucket, so IT must not claim a deferral. Passing len(streamingTests) > 0
	// there would fire the streaming warning before a streaming test was even
	// reached and, via the shared latch, suppress a real precondition failure.
	var mainLoopArg string
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || calleeName(call) != warner || call == inStreamingBlock[0] || len(call.Args) <= streamingArg {
			return true
		}
		mainLoopArg = printNode(t, fset, call.Args[streamingArg])
		return true
	})
	if mainLoopArg != "false" {
		t.Fatalf("the main replay loop passes deferredStreaming=%s, want false.\n\n"+
			"WHY THIS MATTERS: that loop only ever sees the non-streaming bucket. Claiming a deferral "+
			"there warns about streaming before any streaming test runs and, because both call sites "+
			"share one latch, swallows the set-wide precondition warning that should have been emitted.",
			mainLoopArg)
	}
}

// THE FETCH-FAILURE HALF OF THE INERT-KNOB WARNING.
//
// dependencyAssertionInertReason's fourth arm reports that the per-test
// consumed-mock fetch failed. Only the main replay loop knows that — it is the
// function that performs the fetch and stores instrumentConsumedFetchErr — so
// the reason can only ever fire if the loop passes what it observed.
//
// Passing a literal false there type-checks, keeps the call, keeps every other
// identifier, and restores two separate silences: a test set whose fetch failed
// and whose mapping HAS eligible dependencies goes back to reporting
// `dependencies_checked: false` with no warning at all, and one whose mapping
// is also all-reusable goes back to being told the recording's tier is at
// fault when the actionable cause was a transport error. Both keep the suite
// green, which is why this is pinned by shape rather than by behaviour.
func TestMainLoopReportsTheConsumedFetchOutcomeToTheWarner(t *testing.T) {
	fset, fn := runTestSetSource(t)

	const warner = "r.warnDependencyAssertionInert"
	// (testSetID, useMappingBased, isMappingEnabled, consumedFetchFailed,
	//  deferredStreaming, noEligibleDeps, warned)
	const fetchArg = 3

	var args []string
	ast.Inspect(fn, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok || calleeName(call) != warner {
			return true
		}
		if len(call.Args) <= fetchArg {
			t.Fatalf("%s takes %d argument(s); consumedFetchFailed is argument %d", warner, len(call.Args), fetchArg+1)
		}
		args = append(args, printNode(t, fset, call.Args[fetchArg]))
		return true
	})

	sort.Strings(args)
	want := []string{"false", "instrumentConsumedFetchErr != nil"}
	if strings.Join(args, " | ") != strings.Join(want, " | ") {
		t.Fatalf("%s is passed consumedFetchFailed=%v across its call sites, want %v.\n\n"+
			"WHY THIS MATTERS: only the main replay loop performs the per-test consumed fetch, so only it "+
			"can report that the fetch failed. A literal false there silences the fourth reason entirely: "+
			"a run whose fetch failed reports dependencies_checked=false with no warning, or — when the "+
			"mapping is also all-reusable — blames the recording's tier for a transport error the operator "+
			"could have fixed. The Phase-2 streaming pass fetches no consumed set, so it must pass false.",
			warner, args, want)
	}
}

// THE ELIGIBILITY FILTER IS ONE FUNCTION, NOT TWO COPIES.
//
// `isDNSMockEntry(m, kinds) || reusable[m.Name]` decides two things that must
// agree: the persisted deps_checked bit (buildDepResults iterates the survivors)
// and the user-facing "why nothing was asserted" warning (RunTestSet measures
// them for noEligibleDeps, via filteredExpectedNames). It used to be written
// out at both sites.
//
// Nothing pinned that the copies agreed — each half is pinned separately and
// each half is individually correct — so widening one alone stayed green while
// producing a silent-honesty regression: widen the writer's copy and the report
// says NOT-CHECKED with no explanation; widen the call site's and the warning
// fires for test sets that WERE asserted. This test makes the shared call the
// pinned property, so the drift is not expressible.
func TestEligibilityFilterHasExactlyOneDefinition(t *testing.T) {
	fset := token.NewFileSet()
	pkgs, err := parser.ParseDir(fset, ".", func(fi os.FileInfo) bool {
		return !strings.HasSuffix(fi.Name(), "_test.go")
	}, 0)
	if err != nil {
		t.Fatalf("parse package: %v", err)
	}

	type site struct {
		fn  string
		pos string
	}
	var sites []site
	for _, pkg := range pkgs {
		for _, file := range pkg.Files {
			ast.Inspect(file, func(n ast.Node) bool {
				fn, ok := n.(*ast.FuncDecl)
				if !ok {
					return true
				}
				ast.Inspect(fn.Body, func(m ast.Node) bool {
					bin, ok := m.(*ast.BinaryExpr)
					if !ok || bin.Op != token.LOR {
						return true
					}
					// The composite predicate: a call to isDNSMockEntry on one
					// side, an index into the reusable map on the other.
					idents := exprIdents([]ast.Expr{bin})
					if idents["isDNSMockEntry"] && idents["reusableMockNames"] {
						sites = append(sites, site{fn: fn.Name.Name, pos: fset.Position(bin.Pos()).String()})
					}
					return true
				})
				return false
			})
		}
	}

	if len(sites) != 1 || sites[0].fn != "eligibleExpectedEntries" {
		t.Fatalf("the eligibility predicate is written at %d site(s): %+v; want exactly 1, inside eligibleExpectedEntries.\n\n"+
			"WHY THIS MATTERS: this predicate decides BOTH the persisted deps_checked bit and the warning "+
			"that explains it. Two copies can be widened one at a time, and each outcome is a silent "+
			"regression that keeps the suite green: the report reporting NOT-CHECKED with no reason given, "+
			"or the reason being given for test sets whose assertion actually ran. Call "+
			"eligibleExpectedEntries instead of re-writing the filter.", len(sites), sites)
	}
}

// ...and RunTestSet actually routes through it, rather than keeping its own
// list that merely happens to match today.
func TestRunTestSetDerivesItsExpectationListFromTheSharedFilter(t *testing.T) {
	_, fn := runTestSetSource(t)

	calls := callArgIdents(fn, "eligibleExpectedEntries")
	if len(calls) != 1 {
		t.Fatalf("RunTestSet calls eligibleExpectedEntries %d time(s), want exactly 1.\n\n"+
			"WHY THIS MATTERS: filteredExpectedNames (the mockSetMismatch verdict) and noEligibleDeps (the "+
			"warning) must both be derived from the same filtered list buildDepResults iterates, or the "+
			"verdict, the persisted deps_checked bit and the reason shown to the user are three "+
			"computations of one question with nothing holding them together.", len(calls))
	}
	want := []string{"expectedMocks", "mockKindByName", "reusableMockNames"}
	for _, w := range want {
		if !calls[0][w] {
			t.Fatalf("eligibleExpectedEntries is called without %q (got %v).\n\n"+
				"WHY THIS MATTERS: dropping mockKindByName stops DNS entries being excluded and dropping "+
				"reusableMockNames stops session/connection-tier ones being excluded, so healthy tests are "+
				"reported with MISSING dependencies and --assert-dependencies fails them.", w, calls[0])
		}
	}
}

// enclosingBlock returns the innermost *ast.BlockStmt in file that strictly
// contains pos.
func enclosingBlock(file *ast.File, pos token.Pos) *ast.BlockStmt {
	var found *ast.BlockStmt
	ast.Inspect(file, func(n ast.Node) bool {
		if n == nil || pos < n.Pos() || pos >= n.End() {
			return false
		}
		if blk, ok := n.(*ast.BlockStmt); ok && blk.Pos() < pos {
			found = blk
		}
		return true
	})
	return found
}

// GIVING UP ON ONE TEST CASE MUST LEAVE THE LOOP, NOT THE SWITCH.
//
// Every `switch testCase.Kind` arm opens by type-asserting the simulate
// response, and when that fails it persists a synthetic FAILED result and
// abandons the test. That block ended in `break` in the HTTP and gRPC arms for
// years, and the consumer arm was added by copying them — the right instinct,
// and it copied the bug. `break` inside a `switch` leaves the SWITCH: execution
// falls through to recordReqResTimestamps, resolveTestOutcome and the status
// switch with `testResult` still nil, so the same test is counted into
// currentFailures twice, once in the arm and once in `default:`. The intent in
// all three arms — and in the streaming loop's copy of the same block — is
// `continue`.
//
// It is pinned by AST because nothing executes RunTestSet: the block is only
// reachable when the simulate hook returns a response of the wrong dynamic
// type AND the report writer then fails, which no unit harness in this package
// can stage. Reverting any one of the four to `break` fails this test.
func TestGivingUpOnATestCaseContinuesTheLoopRatherThanTheSwitch(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "replay.go", nil, 0)
	if err != nil {
		t.Fatalf("parse replay.go: %v", err)
	}

	// Every `if loopErr != nil { ... }` whose body logs an "insert test case
	// result for ... type assertion error" — the abandon-this-test block.
	var checked int
	var abandonBlocks []*ast.IfStmt
	ast.Inspect(file, func(n ast.Node) bool {
		stmt, ok := n.(*ast.IfStmt)
		if !ok || stmt.Body == nil {
			return true
		}
		var cond strings.Builder
		if err := printer.Fprint(&cond, fset, stmt.Cond); err != nil {
			return true
		}
		if cond.String() != "loopErr != nil" {
			return true
		}
		var buf strings.Builder
		if err := printer.Fprint(&buf, fset, stmt); err != nil {
			return true
		}
		src := buf.String()
		if !strings.Contains(src, "type assertion error") {
			return true
		}
		checked++
		for _, s := range stmt.Body.List {
			br, ok := s.(*ast.BranchStmt)
			if !ok {
				continue
			}
			if br.Tok == token.BREAK {
				t.Errorf("%s: abandoning a test case with `break` leaves the switch, not the per-test loop:\n%s\n\n"+
					"Execution falls through to resolveTestOutcome with testResult nil and counts this test into currentFailures twice. Use `continue`.",
					fset.Position(br.Pos()), src)
			}
		}
		abandonBlocks = append(abandonBlocks, stmt)
		return true
	})

	// THE OTHER SPELLING OF THE SAME DEFECT, and the one the check above
	// cannot see. The abandon block's trailing branch statement is a SIBLING
	// of the `if loopErr != nil` — it is the last statement of the enclosing
	// `if !ok { … }` (or `else { … }` in the streaming copy) — so replacing
	// THAT `continue` with `break` has the identical consequence (execution
	// leaves the switch, falls through to recordReqResTimestamps and
	// resolveTestOutcome with testResult nil, and the test is counted into
	// currentFailures twice) while the loop above sees nothing. Confirmed by
	// mutation: rewriting the CONSUMER arm's trailing `continue` as `break`
	// left the whole pkg/service/replay package green.
	for _, stmt := range abandonBlocks {
		blk := enclosingBlock(file, stmt.Pos())
		if blk == nil || len(blk.List) == 0 {
			t.Errorf("%s: could not find the block that owns this abandon-this-test guard", fset.Position(stmt.Pos()))
			continue
		}
		last := blk.List[len(blk.List)-1]
		br, ok := last.(*ast.BranchStmt)
		if !ok || br.Tok != token.CONTINUE {
			var buf strings.Builder
			if err := printer.Fprint(&buf, fset, last); err != nil {
				buf.WriteString("<unprintable>")
			}
			t.Errorf("%s: the abandon-this-test block ends in `%s`, want `continue`.\n\n"+
				"That statement is a SIBLING of the `if loopErr != nil` guard, not inside it, so the "+
				"check above cannot see it. `break` there leaves the SWITCH: execution falls through "+
				"to resolveTestOutcome with testResult nil and this test is counted into "+
				"currentFailures twice.", fset.Position(last.Pos()), buf.String())
		}
	}

	// Four arms carry this block: HTTP, gRPC and CONSUMER in RunTestSet, plus
	// the streaming loop's copy. A drop to zero means the search string
	// stopped matching and this test silently stopped checking anything.
	if checked != 4 {
		t.Fatalf("found %d abandon-this-test blocks, want 4; the arms were renamed and this test is no longer looking at them", checked)
	}
}
