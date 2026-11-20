//go:build linux

package http

import (
	"bufio"
	"context"
	"crypto/md5"
	"encoding/base64"
	"errors"
	"io"
	"net"
	nethttp "net/http"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
	"golang.org/x/sys/unix"
)

// readRequestHead reads one request head off c, as the destination does.
func readRequestHead(t *testing.T, c net.Conn) string {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()
	r := bufio.NewReader(c)
	var head strings.Builder
	for {
		line, err := r.ReadString('\n')
		if err != nil {
			t.Fatalf("the destination did not get a whole request head: %q, %v", head.String(), err)
		}
		head.WriteString(line)
		if line == "\r\n" {
			if r.Buffered() != 0 {
				t.Fatalf("the destination got %d bytes past the request head", r.Buffered())
			}
			return head.String()
		}
	}
}

// awaitFIN waits until c's peer has sent its FIN: c reads to its end.
func awaitFIN(t *testing.T, c net.Conn) {
	t.Helper()
	_ = c.SetReadDeadline(time.Now().Add(10 * time.Second))
	defer func() { _ = c.SetReadDeadline(time.Time{}) }()
	if rest, err := io.ReadAll(c); err != nil || len(rest) != 0 {
		t.Fatalf("the destination read %d more bytes, %v, before the FIN", len(rest), err)
	}
}

// warnings is what the recording logged at WARN or above, but the note made
// once per process at the first call recorded with a fault
// (noteFaultRecorded), which says nothing of this call.
func (c *relayedConn) warnings() []observer.LoggedEntry {
	return c.logs.Filter(func(e observer.LoggedEntry) bool {
		return e.Level >= zapcore.WarnLevel && !strings.Contains(e.Message, "keploy now records a call that ended in a fault")
	}).All()
}

// Through the relay, as the proxy records: a Go http.Client gives up on a call
// (its context ends, as at a timeout) and closes the connection (close(2), not
// a half-close), and the dependency answers once the FIN has reached it. The
// relay forwards the answer, and the client's TCP stack refuses it with a
// reset: the application never read it. The call is recorded as what the
// application saw, a timeout fault (no answer before it closed), and the late
// answer is not in it: replay holds the call unanswered until the application
// gives up again. Nothing is left out for it. Before, the answer was recorded,
// a mock for a call whose test case saw a timeout; then nothing was, and
// replay had no mock for the call. The call's context is cancelled once the
// dependency has the request, not by a deadline, so a slow runner cannot end
// the call before its request is out.
func TestRecordV2_ThroughTheRelayALateAnswerToACallTheClientGaveUpOnIsRecordedAsATimeout(t *testing.T) {
	c := newRelayedConn(t)
	c.start(t)
	client := &nethttp.Client{
		Transport: &nethttp.Transport{DialContext: func(context.Context, string, string) (net.Conn, error) {
			return c.app, nil
		}},
	}
	ctx, giveUp := context.WithCancel(context.Background())
	defer giveUp()
	req, err := nethttp.NewRequestWithContext(ctx, nethttp.MethodGet, "http://orders.internal:8443/orders/7", nil)
	if err != nil {
		t.Fatal(err)
	}
	called := make(chan error, 1)
	go func() {
		resp, err := client.Do(req)
		if err == nil {
			_ = resp.Body.Close()
		}
		called <- err
	}()
	if head := readRequestHead(t, c.dest); !strings.HasPrefix(head, "GET /orders/7 HTTP/1.1\r\n") {
		t.Fatalf("the destination got %q", head)
	}
	// The client gives up and closes the connection; the relay forwards the
	// FIN.
	giveUp()
	awaitFIN(t, c.dest)
	if err := <-called; !errors.Is(err, context.Canceled) {
		t.Fatalf("the call returned %v, want it given up on", err)
	}
	if _, err := c.dest.Write([]byte(jdkResponse(7))); err != nil {
		t.Fatal(err)
	}
	mocks := c.end(t)
	if len(mocks) != 1 {
		t.Fatalf("recorded %d mock(s), want 1: the call the client gave up on, as a timeout", len(mocks))
	}
	m := mocks[0]
	// A timeout before any answer: the client closed with nothing read.
	wantFault(t, m, models.Fault{Kind: models.FaultTimeout, AppEndedFirst: true})
	if r := m.Spec.HTTPResp; r != nil && (r.StatusCode != 0 || r.Body != "" || len(r.Header) != 0) {
		t.Fatalf("recorded answer %+v, want none: the client never read the late answer", r)
	}
	if s := c.spans.all(); len(s) != 0 || c.mgr.MocksLeftOut() != 0 {
		t.Fatalf("left out %v (%d counted), want nothing: the call is recorded", s, c.mgr.MocksLeftOut())
	}
	for _, w := range c.warnings() {
		if !strings.Contains(w.Message, "keploy now records a call that ended in a fault") {
			t.Fatalf("logged at %v: %q", w.Level, w.Message)
		}
	}
}

