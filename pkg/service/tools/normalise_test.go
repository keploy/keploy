package tools

import (
	"context"
	"crypto/md5"  //nolint:gosec // the ETag form Spring sends
	"crypto/sha1" //nolint:gosec // the ETag form Express sends
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	httpMatcher "go.keploy.io/server/v3/pkg/matcher/http"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/templates"
	"go.keploy.io/server/v3/utils/logcontent/logtest"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// normaliseTestDB serves the test cases and keeps what normalize writes.
type normaliseTestDB struct {
	placementTestDB
	cases []*models.TestCase
}

func (f *normaliseTestDB) GetTestCases(context.Context, string) ([]*models.TestCase, error) {
	return f.cases, nil
}

// keploy normalize accepts the app's new answer but keeps each placeholder
// where the run resolved it to exactly what the app answered: the test case
// still says the id is captured and asserted, and the next run passes. A test
// case with no placeholder gets the answer as it is, as it always did.
func TestNormalizeTestCases_KeepsThePlaceholdersTheAnswerAgreesWith(t *testing.T) {
	templated := tcase("test-1", "POST", "http://h/users", nil, `{}`, 201,
		map[string]string{"Location": "/users/{{string .id}}"}, `{"id":"{{capture string .id}}","shape":"v1"}`)
	plain := tcase("test-2", "GET", "http://h/health", nil, "", 200, nil, `{"ok":true}`)
	db := &normaliseTestDB{placementTestDB: placementTestDB{written: map[string]models.TestCase{}}, cases: []*models.TestCase{templated, plain}}
	tools := &Tools{logger: zap.NewNop(), testDB: db, testSetConf: &placementConfStore{}, config: &config.Config{}}

	results := []models.TestResult{
		{
			TestCaseID: "test-1", Status: models.TestStatusFailed,
			Res: models.HTTPResp{StatusCode: 201, Header: map[string]string{"Location": "/users/usr_LIVE"}, Body: `{"id":"usr_LIVE","shape":"v2"}`},
			Result: models.Result{
				BodyResult:    []models.BodyResult{{Expected: `{"id":"usr_LIVE","shape":"v1"}`}},
				HeadersResult: []models.HeaderResult{{Expected: models.Header{Key: "Location", Value: []string{"/users/usr_LIVE"}}}},
			},
		},
		{
			TestCaseID: "test-2", Status: models.TestStatusFailed,
			Res: models.HTTPResp{StatusCode: 200, Header: map[string]string{"X": "1"}, Body: `{"ok": false}`},
		},
	}
	require.NoError(t, tools.NormalizeTestCases(context.Background(), "test-run-0", "test-set-0", nil, results))

	assert.Equal(t, `{"id":"{{capture string .id}}","shape":"v2"}`, db.written["test-1"].HTTPResp.Body)
	assert.Equal(t, "/users/{{string .id}}", db.written["test-1"].HTTPResp.Header["Location"])
	assert.Equal(t, results[1].Res.Body, db.written["test-2"].HTTPResp.Body)
	assert.Equal(t, results[1].Res.Header, db.written["test-2"].HTTPResp.Header)
}

// A gRPC expected response is written as the app's answer, as it always was;
// each placeholder that drops is said, one warning per placeholder.
func TestNormalizeTestCases_WarnsForEachGRPCPlaceholderItDrops(t *testing.T) {
	grpcCase := &models.TestCase{
		Name: "test-1", Kind: models.GRPC_EXPORT,
		GrpcResp: models.GrpcResp{
			Headers: models.GrpcHeaders{OrdinaryHeaders: map[string]string{"x-id": "{{string .id}}"}},
			Body:    models.GrpcLengthPrefixedMessage{DecodedData: `1: "{{string .id}}" 2: {{int .n}}`},
		},
	}
	db := &normaliseTestDB{placementTestDB: placementTestDB{written: map[string]models.TestCase{}}, cases: []*models.TestCase{grpcCase}}
	core, logs := observer.New(zap.WarnLevel)
	tools := &Tools{logger: zap.New(core), testDB: db, testSetConf: &placementConfStore{}, config: &config.Config{}}
	answer := models.GrpcResp{Body: models.GrpcLengthPrefixedMessage{DecodedData: `1: "usr_LIVE" 2: 7`}}
	require.NoError(t, tools.NormalizeTestCases(context.Background(), "test-run-0", "test-set-0", nil,
		[]models.TestResult{{TestCaseID: "test-1", Status: models.TestStatusFailed, GrpcRes: answer}}))

	assert.Equal(t, answer.Body, db.written["test-1"].GrpcResp.Body)
	var dropped []string
	for _, e := range logs.All() {
		dropped = append(dropped, e.ContextMap()["placeholder"].(string))
	}
	assert.Equal(t, []string{"header x-id: {{string .id}}", "body: {{string .id}}", "body: {{int .n}}"}, dropped)
}

// replayOnce compares an answer with a test case as keploy test does: the
// expected response resolved against the answer with the set's template
// (pkg/templates), then the matcher, told the body as recorded for its digest
// rule. A template failure fails it whatever the matcher says.
func replayOnce(t *testing.T, tc models.TestCase, tmpl map[string]interface{}, answer models.HTTPResp) (bool, *models.Result) {
	t.Helper()
	res := templates.NewRun(nil, tmpl, nil).Expected(tc.HTTPResp, answer, templates.ExpectOptions{})
	tc.HTTPResp = res.Resp
	var opts []httpMatcher.MatchOption
	if res.BodyTemplated {
		opts = append(opts, httpMatcher.WithRecordedBody(res.RecordedBody))
	}
	got := answer
	got.Header = map[string]string{}
	for k, v := range answer.Header {
		got.Header[k] = v
	}
	pass, result := httpMatcher.Match(&tc, &got, map[string]map[string][]string{}, false, false, zap.NewNop(), false, opts...)
	require.NotNil(t, result)
	return pass && len(res.Failures) == 0, result
}

// NORMALIZE, THEN THE NEXT RUN, with a digest ETag. The app computes its ETag
// over the body it sends (Express, Rails' Rack::ETag, Spring's
// ShallowEtagHeaderFilter). keploy normalize keeps the capture, and writes
// the ETag as the digest of the kept body at the recorded values, so every
// later run, whatever id the app issues, passes.
func TestNormalizeTestCases_ADigestETagHoldsOnTheRunsAfterIt(t *testing.T) {
	sha1b64 := func(b string) string { s := sha1.Sum([]byte(b)); return base64.StdEncoding.EncodeToString(s[:]) }
	for _, c := range []struct {
		name string
		etag func(body string) string
	}{
		{"Express", func(b string) string { return fmt.Sprintf(`W/"%x-%s"`, len(b), sha1b64(b)[:27]) }},
		{"Rails Rack::ETag", func(b string) string {
			s := sha256.Sum256([]byte(b))
			return `W/"` + hex.EncodeToString(s[:])[:32] + `"`
		}},
		{"Spring ShallowEtagHeaderFilter", func(b string) string { s := md5.Sum([]byte(b)); return `"0` + hex.EncodeToString(s[:]) + `"` }},
	} {
		t.Run(c.name, func(t *testing.T) {
			tmpl := map[string]interface{}{"id": "usr_REC"}
			answer := func(id, name string) models.HTTPResp {
				b := fmt.Sprintf(`{"id":"%s","name":"%s"}`, id, name)
				return models.HTTPResp{StatusCode: 201, Header: map[string]string{"Content-Type": "application/json", "ETag": c.etag(b)}, Body: b}
			}
			tc := &models.TestCase{Name: "test-1", Kind: models.HTTP, HTTPResp: models.HTTPResp{
				StatusCode: 201,
				Header:     map[string]string{"Content-Type": "application/json", "ETag": c.etag(`{"id":"usr_REC","name":"alice"}`)},
				Body:       `{"id":"{{capture string .id}}","name":"alice"}`,
			}}
			pass, _ := replayOnce(t, *tc, tmpl, answer("usr_L0", "alice"))
			require.True(t, pass, "precondition: the recorded test case passes with a fresh id")
			first := answer("usr_L1", "bob")
			pass, result := replayOnce(t, *tc, tmpl, first)
			require.False(t, pass, "precondition: the app's answer changed (the name)")

			db := &normaliseTestDB{placementTestDB: placementTestDB{written: map[string]models.TestCase{}}, cases: []*models.TestCase{tc}}
			tools := &Tools{logger: zap.NewNop(), testDB: db, testSetConf: &placementConfStore{stored: models.TestSet{Template: tmpl}}, config: &config.Config{}}
			require.NoError(t, tools.NormalizeTestCases(context.Background(), "test-run-0", "test-set-0", nil,
				[]models.TestResult{{TestCaseID: "test-1", Status: models.TestStatusFailed, Res: first, Result: *result}}))
			kept := db.written["test-1"]
			require.Equal(t, `{"id":"{{capture string .id}}","name":"bob"}`, kept.HTTPResp.Body)
			for _, id := range []string{"usr_L2", "usr_L3"} {
				pass, result := replayOnce(t, kept, tmpl, answer(id, "bob"))
				assert.True(t, pass, "the run after normalize with id %s: %+v", id, result.HeadersResult)
			}
		})
	}
}

// A TEST CASE THAT FAILED ON A TEMPLATE VARIABLE is HIGH risk: keploy
// normalize leaves it, and says why for each place and what to do instead,
// both cases: a value the app issues anew each run wants a capture where it
// is issued; a value that changed on purpose wants the expected value edited
// or the case re-recorded. --allow-high-risk would write this run's value
// over the placeholder, which the next run fails on.
func TestNormalizeTestCases_SaysWhyItLeavesATemplateFailure(t *testing.T) {
	read := tcase("test-2", "GET", "http://h/users/{{string .id}}", nil, "", 200, nil, `{"id":"{{string .id}}"}`)
	db := &normaliseTestDB{placementTestDB: placementTestDB{written: map[string]models.TestCase{}}, cases: []*models.TestCase{read}}
	core, logs := observer.New(zap.InfoLevel)
	tools := &Tools{logger: zap.New(core), testDB: db, testSetConf: &placementConfStore{}, config: &config.Config{}}
	result := models.TestResult{
		TestCaseID: "test-2", Status: models.TestStatusFailed,
		FailureInfo:      models.FailureInfo{Risk: models.High, Category: []models.FailureCategory{models.TemplateVariable}},
		TemplateFailures: []models.TemplateFailure{{Where: "body.id", Placeholder: "{{string .id}}", Why: `must equal id's value "usr_LIVE"`}},
		Res:              models.HTTPResp{StatusCode: 200, Body: `{"id":"usr_NEW"}`},
	}
	require.NoError(t, tools.NormalizeTestCases(context.Background(), "test-run-0", "test-set-0", nil, []models.TestResult{result}))
	assert.Empty(t, db.written, "a template failure is not normalized")
	said := logs.FilterMessageSnippet("not normalizing a test case that failed on a template variable").All()
	require.Len(t, said, 1)
	next := fmt.Sprint(said[0].ContextMap()["next_step"])
	for _, want := range []string{"{{capture ...}}", "edit the expected value or re-record", "--allow-high-risk writes this run's value over the placeholder"} {
		assert.Contains(t, next, want)
	}
	assert.Equal(t, "body.id", said[0].ContextMap()["where"])
}

// A TEST CASE REFUSED WHEN ITS SET LOADED was never sent: there is no answer
// to accept, so keploy normalize leaves it, --allow-high-risk or not, rather
// than write an empty response over the expected one.
func TestNormalizeTestCases_LeavesARefusedTestCase(t *testing.T) {
	create := tcase("test-1", "POST", "http://h/users", nil, `{"id":"{{capture string .id}}"}`, 201, nil, `{"id":"usr_1"}`)
	db := &normaliseTestDB{placementTestDB: placementTestDB{written: map[string]models.TestCase{}}, cases: []*models.TestCase{create}}
	core, logs := observer.New(zap.InfoLevel)
	cfg := &config.Config{}
	cfg.Normalize.AllowHighRisk = true
	tools := &Tools{logger: zap.New(core), testDB: db, testSetConf: &placementConfStore{}, config: cfg}
	result := models.TestResult{
		TestCaseID: "test-1", Status: models.TestStatusFailed,
		FailureInfo:      models.FailureInfo{Risk: models.High, Category: []models.FailureCategory{models.TemplateVariable}},
		TemplateFailures: []models.TemplateFailure{{Where: "request body", Placeholder: "{{capture string .id}}", Why: "refused when the test set loaded: a capture says what the app ANSWERS", Refused: true}},
	}
	require.NoError(t, tools.NormalizeTestCases(context.Background(), "test-run-0", "test-set-0", nil, []models.TestResult{result}))
	assert.Empty(t, db.written, "a refused test case is not normalized")
	assert.Len(t, logs.FilterMessageSnippet("refused when its test set loaded").All(), 1)
}

// A Set-Cookie placeholder normalize drops is said with the value the app
// answers now, which is the app's: the line carries it only as logcontent
// allows.
func TestNormalizeTestCases_ADroppedCookieIsSaidWithoutTheValueTheAppAnswers(t *testing.T) {
	const canary = "kp-canary-normalise-cookie-5d2e"
	logtest.Each(t, func(t *testing.T, on bool) {
		templated := tcase("test-1", "POST", "http://h/login", nil, `{}`, 200,
			map[string]string{"Set-Cookie": "sid={{string .sid}}; Path=/"}, `{"ok":true}`)
		db := &normaliseTestDB{placementTestDB: placementTestDB{written: map[string]models.TestCase{}}, cases: []*models.TestCase{templated}}
		logger, logs := logtest.Observe()
		tools := &Tools{logger: logger, testDB: db, testSetConf: &placementConfStore{}, config: &config.Config{}}
		results := []models.TestResult{{
			TestCaseID: "test-1", Status: models.TestStatusFailed,
			Res: models.HTTPResp{StatusCode: 200, Header: map[string]string{"Set-Cookie": "sid=" + canary + "; Path=/"}, Body: `{"ok":true}`},
			Result: models.Result{HeadersResult: []models.HeaderResult{{Expected: models.Header{Key: "Set-Cookie", Value: []string{"sid=resolved-elsewhere; Path=/"}}}}},
		}}
		require.NoError(t, tools.NormalizeTestCases(context.Background(), "test-run-0", "test-set-0", nil, results))
		logtest.Check(t, logs, canary, on, "keploy normalize wrote the app's answer where a placeholder was: the run resolved it to another value than the app answered")
	})
}
