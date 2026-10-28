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
// where it knows it: as recorded where the application sent the recorded
// request, every part the learn step reads, and F gave it the recorded answer
// whole (learn.Earlier.AsRecorded); the recording as the application sent its
// request (a learned noise in it) and F gave the recorded answer whole
// otherwise (learn.Earlier.View). A later test's call then reads the
// recording with that view (learn.Order.ServedView): its value that holds no
// word of it is noise, as before, and one that holds a word only what the
// application sent there holds is refused, naming the recording as record's
// full round gave it. A serve whose view HTTP does not know (one the mock
// manager told alone) is a view no adapter stated: the value's new word is
// refused, naming the recording (fail closed: a false red, never a word F may
// have given passed as noise).
func TestLearnMatchAServedRecordingsViewIsStatedOnlyWhereHTTPKnowsIt(t *testing.T) {
	const (
		unstated   = "where its adapter did not say what it gave the application there"
		givenThere = "as record's full round gave it"
	)
	for _, tc := range []struct {
		name  string
		learn bool   // test 1 served mock-2 through the learn step; else the mock manager told it alone
		live  string // mock-2's request in F
		ref   string // test 2's call's body.ref in F (recorded "7")
		why   string // "" learns; else a part of the refusal
	}{
		{"served as recorded by the learn step", true, `{"max":10}`, "42", ""},
		{"served by the learn step with a learned noise", true, `{"max":12}`, "42", ""},
		{"served by the learn step with a learned noise the value holds", true, `{"max":1203}`, "1203", givenThere},
		{"told by the mock manager alone", false, "", "42", unstated},
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
			got, miss, err := h.learnMatch(ctx, live("POST", "/v1/messages", `{"ref":"`+tc.ref+`"}`, nil), &learnDb{perTest: []*models.Mock{call}}, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if tc.why == "" {
				if got == nil || miss != nil {
					t.Fatalf("served %v miss %+v; want body.ref learned: no word of it stands in what F gave at mock-2", got, miss)
				}
				return
			}
			if got != nil || miss == nil || !strings.Contains(miss.res.Why, "mock-2") || !strings.Contains(miss.res.Why, tc.why) {
				t.Fatalf("FALSE GREEN? served %v miss %+v; want body.ref refused, naming mock-2: %q", got, miss, tc.why)
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
		// A view is stated wherever F gave the recorded answer whole,
		// whatever the request (servedExchange); never of an answer F
		// gave otherwise.
		x, ok := n.servedExchange(learn.NewTracker().Drifts(), m, m, n.shape(rp, destOf(lp, rp)), rp, n.shape(lp, sameDest), in, lp, &served, true)
		whole := served.Spec.HTTPResp != nil && answerAsRecorded(m.Spec.HTTPResp, served.Spec.HTTPResp)
		if !ok || (x.View != nil) != whole || (x.View != nil) != (x.ViewKey != "") {
			t.Errorf("%s: view stated %v (key %q); want %v: F gave the recorded answer whole %v", c.name, x.View != nil, x.ViewKey, whole, whole)
		}
	}
	if e := earlierOf(learn.NewTracker().Drifts(), m, m, learn.Shape{}, learn.Shape{}, nil, true); e.AsRecorded {
		t.Errorf("no mock served: said served as recorded")
	}
}

// F's view of an exchange it served with the recorded answer whole is the
// recording as the application sent its request: its method, URL, query,
// headers and body as sent, the recorded answer as recorded (one parked on
// disk read back). Its key is the same for two requests the view holds the
// same, and another where any part differs, so a set keeps one view of a
// recording served the same way again and each other one beside it.
func TestAServedExchangesViewIsTheRequestTheApplicationSentWithTheRecordedAnswer(t *testing.T) {
	m := learnRecorded("mock-4", 0, "POST", "/v1/orders?page=1", `{"item":"book"}`, map[string]string{"X-Trace": "t-1"}, `{"id":"o-1"}`)
	in := live("POST", "/v1/orders?page=2", `{"item":"pen"}`, map[string]string{"X-Trace": "t-2"})
	load, key := servedLiveView(m, in)
	v, err := load()
	if err != nil {
		t.Fatal(err)
	}
	r := v.Spec.HTTPReq
	if r.Body != `{"item":"pen"}` || r.Header["X-Trace"] != "t-2" || !strings.Contains(r.URL, "page=2") || r.URLParams["page"] != "2" || string(r.Method) != "POST" {
		t.Errorf("view's request %+v; want the one the application sent", r)
	}
	if v.Spec.HTTPResp == nil || v.Spec.HTTPResp.Body != `{"id":"o-1"}` || v.Name != m.Name || m.Spec.HTTPReq.Body != `{"item":"book"}` {
		t.Errorf("view %+v; want the recording's answer and name, the recording itself unchanged", v)
	}
	same, _ := servedLiveView(m, live("POST", "/v1/orders?page=2", `{"item":"pen"}`, map[string]string{"X-Trace": "t-2"}))
	if _, k := servedLiveView(m, live("POST", "/v1/orders?page=2", `{"item":"pen"}`, map[string]string{"X-Trace": "t-2"})); k != key || same == nil {
		t.Errorf("the same request keyed %q and %q; want one key", key, k)
	}
	for name, other := range map[string]*req{
		"another body":    live("POST", "/v1/orders?page=2", `{"item":"cup"}`, map[string]string{"X-Trace": "t-2"}),
		"another header":  live("POST", "/v1/orders?page=2", `{"item":"pen"}`, map[string]string{"X-Trace": "t-3"}),
		"another query":   live("POST", "/v1/orders?page=3", `{"item":"pen"}`, map[string]string{"X-Trace": "t-2"}),
		"one more header": live("POST", "/v1/orders?page=2", `{"item":"pen"}`, map[string]string{"X-Trace": "t-2", "X-Extra": "e"}),
	} {
		if _, k := servedLiveView(m, other); k == key {
			t.Errorf("%s: keyed as the first request's view", name)
		}
	}
}
