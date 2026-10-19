package http

import (
	"bufio"
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	nethttp "net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/async"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils/logcontent/logtest"
	"go.uber.org/zap"
	"golang.org/x/sync/errgroup"
)

// canary is the application's: in a call's query, its Authorization header,
// its body, and the answer it was recorded with.
const canary = "kp-canary-http-7c1d"

// canaryCall is a call that carries the canary in its query, its
// Authorization header and its body.
func canaryCall(method string) string {
	body := `{"note":"` + canary + `-body"}`
	return fmt.Sprintf("%s /profile?ref=%s-q HTTP/1.1\r\nHost: api.test\r\nContent-Type: application/json\r\nAuthorization: Bearer %s-auth\r\nContent-Length: %d\r\n\r\n%s",
		method, canary, canary, len(body), body)
}

// canaryMock is canaryCall's recording, at path?ref=ref with the extra
// headers, answered with the canary.
func canaryMock(name, path, ref string, extra map[string]string) *models.Mock {
	hdr := map[string]string{"Authorization": "Bearer " + canary + "-auth"}
	for k, v := range extra {
		hdr[k] = v
	}
	return learnRecorded(name, 0, "GET", "http://api.test"+path+"?ref="+ref, `{"note":"`+canary+`-body"}`, hdr, `{"user":"`+canary+`-answer"}`)
}

// decodeLogged runs decodeHTTP, as replay does, with its lines kept.
func decodeLogged(t *testing.T, logger *zap.Logger, first string, db *mockMemDb) (net.Conn, <-chan error) {
	t.Helper()
	app, proxy := tcpPair(t)
	h := &HTTP{Logger: logger}
	return app, runDecode(t, proxy, func(ctx context.Context) error {
		return h.decodeHTTP(ctx, []byte(first), proxy, nil, db, models.OutgoingOptions{})
	})
}

// A call replay serves is logged as its method, path, header names and
// lengths and fingerprints of the rest: not its query values, header values
// or body, nor the recordings it was compared with or the answer it was
// served, unless KEPLOY_LOG_CONTENT is set.
func TestDecodeHTTP_AServedCallsLinesCarryNoContent(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		db := &mockMemDb{mocks: []*models.Mock{
			canaryMock("mock-served", "/profile", canary+"-q", nil),
			canaryMock("mock-another-path", "/other", canary+"-q", nil),
			canaryMock("mock-another-header", "/profile", canary+"-q", map[string]string{"X-Required": canary + "-header"}),
		}, updateUnFilteredReturn: true, deleteFilteredReturn: true}
		app, done := decodeLogged(t, logger, canaryCall("GET"), db)
		resp, err := nethttp.ReadResponse(bufio.NewReader(app), nil)
		if err != nil || resp.StatusCode != 200 {
			t.Fatalf("the call was not served (%v)", err)
		}
		_ = app.Close()
		<-done
		logtest.Check(t, logs, canary, on,
			"This is the complete request",
			"Decoded HTTP request headers",
			"decodeHTTP debug logs for input",
			"The url path of mock and request aren't the same",
			"headers missing required keys for mock name",
			"mocks under consideration for exact body match",
			"after matching the http request",
			"Mock Response sending back to client",
			"This was the last response from the server")
	})
}

// A call whose query's value was recorded as another of its type is served
// that recording, and the line that says so carries neither value, unless
// KEPLOY_LOG_CONTENT is set.
func TestDecodeHTTP_ADynamicQueryValueIsLoggedWithoutIt(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		db := &mockMemDb{mocks: []*models.Mock{canaryMock("mock-another-ref", "/profile", canary+"-other", nil)},
			updateUnFilteredReturn: true, deleteFilteredReturn: true}
		app, done := decodeLogged(t, logger, canaryCall("GET"), db)
		if resp, err := nethttp.ReadResponse(bufio.NewReader(app), nil); err != nil || resp.StatusCode != 200 {
			t.Fatalf("the call was not served (%v)", err)
		}
		_ = app.Close()
		<-done
		logtest.Check(t, logs, canary, on, "http query: value treated as auto-detected dynamic")
	})
}

