package http

import (
	"errors"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/variables"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// clusterGiven is the cluster name the test gave as given:cluster: aaa-conn-1794544425372
// when recorded, now in this run.
func clusterGiven(now string) variables.Binding {
	return variables.Binding{Name: "given:cluster", Value: now, Recorded: "aaa-conn-1794544425372", Mock: "test o::TestClusters", Place: "clusters.spec.ts:126"}
}

// gaveDb is a store whose running test gave each of given.
func gaveDb(t *testing.T, mocks []*models.Mock, given ...variables.Binding) *varDb {
	t.Helper()
	db := &varDb{perTest: mocks}
	db.table.Start(nil)
	for _, g := range given {
		if _, ok := db.table.Give(g); !ok {
			t.Fatalf("the test's given %s did not bind", g.Name)
		}
	}
	return db
}

// listMock is a recording of a read whose answer is body, with no variables.
func listMock(body string) *models.Mock {
	return at(varMock("mock-2", "GET", "http://store/cluster/list", "", 200, body, nil), 2)
}

// The page lists the clusters the test created: the list read after the
// create answers with the names the test gave in this run, where the
// recording holds the recorded name whole, and with every other value as
// recorded: another name, and one that holds the recorded name inside it.
func TestGivenAnswers_TheListReadAfterACreateNamesThisRunsValue(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	create := at(onPay(varMock("mock-1", "POST", "/cluster/provision", `{"name":"aaa-conn-1794544425372"}`, 201, `{"id":"c1","name":"aaa-conn-1794544425372"}`,
		models.MockVariables{"given:cluster": useV("aaa-conn-1794544425372", []string{"body.name"}, "body.name")})), 1)
	list := at(onPay(varMock("mock-2", "GET", "/cluster/list", "", 200,
		`{"clusters":[{"name":"aaa-conn-1794544425372","connected":true},{"name":"aaa-conn-17945444253720"},{"name":"copy of aaa-conn-1794544425372"}]}`, nil)), 2)
	db := gaveDb(t, []*models.Mock{create, list}, clusterGiven("aaa-conn-1794544499999"))
	ctx := openTest()
	if got := serveCall(t, ctx, db, false, "POST", "/cluster/provision", `{"name":"aaa-conn-1794544499999"}`); got.status != 201 || got.body != `{"id":"c1","name":"aaa-conn-1794544499999"}` {
		t.Fatalf("the create got %d %q (reports %v); want mock-1's answer with this run's name", got.status, got.body, got.reported)
	}
	got := serveCall(t, ctx, db, false, "GET", "/cluster/list", "")
	want := `{"clusters":[{"name":"aaa-conn-1794544499999","connected":true},{"name":"aaa-conn-17945444253720"},{"name":"copy of aaa-conn-1794544425372"}]}`
	if got.status != 200 || got.body != want {
		t.Fatalf("the list read after the create got %d %q (reports %v); want %q", got.status, got.body, got.reported, want)
	}
	if list.Spec.HTTPResp.Body != `{"clusters":[{"name":"aaa-conn-1794544425372","connected":true},{"name":"aaa-conn-17945444253720"},{"name":"copy of aaa-conn-1794544425372"}]}` {
		t.Fatalf("the recording itself was changed: %q", list.Spec.HTTPResp.Body)
	}
}

// Only a given the test gave another value than when recorded is answered
// with this run's: a given of the same value, a learned variable (not a
// value the test gave) and a store that keeps no variables leave the answer
// as recorded, the recording itself served.
func TestGivenAnswers_OnlyAGivenOfAnotherValueIsAnswered(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	body := `{"clusters":[{"name":"aaa-conn-1794544425372"}]}`
	same := listMock(body)
	if got := h.givenAnswers(same, gaveDb(t, nil, clusterGiven("aaa-conn-1794544425372"))); got != same {
		t.Fatalf("a given of the recorded value served %q, not the recording", got.Spec.HTTPResp.Body)
	}
	learned := gaveDb(t, nil)
	learned.table.Bind(variables.Binding{Name: "clusterName", Value: "aaa-2", Recorded: "aaa-conn-1794544425372", Mock: "mock-1", Place: "body.name"})
	m := listMock(body)
	if got := h.givenAnswers(m, learned); got != m {
		t.Fatalf("a learned variable answered %q; only a value the test gave is", got.Spec.HTTPResp.Body)
	}
	m = listMock(body)
	if got := h.givenAnswers(m, &mockMemDb{}); got != m {
		t.Fatalf("a store with no variables answered %q", got.Spec.HTTPResp.Body)
	}
	// A test case's template key keploy test gave is no value the test gave
	// through keploy's SDKs: its echoes are answered as recorded.
	key := &varDb{}
	key.table.Start([]variables.Binding{{Name: "clusterName", Value: "aaa-2", Recorded: "aaa-conn-1794544425372", Mock: "test case list-1", Place: "body.name"}})
	m = listMock(body)
	if got := h.givenAnswers(m, key); got != m {
		t.Fatalf("a test case's template key answered %q", got.Spec.HTTPResp.Body)
	}
	m = listMock(body)
	if got := h.givenAnswers(m, gaveDb(t, nil, clusterGiven("aaa-2"))); got == m || got.Spec.HTTPResp.Body != `{"clusters":[{"name":"aaa-2"}]}` {
		t.Fatalf("a given of another value served %q", got.Spec.HTTPResp.Body)
	}
}

// A header whose one value is the recorded value whole, and a JSON number
// the given was recorded as, answer this run's value; a header that holds it
// inside a longer value is left as recorded.
func TestGivenAnswers_AHeaderAndANumberAnswerTheValue(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	m := listMock(`{"run":1794544425372001,"name":"x"}`)
	m.Spec.HTTPResp.Header["X-Cluster"] = "aaa-conn-1794544425372"
	m.Spec.HTTPResp.Header["Location"] = "/clusters/aaa-conn-1794544425372"
	run := variables.Binding{Name: "given:run", Value: "1794544499999001", Recorded: "1794544425372001", Mock: "test o::TestClusters"}
	got := h.givenAnswers(m, gaveDb(t, nil, clusterGiven("aaa-2"), run))
	resp := got.Spec.HTTPResp
	if resp.Body != `{"run":1794544499999001,"name":"x"}` || resp.Header["X-Cluster"] != "aaa-2" || resp.Header["Location"] != "/clusters/aaa-conn-1794544425372" {
		t.Fatalf("served body %q, X-Cluster %q, Location %q", resp.Body, resp.Header["X-Cluster"], resp.Header["Location"])
	}
	if m.Spec.HTTPResp.Header["X-Cluster"] != "aaa-conn-1794544425372" {
		t.Fatal("the recording's own header was changed")
	}
}

// A value the test gave that cannot stand where the recording holds its
// recorded value (text where it holds a number) serves the answer as
// recorded, and says so.
func TestGivenAnswers_AValueThatCannotStandThereServesTheRecording(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	core, logs := observer.New(zapcore.InfoLevel)
	h := &HTTP{Logger: zap.New(core)}
	m := listMock(`{"run":1794544425372001}`)
	run := variables.Binding{Name: "given:run", Value: "run-9", Recorded: "1794544425372001", Mock: "test o::TestClusters"}
	if got := h.givenAnswers(m, gaveDb(t, nil, run)); got != m {
		served := "no mock"
		if got != nil && got.Spec.HTTPResp != nil {
			served = got.Spec.HTTPResp.Body
		}
		t.Fatalf("served %q; want the recording", served)
	}
	if n := logs.FilterMessageSnippet("the value the test gives now cannot stand there").Len(); n != 1 {
		t.Fatalf("%d warnings say the answer is served as recorded; want 1 (logs %v)", n, logs.All())
	}
}

// An answer that holds no value the test gave at a place is served as
// recorded, and nothing is logged of it: one that holds none, and a text
// answer (no JSON) that is the value whole, which no variable is answered
// at, as the learn step answers none there.
func TestGivenAnswers_AnAnswerWithoutTheValueSaysNothing(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	core, logs := observer.New(zapcore.InfoLevel)
	h := &HTTP{Logger: zap.New(core)}
	m := listMock(`{"clusters":[]}`)
	if got := h.givenAnswers(m, gaveDb(t, nil, clusterGiven("aaa-2"))); got != m {
		t.Fatalf("served %q; want the recording", got.Spec.HTTPResp.Body)
	}
	text := listMock("aaa-conn-1794544425372")
	text.Spec.HTTPResp.Header["Content-Type"] = "text/plain"
	if got := h.givenAnswers(text, gaveDb(t, nil, clusterGiven("aaa-2"))); got != text {
		t.Fatalf("a text answer served %q; want the recording", got.Spec.HTTPResp.Body)
	}
	if logs.Len() != 0 {
		t.Fatalf("logged %v of an answer that holds no given", logs.All())
	}
	m = listMock(`{"clusters":[{"name":"aaa-conn-1794544425372"}]}`)
	_ = h.givenAnswers(m, gaveDb(t, nil, clusterGiven("aaa-2")))
	if n := logs.FilterMessageSnippet("answered a recorded answer with the values the test gave").Len(); n != 1 {
		t.Fatalf("%d lines say the answer was answered with a given; want 1 (logs %v)", n, logs.All())
	}
}

// A response the agent's disk store keeps apart is read back and answered
// with the given; one that cannot be read back is left to the serve, which
// says why.
func TestGivenAnswers_AResponseKeptApartIsReadBack(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	kept := func(load func() (*models.HTTPResp, []models.MongoResponse, error)) *models.Mock {
		m := listMock("")
		m.Spec.HTTPResp = nil
		m.SetResponseHydrator(load)
		return m
	}
	m := kept(func() (*models.HTTPResp, []models.MongoResponse, error) {
		return &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: `{"name":"aaa-conn-1794544425372"}`}, nil, nil
	})
	got := h.givenAnswers(m, gaveDb(t, nil, clusterGiven("aaa-2")))
	if got.Spec.HTTPResp == nil || got.Spec.HTTPResp.Body != `{"name":"aaa-2"}` {
		t.Fatalf("a response kept apart served %+v", got.Spec.HTTPResp)
	}
	m = kept(func() (*models.HTTPResp, []models.MongoResponse, error) { return nil, nil, errors.New("gone") })
	if got := h.givenAnswers(m, gaveDb(t, nil, clusterGiven("aaa-2"))); got != m || !m.HasSpilledResponse() {
		t.Fatal("a response that cannot be read back was not left to the serve")
	}
}