// Through the relay, as the proxy records: a client that half-closes after its
// request (shutdown(SHUT_WR)) and then reads its answer, as an EOF-delimited
// client does. The dependency answers once it has the FIN, the client takes
// the answer and reads it, and it is recorded as before.
func TestRecordV2_ThroughTheRelayAnAnswerAfterAHalfCloseIsRecorded(t *testing.T) {
	c := newRelayedConn(t)
	c.start(t)
	write(t, c.app, c.src, jdkRequest(7))
	relayed(t, c.dest, jdkRequest(7))
	if err := c.app.(*net.TCPConn).CloseWrite(); err != nil {
		t.Fatal(err)
	}
	awaitFIN(t, c.dest)
	if _, err := c.dest.Write([]byte(jdkResponse(7))); err != nil {
		t.Fatal(err)
	}
	relayed(t, c.app, jdkResponse(7))
	assertPairs(t, c.end(t), 7)
	if s := c.spans.all(); len(s) != 0 || c.mgr.MocksLeftOut() != 0 {
		t.Fatalf("left out %v (%d counted), want nothing", s, c.mgr.MocksLeftOut())
	}
}

// leftOut is what the recording said of the exchanges it left out for reason:
// the left-out lines (the WARN, or the Debug line its process-wide limit holds
// back) with that reason.
func (c *relayedConn) leftOut(reason string) []observer.LoggedEntry {
	return c.logs.Filter(func(e observer.LoggedEntry) bool {
		return (e.Message == supervisor.LeftOutWarnMsg || e.Message == supervisor.LeftOutDebugMsg) &&
			e.ContextMap()["reason"] == reason
	}).All()
}

