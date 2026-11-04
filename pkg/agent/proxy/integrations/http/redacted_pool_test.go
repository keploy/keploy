package http

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"weak"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// poolMiss is the miss report of the live POST path with body at pool,
// matched with the switch off (mode "off", by h, which logs at debug) or on
// ("on").
func poolMiss(t *testing.T, h *HTTP, mode, path string, hdr map[string]string, body string, pool ...*models.Mock) *models.MockMismatchReport {
	t.Helper()
	t.Setenv(provenswitch.Env, map[string]string{"on": "1"}[mode])
	in := sent(path, hdr, body)
	request, _ := http.NewRequest("POST", "http://api.example"+path, strings.NewReader(body))
	request.Header = in.header.Clone()
	var diag *matchDiag
	if mode == "on" {
		got, d := strictMatch(t, in, nil, nil, nil, pool...)
		if got != nil {
			t.Fatalf("served the call with %q", got.Name)
		}
		diag = d
	} else {
		ok, _, d, err := h.match(context.Background(), in, &mockMemDb{mocks: pool, updateUnFilteredReturn: true}, nil, nil, nil, true, false, true, true, true)
		if err != nil {
			t.Fatal(err)
		}
		if ok {
			t.Fatal("served the call")
		}
		diag = d
	}
	r := h.buildHTTPMismatchReport(request, []byte(body), &mockMemDb{mocks: pool}, nil, nil, nil, diag)
	if r == nil {
		t.Fatal("no report")
	}
	for _, d := range r.FieldDiffs {
		t.Logf("diff %s %s exp=%q act=%q", d.Path, d.Kind, d.Expected, d.Actual)
	}
	return r
}

// carriesNo fails t when r carries secret anywhere it persists.
func carriesNo(t *testing.T, r *models.MockMismatchReport, secret string) {
	t.Helper()
	all, err := json.Marshal(r)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(all), secret) {
		t.Errorf("the report carries %q:\nsummary: %s\nDiff: %s\nReceivedReq:\n%s", secret, r.ActualSummary, r.Diff, r.ReceivedReq)
	}
}

// pathDiff is r's "path" field diff; nil when it has none.
func pathDiff(r *models.MockMismatchReport) *models.MockFieldDiff {
	for i := range r.FieldDiffs {
		if r.FieldDiffs[i].Path == "path" {
			return &r.FieldDiffs[i]
		}
	}
	return nil
}

var costRun atomic.Int64

// A pool's recordings are read for their places once each, and each of
// their patterns is compiled once, however many calls miss at the pool or
// are logged at debug: a recording's places are a function of its recorded
// request and its patterns, read again only for another request or other
// patterns.
func TestAPoolIsReadForItsPlacesOnceARecording(t *testing.T) {
	const n, calls = 40, 5
	jsonHdr := map[string]string{"Content-Type": "application/json"}
	for _, mode := range []string{"off", "on"} {
		t.Run(mode, func(t *testing.T) {
			// Patterns no other row, and no earlier run of this one, wrote.
			run := costRun.Add(1)
			pool := make([]*models.Mock, n, n+3)
			for i := range pool {
				placeholder := fmt.Sprintf("REDACTED_cost%s%d_%d", mode, run, i)
				pool[i] = recorded(fmt.Sprintf("m%d", i), fmt.Sprintf("/x/%s/y%d", placeholder, i), jsonHdr, fmt.Sprintf(`{"a":"alpha1","b":["c","d",{"e":"%s"}],"n":%d}`, placeholder, i))
				pool[i].Noise = []string{"^" + placeholder + "$"}
			}
			// And one that redacted nothing, and two that redacted the same
			// value (one pattern, compiled once for both).
			pool = append(pool, recorded("plain", "/x/q9P/y", jsonHdr, `{"a":"alpha1"}`))
			shared := fmt.Sprintf("REDACTED_shared%s%d", mode, run)
			for _, name := range []string{"s1", "s2"} {
				m := recorded(name, "/x/"+shared+"/"+name, jsonHdr, `{"a":"`+shared+`"}`)
				m.Noise = []string{"^" + shared + "$"}
				pool = append(pool, m)
			}
			core, logs := observer.New(zap.DebugLevel)
			h := &HTTP{Logger: zap.New(core)}
			reads, compiles := placesReads.Load(), anchorFreeCompiles.Load()
			for c := 0; c < calls; c++ {
				r := poolMiss(t, h, mode, "/x/"+liveSecret+"/z", jsonHdr, `{"a":"bravo2"}`, pool...)
				carriesNo(t, r, liveSecret)
			}
			if mode == "off" && logs.FilterMessage("The url path of mock and request aren't the same").Len() == 0 {
				t.Fatal("the row is of calls logged at debug: no path line was written")
			}
			if got := placesReads.Load() - reads; got != n+3 {
				t.Errorf("%d calls at a pool of %d recordings read %d recordings for their places, want %d", calls, n+3, got, n+3)
			}
			if got := anchorFreeCompiles.Load() - compiles; got != n+1 {
				t.Errorf("%d calls at a pool of recordings of %d patterns compiled %d patterns, want %d", calls, n+1, got, n+1)
			}
			// A recording given another request, or other patterns as many,
			// is read again, once.
			placeholder := strings.TrimSuffix(strings.TrimPrefix(pool[0].Noise[0], "^"), "$")
			pool[0].Spec.HTTPReq = &models.HTTPReq{Method: "POST", URL: "/x/y/z/" + placeholder, Header: map[string]string{"Host": "api.example"}}
			reads = placesReads.Load()
			if read, again := placesOf(pool[0]), placesOf(pool[0]); !read.segments[3] || !again.segments[3] {
				t.Error("the places of the recording's earlier request were kept")
			}
			pool[1].Noise = []string{`^REDACTED_abc$`}
			pool[1].Spec.HTTPReq.URL = "/x/REDACTED_abc/y"
			if read, again := placesOf(pool[1]), placesOf(pool[1]); !read.segments[1] || !again.segments[1] {
				t.Error("the places of the recording's earlier patterns were kept")
			}
			if got := placesReads.Load() - reads; got != 2 {
				t.Errorf("two recordings given another request or other patterns were read %d times, want 2", got)
			}
		})
	}
}