// aheadGaveDb is gaveDb with the scope of the next test, begun while the
// open one ends (integrations.VariableScopes): ahead (nil: none begun)
// holds the next test's values, and the recording named aheadOf is that
// test's.
type aheadGaveDb struct {
	*varDb
	ahead   *variables.Table
	aheadOf string
}

func (d *aheadGaveDb) AheadVariables() *variables.Table { return d.ahead }

func (d *aheadGaveDb) InAheadScope(m *models.Mock) bool { return m.Name == d.aheadOf }

// A recording of the next test, begun while the open one ends, answers the
// value that test gave; any other recording, and every recording while no
// next test began, the open test's.
func TestGivenAnswers_TheNextTestsRecordingAnswersItsValue(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	var next variables.Table
	next.Start(nil)
	next.Give(clusterGiven("aaa-3"))
	db := &aheadGaveDb{varDb: gaveDb(t, nil, clusterGiven("aaa-2")), ahead: &next, aheadOf: "mock-2"}
	if got := h.givenAnswers(listMock(`{"name":"aaa-conn-1794544425372"}`), db); got.Spec.HTTPResp.Body != `{"name":"aaa-3"}` {
		t.Fatalf("the next test's recording answered %q; want that test's value", got.Spec.HTTPResp.Body)
	}
	db.aheadOf = "mock-9"
	if got := h.givenAnswers(listMock(`{"name":"aaa-conn-1794544425372"}`), db); got.Spec.HTTPResp.Body != `{"name":"aaa-2"}` {
		t.Fatalf("the open test's recording answered %q; want its value", got.Spec.HTTPResp.Body)
	}
	db.ahead, db.aheadOf = nil, "mock-2"
	if got := h.givenAnswers(listMock(`{"name":"aaa-conn-1794544425372"}`), db); got.Spec.HTTPResp.Body != `{"name":"aaa-2"}` {
		t.Fatalf("with no next test begun the recording answered %q; want the open test's value", got.Spec.HTTPResp.Body)
	}
}

