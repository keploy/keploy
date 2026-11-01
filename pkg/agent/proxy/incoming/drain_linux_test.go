package proxy

import (
	"bytes"
	"errors"
	"io"
	"net"
	"runtime"
	"strings"
	"testing"
)

// What has arrived from the app is the kernel's count of what the socket has
// received, its close as one more: a close that has come and is not read is
// counted, as a byte is. Counted as what had been read and then what the
// socket held unread (SIOCINQ, bytes only), it was not, and a close-delimited
// answer whose close had come before its client went was told as ended by its
// client.
func TestTheAppsSideCountsACloseThatCameUnread(t *testing.T) {
	app, ingress := tcpPair(t)
	d := newDrainConn(ingress)
	t.Cleanup(d.release)
	const answer = "the whole answer"
	if _, err := io.WriteString(app, answer); err != nil {
		t.Fatal(err)
	}
	_ = app.(*net.TCPConn).CloseWrite()
	waitAcked(t, app)
	if got, want := d.Arrived(), int64(len(answer)+1); got != want {
		t.Fatalf("with %d bytes and the close come, none read, arrived counts %d, want %d", len(answer), got, want)
	}
	got, err := io.ReadAll(d)
	if err != nil || string(got) != answer {
		t.Fatalf("read %q, %v; want %q", got, err, answer)
	}
	if got, want := d.Arrived(), int64(len(answer)+1); got != want {
		t.Fatalf("once read, arrived counts %d, want %d", got, want)
	}
}

// What has arrived is counted from the wrap, as what is read of it is: what
// was read of the connection before (an earlier exchange on a kept
// connection) is not.
func TestTheAppsSideCountsFromItsWrap(t *testing.T) {
	app, ingress := tcpPair(t)
	if _, err := io.WriteString(app, "before"); err != nil {
		t.Fatal(err)
	}
	if _, err := io.ReadFull(ingress, make([]byte, len("before"))); err != nil {
		t.Fatal(err)
	}
	d := newDrainConn(ingress)
	t.Cleanup(d.release)
	if _, err := io.WriteString(app, "after"); err != nil {
		t.Fatal(err)
	}
	waitAcked(t, app)
	if got, want := d.Arrived(), int64(len("after")); got != want {
		t.Fatalf("arrived counts %d since the wrap, want %d", got, want)
	}
}

// A drain reads what the socket holds without waiting, whatever its consumer
// does: a consumer waiting in a read for bytes the app has not sent leaves the
// socket to it. What it read, the consumer reads next, in order, before
// anything the socket brings after; and it holds at most drainMax.
func TestADrainNeitherWaitsOnItsConsumerNorReordersTheBytes(t *testing.T) {
	saved := drainMax
	drainMax = 4 << 10
	t.Cleanup(func() { drainMax = saved })
	app, ingress := tcpPair(t)
	d := newDrainConn(ingress)
	t.Cleanup(d.release)

	first := strings.Repeat("a", 1000)
	got := make(chan []byte, 1)
	go func() {
		var b bytes.Buffer
		buf := make([]byte, 700)
		for b.Len() < len(first)+64<<10 {
			n, err := d.Read(buf)
			b.Write(buf[:n])
			if err != nil {
				break
			}
			if b.Len() == len(first) {
				got <- append([]byte(nil), b.Bytes()...)
			}
		}
		got <- b.Bytes()
	}()
	// The consumer waits in its read: nothing has come.
	for !readerWaits() {
		select {
		case <-never(t):
			t.Fatal("the consumer never waited in its read")
		default:
			runtime.Gosched()
		}
	}
	d.drain() // returns: it does not wait on the consumer's read
	if _, err := io.WriteString(app, first); err != nil {
		t.Fatal(err)
	}
	select {
	case b := <-got:
		if string(b) != first {
			t.Fatalf("the consumer read %q first", b)
		}
	case <-never(t):
		t.Fatal("the consumer never read the first bytes")
	}

	// Now the consumer reads on as the drain does: 64 KiB the app wrote
	// whole, its order kept, the drain holding at most drainMax at once.
	rest := make([]byte, 64<<10)
	for i := range rest {
		rest[i] = byte('b' + i/(4<<10))
	}
	if _, err := app.Write(rest); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 64; i++ {
		d.drain()
		d.mu.Lock()
		held := len(d.held)
		d.mu.Unlock()
		if int64(held) > drainMax {
			t.Fatalf("the drain holds %d bytes, past its bound of %d", held, drainMax)
		}
	}
	select {
	case b := <-got:
		if want := first + string(rest); string(b) != want {
			t.Fatalf("the consumer read %d bytes, not the app's %d in their order", len(b), len(want))
		}
	case <-never(t):
		t.Fatal("the consumer never read the rest")
	}
}

// readerWaits reports whether a goroutine waits in a drainConn's read for the
// socket to bring something.
func readerWaits() bool {
	buf := make([]byte, 1<<20)
	buf = buf[:runtime.Stack(buf, true)]
	for _, g := range bytes.Split(buf, []byte("\n\n")) {
		if bytes.Contains(g, []byte("(*drainConn).Read")) && bytes.Contains(g, []byte("waitRead")) {
			return true
		}
	}
	return false
}

// What a drain read is the app's, read before the app was told its client
// went: the watch closes the app's connection on a client's reset, and its
// consumer still reads what the drain holds, then the close's error. What
// the drain holds is charged with the read-aheads' (aheadInAll), and given
// back once read, or at the release.
func TestWhatADrainReadOutlivesTheConnsClose(t *testing.T) {
	app, ingress := tcpPair(t)
	before := aheadInAll.Load()
	d := newDrainConn(ingress)
	const answer = "drained before the close"
	if _, err := io.WriteString(app, answer); err != nil {
		t.Fatal(err)
	}
	waitAcked(t, app)
	if got := d.drainArrived(); got != int64(len(answer)) {
		t.Fatalf("arrived counts %d, want %d", got, len(answer))
	}
	if aheadInAll.Load() == before {
		t.Fatal("what the drain holds is not charged")
	}
	_ = ingress.Close()
	got := make([]byte, 64)
	n, err := d.Read(got)
	if err != nil || string(got[:n]) != answer {
		t.Fatalf("read %q, %v after the close; want %q", got[:n], err, answer)
	}
	if aheadInAll.Load() != before {
		t.Fatal("what the drain held is still charged once read")
	}
	if _, err := d.Read(got); !errors.Is(err, net.ErrClosed) {
		t.Fatalf("read past what the drain held: %v, want the close's error", err)
	}

	// Not read: given back at the release.
	app2, ingress2 := tcpPair(t)
	d2 := newDrainConn(ingress2)
	if _, err := io.WriteString(app2, answer); err != nil {
		t.Fatal(err)
	}
	waitAcked(t, app2)
	d2.drain()
	d2.release()
	d2.release()
	if aheadInAll.Load() != before {
		t.Fatal("what the drain held is still charged once released")
	}
}
