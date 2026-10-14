package http

import (
	"context"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// The caller fixtures of keploy #4745 (dsmon F17), as S1's acceptance tests:
// an authorize call whose body names only the resource, so every caller
// sends the same bytes and only Authorization differs.

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

// authorizeReq is the live authorize call made with token. It carries the
// Host the client sends, as decode hands every request to the matcher.
func authorizeReq(token string) *req {
	h := http.Header{}
	h.Set("Authorization", "Bearer "+token)
	h.Set("Content-Type", "application/json")
	h.Set("Host", "api-server")
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

// authorizeCalls serves the calls in order under the switch, with the
// decode-time static list, and returns what each was served ("" for a miss).
func authorizeCalls(t *testing.T, db *mockMemDb, stateful bool, tokens ...string) []string {
	t.Helper()
	t.Setenv("KEPLOY_PROVEN", "1")
	h := newHTTP()
	var got []string
	for _, tok := range tokens {
		ok, stub, _, err := h.match(context.Background(), authorizeReq(tok), db, models.ProvenFlakyHeaderNoise(), nil, nil, true, false, false, stateful, true)
		if err != nil {
			t.Fatalf("match: %v", err)
		}
		if !ok {
			got = append(got, "")
			continue
		}
		got = append(got, stub.Name)
	}
	return got
}

// Under S1 Authorization is compared (design §7.2 item 8): each caller gets
// the answers recorded for it, in its own record order, whatever order the
// callers come in, and saturates on its own last. Each caller's recordings
// keep one record of what they consumed: another caller never moves it.
func TestStrictServesEachCallerItsOwnRecordings(t *testing.T) {
	for _, tc := range []struct {
		name   string
		tokens []string
		want   []string
	}{
		{"B first", []string{"token-B", "token-A", "token-A", "token-A", "token-B"}, []string{"b-1", "a-1", "a-2", "a-2", "b-1"}},
		{"interleaved", []string{"token-A", "token-B", "token-A", "token-B", "token-A"}, []string{"a-1", "b-1", "a-2", "b-1", "a-2"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := authorizeCalls(t, authorizeDb(), true, tc.tokens...)
			if strings.Join(got, ",") != strings.Join(tc.want, ",") {
				t.Fatalf("served %v, want %v", got, tc.want)
			}
		})
	}
	// Both callers recorded twice (A, B, A, B): each has a cursor of its
	// own, and one caller's calls never move the other's.
	four := authorizeDb()
	four.mocks = append(four.mocks, authorizeMock("b-2", "token-B", time.Unix(1_700_000_003, 0)))
	if got := authorizeCalls(t, four, true, "token-A", "token-B", "token-A", "token-B", "token-B", "token-A"); strings.Join(got, ",") != "a-1,b-1,a-2,b-2,b-2,a-2" {
		t.Fatalf("with both callers recorded twice served %v", got)
	}
	four = authorizeDb()
	four.mocks = append(four.mocks, authorizeMock("b-2", "token-B", time.Unix(1_700_000_003, 0)))
	if got := authorizeCalls(t, four, true, "token-B", "token-B", "token-A", "token-A"); strings.Join(got, ",") != "b-1,b-2,a-1,a-2" {
		t.Fatalf("with both callers recorded twice, B first, served %v", got)
	}
	// With stateful mocks off there is no cursor: each caller's first.
	if got := authorizeCalls(t, authorizeDb(), false, "token-B", "token-A", "token-A"); strings.Join(got, ",") != "b-1,a-1,a-1" {
		t.Fatalf("without the cursor served %v", got)
	}
}

// A token no recording was made with (rotated, re-issued) is no recorded
// caller's: it misses. It is answered only when the recordings learned
// Authorization as noise (record's full round saw it drift), and then from
// one group of recordings in record order, whichever token comes: a token
// that matches every recording under the noise shares their one cursor.
func TestStrictARotatedTokenMissesUnlessLearned(t *testing.T) {
	if got := authorizeCalls(t, authorizeDb(), true, "token-rotated", "token-rotated"); strings.Join(got, ",") != "," {
		t.Fatalf("a rotated token was served %v", got)
	}
	learned := authorizeDb()
	for _, m := range learned.mocks {
		m.Spec.ReqBodyNoise = map[string][]string{"header.authorization": {}}
	}
	if got := authorizeCalls(t, learned, true, "token-rotated", "token-A", "token-rotated", "token-B"); strings.Join(got, ",") != "a-1,b-1,a-2,a-2" {
		t.Fatalf("with Authorization learned as noise served %v, want the recordings in record order", got)
	}
}

// S1 compares Authorization, so a token the app mints in the test (rotated
// on every run) misses unless a variable follows it: here the first authorize
// call binds it and the next uses it, so a rotated token is answered by the
// recorded caller's recordings, while another caller's recording still
// answers only its own token. (keploy #4745's caller fixtures, above.)
func TestVariables_ABoundTokenAnswersARotatedCaller(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	h := newHTTP()
	db := authorizeDb()
	a1, b1, a2 := db.mocks[0], db.mocks[1], db.mocks[2]
	a1.Spec.Variables = models.MockVariables{"token": bindV("Bearer token-A", "header.Authorization")}
	a2.Spec.Variables = models.MockVariables{"token": useV("Bearer token-A", []string{"header.Authorization"})}
	for _, m := range db.mocks {
		m.TestModeInfo.Lifetime, m.TestModeInfo.Consume = models.LifetimePerTest, models.ConsumeReuse
	}
	vdb := &varDb{perTest: []*models.Mock{a1, b1, a2}}
	for i, step := range []struct{ token, want string }{
		{"token-R", "a-1"}, // binds the rotated token
		{"token-R", "a-2"}, // uses it
		{"token-B", "b-1"}, // the other caller's own token
		{"token-X", ""},    // another token: no recording, no variable
	} {
		ok, stub, diag := matchVar(t, h, vdb, authorizeReq(step.token))
		got := ""
		if ok {
			got = stub.Name
		}
		if got != step.want {
			t.Fatalf("call %d (%s) served %q, want %q (diag %+v)", i+1, step.token, got, step.want, diag)
		}
	}
}