// A report redacted by another recording's places reads the closest
// recording's path and the call's body as that recording's, whatever the
// closest's patterns: what no recording redacted still shows, and the path
// diff names the recorded path.
func TestAPoolRedactedReportShowsWhatNoRecordingRedacted(t *testing.T) {
	for _, mode := range []string{"off", "on"} {
		for _, closest := range []string{"redacted-nothing", "redacted-a-header"} {
			t.Run(mode+"/"+closest, func(t *testing.T) {
				jsonHdr := map[string]string{"Content-Type": "application/json"}
				login := recorded("login", "/login", jsonHdr, `{"assertion":"REDACTED_abc","user":"u1"}`)
				login.Noise = []string{`^REDACTED_abc$`}
				orders := recorded("orders", "/orders/42", jsonHdr, `{"item":"alpha1","qty":"one"}`)
				hdr := map[string]string{"Content-Type": "application/json"}
				if closest == "redacted-a-header" {
					orders.Spec.HTTPReq.Header["X-Sig"] = "REDACTED_zzz"
					orders.Noise = []string{`^REDACTED_zzz$`}
					hdr["X-Sig"] = "sig9Live"
				}
				r := poolMiss(t, newHTTP(), mode, "/orders/43", hdr, `{"item":"bravo2","assertion":"`+liveSecret+`","qty":"one"}`, orders, login)
				if r.ClosestMock != "orders" {
					t.Fatalf("the row is of a report at the closest recording orders, not %q", r.ClosestMock)
				}
				carriesNo(t, r, liveSecret)
				if !strings.Contains(r.ReceivedReq, "bravo2") {
					t.Errorf("ReceivedReq lost what no recording redacted:\n%s", r.ReceivedReq)
				}
				if d := pathDiff(r); d == nil || d.Expected != "/orders/42" || d.Actual != "/orders/43" {
					t.Errorf("the path diff %+v does not name the recorded path and the call's", d)
				}
				if r.ActualSummary != "POST /orders/43" {
					t.Errorf("summary %q, want the call's path, which no recording redacted", r.ActualSummary)
				}
				// The closest recording's own places are as it redacted them:
				// the report's are a copy.
				if own := placesOf(orders); len(own.fields) != 0 || len(own.keys) != 0 {
					t.Errorf("the report added other recordings' places to the closest's own: %v %v", own.fields, own.keys)
				}
			})
		}
	}
}

