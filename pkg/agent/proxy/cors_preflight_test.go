package proxy

import (
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// A browser's CORS preflight, under the proven switch, is a phase of the call
// it guards (httpint corsPreflightPhase): how many a test sends follows how
// long the run took (Chromium keeps a server's yes five seconds when it sets
// no Access-Control-Max-Age), so a replay serves a recorded one as often as
// it is asked, to any test, and requires it of none. The guarded call stays
// the test's own: made once, required.

const (
	corsPreflight = "OPTIONS /query HTTP/1.1\r\nHost: localhost:8083\r\n" +
		"Origin: http://localhost:3000\r\nAccess-Control-Request-Method: POST\r\n" +
		"Access-Control-Request-Headers: content-type\r\n\r\n"
	corsOrigin = "http://localhost:3000"
)

// corsCall is a test's POST /query, body its own.
func corsCall(body string) string {
	return "POST /query HTTP/1.1\r\nHost: localhost:8083\r\nOrigin: " + corsOrigin +
		"\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
}

// corsPreflightMock is a preflight the server allowed, recorded at at; phase
// says recorded as the HTTP recorder records one under the switch (tagged
// config, stated session: what TestAnAllowedCORSPreflightIsRecordedAsAPhase
// OfTheCallItGuards pins), else as every exchange was before.
func corsPreflightMock(name string, at time.Time, phase bool) *models.Mock {
	meta := map[string]string{"type": models.HTTPClient, "operation": "OPTIONS", models.MetadataLifetimeKey: models.MetadataLifetimePerTest}
	lt := models.LifetimePerTest
	if phase {
		meta["type"] = "config"
		meta[models.MetadataLifetimeKey] = models.MetadataLifetimeSession
		lt = models.LifetimeSession
	}
	return &models.Mock{Version: "api.keploy.io/v1beta1", Name: name, Kind: models.HTTP, Spec: models.MockSpec{
		Metadata: meta,
		HTTPReq: &models.HTTPReq{Method: "OPTIONS", URL: "http://localhost:8083/query", Header: map[string]string{
			"Host": "localhost:8083", "Origin": corsOrigin,
			"Access-Control-Request-Method": "POST", "Access-Control-Request-Headers": "content-type",
		}},
		HTTPResp: &models.HTTPResp{StatusCode: 204, Header: map[string]string{
			"Access-Control-Allow-Origin": corsOrigin, "Access-Control-Allow-Methods": "POST",
			"Access-Control-Allow-Headers": "content-type",
		}},
		ReqTimestampMock: at,
		ResTimestampMock: at.Add(time.Millisecond),
	}, TestModeInfo: models.TestModeInfo{Lifetime: lt, LifetimeDerived: true}}
}

// corsCallMock is a test's POST /query, recorded at at.
func corsCallMock(name string, at time.Time, body string) *models.Mock {
	return &models.Mock{Version: "api.keploy.io/v1beta1", Name: name, Kind: models.HTTP, Spec: models.MockSpec{
		Metadata: map[string]string{"type": models.HTTPClient, "operation": "POST", models.MetadataLifetimeKey: models.MetadataLifetimePerTest},
		HTTPReq: &models.HTTPReq{Method: "POST", URL: "http://localhost:8083/query", Header: map[string]string{
			"Host": "localhost:8083", "Origin": corsOrigin, "Content-Type": "application/json",
		}, Body: body},
		HTTPResp: &models.HTTPResp{StatusCode: 200, Header: map[string]string{
			"Access-Control-Allow-Origin": corsOrigin, "Content-Type": "application/json",
			"Content-Length": strconv.Itoa(len(body)),
		}, Body: body},
		ReqTimestampMock: at,
		ResTimestampMock: at.Add(time.Millisecond),
	}, TestModeInfo: models.TestModeInfo{Lifetime: models.LifetimePerTest, LifetimeDerived: true}}
}

// corsOwned is what a test owns of its mapping, as the agent's begin hands
// it to the proxy (service/agent testMocks): its data-plane mocks.
func corsOwned(mapping ...*models.Mock) []*models.Mock {
	var out []*models.Mock
	for _, m := range mapping {
		if !m.IsProtocolPhase() {
			out = append(out, m)
		}
	}
	return out
}

// corsTest is one test of the row: what its recording maps to it, and what
// its replay sends.
type corsTest struct {
	name    string
	mapping []*models.Mock
	sends   []string
}

// corsRun serves tests one after another as a replay does, and returns, per
// test, the calls that missed (by index of sends) and the recordings it left
// unconsumed.
func corsRun(t *testing.T, set []*models.Mock, tests []corsTest) (missed map[string][]int, unconsumed map[string][]string) {
	t.Helper()
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SetMocksWithWindow(nil, set, models.BaseTime, time.Now().Add(time.Hour))
	mm.ServeExactPool("k", nil)
	missed, unconsumed = map[string][]int{}, map[string][]string{}
	for _, tc := range tests {
		mm.ServeExactTest(tc.name, corsOwned(tc.mapping...))
		for i, req := range tc.sends {
			if r := sendForMiss(t, mm, models.OutgoingOptions{}, nil, req); r != nil {
				missed[tc.name] = append(missed[tc.name], i)
			}
		}
		test, _, left, ok := mm.ExactTestOutcome()
		if !ok || test != tc.name {
			t.Fatalf("not serving %s exactly at its end: %q %v", tc.name, test, ok)
		}
		if len(left) > 0 {
			unconsumed[tc.name] = left
		}
		mm.ServeExactPool("k", nil)
	}
	return missed, unconsumed
}

// The set: test "more" recorded two preflights and sends three (its replay
// ran slower); "fewer" recorded one and sends none (Chromium still held the
// yes); "none" recorded none (the record's yes was still fresh) and sends
// one; "call" sends the preflight but never the call it guards.
func corsSet(phase bool) ([]*models.Mock, []corsTest) {
	t0 := time.Date(2026, 10, 27, 16, 0, 0, 0, time.UTC)
	pfMore1, pfMore2 := corsPreflightMock("pf-more-1", t0, phase), corsPreflightMock("pf-more-2", t0.Add(2*time.Second), phase)
	callMore := corsCallMock("call-more", t0.Add(3*time.Second), `{"q":"more"}`)
	pfFewer := corsPreflightMock("pf-fewer", t0.Add(10*time.Second), phase)
	callFewer := corsCallMock("call-fewer", t0.Add(11*time.Second), `{"q":"fewr"}`)
	callNone := corsCallMock("call-none", t0.Add(12*time.Second), `{"q":"none"}`)
	pfCall := corsPreflightMock("pf-call", t0.Add(20*time.Second), phase)
	callCall := corsCallMock("call-call", t0.Add(21*time.Second), `{"q":"call"}`)
	set := []*models.Mock{pfMore1, pfMore2, callMore, pfFewer, callFewer, callNone, pfCall, callCall}
	return set, []corsTest{
		{"more", []*models.Mock{pfMore1, pfMore2, callMore}, []string{corsPreflight, corsPreflight, corsPreflight, corsCall(`{"q":"more"}`)}},
		{"fewer", []*models.Mock{pfFewer, callFewer}, []string{corsCall(`{"q":"fewr"}`)}},
		{"none", []*models.Mock{callNone}, []string{corsPreflight, corsCall(`{"q":"none"}`)}},
		{"call", []*models.Mock{pfCall, callCall}, []string{corsPreflight}},
	}
}

// Recorded as a phase, a preflight answers every one a test sends, more than
// it recorded, in a test that recorded none too; one a test does not send is
// required of none. The call it guards is still required: a test that sends
// the preflight but not the call fails on the call.
func TestACORSPreflightIsServedAsOftenAsAskedAndRequiredByNoTest(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	set, tests := corsSet(true)
	missed, unconsumed := corsRun(t, set, tests)
	if len(missed) != 0 {
		t.Fatalf("a preflight or a call missed (test: indexes of what it sent): %v", missed)
	}
	if len(unconsumed) != 1 || len(unconsumed["call"]) != 1 || unconsumed["call"][0] != "call-call" {
		t.Fatalf("unconsumed %v; want only the call test call made no call of, call-call", unconsumed)
	}
}

// The same set recorded as every exchange was before: each preflight is its
// test's data, so the slower test's third preflight is an extra call, the
// faster test's unsent one is a recorded call never made, and the test that
// recorded none misses its preflight. What the row above passes is the phase.
func TestACORSPreflightRecordedAsTheTestsDataFailsWhereTheRunsPaceMoves(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	set, tests := corsSet(false)
	missed, unconsumed := corsRun(t, set, tests)
	if got := missed["more"]; len(got) != 1 || got[0] != 2 {
		t.Errorf("more: missed %v; want its third preflight (2)", got)
	}
	if got := missed["none"]; len(got) != 1 || got[0] != 0 {
		t.Errorf("none: missed %v; want its preflight (0)", got)
	}
	if got := unconsumed["fewer"]; len(got) != 1 || got[0] != "pf-fewer" {
		t.Errorf("fewer: unconsumed %v; want pf-fewer", got)
	}
}
