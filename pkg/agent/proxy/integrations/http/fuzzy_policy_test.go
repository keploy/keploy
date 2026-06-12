package http

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func jsonBodyMock(name, body string, sortOrder int64) *models.Mock {
	m := &models.Mock{
		Name: name,
		Kind: models.Kind(models.HTTP),
		Spec: models.MockSpec{
			HTTPReq: &models.HTTPReq{
				Method: "POST",
				URL:    "http://localhost:8080/api/orders",
				Header: map[string]string{"Content-Type": "application/json"},
				Body:   body,
			},
		},
	}
	m.TestModeInfo.SortOrder = sortOrder
	return m
}

func orderInput(body string) *req {
	return &req{
		method: "POST",
		url:    &url.URL{Path: "/api/orders"},
		header: http.Header{"Content-Type": {"application/json"}},
		body:   []byte(body),
		raw:    []byte(body),
	}
}

// Under test.fuzzyMatch=off, multiple body-key-matched candidates resolve via
// the recorded-order (lowest SortOrder) tiebreak — never via similarity.
func TestMatch_FuzzyOff_DeterministicTiebreak(t *testing.T) {
	h := newHTTP()
	early := jsonBodyMock("mock-early", `{"order_id":"o-1"}`, 10)
	late := jsonBodyMock("mock-late", `{"order_id":"o-2"}`, 20)
	db := &mockMemDb{mocks: []*models.Mock{late, early}, updateUnFilteredReturn: true, deleteFilteredReturn: true}

	// Live body shares the key set with both candidates but matches neither
	// exactly — under "on" this would go to fuzzy.
	ok, mock, diag, err := h.match(context.Background(), orderInput(`{"order_id":"o-9"}`), db, nil, nil, false, false, models.FuzzyMatchOff)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || mock == nil {
		t.Fatalf("expected deterministic tiebreak to serve a mock, got ok=%v diag=%+v", ok, diag)
	}
	if mock.Name != "mock-early" {
		t.Errorf("expected recorded-order pick mock-early, got %s", mock.Name)
	}
}

// Under test.fuzzyMatch=off, a request whose body keys match no candidate is
// a structured miss with the fuzzy_match_disabled phase — not a Jaccard guess.
func TestMatch_FuzzyOff_MissInsteadOfGuess(t *testing.T) {
	h := newHTTP()
	candidate := jsonBodyMock("mock-1", `{"order_id":"o-1"}`, 10)
	// Non-JSON live body: exact fails, body-key path doesn't apply, fuzzy disabled.
	input := orderInput(`plain-text-payload`)
	input.header = http.Header{}
	candidate.Spec.HTTPReq.Header = map[string]string{}
	candidate.Spec.HTTPReq.Body = `different-plain-text`
	db := &mockMemDb{mocks: []*models.Mock{candidate}, updateUnFilteredReturn: true}

	ok, _, diag, err := h.match(context.Background(), input, db, nil, nil, false, false, models.FuzzyMatchOff)
	if err != nil {
		t.Fatal(err)
	}
	if ok {
		t.Fatal("expected a miss with fuzzy disabled")
	}
	if diag == nil || diag.phase != models.MatchPhaseFuzzyOff {
		t.Errorf("expected phase %q, got %+v", models.MatchPhaseFuzzyOff, diag)
	}
}

// The default policies (on/warn) keep the legacy fuzzy behaviour: the same
// request that misses under "off" is served via similarity.
func TestMatch_FuzzyWarn_StillServesViaSimilarity(t *testing.T) {
	h := newHTTP()
	candidate := jsonBodyMock("mock-1", `different-plain-text`, 10)
	candidate.Spec.HTTPReq.Header = map[string]string{}
	input := orderInput(`plain-text-payload`)
	input.header = http.Header{}
	db := &mockMemDb{mocks: []*models.Mock{candidate}, updateUnFilteredReturn: true}

	ok, mock, _, err := h.match(context.Background(), input, db, nil, nil, false, false, models.FuzzyMatchWarn)
	if err != nil {
		t.Fatal(err)
	}
	if !ok || mock == nil || mock.Name != "mock-1" {
		t.Fatalf("expected fuzzy match under warn policy, got ok=%v", ok)
	}
}

func TestLowestSortOrder(t *testing.T) {
	a := jsonBodyMock("a", `{}`, 5)
	b := jsonBodyMock("b", `{}`, 2)
	c := jsonBodyMock("c", `{}`, 9)
	if got := lowestSortOrder([]*models.Mock{a, b, c}); got.Name != "b" {
		t.Errorf("expected b, got %s", got.Name)
	}
}
