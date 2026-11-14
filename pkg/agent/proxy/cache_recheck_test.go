package proxy

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// A conditional re-check of an answer keploy already gave the test
// (integrations/http/cache_recheck.go): a browser whose served copy was stale
// on arrival (the answer's recorded Date and max-age) re-checks it with the
// copy's validators, by the run's timing alone. It is decided as the fetch it
// re-checks: answered by the test's recording still owed, or else again with
// the earlier answer, never a call more than recorded and never required.

const (
	hensonPath = "/v3/.deploy_status_henson.json"
	hensonTag  = `"5f1c-9a"`
	hensonMod  = "Fri, 13 Nov 2026 22:04:51 GMT"
	hensonA1   = `{"status":"ok"}`
	hensonA2   = `{"status":"degraded"}`
)

// hensonMock is a recording of an unconditional GET of path, answered 200
// with body, cacheable for 60 s, with tag and mod as its validators ("" for
// none).
func hensonMock(name string, at time.Time, path, body, tag, mod string) *models.Mock {
	mk := readMock(name, at, "GET", path, "", body)
	h := mk.Spec.HTTPResp.Header
	h["Cache-Control"] = "public, max-age=60"
	h["Age"] = "17"
	h["Date"] = at.Format(http.TimeFormat)
	if tag != "" {
		h["ETag"] = tag
	}
	if mod != "" {
		h["Last-Modified"] = mod
	}
	return mk
}

// hensonGet is the app's GET of path with the extra header lines more (each
// "Name: value"), none for an unconditional fetch.
func hensonGet(path string, more ...string) string {
	r := "GET " + path + " HTTP/1.1\r\nHost: localhost:8083\r\n"
	for _, l := range more {
		r += l + "\r\n"
	}
	return r + "\r\n"
}

// recheck is hensonGet of path re-checking a copy with tag and mod ("" leaves
// that validator out).
func recheck(path, tag, mod string, more ...string) string {
	var l []string
	if tag != "" {
		l = append(l, "If-None-Match: "+tag)
	}
	if mod != "" {
		l = append(l, "If-Modified-Since: "+mod)
	}
	return hensonGet(path, append(l, more...)...)
}

