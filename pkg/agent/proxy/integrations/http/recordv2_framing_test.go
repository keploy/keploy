package http

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap/zaptest"
)

// RFC 9112 section 6.3 forbids a message body on 1xx, 204 and 304, and on any
// response to HEAD, regardless of Content-Length / Transfer-Encoding. Framing
// such a response from its headers sends readResponseV2 into the read-until-EOF
// fallback, which then consumes every LATER framingExchange on the same keep-alive
// connection as if it were this response's body.
//
// The damage is silent: recordV2 returns nil, so the supervisor records
// StatusOK, no "parser retired" warning is logged, and no orphan window is
// opened. On a Chromium workload (304 revalidation of static assets, 204 CORS
// preflights) that is most of a connection's recording, lost without a trace.
//
// These tests drive recordV2 over a scripted keep-alive connection and assert
// that every framingExchange is recorded and correctly paired.

type framingExchange struct {
	req  []byte
	resp []byte
}

func framingGetReq(path string) []byte {
	return []byte("GET " + path + " HTTP/1.1\r\nHost: x\r\n\r\n")
}

func framingOKBody(b string) []byte {
	return []byte(fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(b), b))
}

// runFramingExchanges plays the script over one connection and returns the emitted mocks.
func runFramingExchanges(t *testing.T, script []framingExchange) []*models.Mock {
	t.Helper()
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	sess, sendReq, closeReq, sendResp, closeResp, mocks := newTestSession(t)
	base := time.Unix(1_700_000_000, 0)

	// sent is closed once the feeder has finished and closed both streams. The
	// helper waits on it before returning: without that the feeder outlives the
	// test and its closes race t.Cleanup's, which `go test -race` reports.
	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for i, ex := range script {
			ts := base.Add(time.Duration(i) * time.Second)
			sendReq(ex.req, ts, ts)
			time.Sleep(15 * time.Millisecond)
			sendResp(ex.resp, ts, ts)
			time.Sleep(15 * time.Millisecond)
		}
		closeResp()
		closeReq()
	}()

	done := make(chan error, 1)
	go func() { done <- h.recordV2(sess.Ctx, sess) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("parser never returned — it is blocked framing a body that will never arrive")
	}
	select {
	case <-sent:
	case <-time.After(5 * time.Second):
		t.Fatal("feeder goroutine never finished")
	}

	var got []*models.Mock
	for {
		select {
		case m := <-mocks:
			got = append(got, m)
		default:
			return got
		}
	}
}

func framingURLsOf(ms []*models.Mock) []string {
	out := make([]string, 0, len(ms))
	for _, m := range ms {
		out = append(out, m.Spec.HTTPReq.URL)
	}
	return out
}

// A frameless 304 on a keep-alive connection must not swallow the exchanges
// that follow it. Before the fix this recorded exactly ONE mock (the 304) and
// returned nil, losing three of four exchanges silently.
func TestRecordV2_FramelessNotModifiedDoesNotSwallowKeepAlive(t *testing.T) {
	got := runFramingExchanges(t, []framingExchange{
		{framingGetReq("/one"), []byte("HTTP/1.1 304 Not Modified\r\nETag: \"a\"\r\n\r\n")},
		{framingGetReq("/two"), framingOKBody("TWO")},
		{framingGetReq("/three"), framingOKBody("THREE")},
		{framingGetReq("/four"), framingOKBody("FOUR")},
	})
	require.Equal(t, []string{"/one", "/two", "/three", "/four"}, framingURLsOf(got))
	require.Equal(t, 304, got[0].Spec.HTTPResp.StatusCode)
	require.Equal(t, "TWO", got[1].Spec.HTTPResp.Body)
	require.Equal(t, "FOUR", got[3].Spec.HTTPResp.Body)
}

