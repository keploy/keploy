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
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// stagedDb is a mockMemDb that stages more than its reads show
// (integrations.StagedFinder): staged are the recordings no read shows, as
// exact serving keeps another test's.
type stagedDb struct {
	*mockMemDb
	staged []*models.Mock
}

func (d stagedDb) FindStagedNotOwned(kind models.Kind, match func(*models.Mock) bool) string {
	for _, m := range d.staged {
		if m.Kind == kind && match(m) {
			return m.Name
		}
	}
	return ""
}

// A call no read answers that is a staged recording no read shows, by the
// strict comparison, is named as that recording; one that differs from every
// such recording is not, nor is any with a store that stages nothing more.
func TestNotOwnedRecordingIsTheCallMadeElsewhere(t *testing.T) {
	const host = "localhost:50001"
	elsewhere := onHost("other-test-delete", "/delete", host)
	db := stagedDb{mockMemDb: &mockMemDb{}, staged: []*models.Mock{onHost("other-test-get", "/get", host), elsewhere}}
	n := newHTTP().newStrictNoise(nil, nil, nil)
	ctx := context.Background()
	if got := notOwnedRecording(ctx, getTo("/delete", host), db, n); got != elsewhere {
		t.Fatalf("named %v, want the recording that is this call", got)
	}
	if got := notOwnedRecording(ctx, getTo("/changed", host), db, n); got != nil {
		t.Fatalf("a call no recording is was named as %s", got.Name)
	}
	if got := notOwnedRecording(ctx, getTo("/delete", host), &mockMemDb{mocks: []*models.Mock{elsewhere}}, n); got != nil {
		t.Fatalf("a store that stages nothing more named %s", got.Name)
	}

	r := &models.MockMismatchReport{ClosestMock: "visible", Differs: []string{"path"}, FieldDiffs: []models.MockFieldDiff{{Path: "path"}}}
	markNotOwned(r, elsewhere)
	if r.ClosestMock != elsewhere.Name || !r.NotOwned || r.Differs != nil || r.FieldDiffs != nil || r.ClosestMockReq == "" || r.NextSteps == "" {
		t.Fatalf("the report of a call made elsewhere is %+v", r)
	}
}

