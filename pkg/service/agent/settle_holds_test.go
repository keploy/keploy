package agent

import (
	"runtime"
	"testing"
)

// holdsUnknown's text off macOS is the parsers' reason alone, as it was
// before macOS added its own; on macOS it also says that the count of what a
// connection received can be what keploy lacks (the proxy names such a
// connection "TCP (macOS does not give keploy ...)").
func TestHoldsUnknownSaysEachPlatformsReasons(t *testing.T) {
	const parsers = "keploy cannot tell whether grpc, mysql on a connection still holds a request: its parser reads ahead of its answers and does not say which requests it holds, or says that for now it cannot tell"
	want := parsers
	if runtime.GOOS == "darwin" {
		want += ", or macOS does not give keploy the count of what the connection received"
	}
	if got := (holdsUnknown{"grpc", "mysql"}).Error(); got != want {
		t.Fatalf("holdsUnknown on %s says\n  %q\nwant\n  %q", runtime.GOOS, got, want)
	}
}
