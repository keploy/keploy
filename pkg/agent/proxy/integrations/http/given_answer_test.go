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

// clusterGiven is the cluster name the test gave as given:cluster: aaa-1
// when recorded, now in this run.
func clusterGiven(now string) variables.Binding {
	return variables.Binding{Name: "given:cluster", Value: now, Recorded: "aaa-1", Mock: "test o::TestClusters", Place: "clusters.spec.ts:126"}
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
	create := at(onPay(varMock("mock-1", "POST", "/cluster/provision", `{"name":"aaa-1"}`, 201, `{"id":"c1","name":"aaa-1"}`,
		models.MockVariables{"given:cluster": useV("aaa-1", []string{"body.name"}, "body.name")})), 1)
	list := at(onPay(varMock("mock-2", "GET", "/cluster/list", "", 200,
		`{"clusters":[{"name":"aaa-1","connected":true},{"name":"aaa-10"},{"name":"copy of aaa-1"}]}`, nil)), 2)
	db := gaveDb(t, []*models.Mock{create, list}, clusterGiven("aaa-1794544425372"))
	ctx := openTest()
	if got := serveCall(t, ctx, db, false, "POST", "/cluster/provision", `{"name":"aaa-1794544425372"}`); got.status != 201 || got.body != `{"id":"c1","name":"aaa-1794544425372"}` {
		t.Fatalf("the create got %d %q (reports %v); want mock-1's answer with this run's name", got.status, got.body, got.reported)
	}
	got := serveCall(t, ctx, db, false, "GET", "/cluster/list", "")
	want := `{"clusters":[{"name":"aaa-1794544425372","connected":true},{"name":"aaa-10"},{"name":"copy of aaa-1"}]}`
	if got.status != 200 || got.body != want {
		t.Fatalf("the list read after the create got %d %q (reports %v); want %q", got.status, got.body, got.reported, want)
	}
	if list.Spec.HTTPResp.Body != `{"clusters":[{"name":"aaa-1","connected":true},{"name":"aaa-10"},{"name":"copy of aaa-1"}]}` {
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
	body := `{"clusters":[{"name":"aaa-1"}]}`
	same := listMock(body)
	if got := h.givenAnswers(same, gaveDb(t, nil, clusterGiven("aaa-1"))); got != same {
		t.Fatalf("a given of the recorded value served %q, not the recording", got.Spec.HTTPResp.Body)
	}
	learned := gaveDb(t, nil)
	learned.table.Bind(variables.Binding{Name: "clusterName", Value: "aaa-2", Recorded: "aaa-1", Mock: "mock-1", Place: "body.name"})
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
	key.table.Start([]variables.Binding{{Name: "clusterName", Value: "aaa-2", Recorded: "aaa-1", Mock: "test case list-1", Place: "body.name"}})
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
	m := listMock(`{"run":1794544425372,"name":"x"}`)
	m.Spec.HTTPResp.Header["X-Cluster"] = "aaa-1"
	m.Spec.HTTPResp.Header["Location"] = "/clusters/aaa-1"
	run := variables.Binding{Name: "given:run", Value: "1794544499999", Recorded: "1794544425372", Mock: "test o::TestClusters"}
	got := h.givenAnswers(m, gaveDb(t, nil, clusterGiven("aaa-2"), run))
	resp := got.Spec.HTTPResp
	if resp.Body != `{"run":1794544499999,"name":"x"}` || resp.Header["X-Cluster"] != "aaa-2" || resp.Header["Location"] != "/clusters/aaa-1" {
		t.Fatalf("served body %q, X-Cluster %q, Location %q", resp.Body, resp.Header["X-Cluster"], resp.Header["Location"])
	}
	if m.Spec.HTTPResp.Header["X-Cluster"] != "aaa-1" {
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
	m := listMock(`{"run":1794544425372}`)
	run := variables.Binding{Name: "given:run", Value: "run-9", Recorded: "1794544425372", Mock: "test o::TestClusters"}
	if got := h.givenAnswers(m, gaveDb(t, nil, run)); got != m {
		served := "no mock"
		if got != nil && got.Spec.HTTPResp != nil {
			served = got.Spec.HTTPResp.Body
		}
		t.Fatalf("served %q; want the recording", served)
	}
	if n := logs.FilterMessageSnippet("keploy serves it as recorded").Len(); n != 1 {
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
	text := listMock("aaa-1")
	text.Spec.HTTPResp.Header["Content-Type"] = "text/plain"
	if got := h.givenAnswers(text, gaveDb(t, nil, clusterGiven("aaa-2"))); got != text {
		t.Fatalf("a text answer served %q; want the recording", got.Spec.HTTPResp.Body)
	}
	if logs.Len() != 0 {
		t.Fatalf("logged %v of an answer that holds no given", logs.All())
	}
	m = listMock(`{"clusters":[{"name":"aaa-1"}]}`)
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
		return &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: `{"name":"aaa-1"}`}, nil, nil
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
	if got := h.givenAnswers(listMock(`{"name":"aaa-1"}`), db); got.Spec.HTTPResp.Body != `{"name":"aaa-3"}` {
		t.Fatalf("the next test's recording answered %q; want that test's value", got.Spec.HTTPResp.Body)
	}
	db.aheadOf = "mock-9"
	if got := h.givenAnswers(listMock(`{"name":"aaa-1"}`), db); got.Spec.HTTPResp.Body != `{"name":"aaa-2"}` {
		t.Fatalf("the open test's recording answered %q; want its value", got.Spec.HTTPResp.Body)
	}
	db.ahead, db.aheadOf = nil, "mock-2"
	if got := h.givenAnswers(listMock(`{"name":"aaa-1"}`), db); got.Spec.HTTPResp.Body != `{"name":"aaa-2"}` {
		t.Fatalf("with no next test begun the recording answered %q; want the open test's value", got.Spec.HTTPResp.Body)
	}
}
