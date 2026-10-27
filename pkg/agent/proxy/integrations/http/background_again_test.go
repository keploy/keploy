package http

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// againDb is gateDb whose open test's marked background recordings that
// answered are marked (integrations.AnsweredBackgroundReader). It holds each
// call to its round as itself, so the matcher and the miss read it.
type againDb struct {
	gateDb
	marked []*models.Mock
}

func (d againDb) AnsweredBackground() []*models.Mock { return d.marked }

func (d againDb) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return d, func() bool { return false }
}

// answerOf is what the app reads for raw, a whole HTTP/1.1 request to addr
// the HTTP parser answers from db (its status and body), and the report the
// parser made of the call: a miss's, or an answered call's.
func answerOf(t *testing.T, db integrations.MockMemDb, addr, raw string) (int, string, *models.MockMismatchReport) {
	t.Helper()
	var reported error
	ctx := models.WithMockMismatchReporter(context.Background(), func(err error) { reported = err })
	app, prox := net.Pipe()
	defer func() { _ = app.Close() }()
	// A parser that answers nothing fails the row, never hangs it.
	_ = app.SetDeadline(time.Now().Add(10 * time.Second))
	done := make(chan error, 1)
	go func() {
		done <- newHTTP().MockOutgoing(ctx, prox, &models.ConditionalDstCfg{Addr: addr}, db, models.OutgoingOptions{})
	}()
	if _, err := io.WriteString(app, raw); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		t.Fatalf("the app got no answer: %v", err)
	}
	body, _ := io.ReadAll(resp.Body)
	_ = resp.Body.Close()
	_ = app.Close()
	select {
	case err := <-done:
		// How the parser ends once the app closed is no part of the row.
		_ = err
	case <-time.After(10 * time.Second):
		t.Fatal("the parser did not return")
	}
	var withReport interface {
		MismatchReport() *models.MockMismatchReport
	}
	if !errors.As(reported, &withReport) || withReport.MismatchReport() == nil {
		t.Fatalf("the call was reported as %v, with no report", reported)
	}
	return resp.StatusCode, string(body), withReport.MismatchReport()
}

// A timer's call that judging marked as the application's background work
// answered once in its test's window; the timer fires again in the window
// (the replay ran slower, or the timer faster, than at record). The second
// call is answered again with the marked recording, unchecked, and reported
// as such naming it, instead of failing the test as one call more than it
// recorded. An unmarked recording's second call, a call that differs from
// the marked recording, and any call with the switch off still miss.
func TestABackgroundCallMadeAgainInItsWindowIsAnsweredAgain(t *testing.T) {
	const addr, host, path = "127.0.0.1:8099", "localhost:8099", "/internal/ordinals/recompute"
	raw := "GET " + path + " HTTP/1.1\r\nHost: " + host + "\r\nAccept: application/json\r\nX-Request-Id: live-9\r\n\r\n"
	tick := onHost("mock-bg", path, host)
	tick.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Body: "recomputed", Header: map[string]string{"Content-Length": "10"}}
	own := onHost("mock-own", path, host)
	own.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Body: "own", Header: map[string]string{"Content-Length": "3"}}
	store := func(answered []*models.Mock, marked ...*models.Mock) againDb {
		return againDb{gateDb: gateDb{earlierDb: earlierDb{mockMemDb: &mockMemDb{}}, consumed: answered}, marked: marked}
	}

	t.Setenv(provenswitch.Env, "1")
	status, body, r := answerOf(t, store([]*models.Mock{tick}, tick), addr, raw)
	if status != 200 || body != "recomputed" || r.Answer != models.AnswerBackgroundAgain || r.ClosestMock != "mock-bg" || r.CountsAgainstTest() {
		t.Fatalf("a background call made again got %d %q, reported %+v; want mock-bg's answer, reported answered again naming mock-bg", status, body, r)
	}

	// Unmarked: the test's own call made once more than it recorded.
	status, _, r = answerOf(t, store([]*models.Mock{own}), addr, raw)
	if status != 502 || r.Answer != "" || r.MissKind != models.MissExtraCall {
		t.Fatalf("an unmarked recording's call made again got %d, reported %+v; want the extra-call miss", status, r)
	}
	// Marked, but another call: no recording the call repeats.
	other := onHost("mock-bg", "/internal/leases/sweep", host)
	other.Spec.HTTPResp = &models.HTTPResp{StatusCode: 200, Body: "swept", Header: map[string]string{"Content-Length": "5"}}
	status, _, r = answerOf(t, store([]*models.Mock{other}, other), addr, raw)
	if status != 502 || r.Answer != "" || r.MissKind == models.MissExtraCall {
		t.Fatalf("a call that repeats no marked recording got %d, reported %+v; want a miss", status, r)
	}

	t.Setenv(provenswitch.Env, "0")
	status, _, r = answerOf(t, store([]*models.Mock{tick}, tick), addr, raw)
	if status != 502 || r.Answer != "" {
		t.Fatalf("with the switch off a background call made again got %d, reported %+v; want the miss main makes", status, r)
	}
}