// A call no recording answers is logged with how it differs from the
// closest, without the values that differ, unless KEPLOY_LOG_CONTENT is set.
func TestDecodeHTTP_AMissedCallsDiffCarriesNoValue(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		closest := canaryMock("mock-closest", "/profile", canary+"-recorded", nil)
		closest.Spec.HTTPReq.Method = "POST"
		app, done := decodeLogged(t, logger, canaryCall("PUT"), &mockMemDb{mocks: []*models.Mock{closest}})
		_ = readUntil(app, noMockAnswer, 2*time.Second)
		wantMockMiss(t, done)
		logtest.Check(t, logs, canary, on, "no matching http mock found for outgoing request")
	})
}

// A request's metadata names its method, host and path, not its query's
// values, unless KEPLOY_LOG_CONTENT is set.
func TestDecodeHTTP_ARequestsMetadataCarriesNoQuery(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		app, done := decodeLogged(t, logger, canaryCall("GET"), &mockMemDb{err: errors.New("the mock store failed")})
		defer app.Close()
		select {
		case <-done:
		case <-time.After(2 * time.Second):
			t.Fatal("decodeHTTP did not return")
		}
		logtest.Check(t, logs, canary, on, "error while matching http mocks")
	})
}

// encodeLogged is encodeHarness with the record path's lines kept.
func encodeLogged(t *testing.T, logger *zap.Logger, first string, upstream func(net.Conn)) (app net.Conn, done <-chan error) {
	t.Helper()
	out := make(chan *models.Mock, 4)
	app, proxyClient := tcpPair(t)
	up, proxyDest := tcpPair(t)
	go upstream(up)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	g, gctx := errgroup.WithContext(ctx)
	gctx = context.WithValue(gctx, models.ErrGroupKey, g)
	gctx = context.WithValue(gctx, models.ClientConnectionIDKey, "test-conn")
	h := &HTTP{Logger: logger}
	d := make(chan error, 1)
	returned := make(chan struct{})
	go func() {
		defer close(returned)
		d <- h.encodeHTTP(gctx, []byte(first), proxyClient, proxyDest, out, models.OutgoingOptions{}, nil)
	}()
	t.Cleanup(func() {
		_ = proxyClient.Close()
		_ = proxyDest.Close()
		<-returned
		_ = g.Wait()
	})
	return app, d
}

// The record path logs what it relays as lengths and fingerprints: not the
// request, nor the answer as it comes in parts, unless KEPLOY_LOG_CONTENT is
// set.
func TestEncodeHTTP_ARecordedCallsLinesCarryNoContent(t *testing.T) {
	answer := `{"user":"` + canary + `-answer-1","more":"` + canary + `-answer-2"}`
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		app, done := encodeLogged(t, logger, canaryCall("GET"), func(up net.Conn) {
			_ = readUntil(up, canary+`-body"}`, 2*time.Second)
			// The body in two writes: the record path reads the rest of it
			// by its length.
			_, _ = fmt.Fprintf(up, "HTTP/1.1 200 OK\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(answer), answer[:30])
			time.Sleep(100 * time.Millisecond)
			_, _ = up.Write([]byte(answer[30:]))
		})
		if got := readUntil(app, answer, 2*time.Second); !strings.HasSuffix(got, answer) {
			t.Fatalf("the app got %q", got)
		}
		// A reset, not a close: a read that fails with more than the end of
		// the stream.
		_ = app.(*net.TCPConn).SetLinger(0)
		_ = app.Close()
		<-done
		logtest.Check(t, logs, canary, on,
			"This is the initial request",
			"This is the complete request",
			"This is the initial response",
			"This is a chunk of response[content-length]",
			"This is the final response",
			"This was the last response from the server")
	})
}

// A dependency that answers a request's Expect: 100-continue with a final
// response is logged without that response, unless KEPLOY_LOG_CONTENT is
// set.
func TestEncodeHTTP_AnAnswerToExpectIsLoggedWithoutItsBody(t *testing.T) {
	refusal := fmt.Sprintf("HTTP/1.1 417 Expectation Failed\r\nContent-Length: %d\r\n\r\n%s", len(canary), canary)
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		head := "POST /upload HTTP/1.1\r\nHost: up\r\nExpect: 100-continue\r\nContent-Length: 3\r\n\r\n"
		app, done := encodeLogged(t, logger, head, func(up net.Conn) {
			_ = readUntil(up, "\r\n\r\n", 2*time.Second)
			_, _ = up.Write([]byte(refusal))
		})
		_ = readUntil(app, canary, 2*time.Second)
		_ = app.Close()
		<-done
		logtest.Check(t, logs, canary, on, "This is the response from the server after the expect header")
	})
}

