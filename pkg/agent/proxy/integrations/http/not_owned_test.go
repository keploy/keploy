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

// stagedDb is a mockMemDb that stages more than its reads show
// (integrations.StagedFinder): staged are the recordings no read shows, as
// exact serving keeps another test's.
type stagedDb struct {
	*mockMemDb
	staged []*models.Mock
	// unrouted is why the marked call's set is not routed (CallRound.Unrouted).
	unrouted string
}

func (d stagedDb) MarkCallRound() integrations.CallRound {
	return integrations.NewCallRound(1, nil).InRouting(d.unrouted)
}

// HoldToRound implements integrations.RoundHolder: d never stages again, so
// it holds every call it marks as it is.
func (d stagedDb) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return d, func() bool { return false }
}

func (d stagedDb) FindStagedNotOwned(kind models.Kind, call integrations.CallRound, match func(*models.Mock) bool) (string, bool) {
	if !call.Read() {
		return "", false
	}
	for _, m := range d.staged {
		if m.Kind == kind && match(m) {
			return m.Name, false
		}
	}
	return "", false
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
	if got, _ := notOwnedRecording(ctx, getTo("/delete", host), db, n, db.MarkCallRound()); got != elsewhere {
		t.Fatalf("named %v, want the recording that is this call", got)
	}
	if got, _ := notOwnedRecording(ctx, getTo("/changed", host), db, n, db.MarkCallRound()); got != nil {
		t.Fatalf("a call no recording is was named as %s", got.Name)
	}
	if got, _ := notOwnedRecording(ctx, getTo("/delete", host), &mockMemDb{mocks: []*models.Mock{elsewhere}}, n, db.MarkCallRound()); got != nil {
		t.Fatalf("a store that stages nothing more named %s", got.Name)
	}

	r := &models.MockMismatchReport{ClosestMock: "visible", Differs: []string{"path"}, FieldDiffs: []models.MockFieldDiff{{Path: "path"}},
		FieldReasons: []models.FieldReason{{Field: "body.since", Why: "body.since was learned as a read of the application's clock"}}}
	markNotOwned(r, elsewhere, integrations.CallRound{})
	if r.ClosestMock != elsewhere.Name || !r.NotOwned || r.Differs != nil || r.FieldDiffs != nil || r.FieldReasons != nil || r.ClosestMockReq == "" || r.NextSteps == "" {
		t.Fatalf("the report of a call made elsewhere is %+v", r)
	}
}

