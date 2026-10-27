//go:build linux

package grpc

import (
	"io"
	"os"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// The console's failure diff for a protoscope reorder says what differs and
// how to compare a map field as a map: the diff block the user reads, not a
// log line. Captured at fd 1, where the diff printers write.
func TestMatch_ProvenSwitchPrintsTheProtoscopeReorderHint(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	tc, answer := protoscopeCase("1: {\"ord_A\"}\n1: {\"ord_B\"}")
	console := consoleOf(t, func() {
		_, _ = Match(tc, answer("1: {\"ord_B\"}\n1: {\"ord_A\"}"), map[string]map[string][]string{}, true, zap.NewNop(), true)
	})
	if !strings.Contains(console, "--proto-file") || !strings.Contains(console, "another order") {
		t.Fatalf("the failure diff does not say the repeated field came back in another order, with the proto-files way out:\n%s", console)
	}
}

// Under the switch the console's failure diff shows a grpc-status that
// differs, an absent one as "none": a test that fails on its status alone
// says so, not an empty diff. With the switch off the diff is as it was.
func TestMatch_ProvenSwitchPrintsTheGrpcStatusDifference(t *testing.T) {
	rec := splitTrailersRecording()
	for _, c := range []struct {
		name            string
		recorded        *models.GrpcResp
		live            *models.GrpcResp
		shown, notShown []string
	}{
		{"recorded none, live OK", &rec, withGrpcStatus(rec, "0", true), []string{"grpc-status", "none"}, nil},
		{"codes differ", withGrpcStatus(rec, "0", true), withGrpcStatus(rec, "13", true), []string{"grpc-status", "13"}, []string{"none"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			tc := &models.TestCase{Name: "status", GrpcResp: *c.recorded}
			t.Setenv(provenswitch.Env, "")
			if off := consoleOf(t, func() { _, _ = Match(tc, c.live, nil, false, zap.NewNop(), true) }); strings.Contains(off, "grpc-status") {
				t.Fatalf("switch off: the diff changed:\n%s", off)
			}
			t.Setenv(provenswitch.Env, "1")
			console := consoleOf(t, func() { _, _ = Match(tc, c.live, nil, false, zap.NewNop(), true) })
			for _, want := range c.shown {
				if !strings.Contains(console, want) {
					t.Fatalf("KEPLOY_PROVEN=1: the failure diff does not show %q:\n%s", want, console)
				}
			}
			for _, unwanted := range c.notShown {
				if strings.Contains(console, unwanted) {
					t.Fatalf("KEPLOY_PROVEN=1: the failure diff shows %q:\n%s", unwanted, console)
				}
			}
		})
	}
}

// consoleOf is what fn writes at fd 1, where the diff printers write.
func consoleOf(t *testing.T, fn func()) string {
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
	out := make(chan string)
	go func() {
		b, _ := io.ReadAll(r)
		out <- string(b)
	}()
	fn()
	_ = unix.Dup2(saved, 1)
	_ = unix.Close(saved)
	_ = w.Close()
	return <-out
}