// The legacy record path's parse of an exchange logs the answer's body as its
// length and fingerprint, unless KEPLOY_LOG_CONTENT is set.
func TestParseFinalHTTP_TheResponseBodyLineCarriesNoBody(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		resp := fmt.Sprintf("HTTP/1.1 200 OK\r\nContent-Length: %d\r\n\r\n%s", len(canary), canary)
		runLegacyParseFinalHTTP(t, &HTTP{Logger: logger}, []byte(canaryCall("GET")), []byte(resp), 80, "test-conn")
		logtest.Check(t, logs, canary, on, "This is the response body")
	})
}

// A recorded body the body-key matcher cannot read is logged without it, as
// is a live one, unless KEPLOY_LOG_CONTENT is set.
func TestMatch_AnUnreadableBodyIsLoggedWithoutIt(t *testing.T) {
	// JSON, but not an object.
	for name, tc := range map[string]struct{ recorded, live, want string }{
		"recorded": {`["` + canary + `"]`, `{"note":"x"}`, "failed to unmarshal the mock request body"},
		"live":     {`{"note":"x"}`, `["` + canary + `"]`, "failed to unmarshal the request body"},
	} {
		t.Run(name, func(t *testing.T) {
			logtest.Each(t, func(t *testing.T, on bool) {
				logger, logs := logtest.Observe()
				h := &HTTP{Logger: logger}
				m := learnRecorded("mock-1", 0, "POST", "http://api.test/notes", tc.recorded, nil, `{}`)
				_, _, _, _ = h.match(context.Background(), live("POST", "http://api.test/notes", tc.live, nil), &mockMemDb{mocks: []*models.Mock{m}},
					nil, nil, nil, true, false, false, true, true)
				logtest.Check(t, logs, canary, on, tc.want)
			})
		})
	}
}