// A path segment's place is taken from a recording of the pool whose path
// has as many segments as the call's: a path of another shape says nothing
// of the call's segment i. One of the same shape redacts it, though it is
// another route's: the cost of failing closed.
func TestAPathOfAnotherShapeRedactsNoSegmentOfTheCall(t *testing.T) {
	jsonHdr := map[string]string{"Content-Type": "application/json"}
	for _, mode := range []string{"off", "on"} {
		for _, c := range []struct{ shape, reset, summary, path string }{
			{"another-shape", "/reset/REDACTED_abc/confirm", "POST /users/43", "/users/43"},
			{"the-same-shape", "/reset/REDACTED_abc", "POST /users/" + valueRedacted, "/users/" + valueRedacted},
		} {
			t.Run(c.shape+"/"+mode, func(t *testing.T) {
				reset := recorded("reset", c.reset, jsonHdr, `{"x":"alpha1"}`)
				reset.Noise = []string{`^REDACTED_abc$`}
				users := recorded("users", "/users/42", jsonHdr, `{"x":"alpha1"}`)
				r := poolMiss(t, newHTTP(), mode, "/users/43", jsonHdr, `{"x":"bravo2"}`, users, reset)
				if r.ClosestMock != "users" {
					t.Fatalf("the row is of a report at the closest recording users, not %q", r.ClosestMock)
				}
				if r.ActualSummary != c.summary {
					t.Errorf("summary %q, want %q", r.ActualSummary, c.summary)
				}
				if d := pathDiff(r); d == nil || d.Actual != c.path {
					t.Errorf("the path diff %+v does not name the call's path %q", d, c.path)
				}
			})
		}
	}
}

// What the recording kept still shows: a root string body's diff (named
// "body" by the strict comparison, "body." by the matcher), and a number whose literal holds a pattern's value but is not
// it (the patterns name a number's whole literal).
func TestAReportShowsARootStringAndANumberTheRecordingKept(t *testing.T) {
	for _, mode := range []string{"off", "on"} {
		t.Run("root-string/"+mode, func(t *testing.T) {
			m := recorded("m", "/x", map[string]string{"Content-Type": "application/json", "X-Sig": "REDACTED_abc"}, `"alpha1"`)
			m.Noise = []string{`^REDACTED_abc$`}
			r := poolMiss(t, newHTTP(), mode, "/x", map[string]string{"Content-Type": "application/json", "X-Sig": liveSecret}, `"bravo2"`, m)
			carriesNo(t, r, liveSecret)
			shown := false
			for _, d := range r.FieldDiffs {
				shown = shown || (d.Path == "body" || d.Path == "body.") && strings.Contains(d.Actual, "bravo2") && strings.Contains(d.Expected, "alpha1")
			}
			if !shown || !strings.Contains(r.Diff, "bravo2") {
				t.Errorf("the body diff does not show the root string the recording kept: Diff %q", r.Diff)
			}
		})
		t.Run("number/"+mode, func(t *testing.T) {
			jsonHdr := map[string]string{"Content-Type": "application/json"}
			m := recorded("m", "/x", jsonHdr, `{"pin":7,"qty":17,"x":"alpha1"}`)
			m.Noise = []string{`^7$`}
			r := poolMiss(t, newHTTP(), mode, "/x", jsonHdr, `{"pin":9182,"qty":17,"x":"bravo2"}`, m)
			carriesNo(t, r, "9182")
			if !strings.Contains(r.ReceivedReq, `"qty": 17`) || !strings.Contains(r.ReceivedReq, "bravo2") {
				t.Errorf("ReceivedReq does not show the number the recording kept:\n%s", r.ReceivedReq)
			}
		})
	}
}

// A recording's places go with it: the memo keeps no recording that is
// gone. Waited for by GC cycles, not by time.
func TestARecordingsPlacesGoWithIt(t *testing.T) {
	key := func() weak.Pointer[models.Mock] {
		m := recorded("gone", "/x/REDACTED_abc/y", map[string]string{"Content-Type": "application/json"}, `{}`)
		m.Noise = []string{`^REDACTED_abc$`}
		_ = placesOf(m)
		return weak.Make(m)
	}()
	if _, ok := placesMemo.Load(key); !ok {
		t.Fatal("the row is of a recording whose places were kept")
	}
	for i := 0; i < 200; i++ {
		runtime.GC()
		if _, ok := placesMemo.Load(key); !ok {
			return
		}
		runtime.Gosched()
	}
	t.Error("the places of a recording that is gone are still kept after 200 GC cycles")
}

// The compiled patterns are bounded: a full cache is emptied.
func TestTheAnchorFreePatternsAreBounded(t *testing.T) {
	for i := 0; i <= maxAnchorFree; i++ {
		_ = anchorFree(fmt.Sprintf("^bounded%d_%d$", costRun.Add(1), i))
		anchorFreeMu.RLock()
		n := len(anchorFreeCache)
		anchorFreeMu.RUnlock()
		if n > maxAnchorFree {
			t.Fatalf("%d patterns compiled are kept, more than %d", n, maxAnchorFree)
		}
	}
}
