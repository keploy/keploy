package http

import (
	"context"
	"fmt"
	"math/rand"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// learnBodies are request bodies, each of its content type, that the random
// learned rows draw their recordings' and their calls' bodies from: JSON
// documents the learn step holds the same in other bytes, documents of other
// values, documents whose keys hold a '.' or a '[' and so share their
// leaves' paths with documents of other members, forms in another order,
// multipart bodies of other boundaries, text, and none.
var learnBodies = []struct{ ct, body string }{
	{"application/json", ``},
	{"application/json", `{}`},
	{"application/json", `{"a":1}`},
	{"application/json", ` { "a" : 1 } `},
	{"application/json", `{"a":1.0}`},
	{"application/json", `{"a":2}`},
	{"application/json", `{"a":"1"}`},
	{"application/json", `{"a.b":1}`},
	{"application/json", `{"a":{"b":1}}`},
	{"application/json", `{"x":{"a.b":1}}`},
	{"application/json", `{"x":{"a":{"b":1}}}`},
	{"application/json", `[{"a.b":1}]`},
	{"application/json", `[{"a":{"b":1}}]`},
	{"application/json", `{"a[0]":1}`},
	{"application/json", `{"a":[1]}`},
	{"application/json", `[1,2]`},
	{"application/json", `[2,1]`},
	{"application/json", `text`},
	{"application/x-www-form-urlencoded", `a=1&b=2`},
	{"application/x-www-form-urlencoded", `b=2&a=1`},
	{"multipart/form-data; boundary=XX", "--XX\r\nContent-Disposition: form-data; name=\"a\"\r\n\r\n1\r\n--XX--\r\n"},
	{"multipart/form-data; boundary=YY", "--YY\r\nContent-Disposition: form-data; name=\"a\"\r\n\r\n1\r\n--YY--\r\n"},
}

// learnBodyOf is a body of learnBodies and the header that names its content
// type.
func learnBodyOf(rng *rand.Rand) (string, map[string]string) {
	b := learnBodies[rng.Intn(len(learnBodies))]
	return b.body, map[string]string{"Content-Type": b.ct}
}

// learnCallOf is a random learned row's call: in most rounds of the method
// and path of one of its recordings mocks, and of its body in half of
// those, so that calls find their group, and of learnBodies' otherwise.
func learnCallOf(rng *rand.Rand, paths []string, mocks []*models.Mock) *req {
	method, path := []string{"GET", "POST"}[rng.Intn(2)], paths[rng.Intn(len(paths))]
	body, header := learnBodyOf(rng)
	if len(mocks) > 0 && rng.Intn(3) > 0 {
		r := mocks[rng.Intn(len(mocks))].Spec.HTTPReq
		method, path = string(r.Method), r.URL
		if rng.Intn(2) == 0 {
			body, header = r.Body, map[string]string{"Content-Type": r.Header["Content-Type"]}
		}
	}
	return live(method, path, body, header)
}

// learnBodyNoiseOf is keploy.yml's noise of request bodies a random learned
// row reads a round with: none in most rounds, a field of the documents in
// some.
func learnBodyNoiseOf(rng *rand.Rand) map[string][]string {
	if rng.Intn(4) == 0 {
		return map[string][]string{"a": {}}
	}
	return nil
}

// learnNarrows reports whether the learn step's index reads fewer of the
// recordings than a read for the live call lp: whether a recording of its
// method and path that compares its body plainly has another print
// (learnPlainPrint) than the call's (learnLivePrint), so that the row
// compares an indexed group the filing narrowed.
func learnNarrows(n learnNoise, lp httpParts, mocks []*models.Mock) bool {
	body, printed := n.learnLivePrint(lp)
	if !printed {
		return false
	}
	for _, m := range mocks {
		rp := recordedParts(m)
		if rp.method != lp.method || n.maskedPath(rp.path) != n.maskedPath(lp.path) || hasVariables(m) || learnsWide(m) {
			continue
		}
		if sum, ok := learnPlainPrint(m, rp); ok && sum != body {
			return true
		}
	}
	return false
}

// A learned POST reads only the recordings of its path whose body may be its
// own: at 100 recordings of POST /item and at 2000 alike, of a staging
// generation and of an open test's own, a call reads one recording (each of
// the open test's own is read in both its tiers' reads: two), and the
// bucket is filed by its bodies once. Where keploy.yml declares noise of
// request bodies the call reads the whole bucket, as before.
func TestALearnedPOSTReadsOnlyTheRecordingsOfItsBody(t *testing.T) {
	verifyOff(t)
	for _, size := range []int{100, 2000} {
		for _, view := range []bool{false, true} {
			for _, declared := range []bool{false, true} {
				t.Run(fmt.Sprintf("%d/view=%v/declared=%v", size, view, declared), func(t *testing.T) {
					h, _, _ := learnOn(t)
					var bodyNoise map[string][]string
					if declared {
						bodyNoise = map[string][]string{"note": {}}
					}
					n := h.newLearnNoise(models.OutgoingOptions{Learn: true}, bodyNoise, nil)
					mocks := make([]*models.Mock, size)
					for i := range mocks {
						mocks[i] = learnRecorded(fmt.Sprintf("post-%d", i), time.Duration(i)*time.Millisecond, "POST", "/item", postBody(i), nil, `{}`)
					}
					var db integrations.MockMemDb = newGenDb(mocks, nil)
					if view {
						db = newViewDb(mocks, nil)
					}
					group := func(i int) []*models.Mock {
						lp := liveParts(context.Background(), live("POST", "/item", postBody(i), nil))
						g, _, err := h.learnedGroupOf(context.Background(), db, db, nil, lp, n.shape(lp, sameDest), n, false)
						if err != nil {
							t.Fatal(err)
						}
						return g
					}
					for i := 0; i < 2; i++ {
						group(i)
					}
					filings := learnBodyFilings.Load()
					var g []*models.Mock
					visits := learnVisitsOf(func() { g = group(size / 2) })
					// An open test's own recording is in both its tiers' reads.
					if tiers := map[bool]int{false: 1, true: 2}[view]; len(g) != tiers || g[0].Name != fmt.Sprintf("post-%d", size/2) || g[tiers-1] != g[0] {
						t.Fatalf("group %v; want post-%d, in each of %d reads", strictNames(g), size/2, tiers)
					}
					want := int64(1)
					switch {
					case declared:
						want = int64(size)
					case view:
						want = 2
					}
					if view && declared {
						want = int64(2 * size)
					}
					if visits != want {
						t.Fatalf("a POST that is one recording of %d read %d; want %d", size, visits, want)
					}
					if got := learnBodyFilings.Load(); got != filings {
						t.Fatalf("a call filed its bucket by its bodies again (%d filings)", got-filings)
					}
				})
			}
		}
	}
}

// A learned call of a path no recording has files no bucket by its bodies.
func TestALearnedCallOfAnUnrecordedPathFilesNoBodies(t *testing.T) {
	h, n, _ := learnOn(t)
	verifyOff(t)
	db := newGenDb([]*models.Mock{learnRecorded("post-0", 0, "POST", "/item", postBody(0), nil, `{}`)}, nil)
	for _, path := range []string{"/item", "/item", "/other"} {
		filings := learnBodyFilings.Load()
		lp := liveParts(context.Background(), live("POST", path, postBody(1), nil))
		if _, _, err := h.learnedGroupOf(context.Background(), db, db, nil, lp, n.shape(lp, sameDest), n, false); err != nil {
			t.Fatal(err)
		}
		if path == "/other" && learnBodyFilings.Load() != filings {
			t.Fatalf("a call of an unrecorded path filed %d buckets by their bodies; want none", learnBodyFilings.Load()-filings)
		}
	}
}

// A learned call whose context ends while another call files its bucket
// leaves with the context's error, and the call after it is answered.
func TestALearnedCallLeavesWhenItsContextEndsWhileABucketIsFiled(t *testing.T) {
	ix := newBodyFilings(&learnBodyFilings)
	ix.turn <- struct{}{}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := ix.file(ctx, "POST item", []int{0}, func(int) (uint64, bool) { return 1, true }); err == nil {
		t.Fatal("a call whose context ended waited on another's filing")
	}
	<-ix.turn
	b, err := ix.file(context.Background(), "POST item", []int{0, 1}, func(p int) (uint64, bool) { return uint64(p), p == 0 })
	if err != nil || len(b.of(0)) != 2 || len(b.of(7)) != 1 {
		t.Fatalf("filed %+v err %v; want position 0 by its print and 1 whatever the call's", b, err)
	}
}

// learnPrint holds bodies the learn step holds the same as one print, and
// refuses a document a key of which holds a '.' or a '[': its leaves' paths
// are another document's too. learnPlainPrint files a recording by it only
// where nothing the recording learned excuses a leaf of its body, and its
// body is no form and no multipart body.
func TestLearnPrintFilesOnlyABodyTheLearnStepComparesPlainly(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	one := func(a, b string) bool {
		x, okx := learnPrint([]byte(a))
		y, oky := learnPrint([]byte(b))
		return okx && oky && x == y
	}
	if !one(`{"a":1,"b":[1,2]}`, ` {"b":[1, 2], "a":1}`) || !one(`text`, `text`) || !one(``, ``) {
		t.Fatal("bodies the learn step holds the same have other prints")
	}
	if one(`{"a":1}`, `{"a":2}`) || one(`[1,2]`, `[2,1]`) || one(`{"a":"1"}`, `{"a":1}`) || one(``, ` `) {
		t.Fatal("bodies of other values share a print")
	}
	for _, b := range []string{`{"a.b":1}`, `{"x":{"a[0]":1}}`, `[{"a.b":1}]`} {
		if _, ok := learnPrint([]byte(b)); ok {
			t.Errorf("learnPrint(%s) ok; want refused: its leaves' paths are another document's", b)
		}
	}
	if _, ok := learnPrint([]byte(`{"a]":1}`)); !ok {
		t.Error(`learnPrint({"a]":1}) refused: a ']' in a key ends no path segment`)
	}
	plain := func(m *models.Mock) bool {
		_, ok := learnPlainPrint(m, recordedParts(m))
		return ok
	}
	m := learnRecorded("m", 0, "POST", "/item", `{"a":1}`, nil, `{}`)
	if !plain(m) {
		t.Fatal("a JSON recording that learned nothing is not filed by its body")
	}
	m.Spec.ReqBodyNoise = map[string][]string{"body.a": {}}
	if plain(m) {
		t.Error("a recording that learned noise of its body is filed by its body")
	}
	for _, ct := range []string{"application/x-www-form-urlencoded", "multipart/form-data; boundary=XX"} {
		f := learnRecorded("f", 0, "POST", "/item", `a=1`, map[string]string{"Content-Type": ct}, `{}`)
		if plain(f) {
			t.Errorf("a %s recording is filed by its body", ct)
		}
	}
	h, _, _ := learnOn(t)
	lp := liveParts(context.Background(), live("POST", "/item", `{"a":1}`, nil))
	if _, ok := h.newLearnNoise(models.OutgoingOptions{Learn: true}, nil, nil).learnLivePrint(lp); !ok {
		t.Fatal("a call with no body noise declared has no print")
	}
	if _, ok := h.newLearnNoise(models.OutgoingOptions{Learn: true}, map[string][]string{"note": {}}, nil).learnLivePrint(lp); ok {
		t.Error("a call whose keploy.yml declares body noise has a print")
	}
}
