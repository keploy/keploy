package replay

import (
	"context"
	"errors"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	httpMatcher "go.keploy.io/server/v3/pkg/matcher/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

const (
	idRecorded = "0f8fad5b-d9cb-469f-a165-70867728950e"
	idLive     = "7c9e6679-7425-40de-944b-e07fc1f90ae7"
)

// pairInstr is an instrumentation that can read the agent's id pairs.
type pairInstr struct {
	Instrumentation
	pairs map[string]string
	err   error
}

func (p *pairInstr) GetIDPairs(context.Context) (map[string]string, error) { return p.pairs, p.err }

func readBack() *models.TestCase {
	return &models.TestCase{
		Kind: models.HTTP,
		Name: "get-order",
		HTTPReq: models.HTTPReq{
			Method:    "GET",
			URL:       "http://app/orders/" + idRecorded + "?ref=" + idRecorded,
			URLParams: map[string]string{"ref": idRecorded},
			Header:    map[string]string{"X-Order": idRecorded, "Accept": "*/*"},
			Body:      `{"id":"` + idRecorded + `","ref":"order-` + idRecorded + `"}`,
			Form:      []models.FormData{{Key: "order", Values: []string{idRecorded, "plain"}}},
			Timestamp: time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC),
		},
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Header:     map[string]string{"Location": "/orders/" + idRecorded},
			Body:       `{"id":"` + idRecorded + `","name":"widget"}`,
		},
	}
}

// replayerWith is a replayer of a run that starts the app and serves it
// mocks, as keploy test does by default.
func replayerWith(instr Instrumentation) *Replayer {
	cfg := &config.Config{}
	cfg.Test.Mocking = true
	return &Replayer{config: cfg, logger: zap.NewNop(), instrumentation: instr, instrument: true}
}

// following is a follower of the given recorded ids, as a replay makes one
// for a test set whose templates name them: each produced by one test case's
// response and reused by the next one's request.
func following(r *Replayer, recorded ...string) *runIDs {
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	template := map[string]interface{}{}
	var set []*models.TestCase
	for i, id := range recorded {
		key := "id" + strconv.Itoa(i)
		template[key] = id
		set = append(set,
			&models.TestCase{Kind: models.HTTP, Name: key + "-made", HTTPReq: models.HTTPReq{Timestamp: at.Add(time.Duration(2*i) * time.Second)}, HTTPResp: models.HTTPResp{Body: `{"id":"` + id + `"}`}},
			&models.TestCase{Kind: models.HTTP, Name: key + "-used", HTTPReq: models.HTTPReq{URL: "http://app/x/" + id, Timestamp: at.Add(time.Duration(2*i+1) * time.Second)}})
	}
	return r.newRunIDs("set", set, template)
}

// flow is the canonical test set: a create whose response carries the id the
// app made, and a read-back that names it.
func flow() []*models.TestCase {
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	create := &models.TestCase{
		Kind: models.HTTP, Name: "create",
		HTTPReq:  models.HTTPReq{Method: "POST", URL: "http://app/orders", Body: `{"name":"widget"}`, Timestamp: at},
		HTTPResp: models.HTTPResp{StatusCode: 201, Body: `{"id":"` + idRecorded + `"}`},
	}
	read := readBack()
	read.HTTPReq.Timestamp = at.Add(time.Second)
	return []*models.TestCase{read, create} // loaded out of order
}

