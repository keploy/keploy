package replay

import (
	"context"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/templates"
	"go.uber.org/zap"
)

const (
	rewriteRecorded = `{"accessToken":"tok-REC","user":{"id":7}}`
	rewriteLive     = `{"accessToken":"tok-LIVE","user":{"id":7}}`
)

// rewriteCase is a test case whose recorded answer carries an Express ETag
// over its recorded body.
func rewriteCase(body string) *models.TestCase {
	return &models.TestCase{Kind: models.HTTP, Name: "test-1", HTTPResp: models.HTTPResp{
		StatusCode: 200,
		Header:     map[string]string{"Content-Type": "application/json", "Etag": expressETag(rewriteRecorded)},
		Body:       body,
	}}
}

// rewrite does to the copy what a hook that rewrote its expected body for
// this send does: the new body, and what it was as recorded.
func rewrite(sent *models.TestCase, body string) {
	sent.ExpectedRewrite = &models.ExpectedRewrite{RecordedBody: sent.HTTPResp.Body, Places: []models.RewrittenPlace{
		{Where: "body.accessToken", Recorded: "tok-REC", Why: "minted on the app's clock"}}}
	sent.HTTPResp.Body = body
}

// A hook that rewrote the expected body of the copy it was handed says what
// the body was as recorded: each digest header recorded over that body is
// asserted as the same digest of the answer's body. Without it the recorded
// digest is compared as written, and fails.
func TestAnExpectedBodyAHookRewroteHasItsDigestsAssertedOverTheAnswer(t *testing.T) {
	r := &Replayer{logger: zap.NewNop(), config: &config.Config{}}
	answer := func(body, etag string) *models.HTTPResp {
		return &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json", "Etag": etag}, Body: body}
	}
	for _, c := range []struct {
		name    string
		rewrite bool
		answer  *models.HTTPResp
		pass    bool
	}{
		{"the answer's digest", true, answer(rewriteLive, expressETag(rewriteLive)), true},
		{"a stale digest", true, answer(rewriteLive, expressETag(rewriteRecorded)), false},
		{"another difference in the body, its digest the answer's", true,
			answer(`{"accessToken":"tok-LIVE","user":{"id":8}}`, expressETag(`{"accessToken":"tok-LIVE","user":{"id":8}}`)), false},
		{"no rewrite said: the digest as written", false, answer(rewriteLive, expressETag(rewriteLive)), false},
	} {
		t.Run(c.name, func(t *testing.T) {
			run := templates.NewRun(zap.NewNop(), nil, nil)
			sent := toSend(run, rewriteCase(rewriteRecorded))
			if c.rewrite {
				rewrite(&sent, rewriteLive)
			} else {
				sent.HTTPResp.Body = rewriteLive
			}
			res := r.resolveExpected(run, nil, &sent, c.answer, "test-set-0")
			pass, result := r.compareHTTPResp(res.expected, c.answer, "test-set-0", false, res.opts...)
			if pass != c.pass {
				t.Fatalf("pass %v, want %v: headers %+v, body %+v", pass, c.pass, result.HeadersResult, result.BodyResult)
			}
		})
	}
}

// A templated body that a hook also rewrote has its recorded body rendered
// from the rewritten one, which no recorded digest was computed over: each
// digest header is then compared as written, and a moved one fails.
func TestATemplatedBodyAHookRewroteHasItsDigestsComparedAsWritten(t *testing.T) {
	r := &Replayer{logger: zap.NewNop(), config: &config.Config{}}
	run := templates.NewRun(zap.NewNop(), map[string]interface{}{"id": "u_1"}, nil)
	recorded := `{"accessToken":"tok-REC","id":"u_1"}`
	tc := rewriteCase(`{"accessToken":"tok-REC","id":"{{string .id}}"}`)
	tc.HTTPResp.Header["Etag"] = expressETag(recorded)
	sent := toSend(run, tc)
	rewrite(&sent, `{"accessToken":"tok-LIVE","id":"{{string .id}}"}`)
	live := `{"accessToken":"tok-LIVE","id":"u_1"}`
	ans := &models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json", "Etag": expressETag(live)}, Body: live}
	res := r.resolveExpected(run, nil, &sent, ans, "test-set-0")
	if pass, result := r.compareHTTPResp(res.expected, ans, "test-set-0", false, res.opts...); pass {
		t.Fatalf("a moved digest of a templated body a hook rewrote passed: %+v", result.HeadersResult)
	}
}

