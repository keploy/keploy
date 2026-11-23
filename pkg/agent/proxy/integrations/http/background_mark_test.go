package http

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/containerkey"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
)

type bgTestDb struct {
	*mockMemDb
	recs     []models.BackgroundRecording
	byEpoch  map[uint64][]models.BackgroundRecording
	mark     integrations.CallRound
	bound    integrations.BoundValues
	staged   []*models.Mock
	consumed []*models.Mock
	sources  map[string][]models.DeclaredSource
}

func (d *bgTestDb) BackgroundRecordings(kind models.Kind) []models.BackgroundRecording {
	return filterBgByKind(d.recs, kind)
}

func (d *bgTestDb) BackgroundRecordingsIn(call integrations.CallRound, kind models.Kind) []models.BackgroundRecording {
	if d.byEpoch != nil && call.Read() {
		return filterBgByKind(d.byEpoch[call.Epoch()], kind)
	}
	return filterBgByKind(d.recs, kind)
}

func (d *bgTestDb) MarkCallRound() integrations.CallRound { return d.mark }

func (d *bgTestDb) BoundValuesIn(integrations.CallRound) integrations.BoundValues {
	return d.bound
}

func (d *bgTestDb) ConsumedTestMocks() []*models.Mock { return d.consumed }

func (d *bgTestDb) SourcesOf(mock string) []models.DeclaredSource {
	if d.sources == nil {
		return nil
	}
	return d.sources[mock]
}

func (d *bgTestDb) FindStagedNotOwned(kind models.Kind, _ integrations.CallRound, match func(*models.Mock) bool) (string, bool) {
	for _, m := range d.staged {
		if m != nil && m.Kind == kind && match(m) {
			return m.Name, false
		}
	}
	return "", false
}

func filterBgByKind(recs []models.BackgroundRecording, kind models.Kind) []models.BackgroundRecording {
	var out []models.BackgroundRecording
	for _, r := range recs {
		if r.Mock != nil && r.Mock.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

type bgTestBoundValues struct {
	own    []variables.Binding
	ended  map[string][]variables.Binding
	trails map[string]learn.Trail
}

func (b bgTestBoundValues) Own() []variables.Binding                { return b.own }
func (b bgTestBoundValues) EndedOf(test string) []variables.Binding { return b.ended[test] }
func (b bgTestBoundValues) LookupOf(string, string) (variables.Binding, bool, bool) {
	return variables.Binding{}, false, false
}
func (b bgTestBoundValues) TrailOf(mock string) learn.Trail { return b.trails[mock] }

func missReportWithCtx(t *testing.T, ctx context.Context, db integrations.MockMemDb, addr, raw string, opts models.OutgoingOptions) *models.MockMismatchReport {
	t.Helper()
	var reported error
	ctx = models.WithMockMismatchReporter(ctx, func(err error) { reported = err })
	app, prox := net.Pipe()
	defer func() { _ = app.Close() }()
	done := make(chan error, 1)
	go func() {
		done <- newHTTP().MockOutgoing(ctx, prox, &models.ConditionalDstCfg{Addr: addr}, db, opts)
	}()
	if _, err := io.WriteString(app, raw); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		t.Fatalf("the app got no answer: %v", err)
	}
	_ = resp.Body.Close()
	_ = app.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the parser did not return after the miss")
	}
	var withReport interface {
		MismatchReport() *models.MockMismatchReport
	}
	if !errors.As(reported, &withReport) || withReport.MismatchReport() == nil {
		t.Fatalf("the miss was reported as %v, with no mismatch report", reported)
	}
	return withReport.MismatchReport()
}

func openbaoHealthMock(name, host string, at time.Time) *models.Mock {
	return &models.Mock{
		Name: name,
		Kind: models.HTTP,
		Spec: models.MockSpec{
			ReqTimestampMock: at,
			ResTimestampMock: at.Add(5 * time.Millisecond),
			HTTPReq: &models.HTTPReq{
				Method: models.Method("GET"),
				URL:    "http://" + host + "/v1/sys/health",
				Header: map[string]string{
					"Host":            host,
					"User-Agent":      "Go-http-client/1.1",
					"Accept-Encoding": "gzip",
				},
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: 200,
				Header:     map[string]string{"Content-Type": "application/json"},
				Body:       `{"initialized":true,"sealed":false}`,
			},
		},
	}
}

func httpMockAt(name, method, host, pathAndQuery string, headers map[string]string, body string, at time.Time) *models.Mock {
	h := map[string]string{"Host": host}
	for k, v := range headers {
		h[k] = v
	}
	return &models.Mock{
		Name: name,
		Kind: models.HTTP,
		Spec: models.MockSpec{
			ReqTimestampMock: at,
			ResTimestampMock: at.Add(5 * time.Millisecond),
			HTTPReq: &models.HTTPReq{
				Method: models.Method(method),
				URL:    "http://" + host + pathAndQuery,
				Header: h,
				Body:   body,
			},
			HTTPResp: &models.HTTPResp{
				StatusCode: 200,
				Header:     map[string]string{"Content-Type": "application/json"},
				Body:       `{"ok":true}`,
			},
		},
	}
}

func rawHTTP(method, host, pathAndQuery string, headers map[string]string, body string) string {
	s := fmt.Sprintf("%s %s HTTP/1.1\r\nHost: %s\r\n", method, pathAndQuery, host)
	for k, v := range headers {
		s += fmt.Sprintf("%s: %s\r\n", k, v)
	}
	if len(body) > 0 {
		s += fmt.Sprintf("Content-Length: %d\r\n", len(body))
	}
	s += "\r\n" + body
	return s
}

func bgClockCtx(t *testing.T, proven bool, setAt, now time.Time) context.Context {
	t.Helper()
	realNow := time.Now()
	clock := func(real time.Time) (time.Time, bool) {
		return now.Add(real.Sub(realNow)), true
	}
	if proven {
		tr := learn.NewTracker()
		tr.SetAppClock(clock)
		tr.SetAppClockRun(func(time.Time) (time.Time, bool) { return setAt, true })
		tr.NewSetAt(setAt)
		return learn.WithTracker(context.Background(), tr)
	}
	sc := &learn.ServeClock{}
	sc.SetClock(clock)
	sc.NewSetAt(setAt)
	return learn.WithServeClock(context.Background(), sc)
}