// A replay follows the app-random ids in its templates — and none when it
// could not follow them on both sides.
//
// Why a set is left alone is a Debug line, for every reason: a set that is
// left alone replays as it always has, and has nothing to tell a user who did
// not ask.
func TestRunIDs_FollowsWhatTheTemplatesDeclare(t *testing.T) {
	template := map[string]interface{}{"id": idRecorded, "contentType": "application/json", "n": 3}
	on := replayerWith(&pairInstr{})
	require.Equal(t, []string{idRecorded}, on.followedValues("set", flow(), template), "the app-random template value")
	l := on.newRunIDs("set", flow(), template)
	require.Equal(t, []string{idRecorded}, l.values())
	require.Equal(t, map[string]string{"id": idRecorded}, l.keys, "the template keys that hold a followed id")
	none := on.newRunIDs("set", flow(), nil)
	require.Nil(t, none)
	require.Nil(t, none.values())

	require.Nil(t, on.followedValues("set", flow(), nil), "no templates: nothing is followed")
	require.Nil(t, on.followedValues("set", flow(), map[string]interface{}{"contentType": "application/json"}), "no template is an id")
	require.Equal(t, []string{idRecorded}, on.followedValues("set", append(flow(), nil), template), "a nil test case is skipped")

	// A selection that names every test case of the set runs the whole set,
	// and one for another set says nothing about this one.
	whole := replayerWith(&pairInstr{})
	whole.config.Test.SelectedTests = map[string][]string{"set": {"create", "get-order"}, "other": {"create"}}
	require.Equal(t, []string{idRecorded}, whole.followedValues("set", flow(), template))
	whole.config.Test.SelectedTests = map[string][]string{"other": {"get-order"}}
	require.Equal(t, []string{idRecorded}, whole.followedValues("set", flow(), template))

	with := func(change func(r *Replayer)) func() *Replayer {
		return func() *Replayer {
			r := replayerWith(&pairInstr{})
			change(r)
			return r
		}
	}
	mixed := append(flow(), &models.TestCase{Kind: models.GRPC_EXPORT, Name: "g"})
	stream := append(flow(), &models.TestCase{Kind: models.HTTP, Name: "events",
		HTTPResp: models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "text/event-stream"}, Body: "data: {}\n\n"}})
	for name, c := range map[string]struct {
		replayer  func() *Replayer
		set       []*models.TestCase
		template  map[string]interface{}
		wantInLog string // "" = nothing is logged, at any level
	}{
		"rebinding is off":             {with(func(r *Replayer) { r.config.Test.DisableMockRebinding = true }), flow(), template, "disableMockRebinding"},
		"the app is not run by keploy": {with(func(r *Replayer) { r.instrument = false }), flow(), template, "--base-path"},
		"the run serves no mocks":      {with(func(r *Replayer) { r.config.Test.Mocking = false }), flow(), template, "serves no mocks"},
		"this instrumentation cannot read what the agent bound": {func() *Replayer {
			return replayerWith(struct{ Instrumentation }{})
		}, flow(), template, "cannot read"},
		"the set is replayed in several cycles": {with(func(r *Replayer) { r.config.RetryPassing = true }), flow(), template, "retryPassing"},
		"mocks are matched strictly":            {with(func(r *Replayer) { r.config.Test.MockNoiseStrict = true }), flow(), template, "mockNoiseStrict"},
		"only part of the set is selected": {with(func(r *Replayer) {
			r.config.Test.SelectedTests = map[string][]string{"set": {"get-order"}}
		}), flow(), template, "test case create is not run"},
		"a test case of a kind that is not rewritten": {with(func(*Replayer) {}), mixed, template, "test case g is of kind"},
		"a streaming test case":                       {with(func(*Replayer) {}), stream, template, "test case events answers with a stream"},
		// A set whose templates hold no id has nothing it could have followed.
		"several cycles, no id among the templates": {with(func(r *Replayer) { r.config.RetryPassing = true }), mixed, map[string]interface{}{"contentType": "application/json"}, ""},
	} {
		core, logs := observer.New(zap.DebugLevel)
		r := c.replayer()
		r.logger = zap.New(core)
		require.Nil(t, r.followedValues("set", c.set, c.template), name)
		require.Nil(t, r.newRunIDs("set", c.set, c.template), name)
		require.Zero(t, logs.FilterLevelExact(zap.InfoLevel).Len()+logs.FilterLevelExact(zap.WarnLevel).Len()+logs.FilterLevelExact(zap.ErrorLevel).Len(), "%s: nothing is said above Debug", name)
		if c.wantInLog == "" {
			require.Zero(t, logs.Len(), name)
			continue
		}
		require.Equal(t, 2, logs.Len(), "%s: one Debug line a call", name)
		require.Contains(t, logs.All()[0].ContextMap()["why"], c.wantInLog, name)
	}
}

// templatized is flow() as `keploy templatize` leaves it: the id the create's
// response produced and the read-back reused is a placeholder in both, and
// its recorded value is in the set's template map.
func templatized() ([]*models.TestCase, map[string]interface{}) {
	set := flow()
	read, create := set[0], set[1]
	create.HTTPResp.Body = `{"id":"{{string .id}}"}`
	read.HTTPReq.URL = "http://app/orders/{{string .id}}?ref={{string .id}}"
	read.HTTPReq.Header["X-Order"] = "{{string .id}}"
	read.HTTPResp.Body = `{"id":"{{string .id}}","name":"widget"}`
	return set, map[string]interface{}{"id": idRecorded}
}

