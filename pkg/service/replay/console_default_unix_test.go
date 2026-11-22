//go:build linux || darwin

package replay

import (
	"context"
	"io"
	"os"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// stdoutOf runs fn with the process's standard output (file descriptor 1,
// whatever *os.File writes to it) a pipe, and returns what was written there.
func stdoutOf(t *testing.T, fn func()) string {
	t.Helper()
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	saved, err := unix.Dup(1)
	if err != nil {
		t.Fatal(err)
	}
	if err := unix.Dup2(int(w.Fd()), 1); err != nil {
		t.Fatal(err)
	}
	read := make(chan string, 1)
	go func() {
		b, _ := io.ReadAll(r)
		read <- string(b)
	}()
	defer func() {
		_ = r.Close()
	}()
	func() {
		defer func() {
			_ = unix.Dup2(saved, 1)
			_ = unix.Close(saved)
			_ = w.Close()
		}()
		fn()
	}()
	return <-read
}

// A replayer as NewReplayer builds it has no console: it names a call whole
// nowhere, so a program that embeds it (a k8s proxy, whose own stdout is its
// pod's log) puts no call's path or statement in a log. Only a console its
// embedder sets (keploy's CLI: os.Stdout) is written: the mismatch table and
// a call of an earlier set that fails the run.
func TestAReplayerBuiltWithDefaultsWritesNothingToStdout(t *testing.T) {
	r := NewReplayer(zap.NewNop(), nil, nil, nil, nil, nil, nil, nil, nil, &config.Config{}).(*Replayer)
	call := models.UnmatchedCall{Protocol: "HTTP", ActualSummary: "POST /v1/reset/t0k3n", Destination: "auth:80", PathTag: "#0123456789ab"}
	r.mockMismatchFailures.AddUnmatchedCallForTest("test-set-0", "test-1", call)
	late := earlierSetCall{set: "test-set-a", call: call}
	out := stdoutOf(t, func() {
		r.printMockMismatches()
		sayEarlierSetCall(r.console, late)
	})
	if out != "" {
		t.Fatalf("a replayer with no console wrote to stdout:\n%s", out)
	}

	var console syncBuffer
	r.SetConsole(&console)
	out = stdoutOf(t, func() {
		r.printMockMismatches()
		sayEarlierSetCall(r.console, late)
	})
	if out != "" {
		t.Fatalf("a replayer with a console of its own wrote to stdout:\n%s", out)
	}
	for _, want := range []string{
		"  Mock mismatch: [HTTP] POST /v1/reset/t0k3n [path tag #0123456789ab]  →  auth:80",
		"  Mock mismatch of an earlier test set: [HTTP] POST /v1/reset/t0k3n [path tag #0123456789ab]  →  auth:80   (made in no test (between two of its tests, or after its last) of test-set-a)",
	} {
		if !strings.Contains(console.String(), want) {
			t.Errorf("the console said\n%s\nwant it to hold %q", console.String(), want)
		}
	}
}

// A failing test's lines are the console's too: the matchers' "Testrun
// failed" line and diff, which name the test's expected and actual header
// values and bodies whole (the HTTP matcher's, through both of the replay's
// comparisons, and the gRPC matcher's), a passing test's line, and the run's
// summary. A replayer as NewReplayer builds it printed them all to the
// process's stdout whatever console was set, so a k8s proxy's auto-replay
// wrote every failing test's answers into its pod's log. Now one with no
// console writes nothing at file descriptor 1, and one with a console writes
// them there alone.
func TestAReplayersFailingTestsDiffGoesToItsConsoleAlone(t *testing.T) {
	const secret = "s3cr3t-answer-7Qx3vZ9mW2"
	failing := &models.TestCase{Name: "test-1", Kind: models.HTTP, HTTPResp: models.HTTPResp{
		StatusCode: 200, Header: map[string]string{"X-Token": "recorded-" + secret}, Body: `{"token":"recorded-` + secret + `"}`}}
	answer := func() *models.HTTPResp {
		return &models.HTTPResp{StatusCode: 200, Header: map[string]string{"X-Token": "live-" + secret}, Body: `{"token":"live-` + secret + `"}`}
	}
	passing := &models.TestCase{Name: "test-2", Kind: models.HTTP, HTTPResp: models.HTTPResp{StatusCode: 200, Body: `{"ok":true}`}}
	grpcFailing := &models.TestCase{Name: "test-3", Kind: models.GRPC_EXPORT, GrpcResp: models.GrpcResp{
		Body: models.GrpcLengthPrefixedMessage{DecodedData: `{"token":"recorded-` + secret + `"}`}}}
	grpcAnswer := func() *models.GrpcResp {
		return &models.GrpcResp{Body: models.GrpcLengthPrefixedMessage{DecodedData: `{"token":"live-` + secret + `"}`}}
	}
	run := func(r *Replayer) {
		if pass, _ := r.compareHTTPRespForReplay(failing, answer(), "test-set-0", true); pass {
			t.Fatal("precondition: the HTTP answer that differs passed")
		}
		if pass, _ := r.compareHTTPResp(failing, answer(), "test-set-0", true); pass {
			t.Fatal("precondition: the HTTP answer that differs passed")
		}
		if pass, _ := r.compareHTTPRespForReplay(passing, &models.HTTPResp{StatusCode: 200, Body: `{"ok":true}`}, "test-set-0", true); !pass {
			t.Fatal("precondition: the same HTTP answer failed")
		}
		if pass, _ := r.compareGRPCResp(grpcFailing, grpcAnswer(), "test-set-0", true, false); pass {
			t.Fatal("precondition: the gRPC answer that differs passed")
		}
		r.completeTestReportMu.Lock()
		r.totalTests, r.totalTestPassed, r.totalTestFailed = 3, 1, 2
		r.completeTestReport = map[string]TestReportVerdict{"test-set-0": {total: 3, passed: 1, failed: 2}}
		r.completeTestReportMu.Unlock()
		r.printSummary(context.Background(), false)
	}

	r := NewReplayer(zap.NewNop(), nil, nil, nil, nil, nil, nil, nil, nil, &config.Config{}).(*Replayer)
	if out := stdoutOf(t, func() { run(r) }); out != "" {
		t.Fatalf("a replayer with no console wrote a failing test's lines to stdout:\n%s", out)
	}

	var console syncBuffer
	r = NewReplayer(zap.NewNop(), nil, nil, nil, nil, nil, nil, nil, nil, &config.Config{}).(*Replayer)
	r.SetConsole(&console)
	if out := stdoutOf(t, func() { run(r) }); out != "" {
		t.Fatalf("a replayer with a console of its own wrote to stdout:\n%s", out)
	}
	got := console.String()
	for _, want := range []string{"Testrun failed for testcase with id", "Testrun passed for testcase with id", secret, "COMPLETE TESTRUN SUMMARY"} {
		if !strings.Contains(got, want) {
			t.Errorf("the console said\n%s\nwant it to hold %q", got, want)
		}
	}
}
