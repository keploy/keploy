package http

import (
	"bufio"
	"context"
	"io"
	"net"
	"net/http"
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