// Test cases are read as they were recorded: a placeholder stands for its
// template value, so an id templatize chained is followed, and one the client
// supplied — chained from a request, not from a response — is not.
func TestRunIDs_ReadsPlaceholdersAsTheirRecordedValues(t *testing.T) {
	set, template := templatized()
	r := replayerWith(&pairInstr{})
	require.Equal(t, []string{idRecorded}, r.followedValues("set", set, template), "produced by the create's response, reused by the read-back")

	// The client names the order in the create's own request, the app echoes
	// it, and a later request reuses it: every occurrence is a placeholder.
	const supplied = "9b2f1c3e-5d4a-4e8f-8a1b-2c3d4e5f6a7b"
	template["order"] = supplied
	read, create := set[0], set[1]
	create.HTTPReq.URL = "http://app/orders/{{string .order}}"
	create.HTTPResp.Body = `{"id":"{{string .id}}","order":"{{string .order}}"}`
	read.HTTPReq.Header["X-Parent"] = "{{ string .order }}"
	require.Equal(t, []string{idRecorded}, r.followedValues("set", set, template), "the client's value is not followed")

	// A value only requests carry (a token the client sends on every call).
	template["token"] = "5d41402abc4b2a76b9719d911017c592"
	create.HTTPReq.Header = map[string]string{"Authorization": "Bearer {{string .token}}"}
	read.HTTPReq.Header["Authorization"] = "Bearer {{string .token}}"
	require.Equal(t, []string{idRecorded}, r.followedValues("set", set, template), "no response produced it")

	// A placeholder that names no template renders nothing and is ignored.
	read.HTTPReq.Header["X-Other"] = "{{string .missing}} {{not a template"
	require.Equal(t, []string{idRecorded}, r.followedValues("set", set, template))
}

// A template value a test case supplies before any response carries it is
// the client's: it is not followed, so an app that ignores the id it was
// given and makes its own still fails its test.
func TestRunIDs_AClientSuppliedValueIsNotFollowed(t *testing.T) {
	const supplied = "9b2f1c3e-5d4a-4e8f-8a1b-2c3d4e5f6a7b"
	template := map[string]interface{}{"id": idRecorded, "order": supplied}
	set := flow()
	// The client names the order in the create's own request; the app echoes it.
	create := set[1]
	create.HTTPReq.URL = "http://app/orders/" + supplied
	create.HTTPResp.Body = `{"id":"` + idRecorded + `","order":"` + supplied + `"}`
	set[0].HTTPReq.Header["X-Parent"] = supplied // and a later request reuses it

	require.Equal(t, []string{idRecorded}, replayerWith(&pairInstr{}).followedValues("set", set, template))
}

// A nil follower, and one the agent has bound nothing for, leave a test case
// as it is.
func TestRunIDs_NothingBoundChangesNothing(t *testing.T) {
	ctx := context.Background()
	tc := readBack()
	var none *runIDs
	none.logSummary("set", nil)
	require.Same(t, tc, none.outgoing(ctx, tc))
	exp, used := none.expected(ctx, tc, &tc.HTTPResp)
	require.Same(t, tc, exp)
	require.Nil(t, used)

	l := following(replayerWith(&pairInstr{}), idRecorded)
	require.Same(t, tc, l.outgoing(ctx, tc), "the agent bound nothing")
	exp, used = l.expected(ctx, tc, &tc.HTTPResp)
	require.Same(t, tc, exp)
	require.Nil(t, used)
}

// A bound id is the live one in the request that is sent and in the answer
// that is expected — as a whole word, and on copies: the recorded test case
// keeps the recorded id.
func TestRunIDs_ABoundIDIsTheLiveOneOnBothSides(t *testing.T) {
	ctx := context.Background()
	instr := &pairInstr{}
	tc := readBack()
	l := following(replayerWith(instr), idRecorded)
	// Each use reads what the agent has bound by then.
	instr.pairs = map[string]string{idRecorded: idLive}

	sent := l.outgoing(ctx, tc)
	require.NotSame(t, tc, sent)
	require.Equal(t, "http://app/orders/"+idLive+"?ref="+idLive, sent.HTTPReq.URL)
	require.Equal(t, idLive, sent.HTTPReq.URLParams["ref"])
	require.Equal(t, idLive, sent.HTTPReq.Header["X-Order"])
	require.Equal(t, "*/*", sent.HTTPReq.Header["Accept"])
	require.JSONEq(t, `{"id":"`+idLive+`","ref":"order-`+idRecorded+`"}`, sent.HTTPReq.Body, "a longer token that contains the id is another word")
	require.Equal(t, []string{idLive, "plain"}, sent.HTTPReq.Form[0].Values)
	require.Equal(t, tc.HTTPResp, sent.HTTPResp, "the request copy still expects what was recorded")

	exp, used := l.expected(ctx, sent, nil)
	require.JSONEq(t, `{"id":"`+idLive+`","name":"widget"}`, exp.HTTPResp.Body)
	require.Equal(t, "/orders/"+idLive, exp.HTTPResp.Header["Location"])
	require.Equal(t, map[string]string{idRecorded: idLive}, used, "only the pairs this expected response carried")

	require.Equal(t, readBack(), tc, "the recorded test case is never changed")

	// A test case of another kind is left alone.
	grpc := &models.TestCase{Kind: models.GRPC_EXPORT, Name: "g"}
	require.Same(t, grpc, l.outgoing(ctx, grpc))
}

