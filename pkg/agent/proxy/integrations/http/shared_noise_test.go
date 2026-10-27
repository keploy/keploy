package http

import (
	"bufio"
	"context"
	"fmt"
	"net/http"
	"reflect"
	"sort"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
	"go.uber.org/zap"
)

// sharedNoiseDb is a test's own recordings (learnDb: consumed when served,
// every moment in the open test's window) with the replay's variable table.
type sharedNoiseDb struct {
	learnDb
	table variables.Table
}

func (d *sharedNoiseDb) Variables() *variables.Table { return &d.table }

// sharedNoiseOpts are a run's options with noise in every request bucket: two
// headers (one with a value pattern), a request-body path and a URL pattern,
// beside response-body noise.
func sharedNoiseOpts(learn bool) models.OutgoingOptions {
	return models.OutgoingOptions{Learn: learn, NoiseConfig: map[string]map[string][]string{
		"header":      {"X-Request-Id": {}, "X-Trace": {"^t-"}},
		"requestbody": {"order.id": {}},
		"url":         {"/orders/[0-9]+": {}},
		"body":        {"ok": {}},
	}}
}

func orderCall(path string, id, q int, reqID string) string {
	body := fmt.Sprintf(`{"order":{"id":%d,"q":%d}}`, id, q)
	return fmt.Sprintf("POST /orders/%s HTTP/1.1\r\nHost: api.test\r\nContent-Type: application/json\r\nX-Request-Id: %s\r\nX-Trace: t-%s\r\nContent-Length: %d\r\n\r\n%s",
		path, reqID, reqID, len(body), body)
}

func createCall(id string) string {
	body := `{"id":"` + id + `","item":"pen"}`
	return fmt.Sprintf("POST /orders HTTP/1.1\r\nHost: store\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s", len(body), body)
}

func readBackCall(id string) string {
	return "GET /orders/" + id + " HTTP/1.1\r\nHost: store\r\n\r\n"
}

func jsonAnswer(status int, body string) string {
	return fmt.Sprintf("HTTP/1.1 %d %s\r\nContent-Type: application/json\r\nContent-Length: %d\r\n\r\n%s",
		status, http.StatusText(status), len(body), body)
}

// recordedAs is the test's own recording of req answered with resp, the
// seq-th call of the test, with vars as its variable places.
func recordedAs(t *testing.T, h *HTTP, name string, seq int, req, resp string, vars models.MockVariables) *models.Mock {
	t.Helper()
	at := learnAt.Add(time.Duration(seq) * time.Second)
	m, err := h.buildHTTPMock(&FinalHTTP{Req: []byte(req), Resp: []byte(resp), ReqTimestampMock: at, ResTimestampMock: at},
		80, "test-conn", models.OutgoingOptions{})
	if err != nil || m == nil {
		t.Fatalf("buildHTTPMock = (%v, %v)", m, err)
	}
	m.Name = name
	m.Spec.Variables = vars
	m.TestModeInfo.Lifetime = models.LifetimePerTest
	return m
}

// serveOne serves raw, a whole request, on a connection of its own and
// returns the answer's status once decodeHTTP returned.
func serveOne(t *testing.T, h *HTTP, db integrations.MockMemDb, opts models.OutgoingOptions, raw string) int {
	t.Helper()
	app, proxy := tcpPair(t)
	done := runDecode(t, proxy, func(ctx context.Context) error {
		return h.decodeHTTP(ctx, []byte(raw), proxy, nil, db, opts)
	})
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		t.Fatalf("the app got no answer: %v", err)
	}
	_ = resp.Body.Close()
	_ = app.Close()
	select {
	case <-done:
	case <-time.After(30 * time.Second): // the bound only turns a hang into a failure
		t.Fatal("decodeHTTP did not return once the app closed its connection")
	}
	return resp.StatusCode
}

