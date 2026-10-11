package http

import (
	"context"
	"net/http"
	"net/url"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// authorizeBody is the request body of a PAT authorize call: it names only the
// resource, so every caller sends the same bytes and only Authorization differs.
const authorizeBody = `{"resource":{"type":"cluster","id":"c1"}}`

// authorizeMock builds a recorded POST /v1/authorize made with token at ts,
// classified as a lax-promoted data-plane mock (session lifetime, cursor
// consumption), the way DeriveLifetime classifies HTTP_CLIENT mocks.
func authorizeMock(name, token string, ts time.Time) *models.Mock {
	m := httpMock(name, "POST", "http://api-server/v1/authorize")
	m.Spec.HTTPReq.Header = map[string]string{
		"Authorization": "Bearer " + token,
		"Content-Type":  "application/json",
	}
	m.Spec.HTTPReq.Body = authorizeBody
	m.Spec.ReqTimestampMock = ts
	m.TestModeInfo.Lifetime = models.LifetimeSession
	m.TestModeInfo.Consume = models.ConsumeCursorSaturate
	return m
}

func authorizeReq(token string) *req {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	return &req{method: "POST", url: &url.URL{Path: "/v1/authorize"}, header: h, body: []byte(authorizeBody)}
}

// authorizeDb holds callers A, B, A recorded in that order.
func authorizeDb() *mockMemDb {
	base := time.Unix(1_700_000_000, 0)
	return &mockMemDb{
		mocks: []*models.Mock{
			authorizeMock("a-1", "token-A", base),
			authorizeMock("b-1", "token-B", base.Add(time.Second)),
			authorizeMock("a-2", "token-A", base.Add(2*time.Second)),
		},
		updateUnFilteredReturn: true,
	}
}

// TestMatch_CredentialPicksCallersRecording: identical authorize requests from
// two callers must each get the answer recorded for that caller, in that
// caller's record order, whatever order the callers come in at replay. Without
// the credential preference they share one cursor and B gets A's answer.
// Authorization is in the default header noise, as in production.
func TestMatch_CredentialPicksCallersRecording(t *testing.T) {
	h := newHTTP()
	ctx := context.Background()
	db := authorizeDb()

	steps := []struct{ token, want string }{
		{"token-B", "b-1"},
		{"token-A", "a-1"},
		{"token-A", "a-2"},
		{"token-A", "a-2"}, // saturates on the caller's last recording
		{"token-B", "b-1"},
	}
	for i, s := range steps {
		ok, stub, _, err := h.match(ctx, authorizeReq(s.token), db, models.FlakyHeaderNoise(), nil, nil, true, false, false, true, true)
		if err != nil || !ok || stub == nil {
			t.Fatalf("call %d: ok=%v stub=%v err=%v", i+1, ok, stub, err)
		}
		if stub.Name != s.want {
			t.Fatalf("call %d (%s) served %q, want %q — a caller must get its own recorded answer", i+1, s.token, stub.Name, s.want)
		}
	}
}

// TestMatch_CredentialRotatedTokenFallsBack: a token no recording was made with
// (rotated, re-issued) must still be served, through the shared record-order
// cursor exactly as before — the preference never rejects a candidate.
func TestMatch_CredentialRotatedTokenFallsBack(t *testing.T) {
	h := newHTTP()
	ctx := context.Background()
	db := authorizeDb()

	for i, want := range []string{"a-1", "b-1", "a-2", "a-2"} {
		ok, stub, _, err := h.match(ctx, authorizeReq("token-rotated"), db, models.FlakyHeaderNoise(), nil, nil, true, false, false, true, true)
		if err != nil || !ok || stub == nil {
			t.Fatalf("call %d: a rotated token must still be served: ok=%v stub=%v err=%v", i+1, ok, stub, err)
		}
		if stub.Name != want {
			t.Fatalf("call %d served %q, want %q (record order)", i+1, stub.Name, want)
		}
	}
}

// TestMatch_CredentialPreferredWithoutCursor: with stateful mocks off there is
// no cursor, and the first exact-body match used to answer every caller; the
// caller's own recording must be preferred there too.
func TestMatch_CredentialPreferredWithoutCursor(t *testing.T) {
	h := newHTTP()
	ctx := context.Background()
	db := authorizeDb()

	for i, s := range []struct{ token, want string }{{"token-B", "b-1"}, {"token-A", "a-1"}} {
		ok, stub, _, err := h.match(ctx, authorizeReq(s.token), db, models.FlakyHeaderNoise(), nil, nil, true, false, false, false, true)
		if err != nil || !ok || stub == nil {
			t.Fatalf("call %d: ok=%v stub=%v err=%v", i+1, ok, stub, err)
		}
		if stub.Name != s.want {
			t.Fatalf("call %d (%s) served %q, want %q", i+1, s.token, stub.Name, s.want)
		}
	}
}
