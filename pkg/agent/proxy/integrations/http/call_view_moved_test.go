package http

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// movedGateDb is gateDb, holding each call to itself, whose call's view
// moved (integrations.CallViewTeller): since it was held (moved), inside its
// read of the test's answered recordings (inList), or inside its second read
// of them (inDiag: the answered diagnosis's, after the extra-call count
// read the list in one view), so that only a question asked after that read
// sees it. reads counts the reads of the list.
type movedGateDb struct {
	gateDb
	moved, inList, inDiag bool
	reads                 int
}

func (d *movedGateDb) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return d, func() bool { return false }
}

func (d *movedGateDb) ConsumedTestMocks() []*models.Mock {
	d.reads++
	if d.inList || (d.inDiag && d.reads == 2) {
		d.moved = true
	}
	return d.consumed
}

func (d *movedGateDb) CallViewMoved() bool { return d.moved }

// movedPairingDb is pairingDb in record's full round whose open test
// answered consumed, and whose call's view moved since it was held (moved).
type movedPairingDb struct {
	*pairingDb
	consumed []*models.Mock
	moved    bool
}

func (d *movedPairingDb) ConsumedTestMocks() []*models.Mock { return d.consumed }
func (d *movedPairingDb) CallViewMoved() bool               { return d.moved }

// Under exact serving an HTTP call whose view moved between its reads (a
// test's begin or end installed another view since it was held, or while it
// read the test's answered recordings) is told no verdict built from the
// test's answered list: its miss and the list may be of two views. It is not
// one call more than recorded, it is diagnosed against none of the answered
// recordings, no recording made elsewhere and no repeat note are named for
// it, and in record's full round it pairs with no other test's recording: it
// misses plainly. With no move, the same store tells each. A changed call
// whose view moved only inside the answered diagnosis's read of the list,
// after the extra-call count read it in one view, is not diagnosed against
// it either. Before, the answered list was read whatever view it was of.
func TestAnHTTPCallWhoseViewMovedIsToldNoVerdictOfTheAnsweredList(t *testing.T) {
	const addr, host = "127.0.0.1:8099", "localhost:8099"
	const path = "/sap/A_BusinessPartner/$count?$filter=x"
	t.Setenv(provenswitch.Env, "1")
	rec := time.Date(2026, 10, 24, 9, 0, 0, 0, time.UTC)
	earlier := earlierDb{mockMemDb: &mockMemDb{}, answered: []integrations.AnsweredCall{{
		Mock: sapCount("mock-7", host, rec), AnsweredAt: time.Now().Add(-time.Hour),
	}}}
	alone := earlierDb{mockMemDb: &mockMemDb{}}
	own := onHost("mock-own", path, host)
	changed := onHost("mock-c", path+"&$top=1", host)
	elsewhere := onHost("mock-w", path, host)
	elsewhere.Spec.HTTPReq.Header["X-Request-Id"] = "live-9"
	for _, mv := range []struct {
		name                  string
		moved, inList, inDiag bool
	}{
		{name: "not moved"},
		{name: "moved since the hold", moved: true},
		{name: "moved inside the read of the answered list", inList: true},
		{name: "moved inside the answered diagnosis's read of the list", inDiag: true},
	} {
		t.Run(mv.name, func(t *testing.T) {
			if mv.inDiag {
				d := &movedGateDb{gateDb: gateDb{earlierDb: alone, consumed: []*models.Mock{changed}}, inDiag: true}
				r := missReport(t, d, addr, host, path, models.OutgoingOptions{})
				if d.reads < 2 || !d.moved {
					t.Fatalf("the store's list was read %d times (moved %v); want the extra-call count's read and the diagnosis's", d.reads, d.moved)
				}
				if r.ClosestMock == "mock-c" {
					t.Errorf("a changed call whose view moved inside the diagnosis's read is diagnosed against the test's answered mock-c; want it diagnosed against none of the list")
				}
				return
			}
			told := !mv.moved && !mv.inList
			db := func(g gateDb) *movedGateDb { return &movedGateDb{gateDb: g, moved: mv.moved, inList: mv.inList} }
			r := missReport(t, db(gateDb{earlierDb: alone, consumed: []*models.Mock{own}}), addr, host, path, models.OutgoingOptions{})
			if extra := r.MissKind == models.MissExtraCall && r.Recorded == 1; extra != told {
				t.Errorf("a call identical to the test's answered one missed as %q, recorded %d; want one more than recorded %v", r.MissKind, r.Recorded, told)
			}
			r = missReport(t, db(gateDb{earlierDb: alone, consumed: []*models.Mock{changed}}), addr, host, path, models.OutgoingOptions{})
			if diagnosed := r.ClosestMock == "mock-c"; diagnosed != told {
				t.Errorf("a changed call's closest is %q; want it diagnosed against the test's answered mock-c %v", r.ClosestMock, told)
			}
			r = missReport(t, db(gateDb{earlierDb: alone, staged: []*models.Mock{elsewhere}}), addr, host, path, models.OutgoingOptions{})
			if named := r.NotOwned && r.ClosestMock == "mock-w"; named != told {
				t.Errorf("a call another test recorded is named %q (not owned %v); want it named made elsewhere %v", r.ClosestMock, r.NotOwned, told)
			}
			r = missReport(t, db(gateDb{earlierDb: earlier}), addr, host, path, models.OutgoingOptions{})
			if noted := r.Repeat != nil && r.Repeat.Mock == "mock-7"; noted != told {
				t.Errorf("a call an earlier test's answered recording is identical to has the note %+v; want it noted %v", r.Repeat, told)
			}
			if mv.inList {
				return
			}
			status, claimed := pairUnder(t, addr, host, &movedPairingDb{pairingDb: &pairingDb{mockMemDb: &mockMemDb{}, others: []*models.Mock{answered(onHost("other-test-delete", "/delete", host), time.Second, 200, `{"deleted":1}`)}}, moved: mv.moved})
			if paired := status == http.StatusOK && claimed; paired != told {
				t.Errorf("in record's full round a call another test recorded got %d, claimed %v; want it paired %v", status, claimed, told)
			}
		})
	}
}