// Under the proven switch, the HTTP parser's miss on a call that is another
// test's recording reports it as that recording (NotOwned), which record's
// judging reads to bake it into the worker scope (a2). With the switch
// off nothing is named so.
func TestAMissOnACallMadeElsewhereNamesItsRecording(t *testing.T) {
	const addr, host = "127.0.0.1:50001", "localhost:50001"
	miss := func(t *testing.T, unrouted string) *models.MockMismatchReport {
		t.Helper()
		db := stagedDb{mockMemDb: &mockMemDb{}, staged: []*models.Mock{onHost("other-test-delete", "/delete", host)}, unrouted: unrouted}
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
	for _, unrouted := range []string{"", models.UnroutedUnmarked, models.UnroutedNotEnabled, models.UnroutedUnstaged} {
		r := miss(t, unrouted)
		if !r.NotOwned || r.ClosestMock != "other-test-delete" {
			t.Fatalf("under the switch the miss names %q (not owned: %v), want other-test-delete", r.ClosestMock, r.NotOwned)
		}
		// keploy mock replay (no set staged) has no record's judging to
		// move the recording: the miss says the mapping edit, and never to
		// record again.
		if unrouted != models.UnroutedUnstaged {
			if r.NextSteps != notOwnedNextSteps("other-test-delete") {
				t.Fatalf("a set staged by keploy test (%q) was told %q", unrouted, r.NextSteps)
			}
			continue
		}
		if !strings.Contains(r.NextSteps, "made between tests, is other-test-delete") || !strings.Contains(r.NextSteps, "move other-test-delete to worker: in mappings.yaml") || strings.Contains(r.NextSteps, "judging") || strings.Contains(r.NextSteps, "record again") {
			t.Fatalf("keploy mock replay's miss does not say the mapping edit alone: %q", r.NextSteps)
		}
	}
	t.Setenv(provenswitch.Env, "")
	if r := miss(t, ""); r.NotOwned {
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

func (d keyedDb) MarkCallRound() integrations.CallRound { return integrations.NewCallRound(1, nil) }

func (d keyedDb) FindStagedNotOwned(models.Kind, integrations.CallRound, func(*models.Mock) bool) (string, bool) {
	return "", false
}

func (d keyedDb) FindStagedNotOwnedByKeys(kind models.Kind, call integrations.CallRound, keys []string, match func(*models.Mock) bool) (string, bool) {
	if !call.Read() {
		return "", false
	}
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
			return m.Name, false
		}
	}
	return "", false
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
			got, _ := notOwnedRecording(context.Background(), tc.live, db, n, db.MarkCallRound())
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

// roundDb is a store that stages more than its reads show and is asked of a
// call's round: asked is every round it was asked of, restaged makes it say
// the replay staged another set, and refuse makes it refuse the name match
// accepted (another miss spent it first). accepted is what match accepted.
type roundDb struct {
	*mockMemDb
	staged   []*models.Mock
	asked    []integrations.CallRound
	restaged bool
	refuse   bool
	accepted []string
}

func (d *roundDb) MarkCallRound() integrations.CallRound { return integrations.NewCallRound(7, nil) }

func (d *roundDb) FindStagedNotOwned(kind models.Kind, call integrations.CallRound, match func(*models.Mock) bool) (string, bool) {
	d.asked = append(d.asked, call)
	for _, m := range d.staged {
		if m.Kind == kind && match(m) {
			d.accepted = append(d.accepted, m.Name)
			if d.restaged {
				return "", true
			}
			if d.refuse {
				continue
			}
			return m.Name, false
		}
	}
	return "", false
}

// A miss's lookup of a recording made elsewhere is asked of the round the
// call's first read found it made in (notOwnedRecording passes it to the
// store, which decides by it: MockManager.FindStagedNotOwned): what the
// store names is the call's, none when the store says the replay staged
// another set, which the lookup says, and none whose name the store refused
// (another miss spent it).
func TestALookupIsAskedOfTheRoundTheCallWasMadeIn(t *testing.T) {
	const host = "localhost:50001"
	n := newHTTP().newStrictNoise(nil, nil, nil)
	ctx := context.Background()
	call := getTo("/delete", host)
	fresh := func() (*roundDb, *models.Mock) {
		r := onHost("other-test-delete", "/delete", host)
		return &roundDb{mockMemDb: &mockMemDb{}, staged: []*models.Mock{r}}, r
	}
	db, r := fresh()
	at := integrations.NewCallRound(3, nil)
	if got, moved := notOwnedRecording(ctx, call, db, n, at); got != r || moved {
		t.Fatalf("in the call's own round: named %v (restaged %v), want %s", got, moved, r.Name)
	}
	if len(db.asked) != 1 || db.asked[0].Epoch() != 3 || !db.asked[0].Read() {
		t.Fatalf("the store was asked of %+v, want once, of the call's round (staging 3)", db.asked)
	}

	db, _ = fresh()
	db.restaged = true
	if got, moved := notOwnedRecording(ctx, call, db, n, at); got != nil || !moved {
		t.Fatalf("the store said the replay staged another set: named %v (restaged %v), want none, restaged", got, moved)
	}

	db, _ = fresh()
	db.refuse = true
	if got, _ := notOwnedRecording(ctx, call, db, n, at); got != nil || len(db.accepted) != 1 {
		t.Fatalf("a recording whose name the store refused was named: %v (accepted %v)", got, db.accepted)
	}
}

// keploy mock replay's mapping-edit miss names the test whose window the call
// was made in, as the call's first read found it (its sequence view's test),
// and says so between tests.
func TestKeployMocksMappingEditNamesTheTestTheCallWasMadeIn(t *testing.T) {
	m := onHost("mock-3", "/delete", "localhost:1")
	for _, tc := range []struct{ test, want string }{
		{"o::TestA", "This call, made in o::TestA's window, is mock-3"},
		{"", "This call, made between tests, is mock-3"},
	} {
		r := &models.MockMismatchReport{}
		call := integrations.NewCallRound(1, nil).InRouting(models.UnroutedUnstaged).WithSequence(integrations.SequenceView{Test: tc.test})
		markNotOwned(r, m, call)
		if !strings.HasPrefix(r.NextSteps, tc.want) {
			t.Fatalf("in %q the miss says %q; want it to begin %q", tc.test, r.NextSteps, tc.want)
		}
	}
}

// keploy mock replay's mapping-edit miss names, beside the window the call was
// made in, the tests whose lists in mappings.yaml hold its recording, as the
// call's first read found them (CallRound.ListedFor), and the edit for that
// place: out of their lists to worker:, or, for a recording no test lists
// (startup:, another test process's), into the worker: of the process that
// made the call, never to where it already is. With no listing it names the
// receipt.
func TestKeployMocksMappingEditNamesTheTestsItsRecordingIsListedFor(t *testing.T) {
	m := onHost("mock-3", "/delete", "localhost:1")
	for _, tc := range []struct {
		name   string
		listed map[string][]string
		want   []string
		never  []string
	}{
		{"one test", map[string][]string{"mock-3": {"o::TestA"}, "mock-1": {"o::TestB"}},
			[]string{"This call, made in o::TestB's window, is mock-3, which mappings.yaml lists for o::TestA.", "move mock-3 out of o::TestA's list to worker: in mappings.yaml"},
			[]string{"recorded_in", "lists for no test"}},
		{"two tests", map[string][]string{"mock-3": {"o::TestA", "o::TestC"}},
			[]string{"which mappings.yaml lists for o::TestA and o::TestC.", "move mock-3 out of the lists of o::TestA and o::TestC to worker:"},
			nil},
		{"three tests", map[string][]string{"mock-3": {"o::TestA", "o::TestC", "o::TestD"}},
			[]string{"which mappings.yaml lists for o::TestA, o::TestC and o::TestD."},
			nil},
		{"no test", map[string][]string{"mock-1": {"o::TestB"}},
			[]string{"which mappings.yaml lists for no test: in startup:, which answers between tests only, or for another test process.", "move mock-3 to the worker: of the test process that made the call"},
			[]string{"move mock-3 to worker: in mappings.yaml", "recorded_in", "from a place this replay had not reached"}},
		{"no listing", nil,
			[]string{"which mappings.yaml lists where this replay did not make it: for another test (the receipt names it under recorded_in), in startup:, which answers between tests only, or for another test process."},
			[]string{"from a place this replay had not reached"}},
	} {
		r := &models.MockMismatchReport{}
		call := integrations.NewCallRound(1, nil).InRouting(models.UnroutedUnstaged).InListing(tc.listed).WithSequence(integrations.SequenceView{Test: "o::TestB"})
		markNotOwned(r, m, call)
		for _, want := range tc.want {
			if !strings.Contains(r.NextSteps, want) {
				t.Errorf("%s: the miss does not say %q: %q", tc.name, want, r.NextSteps)
			}
		}
		for _, never := range append(tc.never, "record again", "judging") {
			if strings.Contains(r.NextSteps, never) {
				t.Errorf("%s: the miss says %q: %q", tc.name, never, r.NextSteps)
			}
		}
	}
	// keploy test's set never reads the listing: record's judging is told.
	r := &models.MockMismatchReport{}
	markNotOwned(r, m, integrations.NewCallRound(1, nil).InRouting(models.UnroutedUnmarked).InListing(map[string][]string{"mock-3": {"o::TestA"}}))
	if r.NextSteps != notOwnedNextSteps("mock-3") {
		t.Fatalf("an unmarked keploy test set was told %q", r.NextSteps)
	}
}