func TestDecodeHTTP_BackgroundMissMark_HealthPoll(t *testing.T) {
	const addr, host = "127.0.0.1:8200", "openbao:8200"
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	healthRec := openbaoHealthMock("worker-health-1", host, at)
	healthRaw := "GET /v1/sys/health HTTP/1.1\r\nHost: " + host + "\r\nUser-Agent: Go-http-client/1.1\r\nAccept-Encoding: gzip\r\n\r\n"

	for _, proven := range []string{"", "1"} {
		name := "proven=unset"
		if proven == "1" {
			name = "proven=1"
		}
		t.Run(name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			db := &bgTestDb{
				mockMemDb: &mockMemDb{},
				recs:      []models.BackgroundRecording{{Mock: healthRec, From: at.Add(-5 * time.Second)}},
			}
			r := missReportWithCtx(t, context.Background(), db, addr, healthRaw, models.OutgoingOptions{})
			if !r.Background || r.CountsAgainstTest() {
				t.Fatalf("GET /v1/sys/health matching worker: recording got Background=%v CountsAgainstTest=%v; want Background=true CountsAgainstTest=false", r.Background, r.CountsAgainstTest())
			}
		})
	}

	// Pin documented own_repeat.go rule: an extra call identical to a worker:
	// recording (e.g. a timer poll that also fired once during the recorded
	// test window) stays MissExtraCall and is marked Background so it counts
	// against no test.
	t.Run("proven=1/extra_call_identical_to_worker_poll_is_background", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		ownPoll := openbaoHealthMock("own-health-1", host, at.Add(time.Minute))
		db := &bgTestDb{
			mockMemDb: &mockMemDb{},
			recs:      []models.BackgroundRecording{{Mock: healthRec, From: at.Add(-5 * time.Second)}},
			consumed:  []*models.Mock{ownPoll},
		}
		r := missReportWithCtx(t, context.Background(), db, addr, healthRaw, models.OutgoingOptions{})
		if r.MissKind != models.MissExtraCall || r.Recorded != 1 || !r.Background || r.CountsAgainstTest() {
			t.Fatalf("extra poll identical to worker recording got MissKind=%q Recorded=%d Background=%v CountsAgainstTest=%v; want MissExtraCall/1/true/false", r.MissKind, r.Recorded, r.Background, r.CountsAgainstTest())
		}
	})
}

func TestDecodeHTTP_BackgroundMissMark_TestOwnAndDiffersNeverBackground(t *testing.T) {
	const addr, host = "127.0.0.1:8200", "openbao:8200"
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	healthRec := openbaoHealthMock("worker-health-1", host, at)
	pollBodyRec := httpMockAt("worker-poll-1", "POST", host, "/v1/poll", map[string]string{"Content-Type": "application/json", "X-Vault-Token": "tok-a"}, `{"scope":"a"}`, at)
	otherTestRec := httpMockAt("other-test-read", "GET", host, "/v1/kv/data/secret", map[string]string{"Accept": "application/json"}, "", at)
	ownConsumed := httpMockAt("own-seal-status", "GET", host, "/v1/sys/seal-status", map[string]string{"User-Agent": "Go-http-client/1.1", "Accept-Encoding": "gzip"}, "", at)

	cases := []struct {
		name         string
		recs         []models.BackgroundRecording
		staged       []*models.Mock
		consumed     []*models.Mock
		raw          string
		wantExtra    bool
		wantNotOwned bool
	}{
		{
			name: "no worker or startup recordings",
			recs: nil,
			raw:  rawHTTP("GET", host, "/v1/sys/health", map[string]string{"User-Agent": "Go-http-client/1.1", "Accept-Encoding": "gzip"}, ""),
		},
		{
			name: "path differs (seal-status vs health)",
			recs: []models.BackgroundRecording{{Mock: healthRec}},
			raw:  rawHTTP("GET", host, "/v1/sys/seal-status", map[string]string{"User-Agent": "Go-http-client/1.1", "Accept-Encoding": "gzip"}, ""),
		},
		{
			name:      "test own call extra-call miss not in worker",
			recs:      []models.BackgroundRecording{{Mock: healthRec}},
			consumed:  []*models.Mock{ownConsumed},
			raw:       rawHTTP("GET", host, "/v1/sys/seal-status", map[string]string{"User-Agent": "Go-http-client/1.1", "Accept-Encoding": "gzip"}, ""),
			wantExtra: true,
		},
		{
			name:         "unowned call matching another test per-test recording only",
			recs:         []models.BackgroundRecording{{Mock: healthRec}},
			staged:       []*models.Mock{otherTestRec},
			raw:          rawHTTP("GET", host, "/v1/kv/data/secret", map[string]string{"Accept": "application/json"}, ""),
			wantNotOwned: true,
		},
		{
			name: "method differs",
			recs: []models.BackgroundRecording{{Mock: healthRec}},
			raw:  rawHTTP("POST", host, "/v1/sys/health", map[string]string{"User-Agent": "Go-http-client/1.1", "Accept-Encoding": "gzip"}, ""),
		},
		{
			name: "host differs",
			recs: []models.BackgroundRecording{{Mock: healthRec}},
			raw:  rawHTTP("GET", "other:8200", "/v1/sys/health", map[string]string{"User-Agent": "Go-http-client/1.1", "Accept-Encoding": "gzip"}, ""),
		},
		{
			name: "query differs",
			recs: []models.BackgroundRecording{{Mock: healthRec}},
			raw:  rawHTTP("GET", host, "/v1/sys/health?standbyok=true", map[string]string{"User-Agent": "Go-http-client/1.1", "Accept-Encoding": "gzip"}, ""),
		},
		{
			name: "body differs beyond noise",
			recs: []models.BackgroundRecording{{Mock: pollBodyRec}},
			raw:  rawHTTP("POST", host, "/v1/poll", map[string]string{"Content-Type": "application/json", "X-Vault-Token": "tok-a"}, `{"scope":"b"}`),
		},
		{
			name: "non-noisy header differs",
			recs: []models.BackgroundRecording{{Mock: pollBodyRec}},
			raw:  rawHTTP("POST", host, "/v1/poll", map[string]string{"Content-Type": "application/json", "X-Vault-Token": "tok-b"}, `{"scope":"a"}`),
		},
	}

	for _, proven := range []string{"", "1"} {
		mode := "proven=unset"
		if proven == "1" {
			mode = "proven=1"
		}
		t.Run(mode, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			for _, tc := range cases {
				t.Run(tc.name, func(t *testing.T) {
					db := &bgTestDb{
						mockMemDb: &mockMemDb{},
						recs:      tc.recs,
						staged:    tc.staged,
						consumed:  tc.consumed,
					}
					r := missReportWithCtx(t, context.Background(), db, addr, tc.raw, models.OutgoingOptions{})
					if r.Background || !r.CountsAgainstTest() {
						t.Fatalf("%s (%s): got Background=%v CountsAgainstTest=%v; want Background=false CountsAgainstTest=true", tc.name, mode, r.Background, r.CountsAgainstTest())
					}
					if proven == "1" && tc.wantExtra && r.MissKind != models.MissExtraCall {
						t.Fatalf("%s: expected MissExtraCall, got %q", tc.name, r.MissKind)
					}
					if proven == "1" && tc.wantNotOwned && !r.NotOwned {
						t.Fatalf("%s: expected NotOwned=true, got false", tc.name)
					}
				})
			}
		})
	}
}

