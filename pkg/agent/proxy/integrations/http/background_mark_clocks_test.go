package http

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// backgroundDb is a store that hands the set's background recordings
// (integrations.RoundBackgroundReader) as the agent's does: marked, those
// of the set a call was marked in (MarkCallRound's, epoch 1), and current,
// those of the set staged now, which the zero CallRound reads (a call no
// round was marked for, as with the switch off). Its reads show no
// recording, so every call misses.
type backgroundDb struct {
	sourcedDb
	marked, current []models.BackgroundRecording
}

func (d backgroundDb) MarkCallRound() integrations.CallRound {
	return integrations.NewCallRound(1, nil)
}

// FindStagedNotOwned implements integrations.StagedFinder, whose store marks
// a call's round (integrations.MarkCallRound): d stages nothing more than
// its reads show.
func (d backgroundDb) FindStagedNotOwned(models.Kind, integrations.CallRound, func(*models.Mock) bool) (string, bool) {
	return "", false
}

// HoldToRound implements integrations.RoundHolder: d never stages again, so
// it holds every call it marks as it is.
func (d backgroundDb) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return d, func() bool { return false }
}

func (d backgroundDb) BackgroundRecordingsIn(call integrations.CallRound, kind models.Kind) []models.BackgroundRecording {
	in := d.current
	if call.Read() {
		if call.Epoch() != 1 {
			return nil
		}
		in = d.marked
	}
	var out []models.BackgroundRecording
	for _, r := range in {
		if r.Mock == nil || r.Mock.Kind == kind {
			out = append(out, r)
		}
	}
	return out
}

var _ integrations.RoundBackgroundReader = backgroundDb{}

// bgOf is the background recordings of mocks.
func bgOf(mocks ...*models.Mock) []models.BackgroundRecording {
	out := make([]models.BackgroundRecording, len(mocks))
	for i, m := range mocks {
		out[i] = models.BackgroundRecording{Mock: m}
	}
	return out
}

// backgroundMiss is the mismatch report of the miss of request, sent to
// keploy's HTTP parser serving db.
func backgroundMiss(t *testing.T, db integrations.MockMemDb, addr, request string) *models.MockMismatchReport {
	t.Helper()
	var reported error
	ctx := models.WithMockMismatchReporter(context.Background(), func(err error) { reported = err })
	app, prox := net.Pipe()
	defer func() { _ = app.Close() }()
	done := make(chan error, 1)
	go func() {
		done <- newHTTP().MockOutgoing(ctx, prox, &models.ConditionalDstCfg{Addr: addr}, db, models.OutgoingOptions{})
	}()
	if _, err := io.WriteString(app, request); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		t.Fatalf("the app got no answer: %v", err)
	}
	_ = resp.Body.Close()
	if resp.StatusCode != http.StatusBadGateway {
		t.Fatalf("the app got %d; want keploy's miss answer: no recording is in a read", resp.StatusCode)
	}
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

// A call no recording answers that is the same request as one of the set's
// HTTP background recordings (a liveness probe of its worker: scope, made
// once more than a recording was left to answer) is a background miss:
// listed, it counts against no test. Under either switch, as Mongo's
// parser marks one. A call that differs from every one of them (another
// path, another query value) is its test's miss, as is the same call with
// only another kind's background recording of it, or with the store's
// recording held for a set the call was not marked in: under the switch the
// call is judged by the background of the set it was marked in, and with
// the switch off, where keploy marks no round, by the set staged now.
func TestAMissThatIsABackgroundRecordingsRequestCountsAgainstNoTest(t *testing.T) {
	const addr, host = "127.0.0.1:50001", "localhost:50001"
	health := onHost("w-health", "/v1/sys/health", host)
	asMongo := onHost("w-health-mongo", "/v1/sys/health", host)
	asMongo.Kind = models.Mongo
	get := func(target string) string {
		return "GET " + target + " HTTP/1.1\r\nHost: " + host + "\r\nAccept: application/json\r\n\r\n"
	}
	for _, c := range []struct {
		name            string
		marked, current []models.BackgroundRecording
		request         string
		want            bool // under the switch
		wantOff         bool // with it off
	}{
		{name: "the background recording's request", marked: bgOf(health), current: bgOf(health), request: get("/v1/sys/health"), want: true, wantOff: true},
		{name: "another path", marked: bgOf(health), current: bgOf(health), request: get("/v1/sys/seal-status")},
		{name: "another query value", marked: bgOf(health), current: bgOf(health), request: get("/v1/sys/health?standbyok=true")},
		{name: "another kind's background recording only", marked: bgOf(asMongo), current: bgOf(asMongo), request: get("/v1/sys/health")},
		{name: "no background recordings", request: get("/v1/sys/health")},
		{name: "held for the set staged now, not the marked set", current: bgOf(health), request: get("/v1/sys/health"), wantOff: true},
		{name: "held for the marked set, not the set staged now", marked: bgOf(health), request: get("/v1/sys/health"), want: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			for _, on := range []bool{true, false} {
				want := c.want
				if on {
					t.Setenv(provenswitch.Env, "1")
				} else {
					t.Setenv(provenswitch.Env, "")
					want = c.wantOff
				}
				db := backgroundDb{sourcedDb: sourcedDb{mockMemDb: &mockMemDb{}}, marked: c.marked, current: c.current}
				r := backgroundMiss(t, db, addr, c.request)
				if r.Background != want || r.CountsAgainstTest() == want {
					t.Fatalf("switch on %v: the miss is Background %v, counts against its test %v; want Background %v", on, r.Background, r.CountsAgainstTest(), want)
				}
			}
		})
	}
}