// The expected response is rewritten with the pairs bound by the time the app
// answered: the one the test case itself made the app mint is among them.
func TestRunIDs_TheExpectedResponseSeesWhatItsOwnRequestBound(t *testing.T) {
	ctx := context.Background()
	instr := &pairInstr{}
	l := following(replayerWith(instr), idRecorded)
	tc := readBack()
	sent := l.outgoing(ctx, tc)
	require.Same(t, tc, sent, "nothing bound before the request")
	instr.pairs = map[string]string{idRecorded: idLive} // bound while the app handled it
	exp, used := l.expected(ctx, sent, nil)
	require.Contains(t, exp.HTTPResp.Body, idLive)
	require.Equal(t, map[string]string{idRecorded: idLive}, used)
}

// The live ids bound so far are settled: an answer must name them where its
// test case expects them, even in a set whose templates would otherwise learn
// whatever the app answered there.
func TestRunIDs_ABoundIDIsAssertedInTheAnswer(t *testing.T) {
	ctx := context.Background()
	const wrong = "9b2f1c3e-5d4a-4e8f-8a1b-2c3d4e5f6a7b"
	var none *runIDs
	require.Nil(t, none.settled())

	instr := &pairInstr{}
	r := replayerWith(instr)
	r.config.Test.DisableAutoHeaderNoise = true
	l := following(r, idRecorded)
	require.Nil(t, l.settled(), "nothing bound yet")
	instr.pairs = map[string]string{idRecorded: idLive}
	expected, _ := l.expected(ctx, readBack(), nil)
	require.Equal(t, map[string]bool{idLive: true}, l.settled())

	// The set's template holds the live id by now, as after the create.
	saved := utils.TemplatizedValues
	t.Cleanup(func() { utils.TemplatizedValues = saved })
	compare := func(answeredID string, opts ...httpMatcher.MatchOption) bool {
		utils.TemplatizedValues = map[string]interface{}{"id": idLive}
		resp := &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Location": "/orders/" + idLive}, Body: `{"id":"` + answeredID + `","name":"widget"}`}
		pass, _ := r.compareHTTPRespForReplay(expected, resp, "set", false, opts...)
		return pass
	}
	require.True(t, compare(idLive, httpMatcher.WithSettledValues(l.settled())))
	require.False(t, compare(wrong, httpMatcher.WithSettledValues(l.settled())), "the app answered with another id")
	require.True(t, compare(wrong), "without it, the template takes whatever was answered")
}

// Only the ids the replay follows are rewritten, whatever else the agent's
// table holds.
func TestRunIDs_OnlyFollowedIDsAreRewritten(t *testing.T) {
	ctx := context.Background()
	const other, otherLive = "9b2f1c3e-5d4a-4e8f-8a1b-2c3d4e5f6a7b", "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	pairs := map[string]string{idRecorded: idLive, other: otherLive}
	instr := &pairInstr{pairs: pairs}
	l := following(replayerWith(instr), other)
	tc := readBack()
	tc.HTTPReq.Header["X-Other"] = other
	tc.HTTPResp.Header["X-Other"] = other

	sent := l.outgoing(ctx, tc)
	require.Equal(t, otherLive, sent.HTTPReq.Header["X-Other"])
	require.Equal(t, tc.HTTPReq.URL, sent.HTTPReq.URL, "an id the replay does not follow stays as recorded")
	// Nor is one reported for an answer that names its live id.
	exp, used := l.expected(ctx, sent, &models.HTTPResp{Body: `{"id":"` + idLive + `"}`})
	require.Equal(t, tc.HTTPResp.Body, exp.HTTPResp.Body)
	require.Equal(t, map[string]string{other: otherLive}, used)
	require.Len(t, pairs, 2, "what the agent returned is not changed")
}