func TestDecodeHTTP_BackgroundMissMark_NoiseAndLearnedClocks(t *testing.T) {
	const addr, host = "127.0.0.1:8200", "openbao:8200"
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

	for _, proven := range []string{"", "1"} {
		mode := "proven=unset"
		if proven == "1" {
			mode = "proven=1"
		}
		t.Run(mode+"/learned_and_user_noise", func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			rec := httpMockAt("worker-noisy", "POST", host, "/v1/health/run-1?nonce=abc", map[string]string{
				"Content-Type": "application/json",
				"X-Trace-Id":   "trace-1",
				"X-Custom-Tag": "v1",
			}, `{"seq":"1","stable":"ok"}`, at)
			rec.Spec.ReqBodyNoise = map[string][]string{
				"header.x-trace-id": {},
				"url.query.nonce":   {},
				"body.seq":          {},
			}
			opts := models.OutgoingOptions{
				NoiseConfig: map[string]map[string][]string{
					"header": {"x-custom-tag": {`^v[0-9]+$`}},
					"url":    {`run-[0-9]+`: {}},
				},
			}
			db := &bgTestDb{mockMemDb: &mockMemDb{}, recs: []models.BackgroundRecording{{Mock: rec}}}

			okRaw := rawHTTP("POST", host, "/v1/health/run-99?nonce=xyz", map[string]string{
				"Content-Type": "application/json",
				"X-Trace-Id":   "trace-99",
				"X-Custom-Tag": "v42",
			}, `{"seq":"99","stable":"ok"}`)
			r := missReportWithCtx(t, context.Background(), db, addr, okRaw, opts)
			if !r.Background || r.CountsAgainstTest() {
				t.Fatalf("noise-covered poll got Background=%v CountsAgainstTest=%v; want true/false", r.Background, r.CountsAgainstTest())
			}

			badPatRaw := rawHTTP("POST", host, "/v1/health/run-99?nonce=xyz", map[string]string{
				"Content-Type": "application/json",
				"X-Trace-Id":   "trace-99",
				"X-Custom-Tag": "bad-tag",
			}, `{"seq":"99","stable":"ok"}`)
			r = missReportWithCtx(t, context.Background(), db, addr, badPatRaw, opts)
			if r.Background || !r.CountsAgainstTest() {
				t.Fatalf("pattern-refused poll got Background=%v CountsAgainstTest=%v; want false/true", r.Background, r.CountsAgainstTest())
			}
		})
	}

	t.Run("proven=1/learned_clock_across_intervening_serve", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		recordedAt := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
		rec := httpMockAt("worker-clock", "GET", host, "/v1/poll?ts="+strconv.FormatInt(recordedAt.Unix(), 10), nil, "", recordedAt)
		rec.Spec.Clocks = map[string]models.ClockOffset{
			"url.query.ts": {Res: time.Second, Layout: "epoch-s"},
		}
		db := &bgTestDb{mockMemDb: &mockMemDb{}, recs: []models.BackgroundRecording{{Mock: rec}}}

		// Set began 10s ago on app clock; an intervening test serve happened
		// 100ms ago, while the poller read the clock 4s ago (still inside
		// CallSpansOf [setBegan, arrived], outside ClockSpanOf [served, arrived]).
		realNow := time.Now()
		appNow := recordedAt.Add(24 * time.Hour)
		setAt := realNow.Add(-10 * time.Second)
		tr := learn.NewTracker()
		tr.SetAppClock(func(real time.Time) (time.Time, bool) {
			return appNow.Add(real.Sub(realNow)), true
		})
		tr.SetAppClockRun(func(time.Time) (time.Time, bool) { return setAt, true })
		tr.NewSetAt(setAt)
		tr.ServedMockAt(&models.Mock{Name: "intervening"}, realNow.Add(-100*time.Millisecond))
		ctx := learn.WithTracker(context.Background(), tr)

		inSetRaw := rawHTTP("GET", host, "/v1/poll?ts="+strconv.FormatInt(appNow.Add(-4*time.Second).Unix(), 10), nil, "")
		r := missReportWithCtx(t, ctx, db, addr, inSetRaw, models.OutgoingOptions{})
		if !r.Background || r.CountsAgainstTest() {
			t.Fatalf("poll whose learned clock read preceded an intervening serve got Background=%v CountsAgainstTest=%v; want true/false", r.Background, r.CountsAgainstTest())
		}

		staleRaw := rawHTTP("GET", host, "/v1/poll?ts="+strconv.FormatInt(appNow.Add(-time.Hour).Unix(), 10), nil, "")
		r = missReportWithCtx(t, ctx, db, addr, staleRaw, models.OutgoingOptions{})
		if r.Background || !r.CountsAgainstTest() {
			t.Fatalf("stale clock poll got Background=%v CountsAgainstTest=%v; want false/true", r.Background, r.CountsAgainstTest())
		}

		// No clock attached -> fails closed.
		r = missReportWithCtx(t, context.Background(), db, addr, inSetRaw, models.OutgoingOptions{})
		if r.Background || !r.CountsAgainstTest() {
			t.Fatalf("poll with no clock hooks got Background=%v CountsAgainstTest=%v; want false/true", r.Background, r.CountsAgainstTest())
		}
	})
}

