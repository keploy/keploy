package http

import (
	"bufio"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// pairingDb is a store in record's full round of a set served by S12's
// rules: it offers other tests' recordings to pair (integrations.Pairing),
// claims each once, and holds a background (integrations.ByKindBackground).
// own are the recordings it says are the open test's own
// (integrations.TestOwner). offersClaimed: it offers claimed recordings too,
// as a store does whose offer another connection's call claimed from since.
type pairingDb struct {
	*mockMemDb
	others        []*models.Mock
	bg            []*models.Mock
	own           map[string]bool
	inFlight      map[string]bool
	claimed       map[string]bool
	offersClaimed bool
}

func (d *pairingDb) PairingCandidates(kind models.Kind) []*models.Mock {
	var out []*models.Mock
	for _, m := range d.others {
		if m.Kind == kind && (d.offersClaimed || !d.claimed[m.Name]) {
			out = append(out, m)
		}
	}
	return out
}

func (d *pairingDb) ClaimPairing(m *models.Mock) bool {
	if d.claimed == nil {
		d.claimed = map[string]bool{}
	}
	if d.claimed[m.Name] {
		return false
	}
	d.claimed[m.Name] = true
	return true
}

func (d *pairingDb) KindBackground(models.Kind) (unused, spent []*models.Mock) { return d.bg, nil }
func (d *pairingDb) SpendByKind(*models.Mock) bool                             { return false }
func (d *pairingDb) OwnedByOpenTest(m *models.Mock) bool                       { return d.own[m.Name] }
func (d *pairingDb) InFlightIntoOpenTest(m *models.Mock) bool                  { return d.inFlight[m.Name] }

var (
	_ integrations.Pairing          = (*pairingDb)(nil)
	_ integrations.ByKindBackground = (*pairingDb)(nil)
	_ integrations.TestOwner        = (*pairingDb)(nil)
	_ integrations.InFlightOwner    = (*pairingDb)(nil)
)

// answered is m recorded at at, answering status with body.
func answered(m *models.Mock, at time.Duration, status int, body string) *models.Mock {
	m.Spec.ReqTimestampMock = time.Unix(1_700_000_000, 0).Add(at)
	m.Spec.HTTPResp = &models.HTTPResp{StatusCode: status, Body: body, Header: map[string]string{"Content-Type": "application/json", "Content-Length": strconv.Itoa(len(body))}}
	return m
}

// Record's full round pairs an HTTP call nothing else answered with a
// recording another test owns only when the call is exactly that recording
// (HTTP pairs exactly, never by drift); the first in recorded
// order when recordings of the call hold one answer, none when they, or the
// background's, hold different ones; a recording claimed once is passed
// over; a poll (an SQS ReceiveMessage), the open test's own recording and a
// store that offers nothing never pair.
func TestHTTPPairsExactlyWithAnotherTestsRecording(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const host = "localhost:50001"
	h := newHTTP()
	n := h.newStrictNoise(nil, nil, nil)
	ctx := context.Background()
	call := getTo("/delete", host)
	x := func(name string, at time.Duration, body string) *models.Mock {
		return answered(onHost(name, "/delete", host), at, 200, body)
	}

	t.Run("exact", func(t *testing.T) {
		later, first := x("mock-9", 2*time.Second, `{"ok":true}`), x("mock-5", time.Second, `{"ok":true}`)
		db := &pairingDb{mockMemDb: &mockMemDb{}, others: []*models.Mock{later, first}}
		answer, rec := h.pairExactly(ctx, call, db, n)
		if rec != first || answer == nil || answer.Spec.HTTPResp.Body != `{"ok":true}` {
			t.Fatalf("paired %v; want mock-5, the first recorded", rec)
		}
		if !db.claimed["mock-5"] || db.claimed["mock-9"] {
			t.Fatalf("claims %v; want mock-5 alone", db.claimed)
		}
		// Claimed, it is passed over: the next call pairs with mock-9.
		if _, rec := h.pairExactly(ctx, call, db, n); rec != later {
			t.Fatalf("a second call paired %v; want mock-9", rec)
		}
		// Every recording of the call claimed: none.
		if _, rec := h.pairExactly(ctx, call, db, n); rec != nil {
			t.Fatalf("a third call paired %s", rec.Name)
		}
	})
	t.Run("a recording another call claimed after the store offered it", func(t *testing.T) {
		// Another connection's call claimed mock-5 between the store's offer
		// and this call's claim: the claim alone decides, and mock-5 is
		// passed over.
		first, later := x("mock-5", time.Second, `{"ok":true}`), x("mock-9", 2*time.Second, `{"ok":true}`)
		db := &pairingDb{mockMemDb: &mockMemDb{}, others: []*models.Mock{later, first}, offersClaimed: true, claimed: map[string]bool{"mock-5": true}}
		if _, rec := h.pairExactly(ctx, call, db, n); rec != later {
			t.Fatalf("paired %v; want mock-9: mock-5 was another call's claim", rec)
		}
		if _, rec := h.pairExactly(ctx, call, db, n); rec != nil {
			t.Fatalf("paired %s; every recording of the call was claimed", rec.Name)
		}
	})
	t.Run("drift never pairs", func(t *testing.T) {
		db := &pairingDb{mockMemDb: &mockMemDb{}, others: []*models.Mock{x("mock-5", time.Second, `{}`)}}
		if _, rec := h.pairExactly(ctx, getTo("/delete-2", host), db, n); rec != nil {
			t.Fatalf("a call that differs from %s was paired with it", rec.Name)
		}
	})
	t.Run("ties whose answers differ in their Date alone", func(t *testing.T) {
		first, later := x("mock-5", time.Second, `{"n":1}`), x("mock-9", 2*time.Second, `{"n":1}`)
		first.Spec.HTTPResp.Header["Date"] = "Thu, 22 Oct 2026 11:14:16 GMT"
		later.Spec.HTTPResp.Header["Date"] = "Thu, 22 Oct 2026 11:14:17 GMT"
		db := &pairingDb{mockMemDb: &mockMemDb{}, others: []*models.Mock{later, first}}
		if _, rec := h.pairExactly(ctx, call, db, n); rec != first {
			t.Fatalf("paired %v; want mock-5, the first recorded: answers a second apart are one", rec)
		}
		other := x("mock-9", 2*time.Second, `{"n":1}`)
		other.Spec.HTTPResp.Header["X-Request-Id"] = "r-2"
		db = &pairingDb{mockMemDb: &mockMemDb{}, others: []*models.Mock{x("mock-5", time.Second, `{"n":1}`), other}}
		if _, rec := h.pairExactly(ctx, call, db, n); rec != nil {
			t.Fatalf("paired %s; answers with another header are two", rec.Name)
		}
	})
	t.Run("ties with different answers", func(t *testing.T) {
		db := &pairingDb{mockMemDb: &mockMemDb{}, others: []*models.Mock{x("mock-5", time.Second, `{"n":1}`), x("mock-9", 2*time.Second, `{"n":2}`)}}
		if _, rec := h.pairExactly(ctx, call, db, n); rec != nil || len(db.claimed) != 0 {
			t.Fatalf("recordings of the call with different answers paired %v", rec)
		}
	})
	t.Run("a background sibling with another answer", func(t *testing.T) {
		db := &pairingDb{mockMemDb: &mockMemDb{}, others: []*models.Mock{x("mock-5", time.Second, `{"n":1}`)}, bg: []*models.Mock{x("mock-2", 0, `{"n":2}`)}}
		if _, rec := h.pairExactly(ctx, call, db, n); rec != nil {
			t.Fatalf("paired %s beside a background recording of the call with another answer", rec.Name)
		}
		db = &pairingDb{mockMemDb: &mockMemDb{}, others: []*models.Mock{x("mock-5", time.Second, `{"n":1}`)}, bg: []*models.Mock{x("mock-2", 0, `{"n":1}`)}}
		if _, rec := h.pairExactly(ctx, call, db, n); rec == nil {
			t.Fatal("a background recording of the call with the same answer kept the call from pairing")
		}
	})
	t.Run("a poll never pairs", func(t *testing.T) {
		poll := answered(onHost("mock-4", "/", host), time.Second, 200, `{"Messages":[]}`)
		poll.Spec.HTTPReq.Header["X-Amz-Target"] = "AmazonSQS.ReceiveMessage"
		db := &pairingDb{mockMemDb: &mockMemDb{}, others: []*models.Mock{poll}}
		live := getTo("/", host)
		live.header.Set("X-Amz-Target", "AmazonSQS.ReceiveMessage")
		if _, rec := h.pairExactly(ctx, live, db, n); rec != nil {
			t.Fatal("a ReceiveMessage paired")
		}
		form := getTo("/", host)
		form.method = "POST"
		form.header.Set("Content-Type", "application/x-www-form-urlencoded")
		form.body = []byte("Action=ReceiveMessage&QueueUrl=q")
		if !sqsPoll(form) {
			t.Fatal("a query-protocol ReceiveMessage is not a poll")
		}
		if sqsPoll(call) {
			t.Fatal("a GET is a poll")
		}
		// The query protocol's ReceiveMessage in the query string: exactly
		// its recording, it pairs with none; another action of that
		// protocol, exactly its recording, pairs.
		for _, action := range []string{"ReceiveMessage", "DeleteMessage"} {
			q := "Action=" + action + "&QueueUrl=q"
			rec := answered(onHost("mock-4", "/", host), time.Second, 200, `<ReceiveMessageResponse/>`)
			rec.Spec.HTTPReq.URL = "/?" + q
			rec.Spec.HTTPReq.URLParams = map[string]string{"Action": action, "QueueUrl": "q"}
			db := &pairingDb{mockMemDb: &mockMemDb{}, others: []*models.Mock{rec}}
			get := getTo("/", host)
			get.url.RawQuery = q
			_, got := h.pairExactly(ctx, get, db, n)
			if poll := action == "ReceiveMessage"; sqsPoll(get) != poll || poll && got != nil || !poll && got != rec {
				t.Fatalf("?%s: a poll %v, paired %v; want a poll and no pairing for ReceiveMessage alone", q, sqsPoll(get), got)
			}
		}
	})
	t.Run("a recording that declares variables, an async lane's or a protocol phase's never pairs", func(t *testing.T) {
		for name, mark := range map[string]func(*models.Mock){
			"none": func(*models.Mock) {},
			"variables": func(m *models.Mock) {
				m.Spec.Variables = models.MockVariables{"orderId": {Recorded: "o-1", Answer: []string{"body.id"}}}
			},
			"an async lane's":    func(m *models.Mock) { m.Spec.Async = &models.AsyncMeta{} },
			"a protocol phase's": func(m *models.Mock) { m.Spec.Metadata = map[string]string{"type": "config"} },
		} {
			x := x("mock-5", time.Second, `{"id":"o-1"}`)
			mark(x)
			db := &pairingDb{mockMemDb: &mockMemDb{}, others: []*models.Mock{x}}
			if _, rec := h.pairExactly(ctx, call, db, n); name == "none" && rec != x || name != "none" && rec != nil {
				t.Fatalf("%s: paired %v", name, rec)
			}
		}
	})
	t.Run("the open test's own, or nothing offered", func(t *testing.T) {
		mine := x("mock-5", time.Second, `{}`)
		db := &pairingDb{mockMemDb: &mockMemDb{}, others: []*models.Mock{mine}, own: map[string]bool{"mock-5": true}}
		if _, rec := h.pairExactly(ctx, call, db, n); rec != nil {
			t.Fatal("the open test's own recording paired")
		}
		if _, rec := h.pairExactly(ctx, call, &mockMemDb{mocks: []*models.Mock{mine}}, n); rec != nil {
			t.Fatal("a store that offers nothing to pair paired")
		}
		// In flight into the open test (sent before it began, answered in
		// it): the open test's own too, though the store offers it.
		db = &pairingDb{mockMemDb: &mockMemDb{}, others: []*models.Mock{mine}, inFlight: map[string]bool{"mock-5": true}}
		if _, rec := h.pairExactly(ctx, call, db, n); rec != nil {
			t.Fatal("a recording in flight into the open test paired")
		}
	})
}

// roundPairingDb is pairingDb read in one snapshot of the round
// (integrations.RoundReader), as the agent's MockManager is under exact
// serving: the store shows test-1's view and the tracker has it open, its
// candidates and background are pairingDb's, owners says whose the view
// holds each recording as, and a claim lands only while the round is still
// at the snapshot (moved: that many claims find it moved first, each
// followed by a fresh read).
type roundPairingDb struct {
	*pairingDb
	owners map[string]integrations.RoundOwner
	moved  int
	reads  int
}

func (d *roundPairingDb) RoundSnapshot(q integrations.RoundQuery) (integrations.RoundSnapshot, bool) {
	d.reads++
	s := integrations.RoundSnapshot{
		At:   integrations.RoundAt{Epoch: 1, Gen: uint64(d.reads)},
		Test: "test-1", Phase: integrations.RoundInTest, Open: true, Background: true,
		BackgroundUnused: d.bg,
		Candidates:       d.PairingCandidates(q.Kind),
	}
	return s.WithParts(integrations.RoundParts{Owners: d.owners}), true
}

func (d *roundPairingDb) WithinRound(integrations.RoundAt) integrations.RoundScope { return nil }

func (d *roundPairingDb) ClaimPairingIn(m *models.Mock, at integrations.RoundAt) (claimed, moved bool, why string) {
	if d.moved > 0 {
		d.moved--
		return false, true, "a test's view was installed since the read"
	}
	return d.ClaimPairing(m), false, ""
}

func (d *roundPairingDb) SpendByKindIn(*models.Mock, integrations.RoundAt) (spent, moved bool, why string) {
	return false, false, ""
}

func (d *roundPairingDb) TakenLog(uint64, int, int) []integrations.Take { return nil }

var _ integrations.RoundReader = (*roundPairingDb)(nil)

// Read from one snapshot of the round (the agent's production path), a
// candidate the view holds as any of the open view's own (the open test's,
// in flight into it, shared with the tests recorded beside it, taken by
// another test, a pool view's ahead) never pairs, whatever the store
// offers: only a recording no test the view knows of owns does, claimed
// once. A claim the round moved under reads the round again and pairs in
// the round as it is now.
func TestHTTPPairsExactlyWithAnotherTestsRecordingInOneReadOfTheRound(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const host = "localhost:50001"
	h := newHTTP()
	n := h.newStrictNoise(nil, nil, nil)
	ctx := context.Background()
	call := getTo("/delete", host)
	for _, tc := range []struct {
		name  string
		owner integrations.RoundOwner
		pairs bool
	}{
		{"another test's", integrations.OwnerNone, true},
		{"the open test's own", integrations.OwnerTest, false},
		{"in flight into the open test", integrations.OwnerInFlight, false},
		{"shared with the tests recorded beside it", integrations.OwnerShared, false},
		{"shared, and taken by another test", integrations.OwnerSharedTaken, false},
		{"a pool view's ahead", integrations.OwnerAhead, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := answered(onHost("mock-5", "/delete", host), time.Second, 200, `{"ok":true}`)
			db := &roundPairingDb{pairingDb: &pairingDb{mockMemDb: &mockMemDb{}, others: []*models.Mock{rec}}, owners: map[string]integrations.RoundOwner{"mock-5": tc.owner}}
			answer, got := h.pairExactly(ctx, call, db, n)
			if !tc.pairs {
				if got != nil || answer != nil || len(db.claimed) != 0 {
					t.Fatalf("a recording the view holds as %s paired (%v), claims %v", tc.name, got, db.claimed)
				}
				return
			}
			if got != rec || answer == nil || answer.Spec.HTTPResp.Body != `{"ok":true}` || !db.claimed["mock-5"] {
				t.Fatalf("paired %v, claims %v; want mock-5, claimed", got, db.claimed)
			}
		})
	}
	t.Run("a claim the round moved under", func(t *testing.T) {
		rec := answered(onHost("mock-5", "/delete", host), time.Second, 200, `{"ok":true}`)
		db := &roundPairingDb{pairingDb: &pairingDb{mockMemDb: &mockMemDb{}, others: []*models.Mock{rec}}, moved: 1}
		if _, got := h.pairExactly(ctx, call, db, n); got != rec || db.reads != 2 || !db.claimed["mock-5"] {
			t.Fatalf("paired %v after %d reads, claims %v; want mock-5, claimed in the second read", got, db.reads, db.claimed)
		}
	})
}

