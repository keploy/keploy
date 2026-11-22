package http

import (
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// kpUUID is keploy.yml url noise that masks a key's id in a path.
const kpUUID = `[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}`

// kpLiveRecord is the GET's answer as this run's PUT stored it.
var kpLiveRecord = kpGetAnswer(kpRecKey, kpLiveBy, kpLiveNano)

// kpStatus sets the recorded answer's status of m.
func kpStatus(m *models.Mock, code int) *models.Mock {
	m.Spec.HTTPResp.StatusCode = code
	return m
}

// The read-back holds over the calls that do not change the record: a GET of
// it between (a read changes nothing), a PUT the store refused (a 4xx stored
// nothing), a DELETE of another key; with a header of the GET's answer
// holding a stored value (only its body is read back, the header answered as
// recorded); where the PUT's mock bears the name of
// the running test case's place (the PUT's exchange is its own, not that
// place); and where the PUT and the GET went in this run to one URL
// keploy.yml masks.
func TestReadBackOfAPutHoldsOverCallsThatKeepTheRecord(t *testing.T) {
	const other = "8d2cc73e-0b5d-46b0-b576-fb5967f22915"
	const liveKey = "9e3f6a1b-2c4d-4e5f-8a9b-0c1d2e3f4a5b"
	recordedAnswer := kpGetAnswer(kpRecKey, kpRecBy, kpRecNano)
	put := func() *req { return live("PUT", kpPath(kpRecKey), kpPut(kpRecKey, kpLiveBy, kpLiveNano), kpVault) }
	get := func() *req { return live("GET", kpPath(kpRecKey), "", kpVault) }
	for _, proven := range []string{"1", "0"} {
		t.Run("proven="+proven+"/a-get-between", func(t *testing.T) {
			m := kpMocks(recordedAnswer)
			m = append(m, learnRecorded("mock-2", 1400*time.Millisecond, "GET", kpPath(kpRecKey), "", kpVault, recordedAnswer))
			_, served := kpRun(t, proven, m, []*req{put(), get(), get()})
			for i := 1; i <= 2; i++ {
				if got := served[i].Spec.HTTPResp.Body; got != kpLiveRecord {
					t.Fatalf("GET %d answered\n%s\nwant what this run's PUT stored", i, got)
				}
			}
		})
		t.Run("proven="+proven+"/a-refused-put-between", func(t *testing.T) {
			const cas = `{"data":{"status":"active"},"options":{"cas":0}}`
			m := kpMocks(recordedAnswer)
			refused := kpStatus(learnRecorded("mock-2", 1200*time.Millisecond, "PUT", kpPath(kpRecKey), cas, kpVault, `{"errors":["check-and-set parameter did not match"]}`), 400)
			m = []*models.Mock{m[0], refused, m[1]}
			_, served := kpRun(t, proven, m, []*req{put(), live("PUT", kpPath(kpRecKey), cas, kpVault), get()})
			if got := served[2].Spec.HTTPResp.Body; got != kpLiveRecord {
				t.Fatalf("a GET after a PUT answered 400 answered\n%s\nwant what the PUT before it stored", got)
			}
		})
		t.Run("proven="+proven+"/another-key-deleted-between", func(t *testing.T) {
			m := kpMocks(recordedAnswer)
			del := learnRecorded("mock-2", 1200*time.Millisecond, "DELETE", kpPath(other), "", kpVault, `{}`)
			m = []*models.Mock{m[0], del, m[1]}
			_, served := kpRun(t, proven, m, []*req{put(), live("DELETE", kpPath(other), "", kpVault), get()})
			if got := served[2].Spec.HTTPResp.Body; got != kpLiveRecord {
				t.Fatalf("a GET after a DELETE of another key answered\n%s\nwant what this run's PUT stored", got)
			}
		})
		t.Run("proven="+proven+"/named-as-the-test-case", func(t *testing.T) {
			m := kpMocks(recordedAnswer)
			m[0].Name = "test case post-query-83"
			_, served := kpRun(t, proven, m, []*req{live("PUT", kpPath(kpRecKey), kpPut(kpRecKey, kpLiveBy, kpRecNano), kpVault), get()})
			if got := served[1].Spec.HTTPResp.Body; got != kpGetAnswer(kpRecKey, kpLiveBy, kpRecNano) {
				t.Fatalf("a GET after a PUT whose mock bears the name of the test case's place answered\n%s\nwant what the PUT stored", got)
			}
		})
		t.Run("proven="+proven+"/an-answer-header", func(t *testing.T) {
			m := kpMocks(recordedAnswer)
			m[1].Spec.HTTPResp.Header["X-Owner"] = kpRecBy
			_, served := kpRun(t, proven, m, []*req{put(), get()})
			if got := served[1].Spec.HTTPResp.Body; got != kpLiveRecord {
				t.Fatalf("the GET answered\n%s\nwant what this run's PUT stored", got)
			}
			if got := served[1].Spec.HTTPResp.Header["X-Owner"]; got != kpRecBy {
				t.Fatalf("the GET's header X-Owner answered %q, want its recording: a store answers what it stored in its body", got)
			}
		})
		t.Run("proven="+proven+"/one-masked-url", func(t *testing.T) {
			_, served := kpRunNoise(t, proven, []string{kpUUID}, kpMocks(recordedAnswer), []*req{
				live("PUT", kpPath(liveKey), kpPut(kpRecKey, kpLiveBy, kpLiveNano), kpVault),
				live("GET", kpPath(liveKey), "", kpVault),
			})
			if got := served[1].Spec.HTTPResp.Body; got != kpLiveRecord {
				t.Fatalf("a GET of the URL the PUT went to answered\n%s\nwant what the PUT stored", got)
			}
		})
	}
}

// The read-back stops where the GET may not read what the PUT stored: a PUT
// answered 500 after it (whether it stored is unknown), the PUT and the GET in this
// run to two URLs keploy.yml masks to one, requests with no Host (no one
// resource is named), a POST or a PATCH of the record after it, and a later
// PUT that replaced the record (a field only the earlier PUT stored is not
// read back).
func TestReadBackOfAPutStopsWhereTheRecordMayDiffer(t *testing.T) {
	const keyA, keyB = "9e3f6a1b-2c4d-4e5f-8a9b-0c1d2e3f4a5b", "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	recordedAnswer := kpGetAnswer(kpRecKey, kpRecBy, kpRecNano)
	put := func() *req { return live("PUT", kpPath(kpRecKey), kpPut(kpRecKey, kpLiveBy, kpLiveNano), kpVault) }
	get := func() *req { return live("GET", kpPath(kpRecKey), "", kpVault) }
	for _, proven := range []string{"1", "0"} {
		t.Run("proven="+proven+"/put-answered-500", func(t *testing.T) {
			m := kpMocks(recordedAnswer)
			failed := kpStatus(learnRecorded("mock-2", 1200*time.Millisecond, "PUT", kpPath(kpRecKey), kpPut(kpRecKey, kpRecBy, kpRecNano), kpVault, `{"errors":["internal error"]}`), 500)
			m = []*models.Mock{m[0], failed, m[1]}
			_, served := kpRun(t, proven, m, []*req{put(), put(), get()})
			if got := served[2].Spec.HTTPResp.Body; got != recordedAnswer {
				t.Fatalf("a GET after a PUT answered 500 answered\n%s\nwant its recording", got)
			}
		})
		t.Run("proven="+proven+"/two-masked-urls", func(t *testing.T) {
			_, served := kpRunNoise(t, proven, []string{kpUUID}, kpMocks(recordedAnswer), []*req{
				live("PUT", kpPath(keyA), kpPut(kpRecKey, kpLiveBy, kpLiveNano), kpVault),
				live("GET", kpPath(keyB), "", kpVault),
			})
			if got := served[1].Spec.HTTPResp.Body; got != recordedAnswer {
				t.Fatalf("a GET of another URL than the PUT's in this run answered\n%s\nwant its recording", got)
			}
		})
		t.Run("proven="+proven+"/no-host", func(t *testing.T) {
			m := kpMocks(recordedAnswer)
			calls := []*req{put(), get()}
			for i := range m {
				delete(m[i].Spec.HTTPReq.Header, "Host")
				calls[i].header.Del("Host")
			}
			_, served := kpRun(t, proven, m, calls)
			if got := served[1].Spec.HTTPResp.Body; got != recordedAnswer {
				t.Fatalf("a GET with no Host answered\n%s\nwant its recording", got)
			}
		})
		for _, method := range []string{"POST", "PATCH"} {
			// A POST or a PATCH to the record's URL after the PUT changes it
			// in a way keploy does not follow, though its JSON body holds a
			// value the GET's recorded answer holds, drifted with the PUT's.
			t.Run("proven="+proven+"/a-"+method+"-between", func(t *testing.T) {
				owner := func(by string) string { return `{"data":{"createdBy":"` + by + `"}}` }
				m := kpMocks(recordedAnswer)
				w := learnRecorded("mock-2", 1200*time.Millisecond, method, kpPath(kpRecKey), owner(kpRecBy), kpVault, `{"data":{"version":2}}`)
				m = []*models.Mock{m[0], w, m[1]}
				_, served := kpRun(t, proven, m, []*req{put(), live(method, kpPath(kpRecKey), owner(kpLiveBy), kpVault), get()})
				if got := served[2].Spec.HTTPResp.Body; got != recordedAnswer {
					t.Fatalf("a GET after a %s of the record answered\n%s\nwant its recording", method, got)
				}
			})
		}
		t.Run("proven="+proven+"/replaced", func(t *testing.T) {
			const revoked = `{"data":{"_id":"` + kpRecKey + `","status":"revoked"}}`
			answer := `{"data":{"data":{"_id":"` + kpRecKey + `","status":"revoked"},"metadata":{"owner":"` + kpRecBy + `","version":2}}}`
			m := kpMocks(answer)
			second := learnRecorded("mock-2", 1200*time.Millisecond, "PUT", kpPath(kpRecKey), revoked, kpVault, `{"data":{"version":2}}`)
			m = []*models.Mock{m[0], second, m[1]}
			_, served := kpRun(t, proven, m, []*req{put(), live("PUT", kpPath(kpRecKey), revoked, kpVault), get()})
			if got := served[2].Spec.HTTPResp.Body; got != answer {
				t.Fatalf("a GET after a second PUT answered\n%s\nwant its recording: the field only the first PUT stored is not read back", got)
			}
		})
	}
}

// noPanic runs f, failing t where it panics: a guard against an index or a
// nil it reads.
func noPanic(t *testing.T, what string, f func()) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("%s panicked: %v", what, r)
		}
	}()
	f()
}

