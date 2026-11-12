package http

import (
	"slices"
	"strconv"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// goneOwnGateDb is ownGateDb whose call's view a test's begin or end
// replaced (integrations.CallViewGoneTeller) when why is not "".
type goneOwnGateDb struct {
	ownGateDb
	why string
}

func (d goneOwnGateDb) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return d, func() bool { return false }
}

func (d goneOwnGateDb) CallViewGoneWhy() string { return d.why }

// A changed call whose view is gone carries nothing a read of that view
// found: no earlier test's recording of its method and path either, which
// judging would read as the write a skipped test made.
func TestAChangedCallWhoseViewIsGoneNamesNoEarlierRecording(t *testing.T) {
	const addr, host = "127.0.0.1:8200", "localhost:8200"
	const path = "/v1/secret/data/keploy/personal-access-tokens/keploy.io/u1"
	t.Setenv(provenswitch.Env, "1")
	rec := time.Date(2026, 11, 2, 9, 0, 0, 0, time.UTC)
	own := secretPut("mock-own", "PUT", path, host, 2, rec)
	early := secretPut("mock-early", "PUT", path, host, 1, rec.Add(-time.Minute))
	body := `{"options":{"cas":5},"data":{"token":"t"}}`
	put := "PUT " + path + " HTTP/1.1\r\nHost: " + host + "\r\nAccept: application/json\r\nContent-Type: application/json\r\nContent-Length: " + strconv.Itoa(len(body)) + "\r\n\r\n" + body
	for _, c := range []struct {
		why  string
		want string
	}{
		{why: "", want: "mock-early"},
		{why: "the replay ended test .::TestA before the call was diagnosed", want: ""},
	} {
		d := goneOwnGateDb{ownGateDb: ownGateDb{roundGateDb: roundGateDb{gateDb{earlierDb: earlierDb{mockMemDb: &mockMemDb{}}, staged: []*models.Mock{early}}}, own: []*models.Mock{own}, lookups: new(int)}, why: c.why}
		r := missReportOf(t, d, addr, put, models.OutgoingOptions{})
		if r.Earlier != c.want {
			t.Errorf("view gone %q: a changed PUT names earlier %q; want %q", c.why, r.Earlier, c.want)
		}
	}
}

// skippedMovedDb is movedGateDb whose store lists skipped, a skipped test's
// read that answered in the open test's window
// (integrations.ExactSkippedReadsReader), and whose view moves as that list
// is read when moveOnSkipped.
type skippedMovedDb struct {
	*movedGateDb
	skipped       []*models.Mock
	moveOnSkipped bool
}

func (d *skippedMovedDb) HoldToRound(integrations.CallRound) (integrations.MockMemDb, func() bool) {
	return d, func() bool { return false }
}

func (d *skippedMovedDb) SkippedReadsAnswered() []*models.Mock {
	if d.moveOnSkipped {
		d.moved = true
	}
	return d.skipped
}

// An extra call's line joins the test's answered list with the skipped
// tests' reads that answered the same call: both must be of the call's own
// view. A skipped list read after the view moved may be the next test's, so
// it is counted in no line.
func TestAnExtraCallCountsOnlyTheSkippedReadsOfItsOwnView(t *testing.T) {
	const addr, host = "127.0.0.1:8099", "localhost:8099"
	const path = "/sap/A_BusinessPartner/$count?$filter=x"
	t.Setenv(provenswitch.Env, "1")
	own := onHost("mock-own", path, host)
	skip := onHost("mock-skip", path, host)
	for _, moves := range []bool{false, true} {
		d := &skippedMovedDb{movedGateDb: &movedGateDb{gateDb: gateDb{earlierDb: earlierDb{mockMemDb: &mockMemDb{}}, consumed: []*models.Mock{own}}}, skipped: []*models.Mock{skip}, moveOnSkipped: moves}
		r := missReport(t, d, addr, host, path, models.OutgoingOptions{})
		if r.MissKind != models.MissExtraCall || r.Recorded != 1 {
			t.Fatalf("moves %v: the call missed as %q, recorded %d; want one more than the 1 recorded", moves, r.MissKind, r.Recorded)
		}
		if named := slices.Equal(r.SkippedReads, []string{"mock-skip"}); named == moves {
			t.Errorf("moves %v: the extra call counts skipped reads %v; want mock-skip counted %v", moves, r.SkippedReads, !moves)
		}
	}
}