// Through the relay, as the proxy records: a client reads a response's head and
// the start of its body, all it has been sent so far, and closes the
// connection (its timeout, a cancelled call, or it wanted only the start); the
// rest of the response reaches the proxy after the client's FIN. The relay
// forwards it, the client refuses it with a reset, and the parser is not given
// it. The exchange is recorded as far as the client read it, marked as one the
// app stopped reading (models.FaultStoppedReading), with no length:
// replay serves that much and holds the connection open without the end until
// the app closes it, so a call that timed out at record waits and times out
// again, and is never served a complete-looking shorter response. That holds
// for a parser running behind its capture too, which reads the cut response
// once the relay has ended and closed its streams: their end reads io.EOF
// however late it is read. A body the server's close ends (no Content-Length,
// no chunking) is recorded so too: there the stream's end is the body's end,
// and only the relay's word that the stream ended where the client stopped
// keeps its first 500 bytes from being recorded as a whole, shorter 200. No
// digest is noted to recompute at replay, even a Content-MD5 of what the
// client read: the client never had the body's end, so it checked none. What
// the client took is no response when it closed before the whole head came, or
// when the capture lost a chunk before the cut (a mark on the incomplete-mock
// flag): the exchange is left out, and said so, under that mark when there is
// one, with its next step, and the mark is taken with it. It is not taken for
// a response the server cut short (a decode error), and recordV2 returns nil.
func TestRecordV2_ThroughTheRelayAResponseTheClientClosedPartwayIsRecordedAsFarAsItRead(t *testing.T) {
	body := strings.Repeat("0123456789", 200)
	for _, row := range []struct {
		name string
		// behind: recordV2 starts only once the relay has ended.
		behind bool
		// mark: the capture lost a chunk before the client closed, and
		// marked the mock incomplete for it.
		mark string
		// head, read: the response's head, and what the client reads of
		// the response before it closes (head and body[:500] when empty).
		head, read string
		// leftOut: the exchange is left out; else recorded as far as read.
		leftOut bool
		// noBody: the response has no body by rule, and what the server
		// sends after its head is bytes the client did not take.
		noBody bool
	}{
		{name: "the parser keeping up"},
		{name: "the parser behind its capture", behind: true},
		{name: "a chunk lost before the client closed", mark: "per_conn_cap", leftOut: true},
		{name: "a body the server's close ends", head: "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\n"},
		{name: "a body the server's close ends, the parser behind its capture", behind: true,
			head: "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nConnection: close\r\n\r\n"},
		// Its Content-MD5 is of what the client read: the body read so far
		// matches it, and still no digest is noted.
		{name: "a body the server's close ends, with a digest of what the client read",
			head: "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-MD5: " + md5Base64(body[:500]) + "\r\nConnection: close\r\n\r\n"},
		{name: "a head the client did not read whole", read: "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n", leftOut: true},
		{name: "a head the client did not read whole, the parser behind its capture", behind: true,
			read: "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n", leftOut: true},
		// The interim answer's head is whole; the final one's is not, and
		// it is the final answer that is no response.
		{name: "an interim answer, then a final head the client did not read whole",
			head: "HTTP/1.1 103 Early Hints\r\nLink: </app.css>; rel=preload\r\n\r\nHTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2000\r\n\r\n",
			read: "HTTP/1.1 103 Early Hints\r\nLink: </app.css>; rel=preload\r\n\r\nHTTP/1.1 200 OK\r\nContent-Type: text/plain\r\n", leftOut: true},
		// A 304's Content-Length is the size of the body a 200 would have
		// had: the head is the whole answer, recorded with the length it
		// was sent with, and served whole.
		{name: "a 304 with a length, then bytes the client did not take", noBody: true,
			head: "HTTP/1.1 304 Not Modified\r\nContent-Length: 2000\r\n\r\n", read: "HTTP/1.1 304 Not Modified\r\nContent-Length: 2000\r\n\r\n"},
	} {
		// silent: the server sends nothing after the client's FIN, so the
		// relay never says the stream ended where the client stopped: the
		// client's end came first, and the same rows hold.
		for _, silent := range []bool{false, true} {
			for _, proven := range []string{"", "1"} {
				after := "the rest after the FIN"
				if silent {
					after = "the server silent after the FIN"
				}
				t.Run(row.name+"/"+after+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
					t.Setenv("KEPLOY_PROVEN", proven)
					c := newRelayedConn(t)
					c.startRelay(t)
					if !row.behind {
						c.startParser(t)
					}
					write(t, c.app, c.src, jdkRequest(7))
					relayed(t, c.dest, jdkRequest(7))
					head := row.head
					if head == "" {
						head = "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2000\r\n\r\n"
					}
					whole := head + body
					read := row.read
					if read == "" {
						read = head + body[:500]
					}
					if _, err := c.dest.Write([]byte(read)); err != nil {
						t.Fatal(err)
					}
					relayed(t, c.app, read)
					if row.mark != "" {
						c.sess.MarkMockIncomplete(row.mark)
					}
					if err := c.app.Close(); err != nil {
						t.Fatal(err)
					}
					awaitFIN(t, c.dest)
					if !silent {
						if _, err := c.dest.Write([]byte(whole[len(read):])); err != nil {
							t.Fatal(err)
						}
					}
					if row.behind {
						_ = c.dest.Close()
						select {
						case <-c.relayDone:
						case <-time.After(10 * time.Second):
							t.Fatal("the relay did not end once both sides had closed")
						}
						c.startParser(t)
					}
					mocks := c.end(t)
					if !row.leftOut {
						if len(mocks) != 1 {
							t.Fatalf("recorded %d mocks, want the one exchange, as far as the client read it", len(mocks))
						}
						if row.noBody {
							got := mocks[0].Spec.HTTPResp
							if got == nil || got.StatusCode != 304 || got.Body != "" || got.Header["Content-Length"] != "2000" {
								t.Fatalf("recorded %+v, want the 304 with its Content-Length of 2000 and no body", got)
							}
							if f := mocks[0].Spec.Fault; f != nil {
								t.Fatalf("the 304 is recorded with the fault %s, want it whole: its head is all of it", f)
							}
						} else {
							// A row's own head is one the server's close ends.
							sent := sentAs{length: 2000}
							if row.head != "" {
								sent = sentByClose
							}
							wantStoppedReading(t, mocks[0], body[:500], sent)
						}
						if s := c.spans.all(); len(s) != 0 || c.mgr.MocksLeftOut() != 0 {
							t.Fatalf("left out %v (%d counted), want nothing", s, c.mgr.MocksLeftOut())
						}
						if w := c.warnings(); len(w) != 0 {
							t.Fatalf("logged at %v: %q", w[0].Level, w[0].Message)
						}
						return
					}
					if len(mocks) != 0 {
						t.Fatalf("recorded %d mock(s), the first a %d with a %d-byte body; want none: what the client took is no response",
							len(mocks), mocks[0].Spec.HTTPResp.StatusCode, len(mocks[0].Spec.HTTPResp.Body))
					}
					if n := c.mgr.MocksLeftOut(); n != 1 || len(c.spans.all()) != 1 {
						t.Fatalf("%d mocks left out, spans %v; want the one exchange, spanned", n, c.spans.all())
					}
					reason, step := supervisor.LeftOutClientClosedPartway, "the client closed partway"
					if row.mark != "" {
						reason, step = row.mark+": "+supervisor.LeftOutClientClosedPartway, "reason=per_conn_cap"
					}
					if l := c.leftOut(reason); len(l) != 1 {
						t.Fatalf("%d left-out lines with the reason %q, want one", len(l), reason)
					} else if l[0].Message == supervisor.LeftOutWarnMsg && !strings.Contains(l[0].ContextMap()["next_step"].(string), step) {
						t.Fatalf("the left-out WARN's next_step is %q, want the one of its cause (%q)", l[0].ContextMap()["next_step"], step)
					}
					if m, ok := c.sess.TakeMockIncomplete(); ok {
						t.Fatalf("the mark %q is still set: the exchange reported under it did not take it", m)
					}
					for _, w := range c.warnings() {
						if w.Message != supervisor.LeftOutWarnMsg {
							t.Fatalf("logged at %v: %q", w.Level, w.Message)
						}
					}
				})
			}
		}
	}
}

