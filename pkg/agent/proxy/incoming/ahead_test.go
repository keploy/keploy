package proxy

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"
	"time"
)

// tcpPair returns the two ends of a loopback TCP connection.
func tcpPair(t testing.TB) (app, ingress net.Conn) {
	t.Helper()
	ln, err := net.Listen("tcp4", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	accepted := make(chan net.Conn, 1)
	go func() {
		c, aerr := ln.Accept()
		if aerr != nil {
			close(accepted)
			return
		}
		accepted <- c
	}()
	ingress, err = net.Dial("tcp4", ln.Addr().String())
	if err != nil {
		t.Fatal(err)
	}
	app = <-accepted
	if app == nil {
		t.Fatal("accept failed")
	}
	t.Cleanup(func() { _ = app.Close(); _ = ingress.Close() })
	return app, ingress
}

// queuedData is the bytes a holds, read and not yet consumed.
func queuedData(a *aheadReader) (n int) {
	a.mu.Lock()
	defer a.mu.Unlock()
	for _, p := range a.queue {
		n += len(p.data)
	}
	return n
}

// waitAhead waits until a has read everything up to the end of its stream:
// what the consumer reads from then on arrived before.
func waitAhead(t *testing.T, a *aheadReader, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		a.mu.Lock()
		ended := a.err != nil
		a.mu.Unlock()
		done := ended && queuedData(a) == want
		if done {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("the read-ahead did not read the %d bytes the app sent and its close", want)
		}
		time.Sleep(time.Millisecond)
	}
}

// The response's last byte is stamped when it came from the app, not when the
// consumer (forwarding to a slow client) got to it; for a body that ends with
// the close of its connection, when the close came.
func TestAheadReaderStampsWhenTheAppSentNotWhenItWasRead(t *testing.T) {
	app, ingress := tcpPair(t)
	a := newAheadReader(ingress)
	defer a.Close()
	body := bytes.Repeat([]byte("x"), 64<<10)
	if _, err := app.Write(body); err != nil {
		t.Fatal(err)
	}
	_ = app.Close()
	sent := time.Now()
	waitAhead(t, a, len(body))
	time.Sleep(30 * time.Millisecond) // a slow client
	reading := time.Now()

	got := make([]byte, len(body))
	if _, err := io.ReadFull(a, got); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(got, body) {
		t.Fatal("read other bytes than the app sent")
	}
	for what, at := range map[string]time.Time{"the last data byte": a.LastReadTime()} {
		if !at.Before(reading) {
			t.Fatalf("%s is stamped %s after the consumer began reading, not when it came from the app", what, at.Sub(reading))
		}
	}
	if n, err := a.Read(make([]byte, 1)); n != 0 || !errors.Is(err, io.EOF) {
		t.Fatalf("after the body: %d, %v, want the end of the stream", n, err)
	}
	last := a.LastReadTime()
	if !last.Before(reading) {
		t.Fatalf("the close is stamped %s after the consumer began reading, not when it came from the app", last.Sub(reading))
	}
	if last.Before(sent.Add(-time.Second)) {
		t.Fatalf("the close is stamped %s before the app closed", sent.Sub(last))
	}
}

