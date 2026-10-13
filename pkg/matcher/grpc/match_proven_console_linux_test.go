//go:build linux

package grpc

import (
	"io"
	"os"
	"strings"
	"testing"

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
	_, _ = Match(tc, answer("1: {\"ord_B\"}\n1: {\"ord_A\"}"), map[string]map[string][]string{}, true, zap.NewNop(), true)
	_ = unix.Dup2(saved, 1)
	_ = unix.Close(saved)
	_ = w.Close()
	if console := <-out; !strings.Contains(console, "--proto-file") || !strings.Contains(console, "another order") {
		t.Fatalf("the failure diff does not say the repeated field came back in another order, with the proto-files way out:\n%s", console)
	}
}