// A 204 CORS preflight is the same shape and the most likely trigger on this
// project's workload — the browser preflights every cross-origin API call.
func TestRecordV2_NoContentPreflightDoesNotSwallowKeepAlive(t *testing.T) {
	preflight := []byte("OPTIONS /api HTTP/1.1\r\nHost: x\r\nAccess-Control-Request-Method: POST\r\n\r\n")
	got := runFramingExchanges(t, []framingExchange{
		{preflight, []byte("HTTP/1.1 204 No Content\r\nAccess-Control-Allow-Origin: *\r\n\r\n")},
		{framingGetReq("/after"), framingOKBody("AFTER")},
	})
	require.Equal(t, []string{"/api", "/after"}, framingURLsOf(got))
	require.Equal(t, "AFTER", got[1].Spec.HTTPResp.Body)
}

// A 304 that DOES carry Content-Length still has no body — the header describes
// the body the response would have had. Trusting it blocks the parser forever.
func TestRecordV2_NotModifiedWithContentLengthHasNoBody(t *testing.T) {
	got := runFramingExchanges(t, []framingExchange{
		{framingGetReq("/one"), []byte("HTTP/1.1 304 Not Modified\r\nContent-Length: 1024\r\n\r\n")},
		{framingGetReq("/two"), framingOKBody("TWO")},
	})
	require.Equal(t, []string{"/one", "/two"}, framingURLsOf(got))
	require.Equal(t, "TWO", got[1].Spec.HTTPResp.Body)
}

// A response to HEAD is bodyless whatever its Content-Length says.
func TestRecordV2_HeadResponseHasNoBody(t *testing.T) {
	head := []byte("HEAD /asset HTTP/1.1\r\nHost: x\r\n\r\n")
	got := runFramingExchanges(t, []framingExchange{
		{head, []byte("HTTP/1.1 200 OK\r\nContent-Length: 1024\r\n\r\n")},
		{framingGetReq("/after"), framingOKBody("AFTER")},
	})
	require.Equal(t, []string{"/asset", "/after"}, framingURLsOf(got))
	require.Equal(t, "AFTER", got[1].Spec.HTTPResp.Body)
}
func TestRecordV2_ChunkedPayloadContainingTerminatorIsNotTruncated(t *testing.T) {
	chunked := []byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n" +
		"1\r\n0\r\n" + // a chunk whose single byte is "0"
		"3\r\nabc\r\n" +
		"0\r\n\r\n")
	got := runFramingExchanges(t, []framingExchange{
		{framingGetReq("/stream"), chunked},
		{framingGetReq("/after"), framingOKBody("AFTER")},
	})
	require.Equal(t, []string{"/stream", "/after"}, framingURLsOf(got),
		"a chunked payload containing the last-chunk marker must not end the body early")
	require.Equal(t, "AFTER", got[1].Spec.HTTPResp.Body)
}

// Control: an ordinary keep-alive conversation with no bodyless response is
// unaffected. Proves the harness detects success, not just failure.
func TestRecordV2_OrdinaryKeepAliveUnaffected(t *testing.T) {
	got := runFramingExchanges(t, []framingExchange{
		{framingGetReq("/one"), framingOKBody("ONE")},
		{framingGetReq("/two"), framingOKBody("TWO")},
	})
	require.Equal(t, []string{"/one", "/two"}, framingURLsOf(got))
	require.Equal(t, "ONE", got[0].Spec.HTTPResp.Body)
	require.Equal(t, "TWO", got[1].Spec.HTTPResp.Body)
}

// chunkedRespParts builds a chunked response and returns it as the caller's
// chosen split across tee'd chunks, so a boundary can be placed anywhere.
func chunkedBody(data []string, lastChunkExt string, trailers ...string) string {
	var b strings.Builder
	for _, d := range data {
		fmt.Fprintf(&b, "%x\r\n%s\r\n", len(d), d)
	}
	b.WriteString("0" + lastChunkExt + "\r\n")
	for _, tr := range trailers {
		b.WriteString(tr + "\r\n")
	}
	b.WriteString("\r\n")
	return b.String()
}

