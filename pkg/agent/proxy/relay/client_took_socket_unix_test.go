//go:build linux || darwin

package relay

import (
	"testing"

	"go.uber.org/zap"
)

// The row here needs a count Windows cannot give: on Windows a socket with
// no count from its accept says only the bytes it sent, not those its send
// buffer still holds (client_socket_windows.go). The rows that close the
// client's receive window are in client_took_window_test.go, and those that
// need neither in client_took_socket_any_test.go; both run on Windows too.

// On a real socket, written counts every byte written to it: those the
// client's stack has acknowledged and those it has not yet, from one answer
// on macOS. Here what the proxy wrote fits the client's window, so the count
// is exact whatever the client has acknowledged by the time it is asked.
// Not on Windows: there a socket with no count from its accept says only the
// bytes it sent, not those its send buffer still holds (client_socket_windows.go),
// and client_count_windows_test.go's rows hold the count from the accept.
func TestWrittenCountsEveryByteWrittenToARealSocket(t *testing.T) {
	t.Parallel()
	_, proxyClient := loopbackPair(t)
	s := clientSocketOf(proxyClient, zap.NewNop())
	if s == nil {
		t.Fatal("the client's socket is not asked")
	}
	// The relay's first look comes before it writes (clientFINBefore), and
	// macOS counts from the first look (client_took_darwin.go).
	_ = s.look()
	const n = 64 << 10
	if _, err := proxyClient.Write(make([]byte, n)); err != nil {
		t.Fatal(err)
	}
	if got, ok := s.written(); !ok || got != n {
		t.Fatalf("written %d (ok %v), want the %d bytes written", got, ok, n)
	}
}
