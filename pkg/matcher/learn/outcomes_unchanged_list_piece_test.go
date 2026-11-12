package learn

import (
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// Run m1c-x2, TestSuiteVersionAddedStep's insert on app_config_audits
// (mock-2133, 08:25:05.4997) mints _id 294e693b-0963-42b8-a8ba-8cdf7fb6d3f1;
// round F's id holds 412d where the recording's holds 42b8. The set's
// start-up aggregate on testSuiteEvents (mock-45, 08:23:27.1690) answers the
// id of every suite, the same list in both runs: one listed id holds 42b8
// (d4927bdf-978e-42b8-86de-eeb6c761151c) and another 412d
// (35cd9af2-dc5d-412d-a1c4-3e21b56315a6). The recording as a whole differs
// between the runs elsewhere (its session), so it is no unchanged place, and
// round F bound the group to mock-45 and refused it ("keploy names no place
// of mock-45 to bind it at"). The fields that hold the two groups are the
// same text in both runs: they show no change, so the id's long words, made
// anew by the call, judge the place, and the group is made anew with them.
// What stays: a field that changed and holds each run's group at one spot,
// and a group that is the field's whole value, are not chance, and a whole
// id picked from the list has no word made anew.
func TestOutcomeAMintedIdsGroupInsideAnUnchangedFieldOfAChangedListIsChance(t *testing.T) {
	const (
		path      = "body.docs.msgs.0._id"
		rec, live = "294e693b-0963-42b8-a8ba-8cdf7fb6d3f1", "22ee59fa-7c10-412d-b6e2-90ad3f5c7e18"
		withR     = "d4927bdf-978e-42b8-86de-eeb6c761151c"
		withF     = "35cd9af2-dc5d-412d-a1c4-3e21b56315a6"
		other     = "ab4e1a5f-ccca-48d0-8fb3-144c278387a7"
	)
	// mock-45 was answered 97.3 s before the call.
	before := time.Date(2026, 11, 10, 8, 25, 5, 499702569, time.UTC).Sub(time.Date(2026, 11, 10, 8, 23, 27, 168962254, time.UTC))
	list := func(session string, ids ...string) *models.Mock {
		rows := []models.PostgresV3Cells{{{Value: session}}}
		for _, id := range ids {
			rows = append(rows, models.PostgresV3Cells{{Value: id}})
		}
		return oPgMock("mock-45", before, "SELECT id FROM suites", rows, "SELECT 4")
	}
	served := func(r, f *models.Mock) Raw { return RawOf(r, true).ServedAs(1).ServedLive(RawOf(f, true)) }
	decide := func(raw Raw) Decision {
		t.Helper()
		return oWord(t, Decide(oInput(path, rec, live, nil, []Raw{raw})), "42b8", "412d")
	}
	fresh := func(what string, d Decision) {
		t.Helper()
		if d.Outcome != OutcomeFresh {
			t.Errorf("%s: %s; want the group made anew with its id", what, oDescribe([]Decision{d}))
		}
	}
	fresh("both groups in unchanged fields, the list changed elsewhere (its session)", decide(served(list("s-1", withR, withF, other), list("s-2", withR, withF, other))))
	// The list in one field, as R3b of the trace held it: round F bound the
	// group to mock-45 there ("value from mock-45").
	oneCell := withR + " " + withF + " " + other
	fresh("both groups in one unchanged field, the list changed elsewhere", decide(served(list("s-1", oneCell), list("s-2", oneCell))))
	fresh("both groups in unchanged fields, served as recorded", decide(RawOf(list("s-1", withR, withF, other), true).ServedAs(1).ServedAsRecorded()))
	fresh("control: the recorded group only, the list changed elsewhere", decide(served(list("s-1", withR, other), list("s-2", withR, other))))

	notChance := func(what string, d Decision) {
		t.Helper()
		if d.Outcome == OutcomeFresh {
			t.Errorf("FALSE GREEN? %s: %s; want it not read as chance", what, oDescribe([]Decision{d}))
		}
	}
	// One field holds the recorded group in the recording and round F's in
	// round F's view, at one spot: the place shows the change.
	moved := "d4927bdf-978e-412d-86de-eeb6c761151c"
	notChance("a changed field holding each run's group at one spot", decide(served(list("s-1", withR, other), list("s-2", moved, other))))
	// The group is the field's whole value, no piece of an id.
	changed := served(list("s-1", withR, withF, other), list("s-2", withR, withF, other))
	notChance("the group as the field's whole value", oWord(t, Decide(oInput("body.slot", "42b8", "412d", nil, []Raw{changed})), "42b8", "412d"))
	// A whole id picked from the list: its long words stand there, so no
	// word of it is made anew.
	picked := Decide(oInput("body.cmd.filter.suite_id", withR, withF, nil, []Raw{changed}))
	if len(picked) == 0 || slices.ContainsFunc(picked, func(d Decision) bool { return d.Outcome == OutcomeFresh }) {
		t.Errorf("FALSE GREEN? a whole id picked from the list: %s; want no word of it made anew", oDescribe(picked))
	}
}
