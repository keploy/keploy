package http

import (
	"context"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// A test's own call in keploy record's server tests is read in its own
// window (learn.OwnCall), from the test's own and the start-up's calls, as a
// replay checks its reads. The shape: a test stamps an earlier call's body
// with its clock (mock-a, read at once), then hashes a password and stamps
// mock-b's body (Sequelize's form, its zone after a space). Record's full
// round ran the hash late, so mock-b's read moved by the lead plus late:
// past a second more, the lead no longer shows it, and the read's minute is
// a word mock-a's body holds in each run (the minute both reads fell in).
// Without a window, the verdict followed how late the round ran: mock-b was
// refused as carried from mock-a. In the call's own window, from mock-a's
// call in both runs to mock-b's, both reads stand: learned at every delay.
// A worker: scope's serve and a background recording landing between the
// read and the send start neither half, so they never refuse the read. A
// value read before mock-a's call, which the lead does not show either, is
// before the window, and stays refused.
func TestLearnMatchATestsOwnCallsClockReadIsReadInItsOwnWindow(t *testing.T) {
	const form = "2006-01-02 15:04:05.000 -07:00"
	recA, recB := 47*time.Minute+52299*time.Millisecond, 47*time.Minute+52994*time.Millisecond
	cases := []struct {
		name string
		late time.Duration
		// beside: a worker: serve and a background recording between the
		// read and the send, in each run.
		beside bool
		// stale: mock-b's value is a read made before mock-a's call, in
		// both runs.
		stale  bool
		learns bool
	}{
		{"read 10ms late", 10 * time.Millisecond, false, false, true},
		{"read 300ms late", 300 * time.Millisecond, false, false, true},
		{"read 1.031s late", 1031 * time.Millisecond, false, false, true},
		{"read 2.5s late", 2500 * time.Millisecond, false, false, true},
		{"read 1.031s late, a worker: serve and a background recording between the read and the send", 1031 * time.Millisecond, true, false, true},
		{"read 10ms late, a worker: serve and a background recording between the read and the send", 10 * time.Millisecond, true, false, true},
		{"read before mock-a's call, 2.5s late", 2500 * time.Millisecond, false, true, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			readA, readB := learnAt.Add(recA), learnAt.Add(recB)
			if tc.stale {
				readB = learnAt.Add(recA - 200*time.Millisecond)
			}
			fBegan := learnAt.Add(recA + learn.FullRoundLead - time.Second)
			fReadA := readA.Add(learn.FullRoundLead + 10*time.Millisecond)
			fReadB := readB.Add(learn.FullRoundLead + tc.late)
			fA, fB := fReadA.Add(time.Millisecond), fReadB.Add(time.Millisecond)
			tr := learn.NewTracker()
			tr.SetAppClock(func(r time.Time) (time.Time, bool) { return r, true })
			tr.SetServeOwner(func(m *models.Mock) bool { return m.Name == "mock-a" || m.Name == "mock-b" })
			tr.NewSetAt(fBegan.Add(-time.Second))
			tr.BeginAt(fBegan)
			ctx := learn.WithTracker(context.Background(), tr)
			a := learnRecorded("mock-a", recA+time.Millisecond, "POST", "/v1/a", `{"at":"`+readA.Format(form)+`"}`, nil, `{"ok":true}`)
			b := learnRecorded("mock-b", recB+time.Millisecond, "POST", "/v1/b", `{"createdAt":"`+readB.Format(form)+`"}`, nil, `{"ok":true}`)
			staged := []*models.Mock{a}
			if tc.beside {
				bg := learnRecorded("mock-bg", recB+time.Millisecond/2, "GET", "/v1/bg", "", nil, `{}`)
				bg.Spec.ResTimestampMock = bg.Spec.ReqTimestampMock.Add(100 * time.Microsecond)
				staged = append(staged, bg)
			}
			db := &learnDb{perTest: []*models.Mock{a, b}, staged: staged}
			db.observe = func(m *models.Mock) { tr.ServedMockAt(m, fA) }
			in := live("POST", "/v1/a", `{"at":"`+fReadA.Format(form)+`"}`, nil)
			in.arrived = fA
			if got, miss, err := h.learnMatch(ctx, in, db, n, opts); err != nil || got == nil {
				t.Fatalf("mock-a: served %v miss %+v err %v", got != nil, miss, err)
			}
			db.observe = nil
			if tc.beside {
				tr.ServedMockAt(rec("mock-worker"), fReadB.Add(time.Millisecond/2))
			}
			in = live("POST", "/v1/b", `{"createdAt":"`+fReadB.Format(form)+`"}`, nil)
			in.arrived = fB
			got, miss, err := h.learnMatch(ctx, in, db, n, opts)
			if err != nil {
				t.Fatal(err)
			}
			if tc.learns && (got == nil || miss != nil) {
				t.Fatalf("mock-b: served %v miss %+v; want body.createdAt learned in its call's own window", got != nil, miss)
			}
			if !tc.learns && (got != nil || miss == nil || !strings.Contains(miss.res.Why, "body.createdAt")) {
				t.Fatalf("mock-b: served %v miss %+v; want it refused, naming body.createdAt", got != nil, miss)
			}
		})
	}
}

func rec(name string) *models.Mock { return &models.Mock{Name: name, Kind: models.HTTP} }
