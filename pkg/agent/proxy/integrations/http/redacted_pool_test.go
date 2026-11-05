package http

import (
	"context"
	"encoding/json"
	"fmt"
	"maps"
	"net/http"
	"reflect"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
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
	if strings.Contains(persisted(t, r), secret) {
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
// patterns. A recording with no patterns is not read: it redacted nothing.
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
			// value among other text (one pattern, read within both and
			// compiled once for both).
			pool = append(pool, recorded("plain", "/x/q9P/y", jsonHdr, `{"a":"alpha1"}`))
			shared := fmt.Sprintf("REDACTED_shared%s%d", mode, run)
			for _, name := range []string{"s1", "s2"} {
				m := recorded(name, "/x/"+shared+"/"+name, jsonHdr, `{"a":"id `+shared+`"}`)
				m.Noise = []string{"^" + shared + "$"}
				pool = append(pool, m)
			}
			core, logs := observer.New(zap.DebugLevel)
			h := &HTTP{Logger: zap.New(core)}
			reads, compiles := placesReads.Load(), util.UnanchoredCompiles()
			for c := 0; c < calls; c++ {
				r := poolMiss(t, h, mode, "/x/"+liveSecret+"/z", jsonHdr, `{"a":"bravo2"}`, pool...)
				carriesNo(t, r, liveSecret)
			}
			if mode == "off" && logs.FilterMessage("The url path of mock and request aren't the same").Len() == 0 {
				t.Fatal("the row is of calls logged at debug: no path line was written")
			}
			if got := placesReads.Load() - reads; got != n+2 {
				t.Errorf("%d calls at a pool of %d recordings with patterns read %d recordings for their places, want %d", calls, n+2, got, n+2)
			}
			if got := util.UnanchoredCompiles() - compiles; got != n+1 {
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
			// A copy of a recording, as a test's pool holds, is not read
			// again: its places are its content's.
			reads = placesReads.Load()
			if copied := placesOf(pool[2].DeepCopy()); !copied.segments[1] {
				t.Error("the copy's places are not the recording's")
			}
			if got := placesReads.Load() - reads; got != 0 {
				t.Errorf("a copy of a recording read was read again (%d reads)", got)
			}
		})
	}
}