func TestDecodeHTTP_BackgroundMissMark_EpochTrack(t *testing.T) {
	const addr, host = "127.0.0.1:8200", "openbao:8200"
	recordedAt := time.Date(2026, 11, 20, 4, 10, 0, 500_000_000, time.UTC)
	appNow := recordedAt.Add(48 * time.Hour)

	makeEpochRecs := func(n int, build func(i int, sent time.Time, bound int64) *models.Mock) []models.BackgroundRecording {
		out := make([]models.BackgroundRecording, 0, n)
		for i := range n {
			sent := recordedAt.Add(time.Duration(i) * 7 * time.Second)
			read := sent.Add(-100 * time.Millisecond)
			m := build(i, sent, read.Unix()-60)
			out = append(out, models.BackgroundRecording{Mock: m, From: sent.Add(-2 * time.Second)})
		}
		return out
	}

	for _, proven := range []string{"", "1"} {
		isProven := proven == "1"
		mode := "proven=unset"
		if isProven {
			mode = "proven=1"
		}
		t.Run(mode, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)
			setAt := time.Now().Add(-5 * time.Second)
			ctx := bgClockCtx(t, isProven, setAt, appNow)
			liveBound := appNow.Add(-100*time.Millisecond).Unix() - 60
			staleBound := appNow.Add(-time.Hour).Unix() - 60

			t.Run("query epoch tracks across worker polls", func(t *testing.T) {
				recs := makeEpochRecs(3, func(i int, sent time.Time, bound int64) *models.Mock {
					return httpMockAt("worker-q-"+strconv.Itoa(i), "GET", host, "/v1/poll?since="+strconv.FormatInt(bound, 10), nil, "", sent)
				})
				db := &bgTestDb{mockMemDb: &mockMemDb{}, recs: recs}
				r := missReportWithCtx(t, ctx, db, addr, rawHTTP("GET", host, "/v1/poll?since="+strconv.FormatInt(liveBound, 10), nil, ""), models.OutgoingOptions{})
				if !r.Background || r.CountsAgainstTest() {
					t.Fatalf("epoch-tracked query poll got Background=%v CountsAgainstTest=%v; want true/false", r.Background, r.CountsAgainstTest())
				}

				// Stale epoch fails closed.
				r = missReportWithCtx(t, ctx, db, addr, rawHTTP("GET", host, "/v1/poll?since="+strconv.FormatInt(staleBound, 10), nil, ""), models.OutgoingOptions{})
				if r.Background || !r.CountsAgainstTest() {
					t.Fatalf("stale query epoch got Background=%v; want false", r.Background)
				}

				// 2 recordings: learn.EpochTrack requires learn.MinEpochReads (3).
				twoDb := &bgTestDb{mockMemDb: &mockMemDb{}, recs: recs[:2]}
				r = missReportWithCtx(t, ctx, twoDb, addr, rawHTTP("GET", host, "/v1/poll?since="+strconv.FormatInt(liveBound, 10), nil, ""), models.OutgoingOptions{})
				if r.Background || !r.CountsAgainstTest() {
					t.Fatalf("two-recording query epoch got Background=%v; want false", r.Background)
				}

				// No clock on ctx fails closed.
				r = missReportWithCtx(t, context.Background(), db, addr, rawHTTP("GET", host, "/v1/poll?since="+strconv.FormatInt(liveBound, 10), nil, ""), models.OutgoingOptions{})
				if r.Background || !r.CountsAgainstTest() {
					t.Fatalf("no-clock query epoch got Background=%v; want false", r.Background)
				}
			})

			t.Run("json body epoch tracks across worker polls", func(t *testing.T) {
				recs := makeEpochRecs(3, func(i int, sent time.Time, bound int64) *models.Mock {
					return httpMockAt("worker-b-"+strconv.Itoa(i), "POST", host, "/v1/poll", map[string]string{"Content-Type": "application/json"}, fmt.Sprintf(`{"dirty":true,"claimedBefore":%d}`, bound), sent)
				})
				db := &bgTestDb{mockMemDb: &mockMemDb{}, recs: recs}
				r := missReportWithCtx(t, ctx, db, addr, rawHTTP("POST", host, "/v1/poll", map[string]string{"Content-Type": "application/json"}, fmt.Sprintf(`{"dirty":true,"claimedBefore":%d}`, liveBound)), models.OutgoingOptions{})
				if !r.Background || r.CountsAgainstTest() {
					t.Fatalf("epoch-tracked body poll got Background=%v; want true", r.Background)
				}

				// Extra body field difference must stay counted.
				r = missReportWithCtx(t, ctx, db, addr, rawHTTP("POST", host, "/v1/poll", map[string]string{"Content-Type": "application/json"}, fmt.Sprintf(`{"dirty":false,"claimedBefore":%d}`, liveBound)), models.OutgoingOptions{})
				if r.Background || !r.CountsAgainstTest() {
					t.Fatalf("body poll with non-epoch diff got Background=%v; want false", r.Background)
				}
			})

			t.Run("path and header epoch track across worker polls", func(t *testing.T) {
				recs := makeEpochRecs(3, func(i int, sent time.Time, bound int64) *models.Mock {
					return httpMockAt("worker-ph-"+strconv.Itoa(i), "GET", host, "/v1/poll/"+strconv.FormatInt(bound, 10), map[string]string{"X-Poll-Since": strconv.FormatInt(bound, 10)}, "", sent)
				})
				db := &bgTestDb{mockMemDb: &mockMemDb{}, recs: recs}
				r := missReportWithCtx(t, ctx, db, addr, rawHTTP("GET", host, "/v1/poll/"+strconv.FormatInt(liveBound, 10), map[string]string{"X-Poll-Since": strconv.FormatInt(liveBound, 10)}, ""), models.OutgoingOptions{})
				if !r.Background || r.CountsAgainstTest() {
					t.Fatalf("epoch-tracked path+header poll got Background=%v; want true", r.Background)
				}
			})
		})
	}

	// Dedup safety: a recording that shares the same differing place key but
	// fails its own strictDiffWith check (e.g. a failing learned clock on a
	// different field) must NOT poison tried[key] for later recordings.
	t.Run("proven=1/failed_recording_does_not_poison_epoch_dedup", func(t *testing.T) {
		t.Setenv(provenswitch.Env, "1")
		setAt := time.Now().Add(-5 * time.Second)
		ctx := bgClockCtx(t, true, setAt, appNow)
		liveBound := appNow.Add(-100*time.Millisecond).Unix() - 60

		bad := httpMockAt("worker-bad", "GET", host, "/v1/poll?since="+strconv.FormatInt(recordedAt.Unix()-60, 10), map[string]string{"X-Other": "not-a-time"}, "", recordedAt)
		bad.Spec.Clocks = map[string]models.ClockOffset{"header.x-other": {Res: time.Second, Layout: "epoch-s"}}
		good := makeEpochRecs(3, func(i int, sent time.Time, bound int64) *models.Mock {
			return httpMockAt("worker-good-"+strconv.Itoa(i), "GET", host, "/v1/poll?since="+strconv.FormatInt(bound, 10), map[string]string{"X-Other": "not-a-time"}, "", sent)
		})
		recs := append([]models.BackgroundRecording{{Mock: bad, From: recordedAt.Add(-2 * time.Second)}}, good...)
		db := &bgTestDb{mockMemDb: &mockMemDb{}, recs: recs}
		r := missReportWithCtx(t, ctx, db, addr, rawHTTP("GET", host, "/v1/poll?since="+strconv.FormatInt(liveBound, 10), map[string]string{"X-Other": "not-a-time"}, ""), models.OutgoingOptions{})
		if !r.Background || r.CountsAgainstTest() {
			t.Fatalf("recording failing strictDiffWith poisoned tried dedup for good epoch polls: Background=%v", r.Background)
		}
	})
}