// Under the proven switch, the HTTP parser's miss on a call that is another
// test's recording reports it as that recording (NotOwned), which record's
// judging reads to bake it into the worker scope (S12, a2). With the switch
// off nothing is named so.
func TestAMissOnACallMadeElsewhereNamesItsRecording(t *testing.T) {
	const addr, host = "127.0.0.1:50001", "localhost:50001"
	miss := func(t *testing.T) *models.MockMismatchReport {
		t.Helper()
		db := stagedDb{mockMemDb: &mockMemDb{}, staged: []*models.Mock{onHost("other-test-delete", "/delete", host)}}
		var reported error
		ctx := models.WithMockMismatchReporter(context.Background(), func(err error) { reported = err })
		app, prox := net.Pipe()
		defer app.Close()
		done := make(chan error, 1)
		go func() {
			done <- newHTTP().MockOutgoing(ctx, prox, &models.ConditionalDstCfg{Addr: addr}, db, models.OutgoingOptions{})
		}()
		if _, err := io.WriteString(app, "GET /delete HTTP/1.1\r\nHost: "+host+"\r\nAccept: application/json\r\n\r\n"); err != nil {
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
	t.Setenv(provenswitch.Env, "1")
	if r := miss(t); !r.NotOwned || r.ClosestMock != "other-test-delete" {
		t.Fatalf("under the switch the miss names %q (not owned: %v), want other-test-delete", r.ClosestMock, r.NotOwned)
	}
	t.Setenv(provenswitch.Env, "")
	if r := miss(t); r.NotOwned {
		t.Fatalf("without the switch the miss is named not owned: %+v", r)
	}
}

// keyedDb is a store that keeps its recordings on disk, as the agent's does
// (integrations.StagedKeyFinder): its trees hold none, and a miss is offered
// only the recordings indexed under a key it asks by (models.RequestKeys).
// offered is every recording it offered.
type keyedDb struct {
	*mockMemDb
	onDisk  []*models.Mock
	offered *[]string
}

func (d keyedDb) FindStagedNotOwned(models.Kind, func(*models.Mock) bool) string { return "" }

func (d keyedDb) FindStagedNotOwnedByKeys(kind models.Kind, keys []string, match func(*models.Mock) bool) string {
	asked := map[string]bool{}
	for _, k := range keys {
		asked[k] = true
	}
	for _, m := range d.onDisk {
		under := false
		for _, k := range models.RequestKeys(m) {
			under = under || asked[k]
		}
		if m.Kind != kind || !under {
			continue
		}
		*d.offered = append(*d.offered, m.Name)
		if match(m) {
			return m.Name
		}
	}
	return ""
}

// A call that is a recording kept on disk is named from the recordings under
// the keys it asks by (RequestKeysOf, recordedRequestKeys), whatever the
// strict comparison tolerates: a path segment or X-Amz-Target the recording
// learned as noise or the obfuscator redacted, keploy.yml's url or header
// noise, the target header in another case. Only those are decoded: with no
// keploy.yml noise on the path or target, a recording of another path,
// method or target is never offered.
func TestAMissAsksTheDiskStoreByAKeyItsRecordingIsUnder(t *testing.T) {
	const host = "localhost:50001"
	post := func(m *models.Mock, target string) *models.Mock {
		m.Spec.HTTPReq.Method = "POST"
		if target != "" {
			m.Spec.HTTPReq.Header["x-amz-target"] = target
		}
		return m
	}
	postTo := func(path, target string) *req {
		r := getTo(path, host)
		r.method = "POST"
		r.header.Set("X-Amz-Target", target)
		return r
	}
	learned := func(m *models.Mock, field string) *models.Mock {
		m.Spec.ReqBodyNoise = map[string][]string{field: {}}
		return m
	}
	for _, tc := range []struct {
		name          string
		rec           *models.Mock
		live          *req
		header, url   []string
		want          bool
		offeredOthers bool
	}{
		{name: "the same path", rec: onHost("rec", "/orders", host), live: getTo("/orders", host), want: true},
		{name: "another path", rec: onHost("rec", "/orders", host), live: getTo("/carts", host)},
		{name: "a path segment learned", rec: learned(onHost("rec", "/orders/1", host), "url.path.1"), live: getTo("/orders/2", host), want: true},
		{name: "a path learned whole", rec: learned(onHost("rec", "/orders/1", host), "url"), live: getTo("/orders/2", host), want: true},
		{name: "a learned segment, another count", rec: learned(onHost("rec", "/orders/1", host), "url.path.1"), live: getTo("/orders/2/items", host)},
		{name: "the target in another case", rec: post(onHost("rec", "/", host), "AmazonSQS.DeleteMessage"), live: postTo("/", "AmazonSQS.DeleteMessage"), want: true},
		{name: "another target", rec: post(onHost("rec", "/", host), "AmazonSQS.DeleteMessage"), live: postTo("/", "AmazonSQS.SendMessage")},
		{name: "a target learned", rec: learned(post(onHost("rec", "/", host), "AmazonSQS.DeleteMessage"), "header.x-amz-target"), live: postTo("/", "AmazonSQS.SendMessage"), want: true},
		{name: "keploy.yml url noise", rec: onHost("rec", "/orders/123", host), live: getTo("/orders/456", host), url: []string{"[0-9]+"}, want: true, offeredOthers: true},
		{name: "keploy.yml header noise on the target", rec: post(onHost("rec", "/", host), "AmazonSQS.DeleteMessage"), live: postTo("/", "AmazonSQS.SendMessage"), header: []string{"x-amz-target"}, want: true, offeredOthers: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			hn := map[string][]string{}
			for _, h := range tc.header {
				hn[h] = nil
			}
			n := newHTTP().newStrictNoise(hn, nil, tc.url)
			// The recordings of another path and of another method, which the
			// call can never be: offered only when keploy.yml's noise reaches
			// the path or target, under the shape key, if of its method and
			// segment count.
			others := []*models.Mock{onHost("other-path", "/elsewhere/9", host), post(onHost("other-method", tc.rec.Spec.HTTPReq.URL, host), "")}
			if tc.rec.Spec.HTTPReq.Method == "POST" {
				others[1].Spec.HTTPReq.Method = "PUT"
			}
			var offered []string
			db := keyedDb{mockMemDb: &mockMemDb{}, onDisk: append(others, tc.rec), offered: &offered}
			got := notOwnedRecording(context.Background(), tc.live, db, n)
			if named := got == tc.rec; named != tc.want {
				t.Fatalf("named %v (offered %v); want the recording named: %v", got, offered, tc.want)
			}
			for _, name := range offered {
				if name == "other-method" || (name == "other-path" && !tc.offeredOthers) {
					t.Fatalf("offered %v: a recording the call can never be was decoded", offered)
				}
			}
		})
	}
}

// learnStepMiss sends request through the HTTP parser in record's full round
// (OutgoingOptions.Learn) with tracker tr and store db, and returns the status
// the app got and the report of the miss it was, nil for none.
func learnStepMiss(t *testing.T, tr *learn.Tracker, db integrations.MockMemDb, request string) (int, *models.MockMismatchReport) {
	t.Helper()
	t.Setenv(provenswitch.Env, "1")
	var reported error
	ctx := learn.WithTracker(models.WithMockMismatchReporter(context.Background(), func(err error) { reported = err }), tr)
	app, prox := net.Pipe()
	defer app.Close()
	done := make(chan error, 1)
	go func() {
		done <- newHTTP().MockOutgoing(ctx, prox, &models.ConditionalDstCfg{Addr: "127.0.0.1:50001"}, db, models.OutgoingOptions{Learn: true})
	}()
	if _, err := io.WriteString(app, request); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		t.Fatalf("the app got no answer: %v", err)
	}
	_ = resp.Body.Close()
	_ = app.Close()
	<-done
	var withReport interface {
		MismatchReport() *models.MockMismatchReport
	}
	if !errors.As(reported, &withReport) {
		return resp.StatusCode, nil
	}
	return resp.StatusCode, withReport.MismatchReport()
}

