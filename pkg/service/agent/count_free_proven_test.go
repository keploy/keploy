package agent

import (
	"errors"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
)

// errNoAnswer is a store that cannot read an answer back.
var errNoAnswer = errors.New("the store could not read the answer")

// readsAreGETs declares, for these tests, an HTTP GET a read and nothing else
// (the HTTP parser's own rule says the same of these mocks).
func readsAreGETs(t *testing.T) {
	t.Helper()
	t.Cleanup(models.RegisterReadCall(models.HTTP, func(m *models.Mock) bool {
		return m.Spec.HTTPReq != nil && m.Spec.HTTPReq.Method == "GET"
	}))
}

// answeredMock is a recorded HTTP call of method to path, answered 200 with
// body and the cookie it set.
func answeredMock(name, method, path, body, setCookie string) *models.Mock {
	h := map[string]string{}
	if setCookie != "" {
		h["Set-Cookie"] = setCookie
	}
	return &models.Mock{Name: name, Kind: models.HTTP, Spec: models.MockSpec{
		HTTPReq:  &models.HTTPReq{Method: models.Method(method), URL: "http://api.example.com" + path},
		HTTPResp: &models.HTTPResp{StatusCode: 200, Body: body, Header: h},
	}}
}

// One read more than recorded is not counted in a test whose own checks
// passed, and is said; an extra write, a call that differs, a read made
// before the test began, and any extra call of a test that failed its own
// checks still count.
func TestAnExtraReadIsSaidNotCountedOnlyInATestWhoseChecksPassed(t *testing.T) {
	began := time.Unix(100, 0)
	misses := func() []models.UnmatchedCall {
		return []models.UnmatchedCall{
			{Protocol: "HTTP", ActualSummary: "GET /jobs", Destination: "api", Kind: models.MissExtraCall, Recorded: 1, Read: true, At: began.Add(time.Second)},
			{Protocol: "HTTP", ActualSummary: "GET /jobs", Destination: "api", Kind: models.MissExtraCall, Recorded: 1, Read: true, At: began.Add(2 * time.Second)},
			{Protocol: "HTTP", ActualSummary: "POST /charge", Destination: "api", Kind: models.MissExtraCall, Recorded: 1, At: began.Add(time.Second)},
			{Protocol: "HTTP", ActualSummary: "GET /other", Destination: "api", Kind: models.MissInTest, At: began.Add(time.Second)},
			{Protocol: "HTTP", ActualSummary: "GET /late", Destination: "api", Kind: models.MissExtraCall, Recorded: 1, Read: true, At: began.Add(-time.Second)},
		}
	}

	ev := &models.ScopeEvidence{Misses: misses()}
	notes := countFreeReads(ev, true, began, nil, nil)
	require.Equal(t, []string{"GET /jobs to api was called 3 times, recorded 1: a read the page made more often, not counted"}, notes)
	require.Len(t, ev.Misses, 3)
	for _, m := range ev.Misses {
		require.NotEqual(t, "GET /jobs", m.ActualSummary, "the extra read is out of the evidence")
	}

	ev = &models.ScopeEvidence{Misses: misses()}
	require.Empty(t, countFreeReads(ev, false, began, nil, nil), "a test that failed its own checks counts every extra call")
	require.Len(t, ev.Misses, 5)
}

// A recorded read the test never made is not counted when the test made the
// same read and another of its recordings answered it the same; it counts
// when the answer differs (body or cookie), when no answered recording made
// that read, when it is a write, and when keploy cannot load it.
func TestAnUnmadeReadIsNotCountedOnlyWhenTheSameReadWasAnsweredTheSame(t *testing.T) {
	readsAreGETs(t)
	mocks := map[string]*models.Mock{}
	add := func(m *models.Mock) string { mocks[m.Name] = m; return m.Name }
	answered := []string{
		add(answeredMock("a-jobs", "GET", "/jobs", `[]`, "")),
		add(answeredMock("a-status", "GET", "/status", `running`, "")),
		add(answeredMock("a-me", "GET", "/me", `{}`, "s=1")),
		add(answeredMock("a-charge", "POST", "/charge", `ok`, "")),
	}
	unconsumed := []string{
		add(answeredMock("u-jobs", "GET", "/jobs", `[]`, "")),       // the same read, the same answer
		add(answeredMock("u-status", "GET", "/status", `done`, "")), // the poll that would have seen "done"
		add(answeredMock("u-me", "GET", "/me", `{}`, "s=2")),        // another cookie set
		add(answeredMock("u-other", "GET", "/other", `[]`, "")),     // a read never made at all
		add(answeredMock("u-charge", "POST", "/charge", `ok`, "")),  // a write
		"u-unloaded", // not in mocks
	}

	ev := &models.ScopeEvidence{Unconsumed: append([]string(nil), unconsumed...)}
	notes := countFreeReads(ev, false, time.Time{}, mocks, answered)
	require.Equal(t, []string{"recorded call GET api.example.com/jobs (u-jobs) was never made, but the test made the same read and a-jobs answered it the same: not counted"}, notes)
	require.Equal(t, []string{"u-status", "u-me", "u-other", "u-charge", "u-unloaded"}, ev.Unconsumed)
}

// An answer the store keeps apart from its mock is loaded, on a copy, to be
// compared; one that cannot be loaded is not the same.
func TestAnUnmadeReadIsComparedWithAnAnswerKeptApartOnceLoaded(t *testing.T) {
	readsAreGETs(t)
	kept := func(name, body string, fail bool) *models.Mock {
		m := answeredMock(name, "GET", "/jobs", "", "")
		m.Spec.HTTPResp = nil
		m.SetResponseHydrator(func() (*models.HTTPResp, []models.MongoResponse, error) {
			if fail {
				return nil, nil, errNoAnswer
			}
			return &models.HTTPResp{StatusCode: 200, Body: body}, nil, nil
		})
		return m
	}
	mocks := map[string]*models.Mock{"a": kept("a", "[]", false), "u": kept("u", "[]", false)}
	ev := &models.ScopeEvidence{Unconsumed: []string{"u"}}
	require.Len(t, countFreeReads(ev, true, time.Time{}, mocks, []string{"a"}), 1)
	require.Empty(t, ev.Unconsumed)
	require.True(t, mocks["a"].HasSpilledResponse(), "the stored mock is left as it was")

	mocks = map[string]*models.Mock{"a": kept("a", "[]", true), "u": kept("u", "[]", false)}
	ev = &models.ScopeEvidence{Unconsumed: []string{"u"}}
	require.Empty(t, countFreeReads(ev, true, time.Time{}, mocks, []string{"a"}))
	require.Equal(t, []string{"u"}, ev.Unconsumed)
}