// pairUnder is the status a GET of /delete to host gets in record's full
// round from db, and whether db's recording other-test-delete was claimed.
func pairUnder(t *testing.T, addr, host string, db *movedPairingDb) (int, bool) {
	t.Helper()
	ctx := models.WithMockMismatchReporter(context.Background(), func(error) {})
	app, prox := net.Pipe()
	t.Cleanup(func() { _ = app.Close(); _ = prox.Close() })
	done := make(chan error, 1)
	go func() {
		done <- newHTTP().MockOutgoing(ctx, prox, &models.ConditionalDstCfg{Addr: addr}, db, models.OutgoingOptions{Learn: true})
	}()
	if _, err := io.WriteString(app, "GET /delete HTTP/1.1\r\nHost: "+host+"\r\nAccept: application/json\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	resp, err := http.ReadResponse(bufio.NewReader(app), nil)
	if err != nil {
		t.Fatalf("the app got no answer: %v", err)
	}
	_ = resp.Body.Close()
	_ = app.Close()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the parser did not return")
	}
	return resp.StatusCode, db.claimed["other-test-delete"]
}

// newHoldDb is roundGateDb whose every HoldToRound hands out a NEW held
// store, as the agent's MockManager does (a fresh heldScope a hold), so the
// full round's reads are of whichever hold it makes. Which of them say the
// call's view moved: every one (sticky), or only the one whose list read was
// the call's first (firstOnly: a view installed after that hold, before its
// question, so a hold made later, in the new view, is not moved). holds
// counts the holds, reads the reads of the test's answered list.
type newHoldDb struct {
	roundGateDb
	sticky, firstOnly bool
	holds, reads      int
}

type newHeld struct {
	*newHoldDb
	moved bool
}

func (d *newHoldDb) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	d.holds++
	return &newHeld{newHoldDb: d, moved: d.sticky}, func() bool { return false }
}

func (h *newHeld) ConsumedTestMocks() []*models.Mock {
	h.reads++
	if h.firstOnly && h.reads == 1 {
		h.moved = true
	}
	return h.consumed
}

func (h *newHeld) CallViewMoved() bool { return h.moved }

// In record's full round an HTTP call identical to its test's answered
// recording is held once: the pairing's own-repeat count and the miss read
// one hold, so a move that hold saw is seen by the miss too, and the call
// misses plainly, not as one call more than recorded. Before, the miss held
// the call again, and a hold made after the view moved, in the new view,
// told it an extra call from the new view's answered list.
func TestAFullRoundCallWhoseViewMovedAtItsPairingMissesPlainly(t *testing.T) {
	const addr, host = "127.0.0.1:8099", "localhost:8099"
	const path = "/sap/A_BusinessPartner/$count?$filter=x"
	t.Setenv(provenswitch.Env, "1")
	alone := earlierDb{mockMemDb: &mockMemDb{}}
	own := onHost("mock-own", path, host)
	for _, tc := range []struct {
		name              string
		sticky, firstOnly bool
		wantExtra         bool
	}{
		{name: "not moved", wantExtra: true},
		{name: "moved at every hold", sticky: true},
		{name: "moved as the pairing's count asks", firstOnly: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			d := &newHoldDb{roundGateDb: roundGateDb{gateDb{earlierDb: alone, consumed: []*models.Mock{own}}}, sticky: tc.sticky, firstOnly: tc.firstOnly}
			r := missReport(t, d, addr, host, path, models.OutgoingOptions{Learn: true})
			if extra := r.MissKind == models.MissExtraCall; extra != tc.wantExtra {
				t.Errorf("missed as %q (recorded %d); want an extra call %v", r.MissKind, r.Recorded, tc.wantExtra)
			}
			if d.holds != 1 {
				t.Errorf("the call was held %d times; want once", d.holds)
			}
		})
	}
}