func TestDecodeHTTP_BackgroundMissMark_BoundVariables(t *testing.T) {
	const addr, host = "127.0.0.1:8200", "openbao:8200"
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	t.Setenv(provenswitch.Env, "1")

	t.Run("own scope whole binding across path query header body", func(t *testing.T) {
		rec := httpMockAt("worker-cleanup", "POST", host, "/v1/apps/rec-app-1/cleanup?id=rec-app-1", map[string]string{
			"Content-Type": "application/json",
			"X-App-Id":     "rec-app-1",
		}, `{"appId":"rec-app-1","keep":"same"}`, at)
		db := &bgTestDb{
			mockMemDb: &mockMemDb{},
			recs:      []models.BackgroundRecording{{Mock: rec}},
			bound: bgTestBoundValues{
				own: []variables.Binding{{
					Name:     "appId",
					Mock:     "mock-create",
					Place:    "body.id",
					Recorded: "rec-app-1",
					Value:    "live-app-9",
				}},
			},
		}
		raw := rawHTTP("POST", host, "/v1/apps/live-app-9/cleanup?id=live-app-9", map[string]string{
			"Content-Type": "application/json",
			"X-App-Id":     "live-app-9",
		}, `{"appId":"live-app-9","keep":"same"}`)
		r := missReportWithCtx(t, context.Background(), db, addr, raw, models.OutgoingOptions{})
		if !r.Background || r.CountsAgainstTest() {
			t.Fatalf("bound background call got Background=%v CountsAgainstTest=%v; want true/false", r.Background, r.CountsAgainstTest())
		}
		if len(r.BackgroundBound) != 4 {
			t.Fatalf("got %d BackgroundBound entries (%+v); want 4 (body, header, path, query)", len(r.BackgroundBound), r.BackgroundBound)
		}

		// Non-bound field differs -> not background.
		badRaw := rawHTTP("POST", host, "/v1/apps/live-app-9/cleanup?id=live-app-9", map[string]string{
			"Content-Type": "application/json",
			"X-App-Id":     "live-app-9",
		}, `{"appId":"live-app-9","keep":"other"}`)
		r = missReportWithCtx(t, context.Background(), db, addr, badRaw, models.OutgoingOptions{})
		if r.Background || !r.CountsAgainstTest() {
			t.Fatalf("call with extra unbound diff got Background=%v; want false", r.Background)
		}
	})

	t.Run("bound variable combined with learned clock across intervening serve", func(t *testing.T) {
		recordedAt := time.Date(2026, 11, 20, 10, 0, 0, 0, time.UTC)
		rec := httpMockAt("worker-bound-clock", "POST", host, "/v1/cleanup?ts="+strconv.FormatInt(recordedAt.Unix(), 10), map[string]string{
			"Content-Type": "application/json",
		}, `{"appId":"rec-app-1"}`, recordedAt)
		rec.Spec.Clocks = map[string]models.ClockOffset{
			"url.query.ts": {Res: time.Second, Layout: "epoch-s"},
		}
		db := &bgTestDb{
			mockMemDb: &mockMemDb{},
			recs:      []models.BackgroundRecording{{Mock: rec}},
			bound: bgTestBoundValues{
				own: []variables.Binding{{Name: "appId", Mock: "mock-create", Place: "body.id", Recorded: "rec-app-1", Value: "live-app-9"}},
			},
		}
		realNow := time.Now()
		appNow := recordedAt.Add(24 * time.Hour)
		setAt := realNow.Add(-10 * time.Second)
		tr := learn.NewTracker()
		tr.SetAppClock(func(real time.Time) (time.Time, bool) { return appNow.Add(real.Sub(realNow)), true })
		tr.SetAppClockRun(func(time.Time) (time.Time, bool) { return setAt, true })
		tr.NewSetAt(setAt)
		tr.ServedMockAt(&models.Mock{Name: "intervening"}, realNow.Add(-100*time.Millisecond))
		ctx := learn.WithTracker(context.Background(), tr)

		raw := rawHTTP("POST", host, "/v1/cleanup?ts="+strconv.FormatInt(appNow.Add(-4*time.Second).Unix(), 10), map[string]string{
			"Content-Type": "application/json",
		}, `{"appId":"live-app-9"}`)
		r := missReportWithCtx(t, ctx, db, addr, raw, models.OutgoingOptions{})
		if !r.Background || r.CountsAgainstTest() || len(r.BackgroundBound) != 1 {
			t.Fatalf("bound+clock across intervening serve got Background=%v bound=%+v; want true with 1 bound", r.Background, r.BackgroundBound)
		}
	})

	t.Run("word-by-word bound UUID at offset, torn place, and ambiguity rejection", func(t *testing.T) {
		recUUID := "prefix-11111111-2222-3333-4444-555555555555"
		liveUUID := "prefix-aaaaaaaa-bbbb-cccc-dddd-eeeeeeeeeeee"
		rec := httpMockAt("worker-uuid", "DELETE", host, "/v1/items/"+recUUID, nil, "", at)
		wordBindings := []variables.Binding{
			{Name: "id0", Mock: "mock-1", Place: "body.id#0", Recorded: "11111111", Value: "aaaaaaaa"},
			{Name: "id1", Mock: "mock-1", Place: "body.id#1", Recorded: "2222", Value: "bbbb"},
			{Name: "id2", Mock: "mock-1", Place: "body.id#2", Recorded: "3333", Value: "cccc"},
			{Name: "id3", Mock: "mock-1", Place: "body.id#3", Recorded: "4444", Value: "dddd"},
			{Name: "id4", Mock: "mock-1", Place: "body.id#4", Recorded: "555555555555", Value: "eeeeeeeeeeee"},
		}
		db := &bgTestDb{
			mockMemDb: &mockMemDb{},
			recs:      []models.BackgroundRecording{{Mock: rec}},
			bound:     bgTestBoundValues{own: wordBindings},
		}
		r := missReportWithCtx(t, context.Background(), db, addr, rawHTTP("DELETE", host, "/v1/items/"+liveUUID, nil, ""), models.OutgoingOptions{})
		if !r.Background || r.CountsAgainstTest() || len(r.BackgroundBound) != 1 || r.BackgroundBound[0].BoundPlace != "body.id" {
			t.Fatalf("word-bound UUID at offset s>0 got Background=%v bound=%+v; want Background=true bound at body.id", r.Background, r.BackgroundBound)
		}

		// Ambiguous binding for same recorded word -> rejected.
		ambig := append(wordBindings[:len(wordBindings):len(wordBindings)], variables.Binding{
			Name: "other", Mock: "mock-2", Place: "body.other#0", Recorded: "11111111", Value: "ffffffff",
		})
		ambigDb := &bgTestDb{
			mockMemDb: &mockMemDb{},
			recs:      []models.BackgroundRecording{{Mock: rec}},
			bound:     bgTestBoundValues{own: ambig},
		}
		r = missReportWithCtx(t, context.Background(), ambigDb, addr, rawHTTP("DELETE", host, "/v1/items/"+liveUUID, nil, ""), models.OutgoingOptions{})
		if r.Background || !r.CountsAgainstTest() {
			t.Fatalf("ambiguous word binding got Background=%v; want false", r.Background)
		}

		// Torn place (same word slot bound from two distinct recorded words) -> rejected.
		torn := append(wordBindings[:len(wordBindings):len(wordBindings)], variables.Binding{
			Name: "id0dup", Mock: "mock-1", Place: "body.id#0", Recorded: "99999999", Value: "zzzzzzzz",
		})
		tornDb := &bgTestDb{
			mockMemDb: &mockMemDb{},
			recs:      []models.BackgroundRecording{{Mock: rec}},
			bound:     bgTestBoundValues{own: torn},
		}
		r = missReportWithCtx(t, context.Background(), tornDb, addr, rawHTTP("DELETE", host, "/v1/items/"+liveUUID, nil, ""), models.OutgoingOptions{})
		if r.Background || !r.CountsAgainstTest() {
			t.Fatalf("torn word place got Background=%v; want false", r.Background)
		}
	})

	t.Run("trailed ended test binding vs unowned mock and ambiguous whole binding", func(t *testing.T) {
		rec := httpMockAt("worker-trail", "DELETE", host, "/v1/items/rec-9", nil, "", at)
		db := &bgTestDb{
			mockMemDb: &mockMemDb{},
			recs:      []models.BackgroundRecording{{Mock: rec}},
			bound: bgTestBoundValues{
				ended: map[string][]variables.Binding{
					"test-prev": {{Name: "itemId", Mock: "mock-prev-create", Place: "body.id", Recorded: "rec-9", Value: "live-9"}},
				},
				trails: map[string]learn.Trail{
					"worker-trail": {Test: "test-prev", Mocks: []string{"mock-prev-create"}},
				},
			},
		}
		r := missReportWithCtx(t, context.Background(), db, addr, rawHTTP("DELETE", host, "/v1/items/live-9", nil, ""), models.OutgoingOptions{})
		if !r.Background || len(r.BackgroundBound) != 1 || r.BackgroundBound[0].BoundTest != "test-prev" {
			t.Fatalf("trailed binding got Background=%v bound=%+v; want BoundTest=test-prev", r.Background, r.BackgroundBound)
		}

		// Binding made at a mock NOT in tr.Mocks -> rejected.
		otherMockDb := &bgTestDb{
			mockMemDb: &mockMemDb{},
			recs:      []models.BackgroundRecording{{Mock: rec}},
			bound: bgTestBoundValues{
				ended: map[string][]variables.Binding{
					"test-prev": {{Name: "itemId", Mock: "mock-unlisted", Place: "body.id", Recorded: "rec-9", Value: "live-9"}},
				},
				trails: map[string]learn.Trail{
					"worker-trail": {Test: "test-prev", Mocks: []string{"mock-prev-create"}},
				},
			},
		}
		r = missReportWithCtx(t, context.Background(), otherMockDb, addr, rawHTTP("DELETE", host, "/v1/items/live-9", nil, ""), models.OutgoingOptions{})
		if r.Background || !r.CountsAgainstTest() {
			t.Fatalf("unlisted trailed mock got Background=%v; want false", r.Background)
		}

		// Ambiguous whole binding -> rejected.
		ambigDb := &bgTestDb{
			mockMemDb: &mockMemDb{},
			recs:      []models.BackgroundRecording{{Mock: rec}},
			bound: bgTestBoundValues{
				own: []variables.Binding{
					{Name: "a", Mock: "m1", Place: "body.a", Recorded: "rec-9", Value: "live-9"},
					{Name: "b", Mock: "m2", Place: "body.b", Recorded: "rec-9", Value: "live-other"},
				},
			},
		}
		r = missReportWithCtx(t, context.Background(), ambigDb, addr, rawHTTP("DELETE", host, "/v1/items/live-9", nil, ""), models.OutgoingOptions{})
		if r.Background || !r.CountsAgainstTest() {
			t.Fatalf("ambiguous whole binding got Background=%v; want false", r.Background)
		}

		// Whole binding contradicted by a word binding on the same recorded text -> rejected.
		wordConflictDb := &bgTestDb{
			mockMemDb: &mockMemDb{},
			recs:      []models.BackgroundRecording{{Mock: rec}},
			bound: bgTestBoundValues{
				own: []variables.Binding{
					{Name: "a", Mock: "m1", Place: "body.a", Recorded: "rec-9", Value: "live-9"},
					{Name: "w", Mock: "m2", Place: "body.w#0", Recorded: "rec-9", Value: "other-9"},
				},
			},
		}
		r = missReportWithCtx(t, context.Background(), wordConflictDb, addr, rawHTTP("DELETE", host, "/v1/items/live-9", nil, ""), models.OutgoingOptions{})
		if r.Background || !r.CountsAgainstTest() {
			t.Fatalf("whole binding contradicted by word binding got Background=%v; want false", r.Background)
		}

		// Clock-tagged binding -> rejected.
		clockBindDb := &bgTestDb{
			mockMemDb: &mockMemDb{},
			recs:      []models.BackgroundRecording{{Mock: rec}},
			bound: bgTestBoundValues{
				own: []variables.Binding{
					{Name: "a", Mock: "m1", Place: "body.a", Recorded: "rec-9", Value: "live-9", Clock: []models.ClockRead{{At: at, Res: time.Second}}},
				},
			},
		}
		r = missReportWithCtx(t, context.Background(), clockBindDb, addr, rawHTTP("DELETE", host, "/v1/items/live-9", nil, ""), models.OutgoingOptions{})
		if r.Background || !r.CountsAgainstTest() {
			t.Fatalf("clock-tagged binding got Background=%v; want false", r.Background)
		}
	})
}