// A variable bound under the proven switch is logged by name and place, not
// the value it took nor the one it was recorded with, unless
// KEPLOY_LOG_CONTENT is set.
func TestVariables_TheBoundLineCarriesNoValue(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		t.Setenv(provenswitch.Env, "1")
		logger, logs := logtest.Observe()
		h := &HTTP{Logger: logger}
		db := &varDb{perTest: createAndReadBack(canary + "-rec-1")}
		if ok, _, _ := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"`+canary+`-live-9","item":"pen"}`)); !ok {
			t.Fatal("the create was not answered")
		}
		logtest.Check(t, logs, canary, on, "variable bound")
	})
}

// A call replay serves costs a Debug level that is off nothing for the
// switch: the lines decodeHTTP logs for every call check their level before
// they build a logcontent field.
func TestDecodeHTTP_TheSwitchCostsADisabledLevelNothing(t *testing.T) {
	h := &HTTP{Logger: logtest.Disabled()}
	for name, mocks := range map[string]func() []*models.Mock{
		"one recording, which answers it": func() []*models.Mock {
			return []*models.Mock{canaryMock("mock-served", "/profile", canary+"-q", nil)}
		},
		// The schema match's lines for a recording of another path and one
		// that requires another header.
		"recordings of another path and another header too": func() []*models.Mock {
			return []*models.Mock{canaryMock("mock-served", "/profile", canary+"-q", nil),
				canaryMock("mock-another-path", "/other", canary+"-q", nil),
				canaryMock("mock-another-header", "/profile", canary+"-q", map[string]string{"X-Required": canary + "-header"})}
		},
		// The auto-dynamic query line.
		"a recording of another query value": func() []*models.Mock {
			return []*models.Mock{canaryMock("mock-another-ref", "/profile", canary+"-other", nil)}
		},
	} {
		t.Run(name, func(t *testing.T) {
			logtest.CheckDisabledCost(t, func() {
				db := &mockMemDb{mocks: mocks(), updateUnFilteredReturn: true, deleteFilteredReturn: true}
				app, proxy := tcpPair(t)
				done := runDecode(t, proxy, func(ctx context.Context) error {
					return h.decodeHTTP(ctx, []byte(canaryCall("GET")), proxy, nil, db, models.OutgoingOptions{})
				})
				if resp, err := nethttp.ReadResponse(bufio.NewReader(app), nil); err != nil || resp.StatusCode != 200 {
					t.Fatalf("the call was not served (%v)", err)
				}
				// Its own end, not the app's: an app's close is read as a short
				// wait for more (proxy/util.ReadBuffConn), on every run.
				_ = proxy.Close()
				<-done
			})
		})
	}
}

// A variable a call binds costs a Debug level that is off nothing for the
// switch, as decodeHTTP's lines do.
func TestVariables_TheSwitchCostsADisabledLevelNothing(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := &HTTP{Logger: logtest.Disabled()}
	logtest.CheckDisabledCost(t, func() {
		db := &varDb{perTest: createAndReadBack("rec-1")}
		if ok, _, _ := matchVar(t, h, db, liveReq("POST", "/orders", `{"id":"live-9","item":"pen"}`)); !ok {
			t.Fatal("the create was not answered")
		}
	})
}

// A miss served from the real dependency is logged by its method and path,
// not its query's values, unless KEPLOY_LOG_CONTENT is set.
func TestServeOnMiss_TheServedLineCarriesNoQuery(t *testing.T) {
	dep := httptest.NewServer(nethttp.HandlerFunc(func(w nethttp.ResponseWriter, _ *nethttp.Request) {
		_, _ = w.Write([]byte("ok"))
	}))
	t.Cleanup(dep.Close)
	logtest.Each(t, func(t *testing.T, on bool) {
		ResetCaptured()
		t.Cleanup(ResetCaptured)
		reqBuf := []byte("GET /lookup?token=" + canary + " HTTP/1.1\r\nHost: dep.local\r\n\r\n")
		request, err := nethttp.ReadRequest(bufio.NewReader(bytes.NewReader(reqBuf)))
		if err != nil {
			t.Fatal(err)
		}
		keployEnd, appEnd := net.Pipe()
		t.Cleanup(func() { _ = keployEnd.Close(); _ = appEnd.Close() })
		logger, logs := logtest.Observe()
		h := &HTTP{Logger: logger}
		result := make(chan error, 1)
		go func() {
			_, err := h.serveOnMiss(context.Background(), keployEnd, reqBuf, request, nil,
				&models.ConditionalDstCfg{Addr: strings.TrimPrefix(dep.URL, "http://")},
				models.OutgoingOptions{OnMiss: models.MissRecord})
			result <- err
		}()
		if _, err := nethttp.ReadResponse(bufio.NewReader(appEnd), nil); err != nil {
			t.Fatalf("the app got no answer: %v", err)
		}
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		logtest.Check(t, logs, canary, on, "on-miss: served an unrecorded call from the real dependency")
	})
}

// A dependency that fails a recorded call, before it answers or partway
// through its answer's first read, is logged with the call's URL without its
// query's values, unless KEPLOY_LOG_CONTENT is set.
func TestEncodeHTTP_AFailedUpstreamIsLoggedWithoutTheQuery(t *testing.T) {
	// A first read's worth of an answer (pUtil.ReadBytes reads on while each
	// read fills its 1 KiB), so the reset fails the read that holds it.
	head := "HTTP/1.1 200 OK\r\nContent-Length: 4096\r\n\r\n"
	partial := head + strings.Repeat("x", 1024-len(head))
	for want, partial := range map[string]string{
		"upstream call errored before any response bytes; synthesized mock persisted so replay stays deterministic":                     "",
		"upstream errored AFTER partial response bytes were received; discarding truncated bytes and persisting synthesized error mock": partial,
	} {
		t.Run(want, func(t *testing.T) {
			logtest.Each(t, func(t *testing.T, on bool) {
				logger, logs := logtest.Observe()
				app, done := encodeLogged(t, logger, canaryCall("GET"), func(up net.Conn) {
					_ = readUntil(up, canary+`-body"}`, 2*time.Second)
					if partial == "" {
						// A reset: the dependency fails the call before it
						// answers any of it.
						_ = up.(*net.TCPConn).SetLinger(0)
						_ = up.Close()
						return
					}
					_, _ = up.Write([]byte(partial))
					time.Sleep(100 * time.Millisecond)
					_ = up.(*net.TCPConn).SetLinger(0)
					_ = up.Close()
				})
				<-done
				_ = app.Close()
				logtest.Check(t, logs, canary, on, want)
				e := logs.FilterMessage(want).All()
				if got := e[0].ContextMap()["upstream"]; got != "http://api.test/profile" {
					t.Fatalf("the line names the call %v, want its URL without the query", got)
				}
			})
		})
	}
}