// HTTP's own store names no resource of a request with no Host or no path,
// stores no body that is no JSON object or array, reads no PUT with no
// recorded answer as stored, and links a call's URL to the read's only
// where every leaf of the two is alike.
func TestPutStoreReadsOnlyWhatNamesOneResource(t *testing.T) {
	noPanic(t, "putTarget of a mock with no request", func() {
		if _, ok := putTarget(&models.Mock{}); ok {
			t.Fatal("a mock with no request names a resource")
		}
		if putReads(nil) || putReads(&models.Mock{}) {
			t.Fatal("no request reads a resource")
		}
	})
	noHost := learnRecorded("mock-0", 0, "GET", kpPath(kpRecKey), "", nil, `{}`)
	delete(noHost.Spec.HTTPReq.Header, "Host")
	if _, ok := putTarget(noHost); ok {
		t.Fatal("a request with no Host names a resource")
	}
	if _, ok := putTarget(learnRecorded("mock-0", 0, "GET", "", "", nil, `{}`)); ok {
		t.Fatal("a request with no path names a resource")
	}
	for _, body := range []string{`"a string"`, `42`, `null`, `not json`, ``} {
		if _, ok := putStored(learnRecorded("mock-0", 0, "PUT", "/k", body, nil, `{}`)); ok {
			t.Fatalf("a PUT of %q stores leaves", body)
		}
	}
	if got, ok := putStored(learnRecorded("mock-0", 0, "PUT", "/k", `{"a":"1"}`, nil, `{}`)); !ok || len(got) != 1 || got[0] != "body.a" {
		t.Fatalf("a PUT of a JSON object stores %v %v, want body.a", got, ok)
	}

	read := learnRecorded("mock-1", time.Second, "GET", "/k", "", nil, `{"a":"1"}`)
	noAnswer := learnRecorded("mock-0", 0, "PUT", "/k", `{"a":"1"}`, nil, `{}`)
	noAnswer.Spec.HTTPResp = nil
	seen := learn.Earlier{Mock: "mock-0", Seen: []learn.Seen{{Side: learn.Request, Leaf: learn.Leaf{Path: "url.path.0", Value: "k"}, Live: "k"}}}
	liveURL := learn.Shape{Leaves: map[string]learn.Leaf{"url.path.0": {Path: "url.path.0", Value: "k"}}}
	noPanic(t, "putAccess of a GET with no recorded answer", func() {
		bare := learnRecorded("mock-1", time.Second, "GET", "/k", "", nil, `{}`)
		bare.Spec.HTTPResp = nil
		if _, ok := putAccess(bare, nil, nil, learn.Shape{}); ok {
			t.Fatal("a GET with no recorded answer reads a record")
		}
	})
	noPanic(t, "putAccess of a PUT with no recorded answer", func() {
		a, ok := putAccess(read, []*models.Mock{noAnswer}, []learn.Earlier{seen}, liveURL)
		if !ok || a.Written[0] != nil {
			t.Fatalf("a PUT with no recorded answer stored %v (%v)", a.Written, ok)
		}
	})

	// A PUT's live URL is its own exchange's: not that of another mock
	// served before it, which went to the read's URL, nor a given value's.
	other := learn.Earlier{Mock: "mock-9", Seen: seen.Seen}
	given := learn.Earlier{Mock: "mock-0", Given: true, Seen: seen.Seen}
	elsewhere := learn.Earlier{Mock: "mock-0", Seen: []learn.Seen{{Side: learn.Request, Leaf: learn.Leaf{Path: "url.path.0", Value: "k"}, Live: "j"}}}
	put := learnRecorded("mock-0", 0, "PUT", "/k", `{"a":"1"}`, nil, `{}`)
	if a, ok := putAccess(read, []*models.Mock{put}, []learn.Earlier{other, given, elsewhere}, liveURL); !ok || a.Written[0] != nil {
		t.Fatalf("a PUT that went to another URL in this run stored %v (%v)", a.Written, ok)
	}
	if a, ok := putAccess(read, []*models.Mock{put}, []learn.Earlier{other, given, seen}, liveURL); !ok || a.Written[0]["body.a"] == "" {
		t.Fatalf("a PUT that went to the read's URL in this run stored %v (%v)", a.Written, ok)
	}

	shape := func(kv ...string) learn.Shape {
		s := learn.Shape{Leaves: map[string]learn.Leaf{}}
		for i := 0; i < len(kv); i += 2 {
			s.Leaves[kv[i]] = learn.Leaf{Path: kv[i], Value: kv[i+1]}
		}
		return s
	}
	earlier := func(kv ...string) learn.Earlier {
		e := learn.Earlier{Mock: "mock-0"}
		for i := 0; i < len(kv); i += 2 {
			e.Seen = append(e.Seen, learn.Seen{Side: learn.Request, Leaf: learn.Leaf{Path: kv[i], Value: "rec"}, Live: kv[i+1]})
		}
		return e
	}
	for _, c := range []struct {
		name string
		e    learn.Earlier
		live learn.Shape
		want bool
	}{
		{"alike", earlier("url.path.0", "k", "url.query.v", "1", "body.a", "x"), shape("url.path.0", "k", "url.query.v", "1", "body.a", "y"), true},
		{"a path segment differs", earlier("url.path.0", "k"), shape("url.path.0", "j"), false},
		{"a query value differs", earlier("url.path.0", "k", "url.query.v", "1"), shape("url.path.0", "k", "url.query.v", "2"), false},
		{"the read has another segment", earlier("url.path.0", "k"), shape("url.path.0", "k", "url.path.1", "x"), false},
		{"the call has another segment", earlier("url.path.0", "k", "url.path.1", ""), shape("url.path.0", "k"), false},
		{"none served, the read with no URL leaf", learn.Earlier{}, shape(), false},
	} {
		if got := sameLiveURL(c.e, c.live); got != c.want {
			t.Errorf("%s: sameLiveURL %v, want %v", c.name, got, c.want)
		}
	}
}