func TestDecodeHTTP_BackgroundMissMark_NumericBoundRejected(t *testing.T) {
	const addr, host = "127.0.0.1:8200", "openbao:8200"
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)

	numRec := httpMockAt("worker-jobs-claim", "POST", host, "/v1/jobs/claim", map[string]string{
		"Content-Type": "application/json",
	}, `{"batch":1}`, at)
	numRaw := rawHTTP("POST", host, "/v1/jobs/claim", map[string]string{
		"Content-Type": "application/json",
	}, `{"batch":2}`)

	// Positive control: identical fixture differing only in JSON leaf type
	// (text vs number) proves bound-value matching runs in both modes and that
	// rejecting JSON numbers is what keeps {batch:2} counted.
	textRec := httpMockAt("worker-jobs-claim-text", "POST", host, "/v1/jobs/claim", map[string]string{
		"Content-Type": "application/json",
	}, `{"batch":"1"}`, at)
	textRaw := rawHTTP("POST", host, "/v1/jobs/claim", map[string]string{
		"Content-Type": "application/json",
	}, `{"batch":"2"}`)

	binding := bgTestBoundValues{
		own: []variables.Binding{{
			Name:     "orderId",
			Mock:     "mock-create",
			Place:    "body.id",
			Recorded: "1",
			Value:    "2",
		}},
	}

	for _, proven := range []string{"", "1"} {
		mode := "proven=unset"
		if proven == "1" {
			mode = "proven=1"
		}
		t.Run(mode, func(t *testing.T) {
			t.Setenv(provenswitch.Env, proven)

			numDb := &bgTestDb{
				mockMemDb: &mockMemDb{},
				recs:      []models.BackgroundRecording{{Mock: numRec}},
				bound:     binding,
			}
			r := missReportWithCtx(t, context.Background(), numDb, addr, numRaw, models.OutgoingOptions{})
			if r.Background || !r.CountsAgainstTest() || len(r.BackgroundBound) != 0 {
				t.Fatalf("%s: regressed numeric bound-value call {batch:2} got Background=%v CountsAgainstTest=%v bound=%+v; want Background=false CountsAgainstTest=true bound=nil", mode, r.Background, r.CountsAgainstTest(), r.BackgroundBound)
			}

			textDb := &bgTestDb{
				mockMemDb: &mockMemDb{},
				recs:      []models.BackgroundRecording{{Mock: textRec}},
				bound:     binding,
			}
			r = missReportWithCtx(t, context.Background(), textDb, addr, textRaw, models.OutgoingOptions{})
			if !r.Background || r.CountsAgainstTest() || len(r.BackgroundBound) != 1 || r.BackgroundBound[0].Place != "body.batch" {
				t.Fatalf("%s: text bound-value control {batch:\"2\"} got Background=%v CountsAgainstTest=%v bound=%+v; want Background=true CountsAgainstTest=false bound=[body.batch]", mode, r.Background, r.CountsAgainstTest(), r.BackgroundBound)
			}
		})
	}
}

