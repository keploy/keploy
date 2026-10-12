package pkg

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// SimulateHTTP sends a test case as it is given and leaves it as it was. A
// replay renders a case's placeholders on a copy before it gets here
// (pkg/templates); nothing in SimulateHTTP reads or writes template values,
// so sending the case twice, as a reset re-send does, sends it the same way
// twice and leaves its placeholders where they were. (This passes on main
// too, since no template value is set here; it pins that SimulateHTTP has no
// template state of its own.)
func TestSimulateHTTP_SendsTheCaseAsGivenAndLeavesItUnchanged(t *testing.T) {
	var mu sync.Mutex
	var paths, bodies []string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		mu.Lock()
		paths = append(paths, r.URL.RawPath+"|"+r.URL.Path)
		bodies = append(bodies, string(body))
		mu.Unlock()
		w.Header().Set("Set-Cookie", "sid=S1live0001; Path=/")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"token":"T1live0001"}`)
	}))
	defer app.Close()

	tc := &models.TestCase{
		Name: "test-1",
		Kind: models.HTTP,
		HTTPReq: models.HTTPReq{
			Method: "POST",
			URL:    app.URL + "/login/{{string .user}}",
			Header: map[string]string{"Content-Type": "application/json", "Cookie": "sid={{string .sid}}"},
			Body:   `{"user":"{{string .user}}"}`,
		},
		HTTPResp: models.HTTPResp{
			StatusCode: 200,
			Header:     map[string]string{"Set-Cookie": "sid={{capture string .sid}}; Path=/"},
			Body:       `{"token":"{{capture string .token}}"}`,
		},
	}
	before := *tc
	beforeReqHeader := map[string]string{"Content-Type": "application/json", "Cookie": "sid={{string .sid}}"}
	beforeRespHeader := map[string]string{"Set-Cookie": "sid={{capture string .sid}}; Path=/"}

	for i := 0; i < 2; i++ {
		resp, err := SimulateHTTP(context.Background(), tc, "test-set-0", zap.NewNop(), SimulationConfig{APITimeout: 5})
		require.NoError(t, err)
		assert.Equal(t, `{"token":"T1live0001"}`, resp.Body)
	}

	assert.Equal(t, before.HTTPReq.URL, tc.HTTPReq.URL)
	assert.Equal(t, before.HTTPReq.Body, tc.HTTPReq.Body)
	assert.Equal(t, before.HTTPResp.Body, tc.HTTPResp.Body)
	assert.Equal(t, beforeReqHeader, tc.HTTPReq.Header, "SimulateHTTP rewrote the request headers it was given")
	assert.Equal(t, beforeRespHeader, tc.HTTPResp.Header, "SimulateHTTP rewrote the expected response headers it was given")
	require.Len(t, bodies, 2)
	assert.Equal(t, bodies[0], bodies[1], "a re-send of the same case went out differently")
	assert.Equal(t, `{"user":"{{string .user}}"}`, bodies[0], "SimulateHTTP rendered the placeholders itself")
}

// A URL a replay rendered goes out as it stands: each value in it is escaped
// for its place, and unescaping the whole URL again would turn a '+' in a
// value into a space and "%26" into a new query parameter. A URL that was not
// rendered and holds "%7B" is unescaped whole, as it always was.
func TestSimulateHTTP_ARenderedURLIsNotUnescapedAgain(t *testing.T) {
	var mu sync.Mutex
	var got []string
	app := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		got = append(got, r.URL.Query().Get("cursor")+"|"+r.URL.Query().Get("x"))
		mu.Unlock()
		_, _ = io.WriteString(w, `{}`)
	}))
	defer app.Close()

	send := func(rawQuery string, rendered bool) string {
		tc := &models.TestCase{Name: "t", Kind: models.HTTP, HTTPReq: models.HTTPReq{
			Method: "GET", URL: app.URL + "/page?" + rawQuery, URLRendered: rendered,
		}}
		_, err := SimulateHTTP(context.Background(), tc, "test-set-0", zap.NewNop(), SimulationConfig{APITimeout: 5})
		require.NoError(t, err)
		mu.Lock()
		defer mu.Unlock()
		return got[len(got)-1]
	}
	// {"after":"a+b&c"} escaped for a query value, then x=1.
	const escaped = "cursor=%7B%22after%22%3A%22a%2Bb%26c%22%7D&x=1"
	assert.Equal(t, `{"after":"a+b&c"}|1`, send(escaped, true), "a rendered URL is sent as it stands")
	assert.Equal(t, `{"after":"a b|1`, send(escaped, false), "a URL that was not rendered is unescaped whole, as before")
}
