package http

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
)

// d26At is a clock read of the recording, as F's clock moves it by the lead.
func d26At(t *testing.T, layout, recorded string) string {
	t.Helper()
	at, err := time.Parse(layout, recorded)
	if err != nil {
		t.Fatal(err)
	}
	return at.Add(learn.FullRoundLead).Format(layout)
}

// A value that drifted only by F's clock is learned with a mask, in every
// part of the request, so what did not drift is still compared (founder
// decision 26); a value that drifted otherwise is learned whole.
func TestLearnMatchMasksAClockOnlyDrift(t *testing.T) {
	const iso, rfc = time.RFC3339, time.RFC1123Z
	recNote, recSent := "order 1234 placed at 2026-10-14T10:00:41Z", "Wed, 14 Oct 2026 10:00:43 +0000"
	liveNote := "order 1234 placed at " + d26At(t, iso, "2026-10-14T10:00:41Z")
	liveSent := d26At(t, rfc, recSent)
	h, n, opts := learnOn(t)
	db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/orders?since=2026-10-14T09:00:00Z",
		`{"note":"`+recNote+`","request_id":"r-1"}`, map[string]string{"X-Sent-At": recSent}, `{"ok":true}`)}}
	served, miss, err := h.learnMatch(context.Background(), live("POST", "/v1/orders?since="+d26At(t, iso, "2026-10-14T09:00:00Z"),
		`{"note":"`+liveNote+`","request_id":"r-2"}`, map[string]string{"X-Sent-At": liveSent}), db, n, opts)
	if err != nil || miss != nil || served == nil {
		t.Fatalf("served %v miss %+v err %v", served, miss, err)
	}
	got := db.consumed[0].Spec.ReqBodyNoise
	if pats, ok := got["body.request_id"]; !ok || len(pats) != 0 {
		t.Fatalf("a fresh id is learned whole, got %v", got)
	}
	for _, tc := range []struct{ path, later, changed string }{
		{"body.note", "order 1234 placed at 2026-11-02T08:15:00Z", "order 1235 placed at 2026-11-02T08:15:00Z"},
		{"header.x-sent-at", "Mon, 02 Nov 2026 13:45:00 +0530", "Mon, 02 Nov 2026 08:15:00 UTC+1"},
		{"url.query.since", "2026-11-02T08:15:00Z", "yesterday"},
	} {
		pats := got[tc.path]
		if len(pats) != 1 {
			t.Errorf("%s: learned %v, want one mask", tc.path, pats)
			continue
		}
		if !learn.Masked(pats, tc.later, true) || learn.Masked(pats, tc.changed, true) {
			t.Errorf("%s: mask %q excuses %q %v, %q %v", tc.path, pats[0], tc.later, learn.Masked(pats, tc.later, true), tc.changed, learn.Masked(pats, tc.changed, true))
		}
	}
}

// The replays after F honour a mask: a call whose clock read moved is
// served, one that changed what did not drift is not.
func TestLearnedMatchHonoursAMask(t *testing.T) {
	h, n, _ := learnOn(t)
	rec := "order 1234 placed at 2026-10-14T10:00:41Z"
	mask := learn.ClockMask(rec, "order 1234 placed at 2026-10-14T10:01:42Z")
	if mask == "" {
		t.Fatal("no mask")
	}
	recorded := func() *models.Mock {
		m := learnRecorded("mock-0", 0, "POST", "/v1/orders", `{"note":"`+rec+`","amount":5}`, nil, `{"ok":true}`)
		m.Spec.ReqBodyNoise = map[string][]string{"body.note": {mask}}
		return m
	}
	db := &learnDb{perTest: []*models.Mock{recorded()}}
	if served, err := h.learnedMatch(context.Background(), live("POST", "/v1/orders", `{"note":"order 1234 placed at 2026-11-02T08:15:00Z","amount":5}`, nil), db, n, models.OutgoingOptions{}); err != nil || served == nil {
		t.Fatalf("a call whose clock read moved was not served: %v", err)
	}
	db = &learnDb{perTest: []*models.Mock{recorded()}}
	if served, _ := h.learnedMatch(context.Background(), live("POST", "/v1/orders", `{"note":"order 9999 placed at 2026-11-02T08:15:00Z","amount":5}`, nil), db, n, models.OutgoingOptions{}); served != nil {
		t.Fatal("a call that changed the note outside its clock read was served")
	}
	// And in F, the mask is known: a second clock-only drift is exact, a
	// changed note is a drift again, learned whole.
	h, n, opts := learnOn(t)
	db = &learnDb{perTest: []*models.Mock{recorded()}}
	if served, miss, _ := h.learnMatch(context.Background(), live("POST", "/v1/orders", `{"note":"order 9999 placed at 2026-11-02T08:15:00Z","amount":5}`, nil), db, n, opts); served == nil || miss != nil {
		t.Fatalf("F: %+v", miss)
	}
	if pats := db.consumed[0].Spec.ReqBodyNoise["body.note"]; len(pats) != 0 {
		t.Fatalf("a note that drifted outside its clock read stays masked: %v", pats)
	}
}

