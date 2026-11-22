package proxy

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

func learnServe(t *testing.T, h integrations.Integrations, mm integrations.MockMemDb, ctx context.Context, opts models.OutgoingOptions, rawReq string) (*http.Response, []byte) {
	t.Helper()
	app, agent := net.Pipe()
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	served := make(chan error, 1)
	go func() {
		served <- h.MockOutgoing(ctx, agent, &models.ConditionalDstCfg{}, mm, opts)
		agent.Close()
	}()
	defer func() { app.Close(); <-served }()
	if _, err := app.Write([]byte(rawReq)); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		return nil, nil
	}
	var body []byte
	if resp.ContentLength < 0 {
		body, _ = io.ReadAll(resp.Body)
	} else {
		body = make([]byte, resp.ContentLength)
		_, _ = io.ReadFull(resp.Body, body)
	}
	return resp, body
}

func learnRead(t *testing.T, db integrations.MockMemDb, ctx context.Context, opts models.OutgoingOptions, n int, vary bool) string {
	h := httpint.New(zap.NewNop())
	var got []string
	for i := 0; i < n; i++ {
		id := "rec"
		if vary {
			id = "r" + string(rune('a'+i))
		}
		resp, body := learnServe(t, h, db, ctx, opts, "GET /counter HTTP/1.1\r\nHost: api\r\nX-Req-Id: "+id+"\r\n\r\n")
		if resp == nil || resp.StatusCode != 200 {
			got = append(got, "MISS")
			continue
		}
		got = append(got, string(body))
	}
	return strings.Join(got, ",")
}

// A stated stateful recording no open test owns (the pool's, between tests
// or at start-up) is served in recorded order and kept reusable through the
// learn step's doors, as through the matcher: identical calls get a1, a2, a3,
// b1, b2, then b2 again.
func TestALearnStepServesAReusableStatefulSequenceInOrder(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	base := time.Now().Add(-time.Hour)
	pool := func(noise bool) []*models.Mock {
		ms := append(perTestCounterPool(base, "a1", "a2", "a3"), perTestCounterPool(base.Add(time.Minute), "b1", "b2")...)
		for _, m := range ms {
			m.Spec.HTTPReq.Header = map[string]string{"X-Req-Id": "rec"}
			if noise {
				m.Spec.ReqBodyNoise = map[string][]string{"header.x-req-id": {}}
			}
		}
		return ms
	}
	tr := learn.NewTracker() // no test begun
	ctx := learn.WithTracker(context.Background(), tr)
	for _, tc := range []struct {
		name  string
		noise bool
		vary  bool
		opts  models.OutgoingOptions
		exact bool
	}{
		{"EXACT SERVING pool view: CONTROL matcher", false, false, models.OutgoingOptions{}, true},
		{"EXACT SERVING pool view: learnMatch (F)", false, false, models.OutgoingOptions{Learn: true, LearnPool: true}, true},
		{"EXACT SERVING pool view: learnedMatch", true, false, models.OutgoingOptions{}, true},
		{"CONTROL matcher (h.match), identical calls", false, false, models.OutgoingOptions{}, false},
		{"learnedMatch (replay of what F learned), calls vary in the learned header", true, true, models.OutgoingOptions{}, false},
		{"learnedMatch, identical calls", true, false, models.OutgoingOptions{}, false},
		{"learnMatch (F, full record), identical calls", false, false, models.OutgoingOptions{Learn: true, LearnPool: true}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mgr := newCursorManager(t)
			mgr.SetMocksWithWindow(pool(tc.noise), nil, models.BaseTime, time.Now())
			if tc.exact {
				mgr.ServeExactPool("between", []string{"a1", "a2", "a3", "b1", "b2"})
			}
			if got := learnRead(t, mgr, ctx, tc.opts, 7, tc.vary); got != "a1,a2,a3,b1,b2,b2,b2" {
				t.Fatalf("%s: served %s", tc.name, got)
			}
		})
	}
}

// A reusable recording K-B9's recorders state (per-test lifetime, cursor
// consumption) that the learn step pairs twice in a test keeps both
// pairings' echoes on its consumed state, though the door that keeps it
// (MarkMockAsUsed) does not replace the pool's copy.
func TestALearnStepReusedStatedMockKeepsEveryEchoItWasServedWith(t *testing.T) {
	for _, tier := range []string{"perTest", "unfiltered"} {
		t.Run(tier, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			p := startedDrainProxy(t, 100)
			p.learnTracker = learn.NewTracker()
			now := time.Now()
			m := r3Mock("mock-0", now, "GET", "/users/u-1?rid=r-1", "", `{"id":"u-1","rid":"r-1"}`)
			m.Spec.Metadata[models.MetadataLifetimeKey] = models.MetadataLifetimePerTest
			m.DeriveLifetime()
			if m.TestModeInfo.Consume != models.ConsumeCursorSaturate || !m.StatesLifetime() {
				t.Fatalf("precondition: stated per-test cursor mock, got %+v", m.TestModeInfo)
			}
			mm := NewMockManager(nil, nil, zap.NewNop())
			t.Cleanup(mm.Close)
			mm.SeedRecordedWindows([]models.TestWindow{{Start: now.Add(time.Minute), End: now.Add(2 * time.Minute)}})
			if tier == "perTest" {
				mm.SetMocksWithWindow([]*models.Mock{m}, nil, now.Add(-time.Hour), now.Add(time.Hour))
			} else {
				mm.SetMocksWithWindow(nil, []*models.Mock{m}, now.Add(-time.Hour), now.Add(time.Hour))
			}
			p.ContinueTestErrorCapture()
			opts := models.OutgoingOptions{Learn: true, LearnPool: true}
			for _, c := range []struct{ target, want string }{
				{"/users/u-2?rid=r-2", `{"id":"u-2","rid":"r-2"}`},
				{"/users/u-1?rid=r-3", `{"id":"u-1","rid":"r-3"}`},
			} {
				code, got := r3Send(t, p, mm, opts, "GET "+c.target+" HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\n\r\n")
				if code != http.StatusOK || got != c.want {
					t.Fatalf("precondition: %s answered %d %s", c.target, code, got)
				}
			}
			st := r3States(mm)["mock-0"]
			if len(st.Variables) < 2 {
				t.Errorf("the consumed state lost an echo F served the first call with: %+v", st.Variables)
			}
		})
	}
}