const pollC1 = "GET /v1/jobs?cursor=c1 HTTP/1.1\r\nHost: api.test\r\nContent-Type: application/json\r\n\r\n"

// (LEARN-vs-NOTOWNED) In record's full round (OutgoingOptions.Learn), the
// HTTP parser's miss on a call the learn step named as another test's
// recording before pairing it is that recording's (NotOwned), named once:
// the store names a recording for one miss at most, so the miss's report
// does not look it up again, and the miss is no extra call of the test.
//
// That holds because learn.Pair decides one call more than the test recorded
// first: a call one of the test's own answered recordings is exactly (the
// tracker holds it, as the mock manager's served observer feeds it) is a
// miss before any pairing, an extra call, and no recording elsewhere is
// looked up or named for it. A Pair that paired such a call would name the
// recording elsewhere instead, and this test would see it.
func TestALearnStepMissOnACallMadeElsewhereNamesItsRecordingOnce(t *testing.T) {
	ownPoll := func() *models.Mock {
		return learnRecorded("own-poll", 0, "GET", "/v1/jobs?cursor=c2", "", nil, `{"jobs":[]}`)
	}
	otherPoll := func() *models.Mock {
		return learnRecorded("other-test-poll", -time.Second, "GET", "/v1/jobs?cursor=c1", "", nil, `{"jobs":["j1"]}`)
	}
	tr := learn.NewTracker()
	tr.Begin()
	db := &elsewhereDb{learnDb: &learnDb{perTest: []*models.Mock{ownPoll()}}, elsewhere: []*models.Mock{otherPoll()}}
	code, r := learnStepMiss(t, tr, db, pollC1)
	if code != http.StatusBadGateway || r == nil {
		t.Fatalf("the app got %d (report %+v) for a call made elsewhere, want the miss's 502", code, r)
	}
	if !r.NotOwned || r.ClosestMock != "other-test-poll" || r.Reason != "" || r.MissKind != "" || r.Recorded != 0 {
		t.Fatalf("the miss names %q (not owned: %v, reason %q, kind %q of %d), want other-test-poll not owned", r.ClosestMock, r.NotOwned, r.Reason, r.MissKind, r.Recorded)
	}
	if db.lookups != 1 || len(db.consumed) != 0 {
		t.Fatalf("%d lookups of a recording made elsewhere (want the learn step's one); consumed %v", db.lookups, db.consumed)
	}

	// The test's own answered recording is the call: one call more than
	// it recorded, decided before any pairing.
	tr = learn.NewTracker()
	tr.Begin()
	answered := learnRecorded("answered-poll", -2*time.Second, "GET", "/v1/jobs?cursor=c1", "", nil, `{"jobs":["j0"]}`)
	test, _, _, _ := tr.Test()
	if !tr.Answer(test, answered) {
		t.Fatal("fixture: the tracker did not take the answered recording")
	}
	db = &elsewhereDb{learnDb: &learnDb{perTest: []*models.Mock{ownPoll()}}, elsewhere: []*models.Mock{otherPoll()}, answered: []*models.Mock{answered}}
	code, r = learnStepMiss(t, tr, db, pollC1)
	if code != http.StatusBadGateway || r == nil || r.MissKind != models.MissExtraCall || r.Recorded != 1 || r.NotOwned {
		t.Fatalf("the app got %d, report %+v; want one call more than recorded, not a call made elsewhere", code, r)
	}
	if db.named["other-test-poll"] {
		t.Fatal("one call more than the test recorded named a recording made elsewhere")
	}
}