// Through the relay, as the proxy records: the client stops reading a chunked
// response partway, and what it took does not decode: a chunk-size line longer
// than net/http's own limit (4096 bytes), which the recorder's framing reader
// takes (up to 64 KiB). A response net/http cannot read is not recorded, as a
// whole one is not: recordV2 says it could not read the body, and records
// nothing. A last chunk followed by a trailer field line ended by a bare LF,
// which the RFC does not frame, is refused by the recorder's framing reader
// itself, before any mock is built: recordV2 returns its framing error. Only a
// body cut short, or one cut after its last chunk in a trailer framed by the
// RFC (lastChunkRead), is recorded as far as the client read it: an answer
// recorded from any other error would serve the app what it never saw.
func TestRecordV2_ThroughTheRelayAResponseTheClientStoppedReadingThatDoesNotDecodeIsNotRecorded(t *testing.T) {
	const head = "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nConnection: close\r\nTransfer-Encoding: chunked\r\n\r\n"
	for _, row := range []struct {
		name, read, rest, err string
		// cutByTheStream: recordV2 reads to the stream's end, where the
		// relay ended it, before it finds it cannot read the body; a
		// framing error ends the read where it is.
		cutByTheStream bool
	}{
		{"a chunk-size line over net/http's limit", head + "1;note=" + strings.Repeat("x", 5000) + "\r\n{\r\n", "1\r\n}\r\n0\r\n\r\n", "read response body", true},
		{"a trailer line ended by a bare LF", head + "1\r\n{\r\n0\r\nX-Checksum: 1\n", "\r\n", "a line ends with a bare LF", false},
	} {
		read := row.read
		for _, proven := range []string{"", "1"} {
			t.Run(row.name+"/KEPLOY_PROVEN="+proven, func(t *testing.T) {
				t.Setenv("KEPLOY_PROVEN", proven)
				c := newRelayedConn(t)
				c.start(t)
				write(t, c.app, c.src, jdkRequest(7))
				relayed(t, c.dest, jdkRequest(7))
				if _, err := c.dest.Write([]byte(read)); err != nil {
					t.Fatal(err)
				}
				relayed(t, c.app, read)
				if err := c.app.Close(); err != nil {
					t.Fatal(err)
				}
				awaitFIN(t, c.dest)
				if _, err := c.dest.Write([]byte(row.rest)); err != nil {
					t.Fatal(err)
				}
				_ = c.dest.Close()
				select {
				case err := <-c.done:
					if err == nil || !strings.Contains(err.Error(), row.err) {
						t.Fatalf("recordV2 = %v, want the error that it could not read the body (%q)", err, row.err)
					}
				case <-time.After(10 * time.Second):
					t.Fatal("recordV2 did not return within 10s of the connection's end")
				}
				if row.cutByTheStream && !c.sess.EndedWhereClientStopped() {
					t.Fatal("the relay did not say the destination's stream ended where the client stopped")
				}
				select {
				case m := <-c.mocks:
					t.Fatalf("recorded a %d with the body %q (fault: %s); want nothing: net/http does not read it",
						m.Spec.HTTPResp.StatusCode, m.Spec.HTTPResp.Body, m.Spec.Fault)
				default:
				}
			})
		}
	}
}

