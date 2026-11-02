package http

import (
	"context"
	"net/http"
	"net/url"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

// A store that hands out a recording and then refuses every claim of it (the
// claim of a session mock looks in the reusable tier, and the staging filed
// it elsewhere) must not keep the match retrying the same recording: each
// pass whose claim fails leaves that recording out of the call's
// candidates, so the call ends in a miss once none is left. Before, the
// match re-read the same pool and failed the same claim forever, at 99% CPU,
// and the call was never answered.
//
// One row per pass that claims: the exact-body pass, the JSON body pass with
// one candidate, and the fuzzy pass. Each waits for the match to return; a
// hang ends in go test's own -timeout, which names the row.
func TestAClaimTheStoreRefusesEndsTheCall(t *testing.T) {
	for _, tc := range []struct {
		name, recorded, live, contentType string
	}{
		{name: "exact-body", recorded: `{"a":1}`, live: `{"a":1}`, contentType: "application/json"},
		{name: "json-body", recorded: `{"a":1}`, live: `{"a":2}`, contentType: "application/json"},
		{name: "fuzzy", recorded: "a=1&b=xyz", live: "a=1&b=xyw", contentType: "text/plain"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := &models.Mock{
				Name: "refused",
				Kind: models.Kind(models.HTTP),
				Spec: models.MockSpec{
					HTTPReq: &models.HTTPReq{
						Method: "POST", URL: "http://api/q", ProtoMajor: 1, ProtoMinor: 1,
						Header: map[string]string{"Content-Type": tc.contentType},
						Body:   tc.recorded,
					},
					HTTPResp: &models.HTTPResp{StatusCode: 200, Body: "ok"},
				},
				TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimeSession, Consume: models.ConsumeCursorSaturate, LifetimeDerived: true},
			}
			db := &mockMemDb{mocks: []*models.Mock{m}}
			h := http.Header{}
			h.Set("Content-Type", tc.contentType)
			live := &req{method: "POST", url: &url.URL{Path: "/q"}, header: h, body: []byte(tc.live), raw: []byte("POST /q HTTP/1.1\r\nHost: api\r\n\r\n" + tc.live)}
			ok, stub, _, err := newHTTP().match(context.Background(), live, db, nil, nil, nil, true, false, false, true, true)
			if err != nil || ok || stub != nil {
				t.Fatalf("match: ok %v, served %v, err %v; want a miss", ok, stub != nil, err)
			}
			if db.updateUnFilteredMockOld != m {
				t.Fatalf("the pass under test never claimed the recording (claimed %v): the row does not reach its claim", db.updateUnFilteredMockOld)
			}
		})
	}
}