// (LEARN-vs-NOTOWNED) The miss on a call the learn step refused as a
// recording made elsewhere once more than it holds names that recording, as
// its closest mock with no field differing, and says it was made again: not
// as not owned (it was named, or answered, already), with no learn-step
// reason (it did not change), and no extra call of the test.
func TestALearnStepMissOnACallMadeElsewhereAgainNamesItsRecording(t *testing.T) {
	tr := learn.NewTracker()
	tr.Begin()
	db := &elsewhereDb{
		learnDb:   &learnDb{perTest: []*models.Mock{learnRecorded("own-poll", 0, "GET", "/v1/jobs?cursor=c2", "", nil, `{"jobs":[]}`)}},
		elsewhere: []*models.Mock{learnRecorded("other-test-poll", -time.Second, "GET", "/v1/jobs?cursor=c1", "", nil, `{"jobs":["j1"]}`)},
		spent:     map[string]bool{"other-test-poll": true},
	}
	code, r := learnStepMiss(t, tr, db, pollC1)
	if code != http.StatusBadGateway || r == nil {
		t.Fatalf("the app got %d (report %+v) for a call made elsewhere again, want the miss's 502", code, r)
	}
	if r.NotOwned || r.ClosestMock != "other-test-poll" || len(r.FieldDiffs) != 0 || r.Reason != "" || r.MissKind != "" || r.Recorded != 0 {
		t.Fatalf("the miss names %q (not owned: %v, diffs %v, reason %q, kind %q of %d), want other-test-poll made again", r.ClosestMock, r.NotOwned, r.FieldDiffs, r.Reason, r.MissKind, r.Recorded)
	}
	if !strings.Contains(r.NextSteps, "other-test-poll again") {
		t.Fatalf("the miss's next steps do not say the call was made again: %q", r.NextSteps)
	}
	if db.lookups != 1 || len(db.named) != 0 || len(db.consumed) != 0 {
		t.Fatalf("%d lookups, named %v, consumed %v", db.lookups, db.named, db.consumed)
	}
}