// A given recorded shorter than minAnswerGiven is answered in no later
// answer: where the answer holds it whole, it is served as recorded there and
// the log names the given, the place and the shortest length answered, while
// a longer given in the same answer is answered. The run's clock alone (13
// digits) and a token one byte short are such givens; a token recorded at
// that length is answered, and the value the test gives now may be any
// length.
func TestGivenAnswers_AGivenRecordedTooShortIsNotAnswered(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	core, logs := observer.New(zapcore.InfoLevel)
	h := &HTTP{Logger: zap.New(core)}
	const snippet = "recorded shorter than the shortest one keploy answers"
	run := variables.Binding{Name: "given:run", Value: "1794544499999", Recorded: "1794544425372", Mock: "test o::TestClusters"}
	m := listMock(`{"run":1794544425372,"name":"aaa-conn-1794544425372"}`)
	got := h.givenAnswers(m, gaveDb(t, nil, run, clusterGiven("b")))
	if got == m || got.Spec.HTTPResp.Body != `{"run":1794544425372,"name":"b"}` {
		t.Fatalf("served %q; want the run as recorded and the name answered", got.Spec.HTTPResp.Body)
	}
	warns := logs.FilterMessageSnippet(snippet)
	if warns.Len() != 1 {
		t.Fatalf("%d warnings name the short given; want 1 (logs %v)", warns.Len(), logs.All())
	}
	fields := warns.All()[0].ContextMap()
	if places, _ := fields["places"].([]interface{}); len(places) != 1 || places[0] != "given:run at body.run" {
		t.Fatalf("the warning names places %v; want the run's one", fields["places"])
	}
	if fields["shortest"] != int64(minAnswerGiven) || minAnswerGiven != 16 {
		t.Fatalf("the warning names the shortest length %v (minAnswerGiven %d); want 16", fields["shortest"], minAnswerGiven)
	}
	alone := listMock(`{"run":1794544425372}`)
	if got := h.givenAnswers(alone, gaveDb(t, nil, run)); got != alone {
		t.Fatalf("a short given alone served %q; want the recording", got.Spec.HTTPResp.Body)
	}
	fifteen := variables.Binding{Name: "given:token", Value: "t", Recorded: "tok-0123456789a", Mock: "test o::TestClusters"}
	m = listMock(`{"token":"tok-0123456789a"}`)
	if got := h.givenAnswers(m, gaveDb(t, nil, fifteen)); got != m {
		t.Fatalf("a given recorded one byte short served %q; want the recording", got.Spec.HTTPResp.Body)
	}
	sixteen := variables.Binding{Name: "given:token", Value: "t", Recorded: "tok-0123456789ab", Mock: "test o::TestClusters"}
	m = listMock(`{"token":"tok-0123456789ab"}`)
	if got := h.givenAnswers(m, gaveDb(t, nil, sixteen)); got == m || got.Spec.HTTPResp.Body != `{"token":"t"}` {
		t.Fatalf("a given recorded at the shortest length served %q; want it answered", got.Spec.HTTPResp.Body)
	}
	// One more warning each of the run alone and of the token one byte
	// short; none of the token at the shortest length.
	if n := logs.FilterMessageSnippet(snippet).Len(); n != 3 {
		t.Fatalf("%d warnings name a short given in all; want 3 (logs %v)", n, logs.All())
	}
}

// A given recorded short that the answer does not hold whole says nothing:
// an answer without it, and one that holds it inside a longer value.
func TestGivenAnswers_AShortGivenTheAnswerDoesNotHoldSaysNothing(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	core, logs := observer.New(zapcore.InfoLevel)
	h := &HTTP{Logger: zap.New(core)}
	short := variables.Binding{Name: "given:app", Value: "app-9", Recorded: "app-1", Mock: "test o::TestApps"}
	for _, body := range []string{`{"apps":[]}`, `{"apps":["app-10","copy of app-1"]}`} {
		m := listMock(body)
		if got := h.givenAnswers(m, gaveDb(t, nil, short)); got != m {
			t.Fatalf("served %q for %s; want the recording", got.Spec.HTTPResp.Body, body)
		}
	}
	if logs.Len() != 0 {
		t.Fatalf("logged %v of answers that do not hold the short given whole", logs.All())
	}
}
