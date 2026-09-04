package http

import (
	"fmt"
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

	go func() {
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

// An interim 100 Continue is not the response. It must be discarded and the
// real response read, not framed as the response itself.
func TestRecordV2_InterimContinueIsSkipped(t *testing.T) {
	post := []byte("POST /submit HTTP/1.1\r\nHost: x\r\nExpect: 100-continue\r\nContent-Length: 2\r\n\r\nhi")
	got := runFramingExchanges(t, []framingExchange{
		{post, []byte("HTTP/1.1 100 Continue\r\n\r\nHTTP/1.1 200 OK\r\nContent-Length: 4\r\n\r\nDONE")},
		{framingGetReq("/after"), framingOKBody("AFTER")},
	})
	require.Equal(t, []string{"/submit", "/after"}, framingURLsOf(got))
	require.Equal(t, 200, got[0].Spec.HTTPResp.StatusCode, "the interim 100 must not be recorded as the response")
	require.Equal(t, "DONE", got[0].Spec.HTTPResp.Body)
}

// A chunked body may be followed by trailer fields, in which case the message
// does not END with the bare last-chunk marker. HasSuffix never matches and the
// parser blocks for the life of the connection.
func TestRecordV2_ChunkedWithTrailersTerminates(t *testing.T) {
	chunked := []byte("HTTP/1.1 200 OK\r\nTransfer-Encoding: chunked\r\nTrailer: X-Sum\r\n\r\n" +
		"3\r\nabc\r\n0\r\nX-Sum: 9\r\n\r\n")
	got := runFramingExchanges(t, []framingExchange{
		{framingGetReq("/stream"), chunked},
		{framingGetReq("/after"), framingOKBody("AFTER")},
	})
	require.Equal(t, []string{"/stream", "/after"}, framingURLsOf(got))
	require.Equal(t, "AFTER", got[1].Spec.HTTPResp.Body)
}

// A 101 upgrade is recorded, and parsing then stops: what follows on the wire is
// WebSocket frames, not HTTP, and looping back to read "the next request" would
// block until the hang watchdog retires the parser 60s later.
func TestRecordV2_UpgradeIsRecordedThenParsingStops(t *testing.T) {
	upgrade := []byte("GET /ws HTTP/1.1\r\nHost: x\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")
	got := runFramingExchanges(t, []framingExchange{
		{upgrade, []byte("HTTP/1.1 101 Switching Protocols\r\nUpgrade: websocket\r\nConnection: Upgrade\r\n\r\n")},
	})
	require.Equal(t, []string{"/ws"}, framingURLsOf(got))
	require.Equal(t, 101, got[0].Spec.HTTPResp.StatusCode)
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
