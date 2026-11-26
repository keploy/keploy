package proxy

import (
	"bufio"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	hooksUtils "go.keploy.io/server/v3/pkg/agent/hooks/conn"
	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// overSizeRun is what one exchange through the ingress left on its manager.
type overSizeRun struct {
	mgr      *syncMock.SyncMockManager
	tally    *syncMock.OverSizeTally
	captured int64
	told     []models.NotRecordedCall
	ctx      context.Context
}

// runOverSizeExchange sends one POST /big with a request body of reqBytes
// through the ingress (the synchronous loop when synchronous, else the
// zero-copy one), to an app that reads it whole and answers respBytes,
// halfway through which pauseMidAnswer, when set, starts a pause of the
// memory guard. It returns once the handler and every capture it started are
// done.
func runOverSizeExchange(t *testing.T, synchronous bool, reqBytes, respBytes int, pauseMidAnswer bool) overSizeRun {
	t.Helper()
	var paused atomic.Bool
	stubIngressPaused(t, paused.Load)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Length", strconv.Itoa(respBytes))
		half := respBytes / 2
		_, _ = w.Write(bytes.Repeat([]byte("y"), half))
		if pauseMidAnswer {
			// At most this half has reached the ingress when the pause
			// starts, under the budget: whatever the ingress has read of
			// it by then, the pause, not the budget, cuts the capture.
			w.(http.Flusher).Flush()
			paused.Store(true)
		}
		_, _ = w.Write(bytes.Repeat([]byte("y"), respBytes-half))
	}))
	t.Cleanup(upstream.Close)

	var captured atomic.Int64
	stubCaptureHook(t, func(ctx context.Context, logger *zap.Logger, tc chan *models.TestCase,
		req *http.Request, resp *http.Response, reqTS, respTS time.Time,
		opts models.IncomingOptions, sync bool, mapping bool, appPort uint16) {
		captured.Add(1)
	})
	mgr := syncMock.New(zap.NewNop())
	feed, closeFeed := mgr.OpenNotRecordedFeed()
	defer closeFeed()
	tally := mgr.OpenOverSizeTally()
	t.Cleanup(tally.Close)
	pm := &IngressProxyManager{logger: zap.NewNop(), tcChan: make(chan *models.TestCase, 4), synchronous: synchronous, samplingSem: make(chan struct{}, 1)}
	ctx, cancel := context.WithTimeout(syncMock.NewContext(context.Background(), mgr), 20*time.Second)
	defer cancel()
	client, done := ingressPipe(t, ctx, pm, strings.TrimPrefix(upstream.URL, "http://"))

	head := fmt.Sprintf("POST /big HTTP/1.1\r\nHost: app.local\r\nContent-Type: text/plain\r\nContent-Length: %d\r\nConnection: close\r\n\r\n", reqBytes)
	go func() {
		_, _ = client.Write([]byte(head))
		_, _ = client.Write(bytes.Repeat([]byte("x"), reqBytes))
	}()
	resp, err := http.ReadResponse(bufio.NewReader(client), nil)
	if err != nil {
		t.Fatal(err)
	}
	n, _ := io.Copy(io.Discard, resp.Body)
	_ = resp.Body.Close()
	if n != int64(respBytes) {
		t.Fatalf("the client got %d bytes of the answer, want %d", n, respBytes)
	}
	select {
	case <-done:
	case <-ctx.Done():
		t.Fatal("the handler did not end with its connection")
	}
	pm.captures.Wait()
	var told []models.NotRecordedCall
	for {
		select {
		case c := <-feed:
			told = append(told, c)
			continue
		default:
		}
		break
	}
	return overSizeRun{mgr: mgr, tally: tally, captured: captured.Load(), told: told, ctx: ctx}
}