func chunkedResp(data []string, lastChunkExt string, trailers ...string) []byte {
	extra := ""
	if len(trailers) > 0 {
		extra = "Trailer: " + strings.SplitN(trailers[0], ":", 2)[0] + "\r\n"
	}
	return []byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n" + extra + "\r\n" +
		chunkedBody(data, lastChunkExt, trailers...))
}

// splitExchange is framingExchange with the response delivered as several
// tee'd chunks, so a boundary can be placed mid-frame.
type splitExchange struct {
	req   []byte
	parts [][]byte
}

func runSplitExchanges(t *testing.T, script []splitExchange) []*models.Mock {
	t.Helper()
	h := &HTTP{Logger: zaptest.NewLogger(t)}
	sess, sendReq, closeReq, sendResp, closeResp, mocks := newTestSession(t)
	base := time.Unix(1_700_000_000, 0)

	sent := make(chan struct{})
	go func() {
		defer close(sent)
		for i, ex := range script {
			ts := base.Add(time.Duration(i) * time.Second)
			sendReq(ex.req, ts, ts)
			time.Sleep(10 * time.Millisecond)
			for _, p := range ex.parts {
				sendResp(p, ts, ts)
				time.Sleep(10 * time.Millisecond)
			}
		}
		closeResp()
		closeReq()
	}()

	done := make(chan error, 1)
	go func() { done <- h.recordV2(sess.Ctx, sess) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("parser never returned — it is blocked framing a chunked body that has already ended")
	}
	<-sent

	var got []*models.Mock
	for {
		select {
		case m := <-mocks:
			got = append(got, m)
		default:
			return got
		}
	}
}

// A trailer section after the last chunk is part of the framing, not the start
// of the next exchange. Scanning for "0\r\n\r\n" only terminates when the last
// trailer value happens to end in '0'.
func TestRecordV2_ChunkedTrailerDoesNotSwallowKeepAlive(t *testing.T) {
	for _, status := range []string{"5", "0"} {
		t.Run("grpc-status-"+status, func(t *testing.T) {
			got := runSplitExchanges(t, []splitExchange{
				{framingGetReq("/grpc"), [][]byte{chunkedResp([]string{"hello"}, "", "Grpc-Status: "+status)}},
				{framingGetReq("/after"), [][]byte{framingOKBody("AFTER")}},
			})
			require.Equal(t, []string{"/grpc", "/after"}, framingURLsOf(got))
			require.Equal(t, "hello", got[0].Spec.HTTPResp.Body)
			require.Equal(t, "AFTER", got[1].Spec.HTTPResp.Body)
		})
	}
}

// RFC 9112 allows a chunk extension on the last chunk. "0;pad=x\r\n\r\n" is a
// legal end that no literal-marker search can find.
func TestRecordV2_ChunkedLastChunkExtensionTerminates(t *testing.T) {
	got := runSplitExchanges(t, []splitExchange{
		{framingGetReq("/ext"), [][]byte{chunkedResp([]string{"hello"}, ";pad=x")}},
		{framingGetReq("/after"), [][]byte{framingOKBody("AFTER")}},
	})
	require.Equal(t, []string{"/ext", "/after"}, framingURLsOf(got))
	require.Equal(t, "AFTER", got[1].Spec.HTTPResp.Body)
}

// The inverse failure: a data chunk whose payload ends in "0\r\n" is followed
// by the framing CRLF, so the wire really does read "...0\r\n\r\n" in mid-body.
// With the boundary placed right there, a suffix check ends the body early.
func TestRecordV2_ChunkedBoundaryAfterZeroCRLFIsNotTheEnd(t *testing.T) {
	full := chunkedBody([]string{"X0\r\n", "abc"}, "")
	split := strings.Index(full, "3\r\nabc") // boundary right after the first chunk
	got := runSplitExchanges(t, []splitExchange{
		{framingGetReq("/split"), [][]byte{
			[]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n" + full[:split]),
			[]byte(full[split:]),
		}},
		{framingGetReq("/after"), [][]byte{framingOKBody("AFTER")}},
	})
	require.Equal(t, []string{"/split", "/after"}, framingURLsOf(got))
	require.Equal(t, "X0\r\nabc", got[0].Spec.HTTPResp.Body)
}

