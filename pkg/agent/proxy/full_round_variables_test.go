package proxy

import (
	"context"
	"fmt"
	"net/http"
	"strconv"
	"strings"
	"testing"
	"time"

	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Under the proven switch the open test test-0 recorded GET /items/a1 once,
// and the set's background (the worker scope under today's rules, the pool
// under the background rules) holds a recording of GET /items/r1 that binds
// its path's second segment as a variable, so it fits GET /items/a1 too. A
// test-0 that sends GET /items/a1 twice makes one call more than it
// recorded with the second. A replay's matcher reads the store with the
// open test's own-call decision (integrations.OwnRepeatRead), so the
// background never answers it; record's full round decides a call that a
// recording that declares variables fits by the same variable pass, through
// the same read: in both rounds the second call misses as one call more
// than the test recorded, recorded once, and the background's recording is
// left unspent, its variable unbound: it answers the call it is the
// recording of, GET /items/r1, which the test never recorded. Answered by
// it in the full round, a test whose app made its recorded call twice (a
// double submit, a client retry) passed the full round and failed the
// replays after it. The own-call decision compares the call with the
// test's recording under the noise the matcher compares it under, so the
// second call is one call more than the test recorded all the same when it
// differs from the recording only there: a traced app's new traceparent on
// every call (a flaky header keploy excuses by itself), a new nonce in a
// POST's body under keploy.yml's request body noise, a new day in the
// path's last segment under keploy.yml's URL noise.
func TestTheFullRoundsVariablePassNeverAnswersACallOneMoreThanTheTestRecorded(t *testing.T) {
	// call is the app's request of /items/<item> in variant v, and its
	// summary as a miss reports it: each call carries a new traceparent when
	// traced, a new nonce in its body under body noise, a new day in its
	// path under URL noise.
	n := 0
	days := []string{"mon", "tue", "wed", "thu", "fri", "sat"}
	call := func(v, item string) (summary, raw string) {
		n++
		target := "/items/" + item
		switch v {
		case "traced":
			return "GET " + target, strings.Replace(httpGet(target), "\r\n\r\n", fmt.Sprintf("\r\nTraceparent: 00-%032d-%016d-01\r\n\r\n", n, n), 1)
		case "body noise":
			body := fmt.Sprintf(`{"nonce":"n-%d"}`, n)
			return "POST " + target, "POST " + target + " HTTP/1.1\r\nHost: deps.example\r\nContent-Type: application/json\r\nContent-Length: " +
				strconv.Itoa(len(body)) + "\r\n\r\n" + body
		case "URL noise":
			target += "/day-" + days[n%len(days)]
		}
		return "GET " + target, httpGet(target)
	}
	// recording is variant v's recording of /items/<item>: the one the
	// variant's calls differ from only at its noised place, a day the calls
	// never send (sun) under URL noise.
	recording := func(v, name string, at time.Time, item, resp string) *models.Mock {
		switch v {
		case "body noise":
			return r3Mock(name, at, "POST", "/items/"+item, `{"nonce":"n-`+item+`"}`, resp)
		case "URL noise":
			return r3Mock(name, at, "GET", "/items/"+item+"/day-sun", "", resp)
		}
		m := r3Mock(name, at, "GET", "/items/"+item, "", resp)
		if v == "traced" {
			trace, span := "a", "b"
			if item != "a1" {
				trace, span = "c", "d"
			}
			m.Spec.HTTPReq.Header["Traceparent"] = "00-" + strings.Repeat(trace, 32) + "-" + strings.Repeat(span, 16) + "-01"
		}
		return m
	}
	noise := map[string]map[string]map[string][]string{
		"body noise": {"requestbody": {"nonce": {}}},
		"URL noise":  {"url": {"/day-[a-z]+": {}}},
	}
	for _, rules := range []string{"worker scope", "background rules"} {
		for _, round := range []string{"replay", "full round"} {
			for _, v := range []string{"plain", "traced", "body noise", "URL noise"} {
				t.Run(rules+"/"+round+"/"+v, func(t *testing.T) {
					t.Setenv(provenswitch.Env, "1")
					p := startedDrainProxy(t, 100)
					p.learnTracker = learn.NewTracker()
					start := time.Now().Add(time.Second)
					own := recording(v, "mock-own-0", start.Add(100*time.Millisecond), "a1", `{"own":true}`)
					bg := recording(v, "mock-bg", start.Add(-500*time.Millisecond), "r1", `{"bg":true}`)
					bg.Spec.Variables = models.MockVariables{"id": {Recorded: "r1", Bind: "url.path[1]"}}
					mm := NewMockManager(nil, nil, zap.NewNop())
					t.Cleanup(mm.Close)
					mm.SeedRecordedWindows([]models.TestWindow{{TestCase: "test-0", Start: start, End: start.Add(900 * time.Millisecond)}})
					mm.SetMocksWithWindow(nil, []*models.Mock{bg, own}, models.BaseTime, start.Add(time.Hour))
					if rules == "background rules" {
						mm.ServeExactBackground(models.RecordedWithS12Routing, []string{bg.Name}, nil)
						mm.ServeExactWorker(nil)
					} else {
						mm.ServeExactWorker([]string{bg.Name})
					}
					p.setMockManager(mm)
					t.Cleanup(func() { p.setMockManager(nil) })
					mm.ServeExactPool(models.PoolKeyNone, nil)

					h := httpint.New(zap.NewNop())
					opts := models.OutgoingOptions{Learn: round == "full round", NoiseConfig: noise[v]}
					mm.ServeExactTest("test-0", []*models.Mock{roundMock(mm, own.Name)})
					p.ContinueTestErrorCapture()
					first, raw := call(v, "a1")
					if code, body, err := callWithinOpts(h, p, mm, opts, raw, true); err != nil || code != http.StatusOK || body != `{"own":true}` {
						t.Fatalf("test-0's %s got %d %s %v; want its own", first, code, body, err)
					}
					second, raw := call(v, "a1")
					code, body, err := callWithinOpts(h, p, mm, opts, raw, true)
					if err == nil && code == http.StatusOK {
						t.Fatalf("test-0's second call %s, one more than it recorded, was answered %s by the background's recording that declares a variable", second, body)
					}
					misses, err := p.GetMockErrors(context.Background())
					if err != nil {
						t.Fatal(err)
					}
					if len(misses) != 1 || misses[0].ActualSummary != second || misses[0].Kind != models.MissExtraCall || misses[0].Recorded != 1 {
						t.Fatalf("misses %+v; want one extra_call miss of %s, recorded 1", misses, second)
					}

					never, raw := call(v, "r1")
					if code, body, err := callWithinOpts(h, p, mm, opts, raw, true); err != nil || code != http.StatusOK || body != `{"bg":true}` {
						t.Fatalf("test-0's %s, a call it never recorded, got %d %s %v; want the background's recording, unspent and its variable unbound", never, code, body, err)
					}
				})
			}
		}
	}
}