func TestDecodeHTTP_BackgroundMissMark_StructuralAndGuardRejections(t *testing.T) {
	const addr, host = "127.0.0.1:8200", "openbao:8200"
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	t.Setenv(provenswitch.Env, "1")
	ownApp := bgTestBoundValues{
		own: []variables.Binding{{Name: "appId", Mock: "m1", Place: "body.id", Recorded: "REC", Value: "LIVE"}},
	}

	t.Run("colliding dotted JSON key never falsely excused", func(t *testing.T) {
		rec := httpMockAt("worker-dot", "POST", host, "/v1/cleanup", map[string]string{"Content-Type": "application/json"}, `{"a.b":"c1","a":{"b":"REC"}}`, at)
		db := &bgTestDb{mockMemDb: &mockMemDb{}, recs: []models.BackgroundRecording{{Mock: rec}}, bound: ownApp}
		raw := rawHTTP("POST", host, "/v1/cleanup", map[string]string{"Content-Type": "application/json"}, `{"a.b":"c2","a":{"b":"LIVE"}}`)
		// Repeat across iterations to prove independence from Go map iteration order.
		for i := range 20 {
			r := missReportWithCtx(t, context.Background(), db, addr, raw, models.OutgoingOptions{})
			if r.Background || !r.CountsAgainstTest() {
				t.Fatalf("iter %d: colliding dotted JSON key was falsely marked Background", i)
			}
		}
	})

	t.Run("bound place coexisting with type change", func(t *testing.T) {
		rec := httpMockAt("worker-type", "POST", host, "/v1/cleanup", map[string]string{"Content-Type": "application/json"}, `{"id":"REC","n":"1"}`, at)
		db := &bgTestDb{mockMemDb: &mockMemDb{}, recs: []models.BackgroundRecording{{Mock: rec}}, bound: ownApp}
		r := missReportWithCtx(t, context.Background(), db, addr, rawHTTP("POST", host, "/v1/cleanup", map[string]string{"Content-Type": "application/json"}, `{"id":"LIVE","n":1}`), models.OutgoingOptions{})
		if r.Background || !r.CountsAgainstTest() {
			t.Fatalf("type change beside bound field marked Background")
		}
	})

	t.Run("bound place coexisting with array length change", func(t *testing.T) {
		rec := httpMockAt("worker-arr", "POST", host, "/v1/cleanup", map[string]string{"Content-Type": "application/json"}, `{"id":"REC","xs":[1,2]}`, at)
		db := &bgTestDb{mockMemDb: &mockMemDb{}, recs: []models.BackgroundRecording{{Mock: rec}}, bound: ownApp}
		r := missReportWithCtx(t, context.Background(), db, addr, rawHTTP("POST", host, "/v1/cleanup", map[string]string{"Content-Type": "application/json"}, `{"id":"LIVE","xs":[1,2,3]}`), models.OutgoingOptions{})
		if r.Background || !r.CountsAgainstTest() {
			t.Fatalf("array length change beside bound field marked Background")
		}
	})

	t.Run("bound place coexisting with pattern-guarded body field refusal", func(t *testing.T) {
		rec := httpMockAt("worker-pat", "POST", host, "/v1/cleanup", map[string]string{"Content-Type": "application/json"}, `{"id":"REC","tag":"v1"}`, at)
		db := &bgTestDb{mockMemDb: &mockMemDb{}, recs: []models.BackgroundRecording{{Mock: rec}}, bound: ownApp}
		opts := models.OutgoingOptions{
			NoiseConfig: map[string]map[string][]string{"requestbody": {"tag": {`^v[0-9]+$`}}},
		}
		r := missReportWithCtx(t, context.Background(), db, addr, rawHTTP("POST", host, "/v1/cleanup", map[string]string{"Content-Type": "application/json"}, `{"id":"LIVE","tag":"bad"}`), opts)
		if r.Background || !r.CountsAgainstTest() {
			t.Fatalf("pattern-guarded body refusal beside bound field marked Background")
		}
	})

	t.Run("bound place coexisting with multi-valued query change", func(t *testing.T) {
		rec := httpMockAt("worker-mvq", "GET", host, "/v1/cleanup?id=REC&k=1&k=2", nil, "", at)
		db := &bgTestDb{mockMemDb: &mockMemDb{}, recs: []models.BackgroundRecording{{Mock: rec}}, bound: ownApp}
		r := missReportWithCtx(t, context.Background(), db, addr, rawHTTP("GET", host, "/v1/cleanup?id=LIVE&k=1&k=3", nil, ""), models.OutgoingOptions{})
		if r.Background || !r.CountsAgainstTest() {
			t.Fatalf("multi-valued query change beside bound param marked Background")
		}
	})

	t.Run("bound place coexisting with Content-Type change", func(t *testing.T) {
		rec := httpMockAt("worker-ct", "POST", host, "/v1/cleanup", map[string]string{"Content-Type": "application/json"}, `{"id":"REC"}`, at)
		db := &bgTestDb{mockMemDb: &mockMemDb{}, recs: []models.BackgroundRecording{{Mock: rec}}, bound: ownApp}
		r := missReportWithCtx(t, context.Background(), db, addr, rawHTTP("POST", host, "/v1/cleanup", map[string]string{"Content-Type": "text/plain"}, `{"id":"LIVE"}`), models.OutgoingOptions{})
		if r.Background || !r.CountsAgainstTest() {
			t.Fatalf("Content-Type change beside bound field marked Background")
		}
	})

	t.Run("bound place coexisting with SigV4 Authorization diff", func(t *testing.T) {
		sigA := "AWS4-HMAC-SHA256 Credential=AKIA1/20261123/us-east-1/sts/aws4_request, SignedHeaders=host, Signature=aaaa"
		sigB := "AWS4-HMAC-SHA256 Credential=AKIA2/20261123/us-east-1/sts/aws4_request, SignedHeaders=host, Signature=bbbb"
		rec := httpMockAt("worker-sigv4", "POST", host, "/v1/cleanup", map[string]string{"Content-Type": "application/json", "Authorization": sigA}, `{"id":"REC"}`, at)
		db := &bgTestDb{mockMemDb: &mockMemDb{}, recs: []models.BackgroundRecording{{Mock: rec}}, bound: ownApp}
		r := missReportWithCtx(t, context.Background(), db, addr, rawHTTP("POST", host, "/v1/cleanup", map[string]string{"Content-Type": "application/json", "Authorization": sigB}, `{"id":"LIVE"}`), models.OutgoingOptions{})
		if r.Background || !r.CountsAgainstTest() {
			t.Fatalf("SigV4 credential diff beside bound field marked Background")
		}
	})

	t.Run("declared clock rule bars binding and checks clock", func(t *testing.T) {
		rec := httpMockAt("worker-decl-clock", "POST", host, "/v1/cleanup", map[string]string{"Content-Type": "application/json"}, `{"ts":"REC"}`, at)
		db := &bgTestDb{
			mockMemDb: &mockMemDb{},
			recs:      []models.BackgroundRecording{{Mock: rec}},
			bound:     ownApp,
			sources: map[string][]models.DeclaredSource{
				"worker-decl-clock": {{Rule: "clock-ts", Kind: models.DeclaredSourceClock, Field: "body.ts", Clock: &models.ClockOffset{Res: time.Second}}},
			},
		}
		r := missReportWithCtx(t, context.Background(), db, addr, rawHTTP("POST", host, "/v1/cleanup", map[string]string{"Content-Type": "application/json"}, `{"ts":"LIVE"}`), models.OutgoingOptions{})
		if r.Background || !r.CountsAgainstTest() {
			t.Fatalf("declared clock rule field without clock check was excused by binding")
		}
	})

	t.Run("container destination change never marked Background", func(t *testing.T) {
		cA := localstack("cid-a", "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa")
		cB := localstack("cid-b", "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb")
		sess := newSession(containerIndex{"127.0.0.1:8200": cB})
		rec := recordedFrom("worker-ctr", "/v1/sys/health", "localhost:8200", cA, "s1")
		useDefaultKeyer(t, sess.k, rec)
		db := &bgTestDb{mockMemDb: &mockMemDb{}, recs: []models.BackgroundRecording{{Mock: rec}}}
		ctx := containerkey.WithConn(context.Background(), sess.k, "127.0.0.1:8200")
		r := missReportWithCtx(t, ctx, db, "127.0.0.1:8200", rawHTTP("GET", "localhost:8200", "/v1/sys/health", nil, ""), models.OutgoingOptions{})
		if r.Background || !r.CountsAgainstTest() {
			t.Fatalf("call to different container was falsely marked Background")
		}
	})
}

