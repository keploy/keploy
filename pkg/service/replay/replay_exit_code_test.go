package replay

import (
	"go/ast"
	"go/parser"
	"go/token"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// TestReplayRunOutcome_AppFailureExitsNonZero is the bug this pins.
//
// A user runs `keploy test` with --keep-app-alive, their app fails to start
// (image build failure, port in use, missing env var, a crash-looping
// dependency), ZERO tests run — and keploy exits 0 while logging "replay
// completed successfully". Every CI pipeline downstream reads that as a pass.
//
// It escaped because testRunResult starts true and is only ever set false by
// a test-set that actually ran. With no tests, nothing sets it false. The app
// error was returned into an errgroup whose Wait() is never called anywhere
// in this package, so it went nowhere.
func TestReplayRunOutcome_AppFailureExitsNonZero(t *testing.T) {
	appErr := models.AppError{AppErrorType: models.ErrUnExpected}

	// The exact shape of the escape: app died, but no test reported failure.
	code, err := replayRunOutcome(true, &appErr)
	if code == 0 {
		t.Fatal("exit code 0 for a run whose app failed under --keep-app-alive with zero tests " +
			"run. testRunResult stays true when nothing ran, so this is precisely the case " +
			"that reported success while the app was dead.")
	}
	if err == nil {
		t.Fatal("no error returned for a failed app; the reason must reach the caller, not " +
			"just the exit code")
	}
}

// TestReplayRunOutcome_Table covers the remaining combinations, including the
// two that must stay a pass.
func TestReplayRunOutcome_Table(t *testing.T) {
	appErr := models.AppError{AppErrorType: models.ErrUnExpected}

	for _, tc := range []struct {
		name          string
		testRunResult bool
		appErr        *models.AppError
		wantCode      int
		wantErr       bool
	}{
		{"everything passed", true, nil, 0, false},
		{"a test failed", false, nil, 1, false},
		{"app died, tests passed", true, &appErr, 1, true},
		{"app died and tests failed", false, &appErr, 1, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			code, err := replayRunOutcome(tc.testRunResult, tc.appErr)
			if code != tc.wantCode {
				t.Fatalf("exit code = %d, want %d", code, tc.wantCode)
			}
			if (err != nil) != tc.wantErr {
				t.Fatalf("err = %v, want error: %v", err, tc.wantErr)
			}
		})
	}
}

// TestKeepAliveAppErrIsWiredToTheOutcome pins the WIRING, which the tests
// above deliberately cannot reach.
//
// replayRunOutcome is a pure function, so testing it proves the rule and
// nothing about whether the rule is ever consulted. Deleting the
// keepAliveAppErr.Store in the --keep-app-alive goroutine, or the
// .Load() feeding the outcome, restores the original bug — an app that dies
// with zero tests run exits 0 — while every behavioural test above stays
// green. Driving Start() end to end would need a full instrumented replay, so
// this asserts the two connections directly in the source.
func TestKeepAliveAppErrIsWiredToTheOutcome(t *testing.T) {
	fset := token.NewFileSet()
	file, err := parser.ParseFile(fset, "replay.go", nil, 0)
	if err != nil {
		t.Fatalf("parse replay.go: %v", err)
	}

	var stored, loadedIntoOutcome bool
	ast.Inspect(file, func(n ast.Node) bool {
		call, ok := n.(*ast.CallExpr)
		if !ok {
			return true
		}
		// keepAliveAppErr.Store(...)
		if sel, ok := call.Fun.(*ast.SelectorExpr); ok && sel.Sel.Name == "Store" {
			if ident, ok := sel.X.(*ast.Ident); ok && ident.Name == "keepAliveAppErr" {
				stored = true
			}
		}
		// replayRunOutcome(_, keepAliveAppErr.Load())
		if ident, ok := call.Fun.(*ast.Ident); ok && ident.Name == "replayRunOutcome" {
			for _, arg := range call.Args {
				inner, ok := arg.(*ast.CallExpr)
				if !ok {
					continue
				}
				sel, ok := inner.Fun.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "Load" {
					continue
				}
				if x, ok := sel.X.(*ast.Ident); ok && x.Name == "keepAliveAppErr" {
					loadedIntoOutcome = true
				}
			}
		}
		return true
	})

	if !stored {
		t.Error("nothing stores into keepAliveAppErr. The --keep-app-alive goroutine must record " +
			"the app failure, or it is lost: it returns the error into an errgroup whose Wait() " +
			"is never called anywhere in this package.")
	}
	if !loadedIntoOutcome {
		t.Error("replayRunOutcome is not called with keepAliveAppErr.Load(). The rule can be " +
			"correct and still never consulted, which is exactly how a dead app exited 0.")
	}
}