// Through the relay, as the proxy records: the client stops reading a gzip
// body the server's close ends (no Content-Length, no chunking). The parser
// reads that body with no error, as far as the stream goes, so only the mark
// tells the recorder it is a part: it is decoded as far as it goes, and
// recorded marked, its text a leading part of what the server sent. Decoded as
// a whole one, it fails on its cut, and the exchange is lost.
func TestRecordV2_ThroughTheRelayAGzipBodyTheClientStoppedReadingIsDecodedAsFarAsItRead(t *testing.T) {
	text := strings.Repeat("keploy records what the client read. ", 400)
	z := string(gzipped(t, text))
	read := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Encoding: gzip\r\nConnection: close\r\n\r\n" + z[:len(z)/2]
	for _, proven := range []string{"", "1"} {
		t.Run("KEPLOY_PROVEN="+proven, func(t *testing.T) {
			t.Setenv("KEPLOY_PROVEN", proven)
			c := newRelayedConn(t)
			c.start(t)
			write(t, c.app, c.src, jdkRequest(7))
			relayed(t, c.dest, jdkRequest(7))
			if _, err := c.dest.Write([]byte(read)); err != nil {
				t.Fatal(err)
			}
			relayed(t, c.app, read)
			if err := c.app.Close(); err != nil {
				t.Fatal(err)
			}
			awaitFIN(t, c.dest)
			if _, err := c.dest.Write([]byte(z[len(z)/2:])); err != nil {
				t.Fatal(err)
			}
			mocks := c.end(t)
			if len(mocks) != 1 {
				t.Fatalf("recorded %d mocks, want the one exchange, decoded as far as the client read it", len(mocks))
			}
			got := mocks[0].Spec.HTTPResp
			if f := mocks[0].Spec.Fault; f == nil || f.Kind != models.FaultStoppedReading || got.Body == "" || !strings.HasPrefix(text, got.Body) {
				t.Fatalf("recorded %d bytes of body (fault: %s), want a leading part of the %d the server sent, with a stopped_reading fault",
					len(got.Body), f, len(text))
			}
			if cl := got.Header["Content-Length"]; cl != strconv.Itoa(len(got.Body)) {
				t.Fatalf("recorded with a Content-Length of %q, want a whole answer's head: the %d bytes of body the app decoded", cl, len(got.Body))
			}
			// Replay serves the body decoded, compressed again: the fault
			// counts the bytes it serves, not the compressed ones the app
			// read; and the upstream's close was to end the body.
			if f := mocks[0].Spec.Fault; f.BodyBytes != int64(len(got.Body)) || f.ContentLength != 0 || f.Chunked {
				t.Fatalf("recorded the fault %+v, want %d body bytes (the body decoded), framed by the close", f, len(got.Body))
			}
		})
	}
}

// md5Base64 is the Content-MD5 value of s.
func md5Base64(s string) string {
	sum := md5.Sum([]byte(s))
	return base64.StdEncoding.EncodeToString(sum[:])
}