func TestDecodeHTTP_BackgroundMissMark_RoundScoped(t *testing.T) {
	const addr, host = "127.0.0.1:8200", "openbao:8200"
	at := time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)
	healthRec := openbaoHealthMock("worker-health-1", host, at)
	healthRaw := "GET /v1/sys/health HTTP/1.1\r\nHost: " + host + "\r\nUser-Agent: Go-http-client/1.1\r\nAccept-Encoding: gzip\r\n\r\n"
	t.Setenv(provenswitch.Env, "1")

	markSet1 := integrations.NewCallRound(1, nil).ReadAt(integrations.RoundAt{Epoch: 1})
	// Call marked in set 1 (which has healthRec), even though current set 2 has none.
	db := &bgTestDb{
		mockMemDb: &mockMemDb{},
		mark:      markSet1,
		byEpoch: map[uint64][]models.BackgroundRecording{
			1: {{Mock: healthRec}},
			2: nil,
		},
	}
	r := missReportWithCtx(t, context.Background(), db, addr, healthRaw, models.OutgoingOptions{})
	if !r.Background || r.CountsAgainstTest() {
		t.Fatalf("call marked in set 1 (with health poll) got Background=%v; want true", r.Background)
	}

	// Call marked in set 1 (no health poll), even though set 2 has one.
	dbOther := &bgTestDb{
		mockMemDb: &mockMemDb{},
		mark:      markSet1,
		byEpoch: map[uint64][]models.BackgroundRecording{
			1: nil,
			2: {{Mock: healthRec}},
		},
	}
	r = missReportWithCtx(t, context.Background(), dbOther, addr, healthRaw, models.OutgoingOptions{})
	if r.Background || !r.CountsAgainstTest() {
		t.Fatalf("call marked in set 1 (without health poll) got Background=%v; want false", r.Background)
	}
}