// A pair is never dropped within a set: a read that fails, and one that comes
// back without it (an agent that was replaced mid-set starts with an empty
// table), both keep what is known — the app has been using that id since it
// was bound. A read adds to the pairs held, and never replaces them.
func TestRunIDs_APairIsNeverDroppedWithinASet(t *testing.T) {
	ctx := context.Background()
	const other, otherLive = "9b2f1c3e-5d4a-4e8f-8a1b-2c3d4e5f6a7b", "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	instr := &pairInstr{pairs: map[string]string{idRecorded: idLive}}
	tc := readBack()
	tc.HTTPReq.Header["X-Other"] = other
	l := following(replayerWith(instr), idRecorded, other)
	require.Contains(t, l.outgoing(ctx, tc).HTTPReq.URL, idLive)

	instr.pairs, instr.err = nil, errors.New("agent gone")
	require.Contains(t, l.outgoing(ctx, tc).HTTPReq.URL, idLive, "a failed read")

	instr.pairs, instr.err = map[string]string{}, nil
	require.Contains(t, l.outgoing(ctx, tc).HTTPReq.URL, idLive, "a read that holds no pair")

	instr.pairs = map[string]string{other: otherLive}
	sent := l.outgoing(ctx, tc)
	require.Contains(t, sent.HTTPReq.URL, idLive, "a read that holds another pair adds it")
	require.Equal(t, otherLive, sent.HTTPReq.Header["X-Other"])
	require.Equal(t, map[string]bool{idLive: true, otherLive: true}, l.settled())
}

// A test case is reported with every pair it met (TestResult.RunIDs): those
// swapped into its expected response, and those whose live id the app's answer
// names, in its body or a header. The second kind is what an expected response
// that named the id through a placeholder leaves behind — keploy's templates
// have already put this run's value there by the time it is compared, so
// nothing is swapped — and `keploy normalize` needs it to put the recorded id
// back.
func TestRunIDs_ATestCaseIsReportedWithEveryPairItMet(t *testing.T) {
	ctx := context.Background()
	const other, otherLive = "9b2f1c3e-5d4a-4e8f-8a1b-2c3d4e5f6a7b", "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	instr := &pairInstr{pairs: map[string]string{idRecorded: idLive, other: otherLive}}
	l := following(replayerWith(instr), idRecorded, other)

	// As SimulateHTTP leaves a templated test case: the placeholder already
	// rendered with this run's id.
	rendered := readBack()
	rendered.HTTPResp.Body = `{"id":"` + idLive + `","name":"widget"}`
	rendered.HTTPResp.Header = nil
	exp, ids := l.expected(ctx, rendered, &models.HTTPResp{StatusCode: 200, Body: `{"id":"` + idLive + `","name":"gadget"}`})
	require.Same(t, rendered, exp, "nothing is swapped into an expected response that names no recorded id")
	require.Equal(t, map[string]string{idRecorded: idLive}, ids, "the pair whose live id the answer's body names")

	_, ids = l.expected(ctx, rendered, &models.HTTPResp{StatusCode: 201, Header: map[string]string{"Location": "/orders/" + otherLive}, Body: `{}`})
	require.Equal(t, map[string]string{other: otherLive}, ids, "the pair whose live id a header of the answer names")

	// Both kinds together: one swapped in, the other only named by the answer.
	tc := readBack()
	exp, ids = l.expected(ctx, tc, &models.HTTPResp{StatusCode: 200, Body: `{"id":"` + idLive + `","parent":"` + otherLive + `"}`})
	require.Contains(t, exp.HTTPResp.Body, idLive)
	require.Equal(t, map[string]string{idRecorded: idLive, other: otherLive}, ids)

	// An answer that names a live id only inside a longer token names another word.
	_, ids = l.expected(ctx, rendered, &models.HTTPResp{Body: `{"ref":"order-` + idLive + `"}`})
	require.Nil(t, ids)

	// Every pair reported is counted once in the set's summary.
	core, logs := observer.New(zap.InfoLevel)
	l.logger = zap.New(core)
	l.logSummary("set", nil)
	require.Equal(t, 1, logs.FilterMessageSnippet("made new ids").Len())
	require.EqualValues(t, 2, logs.All()[0].ContextMap()["ids"])
}