// viewHoldDb is roundGateDb whose view a test's begin or end replaces (view:
// a new number at each install), each hold of a call held, as the agent's
// MockManager holds one, to the view its round snapshot read
// (integrations.RoundParts.View, integrations.CallRound.StoreView), so a
// view installed after that read is a move to every hold. install says when
// a view is installed: as the call is held (after the learn step's read,
// before its hold), or inside the hold's first read of the test's answered
// recordings.
type viewHoldDb struct {
	roundGateDb
	install            string
	view, holds, reads int
}

type viewHeld struct {
	*viewHoldDb
	in int
}

func (d *viewHoldDb) RoundSnapshot(q integrations.RoundQuery) (integrations.RoundSnapshot, bool) {
	s, ok := d.roundGateDb.RoundSnapshot(q)
	return s.WithParts(integrations.RoundParts{View: d.view}), ok
}

func (d *viewHoldDb) HoldToRound(call integrations.CallRound) (integrations.MockMemDb, func() bool) {
	d.holds++
	if d.install == "at the hold" {
		d.view++
	}
	in := d.view
	if v, ok := call.StoreView().(int); ok {
		in = v
	}
	return &viewHeld{viewHoldDb: d, in: in}, func() bool { return false }
}

func (h *viewHeld) ConsumedTestMocks() []*models.Mock {
	h.reads++
	if h.install == "inside the list's read" && h.reads == 1 {
		h.view++
	}
	return h.consumed
}

func (h *viewHeld) CallViewMoved() bool { return h.view != h.in }

// In record's full round an HTTP call whose learn step read one view is held
// to it: a view a test's begin or end installs after that read, before the
// call's one hold or while it reads the answered list, is a move, and the
// call identical to its test's answered recording misses plainly, never told
// one call more than recorded from the next view's list. With no install it
// is that extra call.
func TestAFullRoundCallIsHeldToTheViewItsLearnStepRead(t *testing.T) {
	const addr, host = "127.0.0.1:8099", "localhost:8099"
	const path = "/sap/A_BusinessPartner/$count?$filter=x"
	t.Setenv(provenswitch.Env, "1")
	alone := earlierDb{mockMemDb: &mockMemDb{}}
	own := onHost("mock-own", path, host)
	for _, install := range []string{"never", "at the hold", "inside the list's read"} {
		t.Run("a view installed "+install, func(t *testing.T) {
			d := &viewHoldDb{roundGateDb: roundGateDb{gateDb{earlierDb: alone, consumed: []*models.Mock{own}}}, install: install}
			r := missReport(t, d, addr, host, path, models.OutgoingOptions{Learn: true})
			if extra := r.MissKind == models.MissExtraCall; extra != (install == "never") {
				t.Errorf("missed as %q (recorded %d); want an extra call %v", r.MissKind, r.Recorded, install == "never")
			}
			if d.holds != 1 || d.reads == 0 {
				t.Errorf("the call was held %d times and read the answered list %d times; want one hold, read", d.holds, d.reads)
			}
		})
	}
}

// A miss whose store no hold of record's full round tells of a staging says
// no set was staged again under it: with exact serving off, and in record's
// full round when the call's hold found no staging, the call that misses,
// moved or not, gets no such note.
func TestAMissNoStagingMovedUnderSaysNoSetWasStagedAgain(t *testing.T) {
	const addr, host = "127.0.0.1:8099", "localhost:8099"
	const path = "/sap/A_BusinessPartner/$count?$filter=x"
	alone := earlierDb{mockMemDb: &mockMemDb{}}
	own := onHost("mock-own", path, host)
	for _, tc := range []struct {
		name   string
		proven string
		opts   models.OutgoingOptions
		sticky bool
	}{
		{name: "exact serving off", proven: "0"},
		{name: "record's full round, moved", proven: "1", opts: models.OutgoingOptions{Learn: true}, sticky: true},
		{name: "record's full round, not moved", proven: "1", opts: models.OutgoingOptions{Learn: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Setenv(provenswitch.Env, tc.proven)
			d := &newHoldDb{roundGateDb: roundGateDb{gateDb{earlierDb: alone, consumed: []*models.Mock{own}}}, sticky: tc.sticky}
			r := missReport(t, d, addr, host, path, tc.opts)
			if strings.Contains(r.NextSteps, restagedNote) {
				t.Fatalf("a miss no staging moved under says a set was staged again: %s", r.NextSteps)
			}
		})
	}
}