// Through the relay, as the proxy records: a client closes partway through a
// response, and the relay ends the destination's stream where the client
// stopped. But before the relay has ended the stream, the session closes the
// parser's stream (its abort, whose fallthrough leaves out the rest of the
// connection under its own reason): the parser's read fails, as a read of a
// closed stream does (ErrClosed), not at the stream's end (io.EOF). Only a
// stream that ran out ended where the client stopped, so the exchange is not
// reported as one the client closed partway: that would name the wrong cause.
func TestRecordV2_ThroughTheRelayAStreamTheSessionClosedIsNotTheClientsClosePartway(t *testing.T) {
	c := newRelayedConn(t)
	// The relay must still be capturing when the session closes the stream:
	// a grace this long keeps it from ending on an idle destination first.
	c.grace = time.Hour
	c.start(t)
	write(t, c.app, c.src, jdkRequest(7))
	relayed(t, c.dest, jdkRequest(7))
	body := strings.Repeat("0123456789", 200)
	read := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2000\r\n\r\n" + body[:500]
	if _, err := c.dest.Write([]byte(read)); err != nil {
		t.Fatal(err)
	}
	relayed(t, c.app, read)
	if err := c.app.Close(); err != nil {
		t.Fatal(err)
	}
	awaitFIN(t, c.dest)
	if _, err := c.dest.Write([]byte(body[500:])); err != nil {
		t.Fatal(err)
	}
	for limit := time.Now().Add(10 * time.Second); !c.sess.EndedWhereClientStopped(); {
		if time.Now().After(limit) {
			t.Fatal("the relay did not end the stream where the client stopped")
		}
		time.Sleep(50 * time.Microsecond)
	}
	select {
	case <-c.relayDone:
		t.Fatal("the relay ended before the session closed the stream")
	default:
	}
	_ = c.sess.DestStream.Close()
	_ = c.sess.ClientStream.Close()
	// What recordV2 returns, and reports, for a stream the session closed
	// under it is the abort's matter, not this row's.
	select {
	case <-c.done:
	case <-time.After(10 * time.Second):
		t.Fatal("recordV2 did not return once the session closed its streams")
	}
	if l := c.leftOut(supervisor.LeftOutClientClosedPartway); len(l) != 0 {
		t.Fatalf("%d left-out lines say the client closed partway, want none: the session closed the stream", len(l))
	}
	if n := c.logs.FilterMessageSnippet("the client closed the connection partway").Len(); n != 0 {
		t.Fatalf("%d lines say the client closed partway, want none", n)
	}
}

// Through the relay, as the proxy records, with the parser behind its capture
// (it starts only once the relay has ended): a client takes a whole response,
// sends its next request, and closes the connection before its answer, which
// it refuses. The parser reads the first response knowing the stream ended
// where the client stopped, and records it all the same: it is whole, and only
// a response the stream ends in the middle of is left out. The second request
// got none of its answer: it is recorded as what the client saw, a timeout
// (the client gave up on it), and nothing is left out.
func TestRecordV2_ThroughTheRelayAWholeResponseBeforeTheClientStoppedIsRecorded(t *testing.T) {
	c := newRelayedConn(t)
	c.startRelay(t)
	write(t, c.app, c.src, jdkRequest(7))
	relayed(t, c.dest, jdkRequest(7))
	if _, err := c.dest.Write([]byte(jdkResponse(7))); err != nil {
		t.Fatal(err)
	}
	relayed(t, c.app, jdkResponse(7))
	write(t, c.app, c.src, jdkRequest(8))
	relayed(t, c.dest, jdkRequest(8))
	if err := c.app.Close(); err != nil {
		t.Fatal(err)
	}
	awaitFIN(t, c.dest)
	if _, err := c.dest.Write([]byte(jdkResponse(8))); err != nil {
		t.Fatal(err)
	}
	_ = c.dest.Close()
	select {
	case <-c.relayDone:
	case <-time.After(10 * time.Second):
		t.Fatal("the relay did not end once both sides had closed")
	}
	if !c.sess.EndedWhereClientStopped() {
		t.Fatal("the relay did not say the destination's stream ended where the client stopped")
	}
	c.startParser(t)
	mocks := c.end(t)
	if len(mocks) != 2 {
		t.Fatalf("recorded %d mocks, want 2: the whole response, then the call the client gave up on", len(mocks))
	}
	wantFault(t, mocks[1], models.Fault{Kind: models.FaultTimeout, AppEndedFirst: true})
	assertPairs(t, mocks[:1], 7)
	if s := c.spans.all(); len(s) != 0 || c.mgr.MocksLeftOut() != 0 {
		t.Fatalf("left out %v (%d counted), want nothing", s, c.mgr.MocksLeftOut())
	}
	for _, w := range c.warnings() {
		if !strings.Contains(w.Message, "keploy now records a call that ended in a fault") {
			t.Fatalf("logged at %v: %q", w.Level, w.Message)
		}
	}
}