// What a set did not get is said only where it can be the reason a test case
// failed: once, for the failed test cases that name a followed id the agent
// bound nothing for, with the ids and their test cases.
//
// It is not said for every id that got no pair, nor on a passing set, nor in
// the report: an id an app keeps from run to run, or one whose test case was
// not reached, would be listed on every healthy run and read as a problem.
func TestRunIDs_SaysWhichFailedTestCasesNameAnIDThatWasNotFollowed(t *testing.T) {
	ctx := context.Background()
	const other = "9b2f1c3e-5d4a-4e8f-8a1b-2c3d4e5f6a7b"
	const said = "did not follow"
	run := func(pairs map[string]string, results ...models.TestResult) *observer.ObservedLogs {
		core, logs := observer.New(zap.InfoLevel)
		r := replayerWith(&pairInstr{pairs: pairs})
		r.logger = zap.New(core)
		l := following(r, idRecorded, other)
		l.outgoing(ctx, readBack())
		l.logSummary("set", results)
		return logs
	}
	result := func(name string, status models.TestStatus) models.TestResult {
		return models.TestResult{TestCaseID: name, Status: status}
	}
	bound := map[string]string{idRecorded: idLive}

	// following() names the test cases id0-made / id0-used for idRecorded and
	// id1-made / id1-used for other.
	require.Zero(t, run(bound, result("id0-used", models.TestStatusPassed), result("id1-used", models.TestStatusPassed)).Len(),
		"a set whose test cases pass has nothing to say")
	require.Zero(t, run(bound).Len(), "nor has a set that ran nothing")
	require.Zero(t, run(bound, result("id0-used", models.TestStatusFailed)).FilterMessageSnippet(said).Len(),
		"a failed test case that names only ids that were followed")
	require.Zero(t, run(bound, result("no-such-test", models.TestStatusFailed), result("id1-used", models.TestStatusObsolete), result("id1-made", models.TestStatusIgnored)).FilterMessageSnippet(said).Len(),
		"a test case that names no followed id, and ones that did not fail")

	logs := run(bound, result("id1-used", models.TestStatusFailed), result("id1-made", models.TestStatusFailed), result("id0-used", models.TestStatusFailed))
	require.Equal(t, 1, logs.Len(), "one line")
	line := logs.FilterMessageSnippet(said).All()
	require.Len(t, line, 1)
	require.Equal(t, map[string][]string{other: {"id1-made", "id1-used"}}, line[0].ContextMap()["first"], "the ids with no pair, and the failed test cases that name each, in order")
	require.Contains(t, line[0].ContextMap()["to_see_more"], "--debug", "where to look")

	// With no pair at all, every followed id a failed test case names.
	logs = run(nil, result("id0-used", models.TestStatusFailed), result("id1-used", models.TestStatusFailed))
	require.Equal(t, map[string][]string{idRecorded: {"id0-used"}, other: {"id1-used"}}, logs.FilterMessageSnippet(said).All()[0].ContextMap()["first"])
}

// The line names five ids at most, and says how many there are.
func TestRunIDs_TheNotFollowedLineNamesFiveIDsAtMost(t *testing.T) {
	var recorded []string
	var results []models.TestResult
	for i := 0; i < 7; i++ {
		recorded = append(recorded, "0f8fad5b-d9cb-469f-a165-7086772895"+strconv.Itoa(10+i))
		results = append(results, models.TestResult{TestCaseID: "id" + strconv.Itoa(i) + "-used", Status: models.TestStatusFailed})
	}
	core, logs := observer.New(zap.InfoLevel)
	r := replayerWith(&pairInstr{})
	r.logger = zap.New(core)
	following(r, recorded...).logSummary("set", results)
	require.Equal(t, 1, logs.Len())
	require.EqualValues(t, 7, logs.All()[0].ContextMap()["ids"])
	first := logs.All()[0].ContextMap()["first"].(map[string][]string)
	require.Len(t, first, 5)
	for _, id := range recorded[:5] {
		require.Contains(t, first, id)
	}
}

// Only an id WRITTEN OUT in a test case counts for that line. Where a
// placeholder stands for the id, keploy's templates send and expect this run's
// value whether or not the agent bound it, so that test case did not run with
// a stale id — and what the placeholder renders (a token, as often as an id)
// is not something to print.
func TestRunIDs_AnIDBehindAPlaceholderIsNotOneATestCaseRanWith(t *testing.T) {
	set, template := templatized() // the create names the id through a placeholder alone; the read-back also writes it out
	core, logs := observer.New(zap.InfoLevel)
	r := replayerWith(&pairInstr{})
	r.logger = zap.New(core)
	l := r.newRunIDs("set", set, template)
	require.Equal(t, []string{idRecorded}, l.values(), "precondition: the id is followed")
	require.Equal(t, map[string][]string{"get-order": {idRecorded}}, l.names)

	l.logSummary("set", []models.TestResult{{TestCaseID: "create", Status: models.TestStatusFailed}})
	require.Zero(t, logs.Len(), "a failed test case that names the id through a placeholder alone")
	l.logSummary("set", []models.TestResult{{TestCaseID: "get-order", Status: models.TestStatusFailed}})
	require.Equal(t, map[string][]string{idRecorded: {"get-order"}}, logs.All()[0].ContextMap()["first"])
}