// assertOverSize fails unless run counted its one exchange over limit, told
// the recording's client of it, recorded nothing, and counted it as nothing a
// pause cost.
func assertOverSize(t *testing.T, run overSizeRun, limit hooksUtils.SizeLimit) {
	t.Helper()
	if run.captured != 0 {
		t.Fatalf("an exchange over the size limit reached the capture %d times", run.captured)
	}
	tag := models.URLPathTag(run.ctx, http.MethodPost, &url.URL{Path: "/big"})
	call := models.WithPathTag("POST /{}", tag)
	if s := run.tally.Read(); s.Total != 1 || s.ByCall[call] != 1 {
		t.Fatalf("the manager counted %+v over the size limit, want the one exchange on %q", s, call)
	}
	if p := run.mgr.NotRecordedUnderPressure().Total; p != 0 {
		t.Fatalf("an exchange over the size limit was counted as one a pause cost too (%d)", p)
	}
	want := models.NotRecordedCall{Method: http.MethodPost, Path: "/big", PathTag: tag, Why: limit.Why()}
	if len(run.told) != 1 || run.told[0] != want {
		t.Fatalf("the recording's client was told %+v, want %+v", run.told, want)
	}
}

// The HTTP/1 ingress holds at most maxHTTPBodyCaptureBytes of either body to
// record an exchange. One whose request or response body is larger is
// forwarded whole and not recorded, and the recording used to count it
// nowhere: a recording of 7,388 requests at 4 MiB payloads lost 151 without a
// word. Each is now counted on the recording's manager by its call, for the
// summary, and told to the recording's client by its path, in both of the
// ingress's loops.
func TestTheIngressCountsAnExchangeWithABodyOverTheBudget(t *testing.T) {
	over := maxHTTPBodyCaptureBytes + 64<<10
	for _, synchronous := range []bool{false, true} {
		for _, side := range []string{"request", "response"} {
			t.Run(fmt.Sprintf("synchronous=%v/%s", synchronous, side), func(t *testing.T) {
				reqBytes, respBytes := 16, 16
				if side == "request" {
					reqBytes = over
				} else {
					respBytes = over
				}
				assertOverSize(t, runOverSizeExchange(t, synchronous, reqBytes, respBytes, false), hooksUtils.BodyLimit)
			})
		}
	}
}

// An exchange whose bodies each fit the budget, and whose heads and bodies
// together are over maxHTTPCombinedCaptureBytes, is left out by the capture
// the ingress starts for it, and counted and told there as over the test
// case's limit, in both loops.
func TestTheIngressCountsAnExchangeOverTheCombinedBudget(t *testing.T) {
	for _, synchronous := range []bool{false, true} {
		t.Run(fmt.Sprintf("synchronous=%v", synchronous), func(t *testing.T) {
			run := runOverSizeExchange(t, synchronous, maxHTTPBodyCaptureBytes, maxHTTPBodyCaptureBytes, false)
			assertOverSize(t, run, hooksUtils.TestCaseLimit)
		})
	}
}

// An exchange a pause of the memory guard cut the capture of is the pause's
// loss, counted as that, and not also as one over the size limit, though its
// body is larger than the budget: the recording's summary counts each lost
// request once. One under the budget is captured, and counted nowhere.
func TestTheIngressCountsAnExchangeAPauseCutAsThePausesAlone(t *testing.T) {
	for _, synchronous := range []bool{false, true} {
		t.Run(fmt.Sprintf("synchronous=%v", synchronous), func(t *testing.T) {
			run := runOverSizeExchange(t, synchronous, 16, maxHTTPBodyCaptureBytes+64<<10, true)
			if s := run.tally.Read(); s.Total != 0 {
				t.Fatalf("an exchange a pause cut was counted over the size limit: %+v", s)
			}
			if p := run.mgr.NotRecordedUnderPressure().Total; p != 1 {
				t.Fatalf("an exchange a pause cut was counted as a pause's loss %d times, want once", p)
			}
			if run.captured != 0 || len(run.told) != 0 {
				t.Fatalf("an exchange a pause cut reached the capture %d times, and was told %v", run.captured, run.told)
			}
		})
		t.Run(fmt.Sprintf("synchronous=%v/under the budget", synchronous), func(t *testing.T) {
			run := runOverSizeExchange(t, synchronous, 16, 64<<10, false)
			if run.captured != 1 || run.tally.Read().Total != 0 || len(run.told) != 0 {
				t.Fatalf("an exchange under the budget: captured %d times, counted %+v over the size limit, told %v; want captured once and counted nowhere",
					run.captured, run.tally.Read(), run.told)
			}
		})
	}
}