// wordUsedAt answers a read at the one word of its field a write's variable
// is used at, and none where the use is at another place, at a word whose
// recorded value is not that word, at a word the field lacks, at a word it
// cannot read, or where two are.
func TestWordUsedAtTheReadsWord(t *testing.T) {
	const place, value = "body.data.name", "CI identity: github octo-org/repo-17"
	use := func(name, recorded string, at ...string) models.MockVariables {
		return models.MockVariables{name: {Recorded: recorded, Use: at}}
	}
	for _, c := range []struct {
		name     string
		vars     models.MockVariables
		wantName string
		wantWord int
		want     bool
	}{
		{"the word", use("value3", "17", "body.data.name#6"), "value3", 6, true},
		{"the whole place", use("value3", "17", "body.data.name"), "", 0, false},
		{"another place", use("value3", "17", "body.data.email#6"), "", 0, false},
		{"another word", use("value3", "17", "body.data.name#5"), "", 0, false},
		{"a word past the field", use("value3", "17", "body.data.name#60"), "", 0, false},
		{"a word before it", use("value3", "17", "body.data.name#-1"), "", 0, false},
		{"no index", use("value3", "CI", "body.data.name#x"), "", 0, false},
		{"two", models.MockVariables{"value3": {Recorded: "17", Use: []string{"body.data.name#6"}}, "value4": {Recorded: "github", Use: []string{"body.data.name#2"}}}, "", 0, false},
	} {
		noPanic(t, c.name, func() {
			name, k, ok := wordUsedAt(c.vars, place, value)
			if ok != c.want || (ok && (name != c.wantName || k != c.wantWord)) {
				t.Errorf("%s: wordUsedAt %s %d %v, want %s %d %v", c.name, name, k, ok, c.wantName, c.wantWord, c.want)
			}
		})
	}
}
