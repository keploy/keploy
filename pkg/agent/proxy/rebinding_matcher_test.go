package proxy

import (
	"fmt"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/pkg/agent/ids"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mocknoise"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// These tests put the two halves of rebinding together as a replay runs them:
// the real MockManager (the binding table, the value index, the pools) under
// the real HTTP integration, serving requests off a connection. The matcher's
// own tests use a stand-in store, and the manager's never reach the matcher.

const (
	rmRec   = "11111111-1111-4111-8111-111111111111"
	rmLive  = "aaaaaaaa-aaaa-4aaa-8aaa-aaaaaaaaaaaa"
	rmOther = "cccccccc-cccc-4ccc-8ccc-cccccccccccc"
)

// rmMock is an HTTP mock as the agent holds one: what Agent.StoreMocks does to
// every mock before it is pooled is done to it (its lifetime derived, its
// request→response echoes found).
func rmMock(name, method, url, reqBody string, status int, respBody string, at time.Time) *models.Mock {
	header := map[string]string{}
	if reqBody != "" {
		header["Content-Type"] = "application/json"
	}
	m := &models.Mock{
		Name: name, Kind: models.Kind(models.HTTP),
		Spec: models.MockSpec{
			Metadata:         map[string]string{"type": models.HTTPClient, "operation": method},
			HTTPReq:          &models.HTTPReq{Method: models.Method(method), URL: url, Header: header, Body: reqBody},
			HTTPResp:         &models.HTTPResp{StatusCode: status, Header: map[string]string{"Content-Type": "application/json", "Content-Length": strconv.Itoa(len(respBody))}, Body: respBody},
			ReqTimestampMock: at, ResTimestampMock: at.Add(10 * time.Millisecond),
		},
	}
	m.DeriveLifetime()
	mocknoise.MaterializeCorrelations(m)
	return m
}

// rmReplay is a replay of pool that follows what the app mints (or nothing,
// when on is false), as `keploy mock replay` stages one.
type rmReplay struct {
	t    *testing.T
	mgr  *MockManager
	h    integrations.Integrations
	logs *observer.ObservedLogs
}

func newRMReplay(t *testing.T, on bool, pool ...*models.Mock) *rmReplay {
	t.Helper()
	ids.Default.Reset()
	t.Cleanup(ids.Default.Reset)
	core, logs := observer.New(zapcore.WarnLevel)
	mgr := NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	t.Cleanup(mgr.Close)
	mgr.SetRebinding(on, nil)
	var filtered, unfiltered []*models.Mock
	for _, m := range pool {
		if m.TestModeInfo.Lifetime == models.LifetimePerTest {
			filtered = append(filtered, m)
		} else {
			unfiltered = append(unfiltered, m)
		}
	}
	mgr.SetMocksWithWindow(filtered, unfiltered, models.BaseTime, time.Now())
	return &rmReplay{t: t, mgr: mgr, h: httpint.New(zap.New(core)), logs: logs}
}

// send serves one request and returns "<status> <body>".
func (r *rmReplay) send(method, path, body string) string {
	r.t.Helper()
	raw := fmt.Sprintf("%s %s HTTP/1.1\r\nHost: dep\r\n", method, path)
	if body != "" {
		raw += fmt.Sprintf("Content-Type: application/json\r\nContent-Length: %d\r\n", len(body))
	}
	resp, got, err := serveOne(r.t, r.h, r.mgr, raw+"\r\n"+body)
	if resp == nil {
		r.t.Fatalf("%s %s: no response: %v", method, path, err)
	}
	return fmt.Sprintf("%d %s", resp.StatusCode, got)
}

// A create made with the id the app minted binds it, and the read-back by
// that id is answered from the recorded read-back, naming it; the CLI can read
// the pair; the pooled recordings are as they were staged.
func TestRebinding_ACreateAndItsReadBackThroughTheMatcher(t *testing.T) {
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	create := rmMock("mock-1", "POST", "http://dep/items", `{"id":"`+rmRec+`","name":"w"}`, 201, `{"id":"`+rmRec+`","seq":1}`, at)
	read := rmMock("mock-2", "GET", "http://dep/items/"+rmRec, "", 200, `{"id":"`+rmRec+`","stock":5}`, at.Add(time.Second))
	r := newRMReplay(t, true, create, read)

	require.Equal(t, `201 {"id":"`+rmLive+`","seq":1}`, r.send("POST", "/items", `{"id":"`+rmLive+`","name":"w"}`))
	require.Equal(t, `200 {"id":"`+rmLive+`","stock":5}`, r.send("GET", "/items/"+rmLive, ""))
	require.Equal(t, map[string]string{rmRec: rmLive}, ids.Default.Pairs(), "the pair the replay CLI reads")
	require.Zero(t, r.logs.Len())

	// A call for an id nobody made is answered by the closest recording, as
	// it is without rebinding — as recorded — and said once.
	require.Equal(t, `200 {"id":"`+rmRec+`","stock":5}`, r.send("GET", "/items/"+rmOther, ""))
	require.Equal(t, `200 {"id":"`+rmRec+`","stock":5}`, r.send("GET", "/items/"+rmOther, ""))
	require.Equal(t, 1, r.logs.FilterMessageSnippet("names another id").Len())

	// The pool still holds the recordings as recorded: a match consumes or
	// updates the recording, never the copy that carried this run's id.
	pooled, err := r.mgr.GetSessionScopedMocks()
	require.NoError(t, err)
	for _, m := range pooled {
		require.NotContains(t, m.Spec.HTTPReq.URL+m.Spec.HTTPReq.Body, rmLive, m.Name)
		require.NotContains(t, m.Spec.HTTPResp.Body, rmLive, m.Name)
	}
	require.Equal(t, `200 {"id":"`+rmLive+`","stock":5}`, r.send("GET", "/items/"+rmLive, ""), "and the read-back still finds it")
}

// The same create recorded more than once is a stateful group: answered 201
// and then 409, or 503 and then 201. Made with the id the app minted, it
// advances through the group like any repeated request.
func TestRebinding_ARepeatedCreateThroughTheMatcher(t *testing.T) {
	for _, c := range []struct {
		statuses []int
		want     string
	}{
		{[]int{201, 409}, "201,409,409"},
		{[]int{503, 201}, "503,201,201"},
	} {
		at := time.Now().Add(-time.Hour).Truncate(time.Second)
		var pool []*models.Mock
		for i, status := range c.statuses {
			pool = append(pool, rmMock(fmt.Sprintf("mock-%d", i+1), "POST", "http://dep/items", `{"id":"`+rmRec+`","name":"w"}`, status,
				fmt.Sprintf(`{"id":"%s","status":%d}`, rmRec, status), at.Add(time.Duration(i)*time.Second)))
		}
		require.NotEmpty(t, pool[0].Spec.Correlations, "precondition: the create's answer echoes its id, so the agent correlates it")
		r := newRMReplay(t, true, pool...)
		var got []string
		for i := 0; i < 3; i++ {
			answer := r.send("POST", "/items", `{"id":"`+rmLive+`","name":"w"}`)
			require.Contains(t, answer, rmLive)
			got = append(got, answer[:3])
		}
		require.Equal(t, c.want, strings.Join(got, ","))
	}
}

// A recording whose id the app sends as recorded is a constant's: a later
// call that names another id in its place is answered as it is without
// rebinding, binds nothing, and the answers that follow still name the
// constant.
func TestRebinding_AConstantIsNotBoundThroughTheMatcher(t *testing.T) {
	at := time.Now().Add(-time.Hour).Truncate(time.Second)
	pool := func() []*models.Mock {
		return []*models.Mock{
			rmMock("mock-1", "GET", "http://dep/users/"+rmRec, "", 200, `{"id":"`+rmRec+`","name":"fixture-user"}`, at),
			rmMock("mock-2", "GET", "http://dep/whoami", "", 200, `{"user":"`+rmRec+`"}`, at.Add(time.Second)),
		}
	}
	on, off := newRMReplay(t, true, pool()...), newRMReplay(t, false, pool()...)
	for _, path := range []string{"/users/" + rmRec, "/users/" + rmOther, "/whoami"} {
		require.Equal(t, off.send("GET", path, ""), on.send("GET", path, ""), path)
	}
	require.Empty(t, ids.Default.Pairs())
}
