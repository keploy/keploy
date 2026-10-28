package http

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// keploy's HTTP adapter states F's view of an exchange its learn step served
// where it knows it (learn.Earlier.AsRecorded): the application sent the
// recorded request, every part the learn step reads, and F gave it the
// recorded answer whole. A later test's call then reads the recording as
// served as recorded (learn.Order.ServedView), and its value that holds no
// word of it is noise, as before. A serve whose view HTTP does not know (one
// the mock manager told alone, or one the learn step served changed: a
// request with a learned noise) is a view no adapter stated: the value's new
// word is refused, naming the recording (fail closed: a false red, never a
// word F may have given passed as noise).
func TestLearnMatchAServedRecordingsViewIsStatedOnlyWhereHTTPKnowsIt(t *testing.T) {
	const unstated = "where its adapter did not say what it gave the application there"
	for _, tc := range []struct {
		name    string
		learn   bool   // test 1 served mock-2 through the learn step; else the mock manager told it alone
		live    string // mock-2's request in F
		refused bool
	}{
		{"served as recorded by the learn step", true, `{"max":10}`, false},
		{"served by the learn step with a learned noise", true, `{"max":12}`, true},
		{"told by the mock manager alone", false, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			other := learnRecorded("mock-2", time.Second, "POST", "/v1/receive", `{"max":10}`, nil, `{"count":"9"}`)
			call := learnRecorded("mock-1", 2*time.Second, "POST", "/v1/messages", `{"ref":"7"}`, nil, `{}`)
			tr := learn.NewTracker()
			ctx := learn.WithTracker(context.Background(), tr)
			tr.Begin()
			if tc.learn {
				// The mock manager tells the tracker of each mock a claim
				// takes; the learn step tells it the exchange.
				db := &learnDb{perTest: []*models.Mock{other}, observe: tr.ServedMock}
				got, miss, err := h.learnMatch(ctx, live("POST", "/v1/receive", tc.live, nil), db, n, opts)
				if err != nil || got == nil {
					t.Fatalf("test 1's call not served: %+v %v", miss, err)
				}
			} else {
				tr.ServedMock(other)
			}
			tr.End()
			tr.Begin()
			got, miss, err := h.learnMatch(ctx, live("POST", "/v1/messages", `{"ref":"42"}`, nil), &learnDb{perTest: []*models.Mock{call}}, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if !tc.refused {
				if got == nil || miss != nil {
					t.Fatalf("served %v miss %+v; want body.ref learned: no word of it stands in what F gave at mock-2", got, miss)
				}
				return
			}
			if got != nil || miss == nil || !strings.Contains(miss.res.Why, "mock-2") || !strings.Contains(miss.res.Why, unstated) {
				t.Fatalf("FALSE GREEN? served %v miss %+v; want body.ref refused, naming mock-2 and that its view was not stated", got, miss)
			}
		})
	}
}

// earlierOf says F served the exchange as recorded only where the
// application sent the recorded request, every part the learn step reads of
// it, and F gave it the recorded answer whole: status, headers and body.
// A request leaf, a header or the body of the answer F served, its status, a
// missing answer: each says nothing (fail closed).
func TestAnExchangeIsSaidServedAsRecordedOnlyWhereRequestAndAnswerAreTheRecordings(t *testing.T) {
	_, n, _ := learnOn(t)
	m := learnRecorded("mock-3", 0, "POST", "/v1/orders", `{"item":"book"}`, map[string]string{"X-Trace": "t-1"}, `{"id":"o-1"}`)
	m.Spec.HTTPResp.Header = map[string]string{"Content-Type": "application/json"}
	for _, c := range []struct {
		name  string
		body  string
		trace string
		serve func(r *models.HTTPResp) *models.HTTPResp
		want  bool
	}{
		{"as recorded", `{"item":"book"}`, "t-1", func(r *models.HTTPResp) *models.HTTPResp { return r }, true},
		{"a request leaf another", `{"item":"pen"}`, "t-1", func(r *models.HTTPResp) *models.HTTPResp { return r }, false},
		{"a request header another", `{"item":"book"}`, "t-2", func(r *models.HTTPResp) *models.HTTPResp { return r }, false},
		{"the answer's body another", `{"item":"book"}`, "t-1", func(r *models.HTTPResp) *models.HTTPResp { r.Body = `{"id":"o-2"}`; return r }, false},
		{"an answer's header another", `{"item":"book"}`, "t-1", func(r *models.HTTPResp) *models.HTTPResp {
			r.Header = map[string]string{"Content-Type": "application/json", "X-Id": "o-2"}
			return r
		}, false},
		{"the answer's status another", `{"item":"book"}`, "t-1", func(r *models.HTTPResp) *models.HTTPResp { r.StatusCode = 201; return r }, false},
		{"no answer served", `{"item":"book"}`, "t-1", func(*models.HTTPResp) *models.HTTPResp { return nil }, false},
	} {
		in := live("POST", "/v1/orders", c.body, map[string]string{"X-Trace": c.trace})
		lp, rp := liveParts(context.Background(), in), recordedParts(m)
		served := *m
		resp := *m.Spec.HTTPResp
		resp.Header = map[string]string{"Content-Type": "application/json"}
		served.Spec.HTTPResp = c.serve(&resp)
		e := earlierOf(learn.NewTracker().Drifts(), m, m, n.shape(rp, destOf(lp, rp)), n.shape(lp, sameDest), &served, true)
		if e.AsRecorded != c.want {
			t.Errorf("%s: said served as recorded %v; want %v", c.name, e.AsRecorded, c.want)
		}
	}
	if e := earlierOf(learn.NewTracker().Drifts(), m, m, learn.Shape{}, learn.Shape{}, nil, true); e.AsRecorded {
		t.Errorf("no mock served: said served as recorded")
	}
}