// A rewrite is the send's: the copy of the next send starts from the test
// case as written, with none.
func TestARewriteDoesNotCarryToTheNextSend(t *testing.T) {
	tc := rewriteCase(rewriteRecorded)
	tc.ExpectedRewrite = &models.ExpectedRewrite{RecordedBody: "stale"}
	if sent := toSend(templates.NewRun(zap.NewNop(), nil, nil), tc); sent.ExpectedRewrite != nil {
		t.Fatalf("the copy carries a rewrite: %+v", sent.ExpectedRewrite)
	}
}

// rewritingHooks rewrite test-1's expected body on the copy they are handed,
// to the token the app answered, the way enterprise's judging at the cut
// does, when tell is set saying so.
type rewritingHooks struct {
	prHooks
	tell bool
	etag func(body string) string
}

func (h rewritingHooks) SimulateRequest(_ context.Context, tc *models.TestCase, _ string) (interface{}, error) {
	if tc.Name != "test-1" {
		return h.prHooks.SimulateRequest(context.Background(), tc, "")
	}
	resp := tc.HTTPResp
	resp.Header = map[string]string{"Etag": h.etag(rewriteLive)}
	resp.Body = rewriteLive
	if h.tell {
		rewrite(tc, rewriteLive)
	} else {
		tc.HTTPResp.Body = rewriteLive
	}
	return &resp, nil
}

// Through a whole test set: the test whose expected body a hook rewrote is
// compared with the rewritten body, its digest asserted over the answer, and
// its result names each place rewritten with its recorded value. The test
// case as loaded is not changed.
func TestRunTestSet_AResultNamesThePlacesAHookRewrote(t *testing.T) {
	for _, c := range []struct {
		name string
		tell bool
		etag func(string) string
		pass bool
	}{
		{"said, the answer's digest", true, expressETag, true},
		{"said, a stale digest", true, func(string) string { return expressETag(rewriteRecorded) }, false},
		{"not said", false, expressETag, false},
	} {
		t.Run(c.name, func(t *testing.T) {
			h := newPartialRunHarness(t, 2, 0)
			h.cases[0].HTTPResp.Body = rewriteRecorded
			h.cases[0].HTTPResp.Header = map[string]string{"Etag": expressETag(rewriteRecorded)}
			h.replayer.hookImpl = rewritingHooks{tell: c.tell, etag: c.etag}
			_ = h.run(t)
			var got *models.TestResult
			for i := range h.report.results {
				if h.report.results[i].TestCaseID == "test-1" {
					got = &h.report.results[i]
				}
			}
			if got == nil {
				t.Fatalf("no result for test-1: %+v", h.report.results)
			}
			if pass := got.Status == models.TestStatusPassed; pass != c.pass {
				t.Fatalf("status %s, want passed=%v: %+v", got.Status, c.pass, got.Result.HeadersResult)
			}
			if !c.tell {
				if len(got.Rewritten) != 0 {
					t.Fatalf("a rewrite not said is in the result: %+v", got.Rewritten)
				}
				return
			}
			want := models.RewrittenPlace{Where: "body.accessToken", Recorded: "tok-REC", Why: "minted on the app's clock"}
			if len(got.Rewritten) != 1 || got.Rewritten[0] != want {
				t.Fatalf("the result's rewritten places are %+v, want [%+v]", got.Rewritten, want)
			}
			if h.cases[0].HTTPResp.Body != rewriteRecorded || h.cases[0].ExpectedRewrite != nil {
				t.Fatalf("the loaded test case changed: body %q, rewrite %+v", h.cases[0].HTTPResp.Body, h.cases[0].ExpectedRewrite)
			}
		})
	}
}