// In record's full round, under the proven switch, an HTTP call that is
// exactly another test's recording is answered with that recording's
// answer, and reported as an answer (models.AnswerProvisional, NotOwned,
// naming the recording), not as a miss: judging moves the recording to the
// background. The recording stays its owner's: it is claimed, not consumed.
// Outside record's full round (no Learn) nothing pairs: the call misses.
func TestAPairedHTTPCallIsAnsweredAndReported(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const addr, host = "127.0.0.1:50001", "localhost:50001"
	var tracker *learn.Tracker
	serve := func(t *testing.T, learnOn bool) (int, string, []error, *pairingDb) {
		t.Helper()
		rec := answered(onHost("other-test-delete", "/delete", host), time.Second, 200, `{"deleted":1}`)
		db := &pairingDb{mockMemDb: &mockMemDb{}, others: []*models.Mock{rec}}
		var reported []error
		tracker = learn.NewTracker()
		ctx := learn.WithTracker(models.WithMockMismatchReporter(context.Background(), func(err error) { reported = append(reported, err) }), tracker)
		app, prox := net.Pipe()
		defer app.Close()
		done := make(chan error, 1)
		go func() {
			done <- newHTTP().MockOutgoing(ctx, prox, &models.ConditionalDstCfg{Addr: addr}, db, models.OutgoingOptions{Learn: learnOn})
		}()
		if _, err := io.WriteString(app, "GET /delete HTTP/1.1\r\nHost: "+host+"\r\nAccept: application/json\r\n\r\n"); err != nil {
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
		case <-done:
		case <-time.After(10 * time.Second):
			t.Fatal("the parser did not return")
		}
		return resp.StatusCode, string(body), reported, db
	}

	status, body, reported, db := serve(t, true)
	if status != 200 || body != `{"deleted":1}` {
		t.Fatalf("the app got %d %q; want the recording's answer", status, body)
	}
	if len(reported) != 1 {
		t.Fatalf("%d reports; want one answer", len(reported))
	}
	var withReport interface {
		MismatchReport() *models.MockMismatchReport
	}
	if !errors.Is(reported[0], models.ErrAnswered) || !errors.As(reported[0], &withReport) {
		t.Fatalf("reported %v; want an answer", reported[0])
	}
	r := withReport.MismatchReport()
	if r.Answer != models.AnswerProvisional || !r.NotOwned || r.ClosestMock != "other-test-delete" || r.Drift {
		t.Fatalf("the answer's report is %+v; want provisional, not owned, naming other-test-delete, exact", r)
	}
	if !db.claimed["other-test-delete"] || db.deletedFiltered != nil || db.updateUnFilteredMockOld != nil {
		t.Fatalf("the recording was not claimed, or was consumed: claims %v", db.claimed)
	}
	// The learn step's tracker was told the recording was served, as a
	// consumed one's store tells it: an earlier place of the calls after it,
	// and the previous call of the next call's window.
	if _, _, served, _ := tracker.Test(); len(served) != 1 || served[0].Name != "other-test-delete" {
		t.Fatalf("the tracker saw %v served; want the paired recording", served)
	}

	status, _, reported, db = serve(t, false)
	if status != http.StatusBadGateway || len(reported) != 1 || errors.Is(reported[0], models.ErrAnswered) || len(db.claimed) != 0 {
		t.Fatalf("without Learn: %d, %d reports (%v), claims %v; want a miss and no pairing", status, len(reported), reported, db.claimed)
	}
}