// recheckSend sends request on a connection of its own and returns the status
// and body the app got, and the miss reported for it (nil for none).
func recheckSend(t *testing.T, logger *zap.Logger, db *MockManager, opts models.OutgoingOptions, tracker *learn.Tracker, request string) (int, string, *models.MockMismatchReport) {
	t.Helper()
	var reported atomic.Value
	ctx := models.WithMockMismatchReporter(context.Background(), func(err error) { reported.Store(err) })
	if tracker != nil {
		ctx = learn.WithTracker(ctx, tracker)
	}
	app, agentEnd := net.Pipe()
	done := make(chan struct{})
	go func() {
		defer close(done)
		_ = httpint.New(logger).MockOutgoing(ctx, agentEnd, &models.ConditionalDstCfg{}, db, opts)
		_ = agentEnd.Close()
	}()
	t.Cleanup(func() { _ = app.Close(); <-done })
	if _, err := io.WriteString(app, request); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		t.Fatalf("the app got no answer: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	_ = app.Close()
	<-done
	err, _ = reported.Load().(error)
	if err == nil {
		return resp.StatusCode, string(body), nil
	}
	var withReport interface {
		MismatchReport() *models.MockMismatchReport
	}
	if !errors.As(err, &withReport) {
		t.Fatalf("the miss was reported as %v, with no mismatch report", err)
	}
	return resp.StatusCode, string(body), withReport.MismatchReport()
}

// recheckOutcome is what one test of a row got: the body of each answered
// send ("miss" for a send that missed, "304" for an empty 304), the sends
// that missed, and the recordings its end found unconsumed.
type recheckOutcome struct {
	got    []string
	missed []int
	left   []string
}

// recheckRun serves one test, its recording mapping, as a replay (opts zero)
// or record's full round (opts.Learn) does, and returns what it got.
func recheckRun(t *testing.T, logger *zap.Logger, opts models.OutgoingOptions, mapping []*models.Mock, sends []string) recheckOutcome {
	t.Helper()
	mm := NewMockManager(nil, nil, logger)
	t.Cleanup(mm.Close)
	mm.SetMocksWithWindow(nil, mapping, models.BaseTime, time.Now().Add(time.Hour))
	// A recording named x-... is staged but another test's, and one named
	// w-... the worker scope's (the app's own background calls, which t's
	// per-test reads show after its own): t's mapping leaves both out.
	var own []*models.Mock
	var worker []string
	for _, m := range mapping {
		switch {
		case strings.HasPrefix(m.Name, "w-"):
			worker = append(worker, m.Name)
		case !strings.HasPrefix(m.Name, "x-"):
			own = append(own, m)
		}
	}
	if worker != nil {
		mm.ServeExactWorker(worker)
	}
	mm.ServeExactPool("k", nil)
	mm.ServeExactTest("t", own)
	var tracker *learn.Tracker
	if opts.Learn {
		tracker = learn.NewTracker()
	}
	var out recheckOutcome
	for i, s := range sends {
		code, body, miss := recheckSend(t, logger, mm, opts, tracker, s)
		switch {
		case miss != nil:
			out.got = append(out.got, "miss")
			out.missed = append(out.missed, i)
		case code == http.StatusNotModified:
			out.got = append(out.got, "304")
		default:
			out.got = append(out.got, body)
		}
	}
	test, _, left, ok := mm.ExactTestOutcome()
	if !ok || test != "t" {
		t.Fatalf("not serving t exactly at its end: %q %v", test, ok)
	}
	out.left = left
	return out
}

// recheckRounds are the two ways a set's calls are decided under the proven
// switch.
var recheckRounds = []struct {
	name string
	opts models.OutgoingOptions
}{{"replay", models.OutgoingOptions{}}, {"record's full round", models.OutgoingOptions{Learn: true, LearnPool: true}}}

// The henson pattern: two fetches a few milliseconds apart, the second (and
// any later one) a re-check of the copy the first got. Recorded once, the
// re-checks are answered again with that answer; recorded twice (the record's
// second fetch went out unconditionally), the re-check is answered by the
// second recording, which is then not left unmade. A re-check whose recorded
// fetch answered otherwise gets that answer: it is not merged into the
// earlier one. Each validator alone is a re-check too.
func TestACacheRecheckOfAnAnswerTheTestGotIsServedAndNeverCounted(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Date(2026, 11, 13, 22, 5, 0, 0, time.UTC)
	for _, r := range recheckRounds {
		for _, tc := range []struct {
			name    string
			mapping []*models.Mock
			sends   []string
			want    []string
		}{
			{"recorded once, re-checked twice", []*models.Mock{hensonMock("h-1", t0, hensonPath, hensonA1, hensonTag, hensonMod)},
				[]string{hensonGet(hensonPath), recheck(hensonPath, hensonTag, hensonMod), recheck(hensonPath, hensonTag, hensonMod)},
				[]string{hensonA1, hensonA1, hensonA1}},
			{"recorded twice", []*models.Mock{hensonMock("h-1", t0, hensonPath, hensonA1, hensonTag, hensonMod), hensonMock("h-2", t0.Add(3*time.Millisecond), hensonPath, hensonA1, hensonTag, hensonMod)},
				[]string{hensonGet(hensonPath), recheck(hensonPath, hensonTag, hensonMod)},
				[]string{hensonA1, hensonA1}},
			{"recorded twice, answered otherwise the second time", []*models.Mock{hensonMock("h-1", t0, hensonPath, hensonA1, hensonTag, hensonMod), hensonMock("h-2", t0.Add(3*time.Millisecond), hensonPath, hensonA2, `"77-b"`, "")},
				[]string{hensonGet(hensonPath), recheck(hensonPath, hensonTag, hensonMod)},
				[]string{hensonA1, hensonA2}},
			{"If-None-Match alone", []*models.Mock{hensonMock("h-1", t0, hensonPath, hensonA1, hensonTag, hensonMod)},
				[]string{hensonGet(hensonPath), recheck(hensonPath, hensonTag, "")},
				[]string{hensonA1, hensonA1}},
			{"If-Modified-Since alone", []*models.Mock{hensonMock("h-1", t0, hensonPath, hensonA1, hensonTag, hensonMod)},
				[]string{hensonGet(hensonPath), recheck(hensonPath, "", hensonMod)},
				[]string{hensonA1, hensonA1}},
			{"a conditional fetch of another path recorded", []*models.Mock{hensonMock("h-1", t0, hensonPath, hensonA1, hensonTag, hensonMod), conditionalMock("c-other", t0.Add(time.Second), "/v3/other.json", hensonTag)},
				[]string{hensonGet(hensonPath), recheck(hensonPath, hensonTag, hensonMod), recheck("/v3/other.json", hensonTag, "")},
				[]string{hensonA1, hensonA1, "304"}},
			{"a conditional HEAD of the path recorded", []*models.Mock{hensonMock("h-1", t0, hensonPath, hensonA1, hensonTag, hensonMod), func() *models.Mock {
				m := conditionalMock("c-head", t0.Add(time.Second), hensonPath, hensonTag)
				m.Spec.HTTPReq.Method = "HEAD"
				return m
			}()},
				[]string{hensonGet(hensonPath), recheck(hensonPath, hensonTag, hensonMod), strings.Replace(recheck(hensonPath, hensonTag, ""), "GET", "HEAD", 1)},
				[]string{hensonA1, hensonA1, "304"}},
			// another test's conditional fetch of the path is not this test's data
			{"another test's conditional fetch recorded", []*models.Mock{hensonMock("h-1", t0, hensonPath, hensonA1, hensonTag, hensonMod), conditionalMock("x-c-1", t0.Add(time.Second), hensonPath, hensonTag)},
				[]string{hensonGet(hensonPath), recheck(hensonPath, hensonTag, hensonMod)},
				[]string{hensonA1, hensonA1}},
			// nor is the worker scope's, which the test's per-test read
			// shows: the app's background re-check, not the test's
			{"the worker scope's conditional fetch recorded", []*models.Mock{hensonMock("h-1", t0, hensonPath, hensonA1, hensonTag, hensonMod), conditionalMock("w-c-1", t0.Add(time.Second), hensonPath, hensonTag)},
				[]string{hensonGet(hensonPath), recheck(hensonPath, hensonTag, hensonMod)},
				[]string{hensonA1, hensonA1}},
			// the recorded re-check answers the first re-check; the next one,
			// which no recording is owed for, gets the earlier 200 again
			{"a recorded re-check spent", []*models.Mock{hensonMock("h-1", t0, hensonPath, hensonA1, hensonTag, hensonMod), conditionalMock("c-1", t0.Add(3*time.Millisecond), hensonPath, hensonTag)},
				[]string{hensonGet(hensonPath), recheck(hensonPath, hensonTag, ""), recheck(hensonPath, hensonTag, "")},
				[]string{hensonA1, "304", hensonA1}},
			// two answers of the call: the re-check gets the one whose
			// validators it carries, not the latest
			{"the answer its validators name", []*models.Mock{hensonMock("h-1", t0, hensonPath, hensonA1, hensonTag, hensonMod), hensonMock("h-2", t0.Add(time.Second), hensonPath, hensonA2, `"77-b"`, "")},
				[]string{hensonGet(hensonPath), hensonGet(hensonPath), recheck(hensonPath, hensonTag, hensonMod)},
				[]string{hensonA1, hensonA2, hensonA1}},
		} {
			t.Run(r.name+"/"+tc.name, func(t *testing.T) {
				core, logs := observer.New(zapcore.InfoLevel)
				out := recheckRun(t, zap.New(core), r.opts, tc.mapping, tc.sends)
				if !reflect.DeepEqual(out.got, tc.want) || len(out.missed) != 0 || len(out.left) != 0 {
					t.Fatalf("got %q, missed %v, left unmade %v; want %q, no miss and nothing unmade", out.got, out.missed, out.left, tc.want)
				}
				if logs.FilterMessageSnippet("cache re-check").Len() == 0 {
					t.Fatal("no line names the re-check: a replay must say it read a call as a cache re-check")
				}
			})
		}
	}
}

// conditionalMock is a recording of a conditional GET of path with
// If-None-Match tag, answered 304: a re-check the record's browser made.
func conditionalMock(name string, at time.Time, path, tag string) *models.Mock {
	mk := readMock(name, at, "GET", path, "", "")
	mk.Spec.HTTPReq.Header["If-None-Match"] = tag
	mk.Spec.HTTPResp = &models.HTTPResp{StatusCode: http.StatusNotModified, Header: map[string]string{"ETag": tag, "Cache-Control": "max-age=60"}}
	return mk
}

// What is no re-check of an answer the test got stays as it is: a
// conditional GET the test sends before any fetch of the path (its recording
// holds none), one whose validators are of no answer the test got, one that
// asks for a range or adds another precondition, one carrying a validator
// twice, one re-checking an answer that was no 200, and one re-checking
// another path's answer each miss as they did. A re-check the recording
// itself holds is the test's data: answered as recorded, and required.
func TestWhatIsNoCacheRecheckOfAnAnswerTheTestGotKeepsItsMatch(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t0 := time.Date(2026, 11, 13, 22, 5, 0, 0, time.UTC)
	h1 := func() *models.Mock { return hensonMock("h-1", t0, hensonPath, hensonA1, hensonTag, hensonMod) }
	for _, r := range recheckRounds {
		for _, tc := range []struct {
			name    string
			mapping []*models.Mock
			sends   []string
			want    []string
			left    []string
		}{
			{"never fetched", []*models.Mock{h1()},
				[]string{recheck(hensonPath, hensonTag, hensonMod), hensonGet(hensonPath)},
				[]string{"miss", hensonA1}, nil},
			{"validators of no answer", []*models.Mock{h1()},
				[]string{hensonGet(hensonPath), recheck(hensonPath, `"other"`, hensonMod)},
				[]string{hensonA1, "miss"}, nil},
			{"one validator of no answer", []*models.Mock{h1()},
				[]string{hensonGet(hensonPath), recheck(hensonPath, hensonTag, "Sat, 14 Nov 2026 01:00:00 GMT")},
				[]string{hensonA1, "miss"}, nil},
			{"a range", []*models.Mock{h1()},
				[]string{hensonGet(hensonPath), recheck(hensonPath, hensonTag, "", "Range: bytes=0-3")},
				[]string{hensonA1, "miss"}, nil},
			{"another precondition", []*models.Mock{h1()},
				[]string{hensonGet(hensonPath), recheck(hensonPath, hensonTag, "", "If-Unmodified-Since: "+hensonMod)},
				[]string{hensonA1, "miss"}, nil},
			// The fetch was of a range too, answered whole: the request
			// without its validators is that fetch, so only the range
			// itself keeps it from reading as a re-check.
			{"a range after a fetch of that range", []*models.Mock{func() *models.Mock {
				m := h1()
				m.Spec.HTTPReq.Header["Range"] = "bytes=0-3"
				return m
			}()},
				[]string{hensonGet(hensonPath, "Range: bytes=0-3"), recheck(hensonPath, hensonTag, hensonMod, "Range: bytes=0-3")},
				[]string{hensonA1, "miss"}, nil},
			{"an unconditional fetch one more", []*models.Mock{h1()},
				[]string{hensonGet(hensonPath), hensonGet(hensonPath)},
				[]string{hensonA1, "miss"}, nil},
			{"an empty validator", []*models.Mock{hensonMock("h-1", t0, hensonPath, hensonA1, "", hensonMod)},
				[]string{hensonGet(hensonPath), recheck(hensonPath, "", hensonMod, "If-None-Match: ")},
				[]string{hensonA1, "miss"}, nil},
			{"a HEAD", []*models.Mock{func() *models.Mock {
				m := h1()
				m.Spec.HTTPReq.Method, m.Spec.HTTPResp.Body = "HEAD", ""
				m.Spec.HTTPResp.Header["Content-Length"] = "0"
				return m
			}()},
				[]string{strings.Replace(hensonGet(hensonPath), "GET", "HEAD", 1), strings.Replace(recheck(hensonPath, hensonTag, hensonMod), "GET", "HEAD", 1)},
				[]string{"", "miss"}, nil},
			{"a validator twice", []*models.Mock{h1()},
				[]string{hensonGet(hensonPath), recheck(hensonPath, hensonTag, "", "If-None-Match: "+hensonTag)},
				[]string{hensonA1, "miss"}, nil},
			{"an answer that was no 200", []*models.Mock{func() *models.Mock {
				m := h1()
				m.Spec.HTTPResp.StatusCode = http.StatusNonAuthoritativeInfo
				return m
			}()},
				[]string{hensonGet(hensonPath), recheck(hensonPath, hensonTag, hensonMod)},
				[]string{hensonA1, "miss"}, nil},
			{"another path's answer", []*models.Mock{h1(), hensonMock("o-1", t0.Add(time.Second), "/v3/other.json", hensonA2, "", "")},
				[]string{hensonGet(hensonPath), recheck("/v3/other.json", hensonTag, hensonMod)},
				[]string{hensonA1, "miss"}, []string{"o-1"}},
			{"recorded re-check", []*models.Mock{h1(), conditionalMock("c-1", t0.Add(3*time.Millisecond), hensonPath, hensonTag)},
				[]string{hensonGet(hensonPath), recheck(hensonPath, hensonTag, "")},
				[]string{hensonA1, "304"}, nil},
			{"recorded re-check not made", []*models.Mock{h1(), conditionalMock("c-1", t0.Add(3*time.Millisecond), hensonPath, hensonTag)},
				[]string{hensonGet(hensonPath)},
				[]string{hensonA1}, []string{"c-1"}},
		} {
			t.Run(r.name+"/"+tc.name, func(t *testing.T) {
				core, logs := observer.New(zapcore.InfoLevel)
				out := recheckRun(t, zap.New(core), r.opts, tc.mapping, tc.sends)
				if !reflect.DeepEqual(out.got, tc.want) || !reflect.DeepEqual(out.left, tc.left) {
					t.Fatalf("got %q, left unmade %v; want %q and %v", out.got, out.left, tc.want, tc.left)
				}
				if n := logs.FilterMessageSnippet("cache re-check").Len(); n != 0 {
					t.Fatalf("%d lines name a cache re-check; want none", n)
				}
			})
		}
	}
}

// Without the proven switch nothing is read as a re-check.
func TestNoCacheRecheckIsReadWithoutTheProvenSwitch(t *testing.T) {
	t.Setenv(provenswitch.Env, "0")
	t0 := time.Date(2026, 11, 13, 22, 5, 0, 0, time.UTC)
	core, logs := observer.New(zapcore.InfoLevel)
	_ = recheckRun(t, zap.New(core), models.OutgoingOptions{}, []*models.Mock{hensonMock("h-1", t0, hensonPath, hensonA1, hensonTag, hensonMod)},
		[]string{hensonGet(hensonPath), recheck(hensonPath, hensonTag, hensonMod)})
	for _, e := range logs.All() {
		if strings.Contains(e.Message, "cache re-check") {
			t.Fatalf("a call was read as a cache re-check with the switch off: %s", e.Message)
		}
	}
}