// The background mark reads the call as the strict comparison does: apart
// from the noise learned onto the background recording and the clocks its
// declared rules name, each a read of the call's own window on the
// application's clock. The PAT store's last-use touch, a declared once
// series of the worker: scope, made again in a later test, stamps
// lastUsedAt and updatedAt from its own clock: a background miss. One that
// writes back another version (options.cas) is not, nor is one that
// stamps the clocks with no rule declaring them, unless the recording
// learned the version as noise. A store that hands a nil recording beside
// the touch's is read past it. A nil report is left as it is.
func TestTheBackgroundMarkReadsLearnedNoiseAndDeclaredClocks(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	base := time.Date(2026, 11, 11, 14, 16, 33, 0, time.UTC)
	recAt := time.Date(2026, 11, 11, 14, 6, 39, 0, time.UTC)
	now := base.Add(time.Second)
	plain := &models.ClockOffset{Min: -time.Second, Res: time.Second}
	both := []models.DeclaredSource{
		{Rule: "pat-last-used-at", Kind: models.DeclaredSourceClock, Field: "body.data.lastUsedAt", Clock: plain},
		{Rule: "pat-updated-at", Kind: models.DeclaredSourceClock, Field: "body.data.updatedAt", Clock: plain},
	}
	for _, c := range []struct {
		name    string
		sources []models.DeclaredSource
		cas     string // the call's options.cas, "" for the recorded 1
		noise   string // a field learned as noise on the recording
		nilRec  bool   // the store hands a nil recording first
		want    bool
	}{
		{name: "stamped in the call's window, both clocks declared", sources: both, want: true},
		{name: "another version written back", sources: both, cas: "2"},
		{name: "another version written back, learned as noise", sources: both, cas: "2", noise: "body.options.cas", want: true},
		{name: "no rule declares the clocks", sources: nil},
		{name: "a nil recording handed first", sources: both, nilRec: true, want: true},
	} {
		t.Run(c.name, func(t *testing.T) {
			m, call := patTouch(recAt)
			if c.noise != "" {
				m.Spec.ReqBodyNoise = map[string][]string{c.noise: {}}
			}
			in := call(now, now)
			if c.cas != "" {
				in = patTouchCall(strings.Replace(string(in.body), `"cas":1`, `"cas":`+c.cas, 1))
			}
			in.arrived = base.Add(2 * time.Second)
			recs := bgOf(m)
			if c.nilRec {
				recs = append([]models.BackgroundRecording{{}}, recs...)
			}
			db := backgroundDb{sourcedDb: sourcedDb{mockMemDb: &mockMemDb{}, sources: map[string][]models.DeclaredSource{"mock-220": c.sources},
				background: map[string]bool{"mock-220": true}}, marked: recs}
			h := newHTTP()
			report := &models.MockMismatchReport{}
			func() {
				defer func() {
					if p := recover(); p != nil {
						t.Fatalf("the mark panicked: %v", p)
					}
				}()
				h.markBackground(clockConn(true, base), report, in, db, db, db.MarkCallRound(), false, models.ProvenFlakyHeaderNoise(), nil, nil)
			}()
			if report.Background != c.want {
				t.Fatalf("Background %v; want %v", report.Background, c.want)
			}
		})
	}
	t.Run("a nil report", func(t *testing.T) {
		m, call := patTouch(recAt)
		db := backgroundDb{sourcedDb: sourcedDb{mockMemDb: &mockMemDb{}}, marked: bgOf(m)}
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("the mark of a nil report panicked: %v", p)
			}
		}()
		h := newHTTP()
		// The recorded touch itself: the recording is the call, so a mark
		// would be written.
		h.markBackground(clockConn(true, base), nil, call(recAt, recAt), db, db, db.MarkCallRound(), false, models.ProvenFlakyHeaderNoise(), nil, nil)
	})
}