// The request noise requestNoiseFor hands every call (one build per options
// value) is never written by what reads it. Main built fresh maps per call, so
// a matcher that wrote into them changed only its own call; the shared noise
// would carry such a write into every later call and test. Calls with header,
// request-body and URL noise are served through decodeHTTP with the switch
// off (schema, fuzzy and exact matching), on (the learned match, the variable
// pass and, for a variable that does not hold, the exact-only pass, the strict
// matcher) and on in record's full round (the learn step, which learns a
// changed field or path as noise there, and the pairing), answered and missed
// (the mismatch report, the extra-call
// count, the answered diagnosis, the recording made elsewhere and the repeat
// note), and then the noise they were all handed must still be the noise of
// the options (NewRequestNoise), and the options' configuration as given.
func TestNoServedCallWritesTheSharedRequestNoise(t *testing.T) {
	h := newHTTP()
	learnRound := false
	type call struct {
		raw  string
		want map[string]int // the status by row
	}
	orderRec := func(t *testing.T) []*models.Mock {
		return []*models.Mock{recordedAs(t, h, "mock-order", 1, orderCall("17", 1, 2, "r-1"), jsonAnswer(200, `{"ok":1}`), nil)}
	}
	// A create the dependency answers with the id it was sent, and its read
	// back: a replay finds the variable recorded on them, record's full round
	// learns it (the recordings carry none yet).
	varRecs := func(t *testing.T) []*models.Mock {
		var bind, use models.MockVariables
		if !learnRound {
			bind = models.MockVariables{"orderId": bindV("ord-rec-1", "body.id", "body.id")}
			use = models.MockVariables{"orderId": useV("ord-rec-1", []string{"url.path[1]"}, "body.id")}
		}
		return []*models.Mock{
			recordedAs(t, h, "mock-create", 1, createCall("ord-rec-1"), jsonAnswer(201, `{"id":"ord-rec-1"}`), bind),
			recordedAs(t, h, "mock-read", 2, readBackCall("ord-rec-1"), jsonAnswer(200, `{"id":"ord-rec-1","item":"pen","n":1}`), use),
		}
	}
	scenarios := []struct {
		name  string
		recs  func(*testing.T) []*models.Mock
		calls []call
	}{
		{"a call that differs in its noise", orderRec, []call{
			{orderCall("18", 2, 2, "r-9"), map[string]int{"off": 200, "on": 200, "learn": 200}}}},
		{"a call that differs in a field", orderRec, []call{
			{orderCall("18", 2, 3, "r-9"), map[string]int{"off": 200, "on": 502, "learn": 200}}}},
		{"an unrecorded call", orderRec, []call{
			{"GET /none HTTP/1.1\r\nHost: api.test\r\nX-Request-Id: r-9\r\n\r\n", map[string]int{"off": 502, "on": 502, "learn": 502}}}},
		{"a variable bound and used", varRecs, []call{
			{createCall("ord-liv-9"), map[string]int{"off": 201, "on": 201, "learn": 201}},
			{readBackCall("ord-liv-9"), map[string]int{"off": 502, "on": 200, "learn": 200}}}},
		{"a variable used with another value", varRecs, []call{
			{createCall("ord-liv-9"), map[string]int{"off": 201, "on": 201, "learn": 201}},
			{readBackCall("ord-oth-7"), map[string]int{"off": 502, "on": 502, "learn": 200}}}},
	}
	for _, row := range []struct {
		name, proven string
		learn        bool
	}{{"off", "0", false}, {"on", "1", false}, {"learn", "1", true}} {
		t.Run(row.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, row.proven)
			learnRound = row.learn
			opts := sharedNoiseOpts(row.learn)
			given := cloneNoiseConfig(opts.NoiseConfig)
			lastRequestNoise.Store(nil)
			before := requestNoiseBuilds.Load()
			for _, sc := range scenarios {
				db := &sharedNoiseDb{learnDb: learnDb{perTest: sc.recs(t)}}
				for i, c := range sc.calls {
					if got := serveOne(t, h, db, opts, c.raw); got != c.want[row.name] {
						t.Errorf("%s, call %d: answered %d, want %d", sc.name, i+1, got, c.want[row.name])
					}
				}
			}
			if builds := requestNoiseBuilds.Load() - before; builds != 1 {
				t.Fatalf("the calls of one options value built the request noise %d times, want once", builds)
			}
			got := sortedRequestNoise(lastRequestNoise.Load().noise)
			if want := sortedRequestNoise(NewRequestNoise(zap.NewNop(), opts)); !reflect.DeepEqual(got, want) {
				t.Errorf("the request noise the calls were handed was written: %s", noiseDiff(got, want))
			}
			if !sameNoiseConfig(opts.NoiseConfig, given) {
				t.Errorf("the options' noise configuration is now %v, want %v as given", opts.NoiseConfig, given)
			}
		})
	}
}

// noiseDiff says where got differs from want.
func noiseDiff(got, want RequestNoise) string {
	var d []string
	for bucket, maps := range map[string][2]map[string][]string{"header": {got.Header, want.Header}, "body": {got.Body, want.Body}} {
		g, w := maps[0], maps[1]
		if (g == nil) != (w == nil) {
			d = append(d, fmt.Sprintf("%s noise %v, want %v", bucket, g, w))
		}
		for k, v := range g {
			if wv, ok := w[k]; !ok || !reflect.DeepEqual(v, wv) {
				d = append(d, fmt.Sprintf("%s %q is %#v, want %#v (in the options' noise: %v)", bucket, k, v, wv, ok))
			}
		}
		for k := range w {
			if _, ok := g[k]; !ok {
				d = append(d, fmt.Sprintf("%s %q is gone", bucket, k))
			}
		}
	}
	if !reflect.DeepEqual(got.URL, want.URL) {
		d = append(d, fmt.Sprintf("url noise %q, want %q", got.URL, want.URL))
	}
	sort.Strings(d)
	return strings.Join(d, "; ")
}