// resetBeforeWriteReturns is the proxy's end of the client connection whose
// Write returns only once the connection has been reset (or after 5s): a reset
// the client sends for what it was written is in before the relay can look at
// the socket again, every time, as it is by chance on a busy host when the
// client runs between the relay's write and its next step.
type resetBeforeWriteReturns struct {
	net.Conn
}

func (c *resetBeforeWriteReturns) Write(p []byte) (int, error) {
	n, err := c.Conn.Write(p)
	if err == nil {
		for limit := time.Now().Add(5 * time.Second); time.Now().Before(limit) && !isReset(c.Conn); {
			time.Sleep(50 * time.Microsecond)
		}
	}
	return n, err
}

func (c *resetBeforeWriteReturns) NetConn() net.Conn { return c.Conn }

// isReset reports whether the TCP socket under c is reset (TCP_CLOSE).
func isReset(c net.Conn) bool {
	rc, err := c.(syscall.Conn).SyscallConn()
	if err != nil {
		return false
	}
	var reset bool
	_ = rc.Control(func(fd uintptr) {
		if info, err := unix.GetsockoptTCPInfo(int(fd), unix.IPPROTO_TCP, unix.TCP_INFO); err == nil {
			reset = info.State == unix.BPF_TCP_CLOSE
		}
	})
	return reset
}

// Through the relay, as the proxy records: a client reads a response's head and
// closes the connection with its body unread, so its stack resets instead of
// sending a FIN (an abortive close). The relay had written it the whole
// response before the reset, and the response is recorded as the server sent
// it, as before: no FIN reached the proxy before it was written, so nothing is
// refused, whenever the reset comes in.
func TestRecordV2_ThroughTheRelayAResponseTheClientResetWithItsBodyUnreadIsRecorded(t *testing.T) {
	for _, row := range []struct {
		name string
		wrap func(net.Conn) net.Conn
	}{
		{name: "the reset in its own time"},
		{name: "the reset in before the relay looks again", wrap: func(c net.Conn) net.Conn { return &resetBeforeWriteReturns{Conn: c} }},
	} {
		t.Run(row.name, func(t *testing.T) {
			c := newRelayedConn(t)
			c.wrapSrc = row.wrap
			c.start(t)
			write(t, c.app, c.src, jdkRequest(7))
			relayed(t, c.dest, jdkRequest(7))
			body := strings.Repeat("0123456789", 200)
			head := "HTTP/1.1 200 OK\r\nContent-Type: text/plain\r\nContent-Length: 2000\r\n\r\n"
			want := uint64(received(t, c.app)) + uint64(len(head)+len(body))
			if _, err := c.dest.Write([]byte(head + body)); err != nil {
				t.Fatal(err)
			}
			relayed(t, c.app, head)
			for limit := time.Now().Add(5 * time.Second); received(t, c.app) < want; {
				if time.Now().After(limit) {
					t.Fatal("the client did not receive the whole response in 5s")
				}
				time.Sleep(50 * time.Microsecond)
			}
			if err := c.app.Close(); err != nil {
				t.Fatal(err)
			}
			mocks := c.end(t)
			if len(mocks) != 1 {
				t.Fatalf("recorded %d mocks, want the one exchange", len(mocks))
			}
			if r := mocks[0].Spec.HTTPResp; r.StatusCode != 200 || r.Body != body {
				t.Fatalf("recorded the response %d with a %d-byte body, want 200 with the %d bytes the server sent", r.StatusCode, len(r.Body), len(body))
			}
			if s := c.spans.all(); len(s) != 0 || c.mgr.MocksLeftOut() != 0 {
				t.Fatalf("left out %v (%d counted), want nothing", s, c.mgr.MocksLeftOut())
			}
			if w := c.warnings(); len(w) != 0 {
				t.Fatalf("logged at %v: %q", w[0].Level, w[0].Message)
			}
		})
	}
}