// Only a generated UUID that a response produces is followed. Being in the
// template map says a value flows from a response into a later request, not
// that it changes from run to run: a name-based UUID, a digest or a token
// flows the same way, and one a regression altered must fail its test.
func TestRunIDs_OnlyAGeneratedUUIDAResponseProducesIsFollowed(t *testing.T) {
	r := replayerWith(&pairInstr{})
	at := time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)
	chained := func(values ...string) ([]*models.TestCase, map[string]interface{}) {
		template := map[string]interface{}{}
		var set []*models.TestCase
		for i, v := range values {
			key := "v" + strconv.Itoa(i)
			template[key] = v
			set = append(set,
				&models.TestCase{Kind: models.HTTP, Name: key + "-made", HTTPReq: models.HTTPReq{Timestamp: at.Add(time.Duration(2*i) * time.Second)}, HTTPResp: models.HTTPResp{Body: `{"v":"` + v + `"}`}},
				&models.TestCase{Kind: models.HTTP, Name: key + "-used", HTTPReq: models.HTTPReq{URL: "http://app/x/" + v, Timestamp: at.Add(time.Duration(2*i+1) * time.Second)}})
		}
		return set, template
	}
	set, template := chained(
		idRecorded,                             // a random UUID: followed
		"2ed6657d-e927-568b-95e1-2665a8aea6a2", // a name-based UUID
		"9f86d081884c7d659a2feaa0c55ad015a3bf4f1b2b0b822cd15d6c15b0f00a08", // a digest
		"5d41402abc4b2a76b9719d911017c592",                                 // a token
		"aB3dEfGhIjKlMnOpQrSt9",                                            // a nanoid
	)
	require.Equal(t, []string{idRecorded}, r.followedValues("set", set, template))

	// A generated UUID the templates hold but no response carries (left over
	// from an earlier recording, or supplied where this cannot see it).
	const stale = "9b2f1c3e-5d4a-4e8f-8a1b-2c3d4e5f6a7b"
	template["stale"] = stale
	require.Equal(t, []string{idRecorded}, r.followedValues("set", set, template), "in no test case at all")
	set = append(set, &models.TestCase{Kind: models.HTTP, Name: "uses-stale", HTTPReq: models.HTTPReq{URL: "http://app/x/" + stale, Timestamp: at.Add(time.Hour)}})
	require.Equal(t, []string{idRecorded}, r.followedValues("set", set, template), "in a request alone")
}

// A pair the agent bound is the template's value from then on, so a
// placeholder that stands for a followed id renders the id of this run even
// where no response taught the template: the id came back in a header, or the
// response that produced it no longer holds a placeholder.
func TestRunIDs_ABoundIDIsTheTemplatesValue(t *testing.T) {
	ctx := context.Background()
	saved := utils.TemplatizedValues
	t.Cleanup(func() { utils.TemplatizedValues = saved })
	instr := &pairInstr{}
	l := following(replayerWith(instr), idRecorded) // its template key is id0
	utils.TemplatizedValues = map[string]interface{}{"id0": idRecorded, "token": "kept"}

	l.outgoing(ctx, readBack())
	require.Equal(t, idRecorded, utils.TemplatizedValues["id0"], "nothing bound: the template is as it was")

	instr.pairs = map[string]string{idRecorded: idLive}
	l.outgoing(ctx, readBack())
	require.Equal(t, map[string]interface{}{"id0": idLive, "token": "kept"}, utils.TemplatizedValues)

	// It is held there: a response that taught the template another value
	// does not outlast the next read.
	utils.TemplatizedValues["id0"] = "9b2f1c3e-5d4a-4e8f-8a1b-2c3d4e5f6a7b"
	l.expected(ctx, readBack(), nil)
	require.Equal(t, idLive, utils.TemplatizedValues["id0"])

	// A set whose templates do not hold the key gets none added.
	utils.TemplatizedValues = map[string]interface{}{"token": "kept"}
	l.outgoing(ctx, readBack())
	require.Equal(t, map[string]interface{}{"token": "kept"}, utils.TemplatizedValues)
}