// What it reads ahead is bounded in heap, per reader (aheadMax) and in all
// (aheadMaxInAll, beyond the piece each is reading): past that the app waits
// for the client, as without it. Every byte still arrives, in order, and what
// was queued is given back. Tiny writes to a stalled client are appended to the
// last piece, so they cost a few pieces, not one each.
func TestAheadReaderBoundsWhatItReadsAhead(t *testing.T) {
	for _, bound := range []string{"per reader", "in all", "tiny writes"} {
		t.Run(bound, func(t *testing.T) {
			prevMax, prevAll := aheadMax, aheadMaxInAll
			t.Cleanup(func() { aheadMax, aheadMaxInAll = prevMax, prevAll })
			readers, sent, write := 1, 1<<20, 1<<20
			switch bound {
			case "per reader", "tiny writes":
				aheadMax = 64 << 10 // the bound in all stays far above it
			case "in all":
				aheadMaxInAll, readers = 96<<10, 3 // each reader's own stays far above it
			}
			if bound == "tiny writes" {
				sent, write = 256<<10, 1
			}
			base := aheadInAll.Load()

			var rs []*aheadReader
			for i := 0; i < readers; i++ {
				app, ingress := tcpPair(t)
				rs = append(rs, newAheadReader(ingress))
				go func(app net.Conn, i int) {
					b := bytes.Repeat([]byte{byte('a' + i)}, sent)
					for len(b) > 0 {
						k := min(write, len(b))
						if _, err := app.Write(b[:k]); err != nil {
							return
						}
						b = b[k:]
					}
					_ = app.Close()
				}(app, i)
			}
			// Each reads its first piece whatever the others hold; then let
			// them fill up to their bounds.
			deadline := time.Now().Add(5 * time.Second)
			for i := 0; i < len(rs); {
				if queuedData(rs[i]) > 0 {
					i++
					continue
				}
				if time.Now().After(deadline) {
					t.Fatalf("reader %d read nothing ahead: the bound in all starved it", i)
				}
				time.Sleep(time.Millisecond)
			}
			time.Sleep(200 * time.Millisecond)
			// A piece is charged its buffer, which appending can grow to
			// twice a read, and its place: the slack over a bound is one
			// such piece per reader.
			const slack = 2*aheadRead + aheadPieceCost
			if inAll := aheadInAll.Load() - base; bound == "in all" && inAll > aheadMaxInAll+int64(readers)*slack {
				t.Fatalf("the readers hold %d bytes ahead in all, over the bound %d (+ one piece each)", inAll, aheadMaxInAll)
			}
			for i, a := range rs {
				a.mu.Lock()
				var sum int64
				for _, p := range a.queue {
					if int64(cap(p.data)) > p.cost-aheadPieceCost {
						a.mu.Unlock()
						t.Fatalf("reader %d: a piece's buffer of %d bytes is charged %d", i, cap(p.data), p.cost)
					}
					sum += p.cost
				}
				charged, pieces := a.charged, len(a.queue)
				a.mu.Unlock()
				if sum != charged {
					t.Fatalf("reader %d is charged %d, its pieces %d", i, charged, sum)
				}
				if bound != "in all" && charged > aheadMax+slack {
					t.Fatalf("reader %d holds %d bytes ahead, over the bound %d (+ one piece)", i, charged, aheadMax)
				}
				if bound == "tiny writes" && pieces > 4 {
					t.Fatalf("tiny writes made %d pieces of what a stalled client has not taken: one each, not appended", pieces)
				}
			}
			for i, a := range rs {
				got, err := io.ReadAll(a)
				if err != nil {
					t.Fatal(err)
				}
				if len(got) != sent || !bytes.Equal(got, bytes.Repeat([]byte{byte('a' + i)}, sent)) {
					t.Fatalf("reader %d: read %d bytes, not what the app sent", i, len(got))
				}
				a.Close()
			}
			if n := aheadInAll.Load() - base; n != 0 {
				t.Fatalf("%d bytes still counted ahead once every reader was read and closed", n)
			}
		})
	}
}

// Close ends a consumer waiting on it and gives back what was queued but not
// read; the reader's own read of the connection ends with the connection.
func TestAheadReaderCloseGivesBackWhatItHeld(t *testing.T) {
	base := aheadInAll.Load()
	app, ingress := tcpPair(t)
	a := newAheadReader(ingress)
	if _, err := app.Write([]byte("queued, never read")); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	for aheadInAll.Load() == base {
		if time.Now().After(deadline) {
			t.Fatal("fixture: nothing was read ahead")
		}
		time.Sleep(time.Millisecond)
	}
	a.Close()
	if n := aheadInAll.Load() - base; n != 0 {
		t.Fatalf("%d bytes still counted ahead after Close", n)
	}

	b := newAheadReader(ingressPairFor(t))
	waiting := make(chan error, 1)
	go func() {
		_, err := b.Read(make([]byte, 16))
		waiting <- err
	}()
	time.Sleep(10 * time.Millisecond)
	b.Close()
	select {
	case err := <-waiting:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatalf("a Read waiting on a closed reader returned %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("Close did not end a Read waiting on it")
	}
}

func ingressPairFor(t *testing.T) net.Conn {
	_, ingress := tcpPair(t)
	return ingress
}

// memConn is a connection that reads one body and then ends.
type memConn struct {
	net.Conn
	r *bytes.Reader
}

func (c *memConn) Read(p []byte) (int, error) { return c.r.Read(p) }
func (c *memConn) Close() error               { return nil }

// What the read-ahead costs per response: one goroutine and a pooled read
// buffer per connection, and a copy of each piece it queues (a 64 KiB body,
// read ahead and then through, against reading the connection directly).
func BenchmarkAheadReader(b *testing.B) {
	body := bytes.Repeat([]byte("x"), 64<<10)
	sink := make([]byte, 4096)
	for _, ahead := range []bool{false, true} {
		b.Run(map[bool]string{false: "direct", true: "ahead"}[ahead], func(b *testing.B) {
			b.ReportAllocs()
			b.SetBytes(int64(len(body)))
			for i := 0; i < b.N; i++ {
				conn := &memConn{r: bytes.NewReader(body)}
				var r io.Reader = conn
				var a *aheadReader
				if ahead {
					a = newAheadReader(conn)
					r = a
				}
				for {
					if _, err := r.Read(sink); err != nil {
						break
					}
				}
				if a != nil {
					a.Close()
				}
			}
		})
	}
}
