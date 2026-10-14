package connseq

import (
	"bytes"
	"errors"
	"io"
	"net"
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
)

// before reports whether the chunk numbered a was numbered before the one
// numbered b.
func before(a, b uint32) bool { return fakeconn.Chunk{ConnSeq: a}.CapturedBefore(b) }

// pipeUpstream is an Upstream over a net.Pipe, which holds nothing back from
// its reader: what the destination has sent is what was read.
func pipeUpstream(t *testing.T) (*Upstream, net.Conn) {
	t.Helper()
	proxyEnd, dest := net.Pipe()
	t.Cleanup(func() { _ = proxyEnd.Close(); _ = dest.Close() })
	return NewUpstream(proxyEnd), dest
}

// Over a conn that is not a socket, a chunk is numbered by when it was read:
// the destination's bytes read before the client chunk was numbered are before
// it, whenever they are numbered, and what is read after it is after it.
func TestFromClientIsAfterWhatWasReadBeforeItOverAPipe(t *testing.T) {
	t.Parallel()
	u, dest := pipeUpstream(t)
	go func() { _, _ = dest.Write([]byte("early")) }()
	buf := make([]byte, 16)
	if n, err := u.Read(buf); err != nil || string(buf[:n]) != "early" {
		t.Fatalf("Read = %q, %v", buf[:n], err)
	}
	req := u.FromClient() // the destination's chunk is read, not yet numbered
	early := u.FromDest()
	go func() { _, _ = dest.Write([]byte("answer")) }()
	n, err := u.Read(buf)
	if err != nil || string(buf[:n]) != "answer" {
		t.Fatalf("Read = %q, %v", buf[:n], err)
	}
	answer := u.FromDest()
	if !before(early, req) || !before(req, answer) {
		t.Fatalf("early %d, request %d, answer %d: want early < request < answer", early, req, answer)
	}
}

// A Read never returns bytes from both sides of a cut: what it read past one
// is returned by the next Read, with the error that came with it.
func TestReadStopsAtACutAndReturnsTheRestNext(t *testing.T) {
	t.Parallel()
	u := NewUpstream(&scriptConn{reads: []scriptRead{{b: []byte("abcdef"), err: io.EOF}}})
	u.cuts, u.last = []cut{{at: 2, n: 1}, {at: 4, n: 3}}, 4
	var got []string
	var errs []error
	buf := make([]byte, 16)
	for i := 0; i < 3; i++ {
		n, err := u.Read(buf)
		got, errs = append(got, string(buf[:n])), append(errs, err)
	}
	if want := []string{"ab", "cd", "ef"}; !equal(got, want) || errs[0] != nil || errs[1] != nil || !errors.Is(errs[2], io.EOF) {
		t.Fatalf("Reads = %q, %v; want %q, the EOF with the last", got, errs, want)
	}
	if u.read != 6 {
		t.Fatalf("read = %d, want 6", u.read)
	}
}

// A small buffer takes what is held back in parts, and still never crosses a
// cut.
func TestReadOfWhatIsHeldBackFitsTheBufferAndTheCut(t *testing.T) {
	t.Parallel()
	u := NewUpstream(&scriptConn{reads: []scriptRead{{b: []byte("abcdefgh")}}})
	u.cuts, u.last = []cut{{at: 1, n: 1}, {at: 6, n: 3}}, 6
	var got []string
	for i, size := range []int{16, 3, 3, 3} { // the first takes all 8 from the conn
		buf := make([]byte, size)
		n, err := u.Read(buf)
		if err != nil {
			t.Fatalf("Read %d: %v", i, err)
		}
		got = append(got, string(buf[:n]))
	}
	if want := []string{"a", "bcd", "ef", "gh"}; !equal(got, want) {
		t.Fatalf("Reads = %q, want %q", got, want)
	}
}

// Chunks read from the destination take the number of the first cut they do
// not pass, so each is before the client chunk whose cut it is under and after
// every earlier one; past every cut, a chunk is after every chunk numbered.
func TestFromDestTakesTheFirstCutItDoesNotPass(t *testing.T) {
	t.Parallel()
	u := NewUpstream(&scriptConn{})
	// Over a conn that is not a socket, what was sent is what was read and
	// what is held back (sentLocked): two client chunks, numbered when 3 and
	// then 7 bytes had been sent, place two cuts.
	u.rest = make([]byte, 3)
	q1 := u.FromClient()
	u.rest = make([]byte, 7)
	q2 := u.FromClient()
	u.rest = nil
	if len(u.cuts) != 2 || u.cuts[0].at != 3 || u.cuts[1].at != 7 {
		t.Fatalf("cuts %+v, want them at 3 and 7", u.cuts)
	}
	var got []uint32
	for _, read := range []uint64{2, 3, 5, 7, 9} {
		u.read = read // the chunk read from the destination ends here
		got = append(got, u.FromDest())
	}
	if !before(got[0], q1) || got[0] != got[1] {
		t.Fatalf("bytes up to 3 numbered %d and %d, the first request %d: want both before it", got[0], got[1], q1)
	}
	if !before(q1, got[2]) || got[2] != got[3] || !before(got[3], q2) {
		t.Fatalf("bytes up to 7 numbered %d and %d, requests %d and %d: want between them", got[2], got[3], q1, q2)
	}
	if !before(q2, got[4]) {
		t.Fatalf("bytes past 7 numbered %d, the second request %d: want after it", got[4], q2)
	}
	if len(u.cuts) != 0 {
		t.Fatalf("cuts left %+v, want none once a chunk is past them", u.cuts)
	}
}

// A client chunk with nothing more sent since the last cut places none.
func TestFromClientPlacesACutOnlyForWhatWasSentSinceTheLast(t *testing.T) {
	t.Parallel()
	u := NewUpstream(&scriptConn{})
	u.FromClient()
	if len(u.cuts) != 0 {
		t.Fatalf("cuts %+v with nothing sent, want none", u.cuts)
	}
	u.rest = make([]byte, 4)
	u.FromClient()
	u.FromClient()
	if len(u.cuts) != 1 {
		t.Fatalf("cuts %+v, want one: nothing was sent between the second client chunk and the third", u.cuts)
	}
}

// Of finds the Upstream beneath the wrappers that expose the conn under them.
func TestOfFindsTheUpstreamBeneathWrappers(t *testing.T) {
	t.Parallel()
	u, _ := pipeUpstream(t)
	if Of(u) != u || Of(netConnWrapper{netConnWrapper{u}}) != u {
		t.Fatal("Of did not find the Upstream")
	}
	if Of(netConnWrapper{&scriptConn{}}) != nil || Of(&scriptConn{}) != nil || Of(nil) != nil {
		t.Fatal("Of found an Upstream where there is none")
	}
}

type netConnWrapper struct{ net.Conn }

func (w netConnWrapper) NetConn() net.Conn { return w.Conn }

type scriptRead struct {
	b   []byte
	err error
}

// scriptConn returns its reads in turn, then io.EOF.
type scriptConn struct {
	net.Conn
	reads []scriptRead
}

func (c *scriptConn) Read(p []byte) (int, error) {
	if len(c.reads) == 0 {
		return 0, io.EOF
	}
	r := c.reads[0]
	n := copy(p, r.b)
	if n < len(r.b) {
		c.reads[0].b = r.b[n:]
		return n, nil
	}
	c.reads = c.reads[1:]
	return n, r.err
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if !bytes.Equal([]byte(a[i]), []byte(b[i])) {
			return false
		}
	}
	return true
}