// The agent stages each test's pool from copies of the recordings
// (pkg.FilterConfigMocksMapping, as mocksForParams stages it): a recording
// is read for its places, and each pattern compiled, once a run, however
// many tests' pools hold a copy of it, and however many calls of each test
// miss or are logged at debug.
func TestATestsPoolOfCopiesIsReadOnceARun(t *testing.T) {
	const n, tests, calls = 30, 2, 2
	jsonHdr := map[string]string{"Content-Type": "application/json"}
	for _, mode := range []string{"off", "on"} {
		t.Run(mode, func(t *testing.T) {
			run := costRun.Add(1)
			stored := make([]*models.Mock, 0, n+1)
			for i := 0; i < n; i++ {
				placeholder := fmt.Sprintf("REDACTED_staged%s%d_%d", mode, run, i)
				m := recorded(fmt.Sprintf("m%d", i), fmt.Sprintf("/x/%s/y%d", placeholder, i), jsonHdr, fmt.Sprintf(`{"a":"alpha1","e":"%s"}`, placeholder))
				m.Noise = []string{"^" + placeholder + "$"}
				stored = append(stored, m)
			}
			stored = append(stored, recorded("plain", "/x/q9P/y", jsonHdr, `{"a":"alpha1"}`))
			core, logs := observer.New(zap.DebugLevel)
			h := &HTTP{Logger: zap.New(core)}
			reads, compiles := placesReads.Load(), util.UnanchoredCompiles()
			for test := 0; test < tests; test++ {
				pool := pkg.FilterConfigMocksMapping(context.Background(), zap.NewNop(), stored, nil)
				if len(pool) != len(stored) || slices.Contains(stored, pool[0]) {
					t.Fatal("the row is of a test's pool of copies of every recording, as the agent stages one")
				}
				for c := 0; c < calls; c++ {
					carriesNo(t, poolMiss(t, h, mode, "/x/"+liveSecret+"/z", jsonHdr, `{"a":"bravo2"}`, pool...), liveSecret)
				}
			}
			if mode == "off" && logs.FilterMessage("The url path of mock and request aren't the same").Len() == 0 {
				t.Fatal("the row is of calls logged at debug: no path line was written")
			}
			if got := placesReads.Load() - reads; got != n {
				t.Errorf("%d tests' pools of copies of %d recordings with patterns read %d recordings for their places, want %d", tests, n, got, n)
			}
			if got := util.UnanchoredCompiles() - compiles; got != n {
				t.Errorf("%d tests' pools of copies of recordings of %d patterns compiled %d patterns, want %d", tests, n, got, n)
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

// ownPool is a closest recording that redacted a body field, beside
// recordings that redacted a header, a query value, a path segment of the
// call's shape and another body field.
func ownPool() (closest *models.Mock, pool []*models.Mock) {
	jsonHdr := map[string]string{"Content-Type": "application/json"}
	closest = recorded("orders", "/orders/42", jsonHdr, `{"item":"alpha1","note":"REDACTED_own"}`)
	closest.Noise = []string{`^REDACTED_own$`}
	hdr := recorded("hdr", "/a", map[string]string{"Content-Type": "application/json", "X-Sig": "REDACTED_h"}, `{"z":"zeta"}`)
	hdr.Noise = []string{`^REDACTED_h$`}
	qry := recorded("qry", "/b?tok=REDACTED_q", jsonHdr, `{"z":"zeta"}`)
	qry.Noise = []string{`^REDACTED_q$`}
	seg := recorded("seg", "/c/REDACTED_s", jsonHdr, `{"z":"zeta"}`)
	seg.Noise = []string{`^REDACTED_s$`}
	fld := recorded("fld", "/d", jsonHdr, `{"pw":"REDACTED_f"}`)
	fld.Noise = []string{`^REDACTED_f$`}
	return closest, []*models.Mock{closest, hdr, qry, seg, fld}
}

// ownPlacesAsRecorded fails t unless closest's own places (ownPool) are as
// it redacted them: a report adds other recordings' places to a copy.
func ownPlacesAsRecorded(t *testing.T, closest *models.Mock) {
	t.Helper()
	own := placesOf(closest)
	if len(own.headers) != 0 || len(own.query) != 0 || len(own.segments) != 0 ||
		!maps.Equal(own.fields, map[string]bool{"note": true}) || !maps.Equal(own.keys, map[string]bool{"note": true}) {
		t.Errorf("the report added other recordings' places to the closest's own: headers %v query %v segments %v fields %v keys %v",
			own.headers, own.query, own.segments, own.fields, own.keys)
	}
}

// A miss report adds the pool's places to a copy of the closest
// recording's: its own places, kept for the next report, are as it
// redacted them, each of the five kinds.
func TestAMissLeavesTheClosestRecordingsOwnPlacesAsTheyWere(t *testing.T) {
	jsonHdr := map[string]string{"Content-Type": "application/json", "X-Sig": "sig9Live"}
	body := `{"item":"bravo2","note":"` + liveSecret + `","pw":"pw9Live"}`
	for _, mode := range []string{"off", "on"} {
		t.Run(mode, func(t *testing.T) {
			closest, pool := ownPool()
			r := poolMiss(t, newHTTP(), mode, "/orders/43?tok=tok9Live", jsonHdr, body, pool...)
			if r.ClosestMock != "orders" {
				t.Fatalf("the row is of a report at the closest recording orders, not %q", r.ClosestMock)
			}
			for _, secret := range []string{liveSecret, "sig9Live", "tok9Live", "pw9Live", "/43"} {
				carriesNo(t, r, secret)
			}
			ownPlacesAsRecorded(t, closest)
		})
	}
}

// Misses at one pool at once (run under -race) leave the closest
// recording's own places as it redacted them, as one by one
// (TestAMissLeavesTheClosestRecordingsOwnPlacesAsTheyWere).
func TestMissesAtOnceLeaveTheClosestRecordingsOwnPlacesAsTheyWere(t *testing.T) {
	jsonHdr := map[string]string{"Content-Type": "application/json", "X-Sig": "sig9Live"}
	body := `{"item":"bravo2","note":"` + liveSecret + `","pw":"pw9Live"}`
	for _, mode := range []string{"off", "on"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv(provenswitch.Env, map[string]string{"on": "1"}[mode])
			closest, pool := ownPool()
			var wg sync.WaitGroup
			for g := 0; g < 8; g++ {
				wg.Add(1)
				go func() {
					defer wg.Done()
					for c := 0; c < 10; c++ {
						in := sent("/orders/43?tok=tok9Live", jsonHdr, body)
						request, _ := http.NewRequest("POST", "http://api.example/orders/43?tok=tok9Live", strings.NewReader(body))
						request.Header = in.header.Clone()
						_, _, diag, err := newHTTP().match(context.Background(), in, &mockMemDb{mocks: pool, updateUnFilteredReturn: true}, nil, nil, nil, true, false, true, true, true)
						if err != nil {
							t.Error(err)
							return
						}
						r := newHTTP().buildHTTPMismatchReport(request, []byte(body), &mockMemDb{mocks: pool}, nil, nil, nil, diag)
						if r == nil || r.ClosestMock != "orders" {
							t.Error("the row is of reports at the closest recording orders")
							return
						}
						if strings.Contains(persisted(t, r), liveSecret) {
							t.Errorf("the report carries %q: %s", liveSecret, r.ActualSummary)
						}
					}
				}()
			}
			wg.Wait()
			ownPlacesAsRecorded(t, closest)
		})
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
			{"a-shorter-shape", "/REDACTED_abc", "POST /users/43", "/users/43"},
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

// The closest recording's own places count whatever its shape: the call's
// path is compared with its path, position by position, so the secret it
// redacted at a position is the call's at that position, though the call
// has a segment more or less.
func TestTheClosestRecordingsOwnSegmentCountsWhateverItsShape(t *testing.T) {
	jsonHdr := map[string]string{"Content-Type": "application/json"}
	for _, mode := range []string{"off", "on"} {
		t.Run(mode, func(t *testing.T) {
			m := recorded("orders", "/orders/REDACTED_abc/items", jsonHdr, `{"x":"alpha1"}`)
			m.Noise = []string{`^REDACTED_abc$`}
			r := poolMiss(t, newHTTP(), mode, "/orders/"+liveSecret, jsonHdr, `{"x":"bravo2"}`, m)
			carriesNo(t, r, liveSecret)
			if r.ActualSummary != "POST /orders/"+valueRedacted {
				t.Errorf("summary %q, want the call's path with its second segment redacted", r.ActualSummary)
			}
		})
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

// A row that looks for a live value in a report reads all the report
// persists but when the call missed (persisted): a report whose time spells
// the number row's live value carries none, and one that holds the value in
// any text field, or in a field diff, carries it.
func TestARowReadsAllAReportPersistsButWhenTheCallMissed(t *testing.T) {
	// 15:38:29.918223411 spells 9182, as the time of about one report of
	// the number row's in 2,000 does.
	at := time.Date(2026, 11, 5, 15, 38, 29, 918223411, time.UTC)
	base := models.MockMismatchReport{At: at, Protocol: "HTTP", ActualSummary: "POST /x"}
	if all, _ := json.Marshal(&base); !strings.Contains(string(all), "9182") {
		t.Fatalf("the row is of a report whose JSON spells 9182 in its time: %s", all)
	}
	if s := persisted(t, &base); strings.Contains(s, "9182") {
		t.Errorf("a row reads when the call missed: %s", s)
	}
	read := 0
	v := reflect.ValueOf(base)
	for i := 0; i < v.NumField(); i++ {
		c := base
		f := reflect.ValueOf(&c).Elem().Field(i)
		switch {
		case f.Kind() == reflect.String:
			f.SetString("POST /x/9182")
		case f.Kind() == reflect.Slice && f.Type().Elem().Kind() == reflect.String:
			f.Set(reflect.ValueOf([]string{"body.pin=9182"}).Convert(f.Type()))
		case f.Type() == reflect.TypeOf([]models.MockFieldDiff(nil)):
			f.Set(reflect.ValueOf([]models.MockFieldDiff{{Path: "body.pin", Actual: "9182"}}))
		default:
			continue
		}
		read++
		if !strings.Contains(persisted(t, &c), "9182") {
			t.Errorf("a row does not read the report's %s", v.Type().Field(i).Name)
		}
	}
	if read < 8 {
		t.Fatalf("the row read %d text fields of a report: it is of a report's text fields", read)
	}
}

// The places kept are bounded by the bytes they hold: past the bound, kept
// places are dropped until new ones fit; places larger than the bound are
// not kept; places kept again under their key are counted once.
func TestTheKeptPlacesAreBounded(t *testing.T) {
	k := keptPlaces{byKey: map[placesDigest]keptEntry{}, max: 1000}
	key := func(i int) placesDigest { return placesDigest{uint64(i), 7} }
	for i := 0; i < 50; i++ {
		k.put(key(i), redactedPlaces{}, 300)
		if k.bytes > k.max || len(k.byKey) > 3 {
			t.Fatalf("after %d places kept: %d bytes in %d, more than the bound %d", i+1, k.bytes, len(k.byKey), k.max)
		}
		if _, ok := k.get(key(i)); !ok {
			t.Fatalf("the places just kept (%d) are not kept", i)
		}
	}
	k.put(key(100), redactedPlaces{}, 1001)
	if _, ok := k.get(key(100)); ok || k.bytes > k.max {
		t.Errorf("places larger than the bound were kept: %d bytes", k.bytes)
	}
	k.put(key(101), redactedPlaces{}, 1000)
	if _, ok := k.get(key(101)); !ok || k.bytes != 1000 {
		t.Errorf("places as large as the bound were not kept alone: %d bytes", k.bytes)
	}
	k = keptPlaces{byKey: map[placesDigest]keptEntry{}, max: 1000}
	k.put(key(1), redactedPlaces{}, 300)
	k.put(key(1), redactedPlaces{}, 400)
	k.put(key(2), redactedPlaces{}, 600)
	if k.bytes != 1000 || len(k.byKey) != 2 {
		t.Errorf("places kept again under their key: %d bytes in %d, want 1000 in 2", k.bytes, len(k.byKey))
	}
	// Places are sized by what they hold: each name at least its bytes,
	// and places that hold nothing cost their entry.
	var names []string
	fields := map[string]string{}
	for i := 0; i < 100; i++ {
		names = append(names, fmt.Sprintf("%s%016d", strings.Repeat("a_field_name_", 14), i))
		fields[names[i]] = "REDACTED_abc"
	}
	body, _ := json.Marshal(fields)
	m := recorded("m", "/x", map[string]string{"Content-Type": "application/json"}, string(body))
	m.Noise = []string{`^REDACTED_abc$`}
	p := placesOf(m)
	if len(p.fields) != 100 {
		t.Fatalf("the row is of places that hold 100 fields, not %d", len(p.fields))
	}
	if got, least := p.size(m.Noise), 2*len(strings.Join(names, "")); got < least {
		t.Errorf("places holding %d bytes of names (as fields and keys) are sized %d", least, got)
	}
	if (redactedPlaces{}).size(nil) <= 0 {
		t.Error("places that hold nothing cost nothing kept")
	}
}

// A recording's places are kept by all they are read from: a recording
// that differs from one read only in its URL, its query parameters, a
// header, its body or its patterns is read for its own places.
func TestARecordingsPlacesAreKeptByAllTheyAreReadFrom(t *testing.T) {
	run := costRun.Add(1)
	secret := fmt.Sprintf("REDACTED_k%d", run)
	base := func() *models.Mock {
		m := recorded("m", "/x/a?q=1", map[string]string{"Content-Type": "application/json", "X-A": "1"}, `{"f":"1"}`)
		m.Spec.HTTPReq.URLParams = map[string]string{"q": "1"}
		m.Noise = []string{"^" + secret + "$"}
		return m
	}
	if p := placesOf(base()); len(p.segments)+len(p.query)+len(p.headers)+len(p.fields) != 0 || p.body {
		t.Fatalf("the row is of a recording that redacted nothing: %+v", p)
	}
	twin := base()
	twin.Spec.HTTPReq.Header["X-A"] = "1" + secret
	if p := placesOf(twin); len(p.headers) != 0 {
		t.Fatalf("the row is of a twin that redacted nothing: %+v", p)
	}
	for _, c := range []struct {
		name   string
		change func(m *models.Mock)
		placed func(p redactedPlaces) bool
	}{
		{"url", func(m *models.Mock) { m.Spec.HTTPReq.URL = "/x/" + secret + "?q=1" }, func(p redactedPlaces) bool { return p.segments[1] }},
		{"query-parameter", func(m *models.Mock) { m.Spec.HTTPReq.URLParams["q"] = secret }, func(p redactedPlaces) bool { return p.query["q"] }},
		{"header", func(m *models.Mock) { m.Spec.HTTPReq.Header["X-A"] = secret }, func(p redactedPlaces) bool { return p.headers["x-a"] }},
		{"body", func(m *models.Mock) { m.Spec.HTTPReq.Body = `{"f":"` + secret + `"}` }, func(p redactedPlaces) bool { return p.fields["f"] }},
		{"patterns", func(m *models.Mock) { m.Noise = []string{"^1$"} }, func(p redactedPlaces) bool { return p.headers["x-a"] && p.query["q"] }},
		// After the twin's: X-A1 and secret, written with no lengths,
		// read as the twin's X-A and "1"+secret.
		{"a-boundary", func(m *models.Mock) {
			delete(m.Spec.HTTPReq.Header, "X-A")
			m.Spec.HTTPReq.Header["X-A1"] = secret
		}, func(p redactedPlaces) bool { return p.headers["x-a1"] }},
	} {
		t.Run(c.name, func(t *testing.T) {
			m := base()
			c.change(m)
			if p := placesOf(m); !c.placed(p) {
				t.Errorf("the recording was given the places of one it differs from: %+v", p)
			}
		})
	}
}