// The app's answer is compared with the test case as this run's ids make it
// and, when that fails, with the test case as recorded. A dependency call the
// agent answered as recorded hands the app the recorded id, and an app that
// passes it on does what it did when recorded: whatever passes without
// following passes with it. An id that is neither still fails.
func TestRunIDs_AnAnswerThatPassesAsRecordedStillPasses(t *testing.T) {
	const wrong = "9b2f1c3e-5d4a-4e8f-8a1b-2c3d4e5f6a7b"
	saved := utils.TemplatizedValues
	t.Cleanup(func() { utils.TemplatizedValues = saved })
	r := replayerWith(&pairInstr{})
	r.config.Test.DisableAutoHeaderNoise = true
	recorded := &models.TestCase{Kind: models.HTTP, Name: "read", HTTPResp: models.HTTPResp{StatusCode: 200, Body: `{"asked":"` + idRecorded + `","item":{"id":"` + idRecorded + `","name":"widget"}}`}}
	followed := *recorded
	followed.HTTPResp.Body = strings.ReplaceAll(recorded.HTTPResp.Body, idRecorded, idLive)
	settled := map[string]bool{idLive: true}
	compare := func(asked, item string) (bool, string) {
		utils.TemplatizedValues = map[string]interface{}{"id": idLive} // as after the create
		answer := &models.HTTPResp{StatusCode: 200, Body: `{"asked":"` + asked + `","item":{"id":"` + item + `","name":"widget"}}`}
		pass, result := r.compareFollowed(&followed, recorded, answer, "set", false, settled)
		return pass, result.BodyResult[0].Expected
	}

	pass, expected := compare(idLive, idLive)
	require.True(t, pass, "the answer names this run's id")
	require.Contains(t, expected, idLive)

	pass, expected = compare(idRecorded, idRecorded)
	require.True(t, pass, "the answer is the recorded one: it passes as it does without following")
	require.Contains(t, expected, idRecorded, "and is reported against the recording it matched")

	pass, expected = compare(idLive, idRecorded)
	require.False(t, pass, "half this run's, half recorded: neither the followed test case nor the recorded one")
	require.Contains(t, expected, idLive, "a failure is reported against the followed test case")

	pass, _ = compare(wrong, wrong)
	require.False(t, pass, "an id the app did not make")
	require.Equal(t, idLive, utils.TemplatizedValues["id"], "and the template is not taught it")

	// A test case with nothing of this run in it is compared once, as ever.
	pass, _ = r.compareFollowed(recorded, recorded, &models.HTTPResp{StatusCode: 200, Body: recorded.HTTPResp.Body}, "set", false, nil)
	require.True(t, pass)
}

// An id that was followed this run is written back to the set's templates as
// RECORDED, whatever the run made in its place: written over, it could never
// be followed again.
//
// Only an id the agent bound is kept so. An id the replay meant to follow and
// the agent did not bind — a set it left alone (it holds a database mock; the
// agent is older) — is written as the same run without rebinding writes it:
// with this run's value.
func TestRunIDs_AnIDFollowedThisRunIsWrittenBackAsRecorded(t *testing.T) {
	ctx := context.Background()
	const other, otherLive = "9b2f1c3e-5d4a-4e8f-8a1b-2c3d4e5f6a7b", "1a2b3c4d-5e6f-4a7b-8c9d-0e1f2a3b4c5d"
	instr := &pairInstr{}
	l := following(replayerWith(instr), idRecorded, other)
	after := map[string]interface{}{"id0": idLive, "id1": otherLive, "token": "fresh-token-of-this-run", "n": 4}

	l.outgoing(ctx, readBack())
	require.Equal(t, after, l.templatesToWrite(after), "nothing was bound: what the run made, as without rebinding")

	instr.pairs = map[string]string{idRecorded: idLive}
	l.outgoing(ctx, readBack())
	require.Equal(t, map[string]interface{}{"id0": idRecorded, "id1": otherLive, "token": "fresh-token-of-this-run", "n": 4}, l.templatesToWrite(after),
		"the id that got a pair as recorded, the one that got none as the run made it")
	require.Equal(t, idLive, after["id0"], "the run's own values are not changed")
	require.Empty(t, l.templatesToWrite(nil), "a run that holds no template values writes none")

	var none *runIDs
	require.Equal(t, after, none.templatesToWrite(after), "a set that follows nothing writes what the run made")
}