// A tee'd chunk boundary can fall anywhere, including inside the chunk-size
// line. Every split of one response must frame identically.
func TestRecordV2_ChunkedEverySplitFramesIdentically(t *testing.T) {
	resp := string(chunkedResp([]string{"hello", "world"}, "", "Grpc-Status: 5"))
	for i := 1; i < len(resp); i++ {
		got := runSplitExchanges(t, []splitExchange{
			{framingGetReq("/s"), [][]byte{[]byte(resp[:i]), []byte(resp[i:])}},
			{framingGetReq("/after"), [][]byte{framingOKBody("AFTER")}},
		})
		require.Equal(t, []string{"/s", "/after"}, framingURLsOf(got), "split at byte %d", i)
		require.Equal(t, "helloworld", got[0].Spec.HTTPResp.Body, "split at byte %d", i)
	}
}

// Malformed framing must fail fast. The streams are deliberately left OPEN, so
// a parser that reads on instead of erroring never returns and the test times
// out rather than passing by accident.
func TestRecordV2_MalformedChunkedFailsFastWithoutClosing(t *testing.T) {
	cases := map[string]string{
		"non-hex chunk size":  "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\nzz\r\nhello\r\n",
		"data not CRLF-ended": "HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhelloXX",
	}
	for name, wire := range cases {
		t.Run(name, func(t *testing.T) {
			h := &HTTP{Logger: zaptest.NewLogger(t)}
			sess, sendReq, _, sendResp, _, _ := newTestSession(t)
			ts := time.Unix(1_700_000_000, 0)
			sendReq(framingGetReq("/bad"), ts, ts)
			sendResp([]byte(wire), ts, ts)

			done := make(chan error, 1)
			go func() { done <- h.recordV2(sess.Ctx, sess) }()
			select {
			case err := <-done:
				require.Error(t, err, "malformed framing must be reported, not accepted")
				require.True(t, sess.IsMockIncomplete(), "a malformed frame must mark the mock incomplete")
			case <-time.After(2 * time.Second):
				t.Fatal("parser did not return on malformed chunked framing — it is reading a body that will never end")
			}
		})
	}
}

// A chunked body cut short by the connection closing must end the parser, not
// hang it. The exchange is unrecordable; what matters is that recordV2 returns.
func TestRecordV2_ChunkedTruncatedByEOFReturns(t *testing.T) {
	got := runSplitExchanges(t, []splitExchange{
		{framingGetReq("/cut"), [][]byte{[]byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\n\r\n5\r\nhel")}},
	})
	require.Empty(t, framingURLsOf(got))
}

// The request side has the same framing and the same defect. An AWS S3
// streaming upload sends its checksum as a trailer after the last chunk.
func TestRecordV2_ChunkedRequestTrailerDoesNotSwallowKeepAlive(t *testing.T) {
	req := []byte("PUT /obj HTTP/1.1\r\nHost: x\r\nTransfer-Encoding: chunked\r\n" +
		"x-amz-trailer: x-amz-checksum-crc32\r\n\r\n" +
		chunkedBody([]string{"hello"}, "", "x-amz-checksum-crc32: AAAAAA=="))
	got := runSplitExchanges(t, []splitExchange{
		{req, [][]byte{framingOKBody("OK1")}},
		{framingGetReq("/after"), [][]byte{framingOKBody("AFTER")}},
	})
	require.Equal(t, []string{"/obj", "/after"}, framingURLsOf(got))
	require.Equal(t, "hello", got[0].Spec.HTTPReq.Body)
}