// A poll of an async lane that drifts from its recording is still served it,
// and the engine's verdict names the drift by both requests' shapes: not the
// recorded URL's query values, which are the application's, unless
// KEPLOY_LOG_CONTENT is set.
func TestDecodeHTTP_AnAsyncDriftIsLoggedByItsShape(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		h := &HTTP{Logger: logger}
		lane := models.AsyncLane{Name: "notifications", Type: "http", Match: map[string]string{"path": "/v1/poll*"}, VolatileParams: []string{"cursor"}}
		engine := async.NewEngine(logger, []models.AsyncLane{lane}, map[string]async.AsyncParser{"http": h})
		h.SetAsyncEngine(engine)
		recorded := httpMock("rec", "GET", "http://notify.internal.svc/v1/poll?cursor=1&ref="+canary+"-rec")
		recorded.Spec.Async = &models.AsyncMeta{Lane: lane.Name, Seq: 1}
		recorded.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Body: `{"v":1}`}
		engine.Load([]*models.Mock{recorded})
		app, proxy := tcpPair(t)
		done := runDecode(t, proxy, func(ctx context.Context) error {
			return h.decodeHTTP(ctx, []byte("GET /v1/poll?cursor=2&ref="+canary+"-live HTTP/1.1\r\nHost: notify.internal.svc\r\n\r\n"), proxy, nil, &mockMemDb{}, models.OutgoingOptions{})
		})
		if resp, err := nethttp.ReadResponse(bufio.NewReader(app), nil); err != nil || resp.StatusCode != 200 {
			t.Fatalf("the poll was not served its recording (%v)", err)
		}
		_ = app.Close()
		<-done
		engine.LogReport(logger)
		const drift = "async egress shape drift (served recorded response anyway); re-record if the request legitimately changed, or widen the lane's volatileParams / match to treat the varying part as noise"
		logtest.Check(t, logs, canary, on, drift)
		if got, want := logs.FilterMessage(drift).All()[0].ContextMap()["detail"], "notifications: request shape drift: GET /v1/poll vs http://notify.internal.svc/v1/poll?cursor={}&ref={}"; !on && got != want {
			t.Fatalf("the drift is named %q, want %q", got, want)
		}
	})
}

// A live poll whose URL does not parse is named in the engine's verdict by
// url.Parse's error, which quotes the URL: by its shape, unless
// KEPLOY_LOG_CONTENT is set.
func TestMatchRequestShape_AnUnparseablePollIsLoggedByItsShape(t *testing.T) {
	logtest.Each(t, func(t *testing.T, on bool) {
		logger, logs := logtest.Observe()
		h := &HTTP{Logger: logger}
		lane := models.AsyncLane{Name: "notifications", Type: "http", Match: map[string]string{"path": "/v1/poll*"}}
		engine := async.NewEngine(logger, []models.AsyncLane{lane}, map[string]async.AsyncParser{"http": h})
		recorded := httpMock("rec", "GET", "http://notify.internal.svc/v1/poll")
		recorded.Spec.Async = &models.AsyncMeta{Lane: lane.Name, Seq: 1}
		recorded.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Body: `{"v":1}`}
		engine.Load([]*models.Mock{recorded})
		live := httpMock("live", "GET", "http://notify.internal.svc/v1/poll%zz?ref="+canary)
		if served, _, err := engine.Decide(context.Background(), lane, live); err != nil || served != recorded {
			t.Fatalf("the poll was not served its recording (%v)", err)
		}
		engine.LogReport(logger)
		const drift = "async egress shape drift (served recorded response anyway); re-record if the request legitimately changed, or widen the lane's volatileParams / match to treat the varying part as noise"
		logtest.Check(t, logs, canary, on, drift)
	})
}