// The variable pass honours a mask beside a variable: the recorded value is
// put back only where the mask holds.
func TestWhatFLearnsReplaysWithAMaskBesideAVariable(t *testing.T) {
	recs := []*models.Mock{
		learnRecorded("mock-1", 0, "POST", "/orders", `{"id":"o-1111","item":"pen"}`, nil, `{"id":"o-1111"}`),
		learnRecorded("mock-2", time.Second, "POST", "/orders/o-1111/notes", `{"note":"placed at 2026-10-14T10:00:41Z"}`, nil, `{"ok":true}`),
	}
	consumed := fullRound(t, recs, []*req{
		live("POST", "/orders", `{"id":"o-9999","item":"pen"}`, nil),
		live("POST", "/orders/o-9999/notes", `{"note":"placed at `+d26At(t, time.RFC3339, "2026-10-14T10:00:41Z")+`"}`, nil),
	})
	mocks := learnedOnto(recs, consumed)
	if pats := mocks[1].Spec.ReqBodyNoise["body.note"]; len(pats) != 1 || len(mocks[1].Spec.Variables) == 0 {
		t.Fatalf("mock-2: noise %v variables %v", mocks[1].Spec.ReqBodyNoise, mocks[1].Spec.Variables)
	}
	inCI(t, mocks, []*req{
		live("POST", "/orders", `{"id":"o-5555","item":"pen"}`, nil),
		live("POST", "/orders/o-5555/notes", `{"note":"placed at 2026-11-02T08:15:00Z"}`, nil),
	})
	h := newHTTP()
	db := &varDb{perTest: learnedOnto(recs, consumed)}
	if ok, _, _ := matchVar(t, h, db, live("POST", "/orders", `{"id":"o-5555","item":"pen"}`, nil)); !ok {
		t.Fatal("the create in CI")
	}
	if ok, _, _ := matchVar(t, h, db, live("POST", "/orders/o-5555/notes", `{"note":"cancelled at 2026-11-02T08:15:00Z"}`, nil)); ok {
		t.Fatal("a note that changed outside its clock read was served")
	}
}

// A value whose moved read is not its instant whole (a time of day beside a
// date and zone no layout reads; a zone, an abbreviation or a region ID
// after a timestamp) is learned whole, so a replay on another date, or on a
// host in another zone, is still served (round-1 and round-2 reviews of
// decision 26): a laptop in IST records, CI in UTC replays.
func TestLearnMatchLearnsAPartialInstantWhole(t *testing.T) {
	for _, tc := range []struct{ name, rec, f, ci string }{
		{"Date.toString", "Wed Oct 14 10:00:41 UTC 2026", "Wed Oct 14 10:01:42 UTC 2026", "Thu Nov 05 04:30:41 IST 2026"},
		{"Go's Time.String", "2026-10-14 10:00:41.123456789 +0530 IST", "2026-10-14 10:01:42.623456789 +0530 IST", "2026-11-05 04:30:41.123456789 +0000 UTC"},
		{"Java's ZonedDateTime", "2026-10-14T10:00:41.123+05:30[Asia/Kolkata]", "2026-10-14T10:01:42.623+05:30[Asia/Kolkata]", "2026-11-05T04:30:41.123Z[UTC]"},
		{"Python's %Z, recorded in UTC", "2026-10-14 10:00:41 UTC", "2026-10-14 10:01:42 UTC", "2026-11-05 10:00:41 IST"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, n, opts := learnOn(t)
			db := &learnDb{perTest: []*models.Mock{learnRecorded("mock-0", 0, "POST", "/v1/events", `{"at":"`+tc.rec+`"}`, nil, `{"ok":true}`)}}
			if served, miss, err := h.learnMatch(context.Background(), live("POST", "/v1/events", `{"at":"`+tc.f+`"}`, nil), db, n, opts); err != nil || miss != nil || served == nil {
				t.Fatalf("served %v miss %+v err %v", served, miss, err)
			}
			pats, ok := db.consumed[0].Spec.ReqBodyNoise["body.at"]
			if !ok || len(pats) != 0 {
				t.Fatalf("learned %v, want body.at whole", db.consumed[0].Spec.ReqBodyNoise)
			}
			later := learnRecorded("mock-0", 0, "POST", "/v1/events", `{"at":"`+tc.rec+`"}`, nil, `{"ok":true}`)
			later.Spec.ReqBodyNoise = db.consumed[0].Spec.ReqBodyNoise
			db = &learnDb{perTest: []*models.Mock{later}}
			if served, err := h.learnedMatch(context.Background(), live("POST", "/v1/events", `{"at":"`+tc.ci+`"}`, nil), db, n, models.OutgoingOptions{}); err != nil || served == nil {
				t.Fatalf("a later date in another zone was refused: %v", err)
			}
			if got, diag := strictMatch(t, live("POST", "/v1/events", `{"at":"`+tc.ci+`"}`, nil), nil, nil, nil, later); got == nil {
				t.Fatalf("the strict matcher refused a later date in another zone: %v", differs(diag))
			}
		})
	}
}
