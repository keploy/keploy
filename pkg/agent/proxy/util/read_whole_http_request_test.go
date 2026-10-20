package util

import (
	"context"
	"fmt"
	"net"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
)

// sized is an HTTP request of exactly size bytes: head, padded by an X-Pad header, then body.
func sized(t *testing.T, head, body string, size int) []byte {
	t.Helper()
	pad := size - len(head) - len("X-Pad: \r\n\r\n") - len(body)
	if pad < 0 {
		t.Fatalf("a request of %d bytes cannot hold head %q and a %d byte body", size, head, len(body))
	}
	req := head + "X-Pad: " + strings.Repeat("p", pad) + "\r\n\r\n" + body
	if len(req) != size {
		t.Fatalf("built %d bytes, want %d", len(req), size)
	}
	return []byte(req)
}

// sent is what read got from a live connection the client wrote msg to and then left open, waiting for an
// answer, as a browser does; it fails the test if read is still waiting after a second.
func sent(t *testing.T, msg []byte, read func(net.Conn) ([]byte, error)) []byte {
	t.Helper()
	client, proxy := net.Pipe()
	t.Cleanup(func() { client.Close(); proxy.Close() })
	go func() { _, _ = client.Write(msg) }()
	type result struct {
		b   []byte
		err error
	}
	done := make(chan result, 1)
	go func() { b, err := read(proxy); done <- result{b, err} }()
	select {
	case r := <-done:
		if r.err != nil {
			t.Fatalf("read: %v", r.err)
		}
		return r.b
	case <-time.After(time.Second):
		t.Fatalf("still reading a %d byte request after 1s: the client sent all of it and waits for the answer", len(msg))
		return nil
	}
}

func initialBuf(c net.Conn) ([]byte, error) {
	return ReadInitialBuf(context.Background(), zap.NewNop(), c)
}
func nextRequest(c net.Conn) ([]byte, error) { return ReadBytes(context.Background(), zap.NewNop(), c) }

// atgr1 / probenl (Chrome's netlog + ss): the browser wrote GET /company/list, exactly 2048 bytes, as the first
// request on a new connection; keploy read both 1024 byte pieces and waited for a third, so the call was held until
// the page closed the connection 30 s later. Replay reads every next request on a kept connection the same way.
func TestReadEndsARequestThatFillsTheLastPiece(t *testing.T) {
	get := "GET /company/list HTTP/1.1\r\nHost: localhost:8083\r\nPragma: no-cache\r\nCache-Control: no-cache\r\n"
	post := "POST /query HTTP/1.1\r\nHost: localhost:8083\r\nContent-Type: application/json\r\nContent-Length: 300\r\n"
	for _, size := range []int{1024, 2048, 3072} {
		for _, c := range []struct {
			name string
			msg  []byte
		}{
			{"GET", sized(t, get, "", size)},
			{"POST with its body", sized(t, post, `{"query":"`+strings.Repeat("q", 288)+`"}`, size)},
		} {
			for _, r := range []struct {
				name string
				read func(net.Conn) ([]byte, error)
			}{{"new connection", initialBuf}, {"kept connection", nextRequest}} {
				t.Run(fmt.Sprintf("%s %d bytes, %s", c.name, size, r.name), func(t *testing.T) {
					if got := sent(t, c.msg, r.read); string(got) != string(c.msg) {
						t.Fatalf("read %d bytes, want the %d byte request", len(got), len(c.msg))
					}
				})
			}
		}
	}
}

// A request whose body is not all there yet at a piece's end is read on, as before.
func TestReadWaitsForTheRestOfABody(t *testing.T) {
	head := "POST /query HTTP/1.1\r\nHost: localhost:8083\r\nContent-Length: 1000\r\n"
	full := sized(t, head, strings.Repeat("b", 1000), 2600)
	split := len(full) - 552 // 2048: the headers and 448 bytes of the body
	client, proxy := net.Pipe()
	t.Cleanup(func() { client.Close(); proxy.Close() })
	go func() {
		_, _ = client.Write(full[:split])
		time.Sleep(50 * time.Millisecond)
		_, _ = client.Write(full[split:])
	}()
	got, err := ReadBytes(context.Background(), zap.NewNop(), proxy)
	if err != nil {
		t.Fatal(err)
	}
	// The second write comes as its own read: 552 bytes, a short read, the end.
	if string(got) != string(full) {
		t.Fatalf("read %d bytes, want all %d", len(got), len(full))
	}
}

// Bytes that are not an HTTP request are framed as before: a full piece is read on.
func TestReadOfOtherBytesIsUnchanged(t *testing.T) {
	client, proxy := net.Pipe()
	t.Cleanup(func() { client.Close(); proxy.Close() })
	msg := []byte(strings.Repeat("\x00\x01mongo", 2048/7) + strings.Repeat("z", 2048%7))
	go func() { _, _ = client.Write(msg); time.Sleep(200 * time.Millisecond); client.Close() }()
	start := time.Now()
	got, _ := ReadBytes(context.Background(), zap.NewNop(), proxy)
	if len(got) != 2048 || time.Since(start) < 150*time.Millisecond {
		t.Fatalf("read %d bytes after %v: want all 2048, at the peer's close as before", len(got), time.Since(start))
	}
}
