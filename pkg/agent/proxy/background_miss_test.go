package proxy

import (
	"context"
	"reflect"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// The agent's read of a test's misses keeps a parser's background mark
// (MockMismatchReport.Background): the replay lists the call and counts it
// against no test. An unmarked miss stays unmarked.
func TestTheMissReadKeepsTheBackgroundMark(t *testing.T) {
	p := &Proxy{logger: zap.NewNop(), errChannel: make(chan error, 16)}
	p.BeginTestErrorCapture()
	bg := mockMiss("findAndModify ptw_endpoints")
	bg.MismatchReport.Background = true
	bound := models.BackgroundBinding{Recording: "mock-2954", Place: "body.docs.msgs.0.q.app_id", Variable: "appId", BoundMock: "mock-226", BoundPlace: "body.docs.msgs.0.appId"}
	bg.MismatchReport.BackgroundBound = []models.BackgroundBinding{bound}
	p.errChannel <- bg
	p.errChannel <- mockMiss("find users")
	got, err := p.GetMockErrors(context.Background())
	if err != nil || len(got) != 2 {
		t.Fatalf("read %+v, %v; want both misses", got, err)
	}
	for _, c := range got {
		if want := c.ActualSummary == "findAndModify ptw_endpoints"; c.Background != want || c.CountsAgainstTest() == want {
			t.Errorf("%s: Background %v, counts against its test %v", c.ActualSummary, c.Background, c.CountsAgainstTest())
		}
		if want := c.Background; (len(c.BackgroundBound) == 1 && c.BackgroundBound[0] == bound) != want {
			t.Errorf("%s: BackgroundBound %+v; want the parser's places only on the background miss", c.ActualSummary, c.BackgroundBound)
		}
	}
}

// bgRec is a background recording of kind named name, sent at sec seconds.
func bgRec(name string, kind models.Kind, sec int64) models.BackgroundRecording {
	return models.BackgroundRecording{Mock: &models.Mock{Name: name, Kind: kind, Spec: models.MockSpec{ReqTimestampMock: time.Unix(1_700_000_000+sec, 0)}}}
}

// bgNames is the names of recs, in order.
func bgNames(recs []models.BackgroundRecording) []string {
	var out []string
	for _, r := range recs {
		out = append(out, r.Mock.Name)
	}
	return out
}

// The set's background recordings are read by kind, in recorded order, through
// any view (a test's allowlist names none of them), and they follow the
// windows' lifecycle: a seed at a set boundary waits for that set's staging,
// a set that seeds none has none, and an aborted set's park is dropped.
func TestBackgroundRecordingsFollowTheSetsStaging(t *testing.T) {
	mm := NewMockManager(nil, nil, zap.NewNop())
	defer mm.Close()
	if mm.BackgroundRecordings(models.Mongo) != nil {
		t.Fatal("a fresh manager has background recordings")
	}
	mm.SeedBackgroundRecordings([]models.BackgroundRecording{bgRec("poll-2", models.Mongo, 4), bgRec("dns-1", models.DNS, 1), bgRec("poll-1", models.Mongo, 2)})
	if got := bgNames(mm.BackgroundRecordings(models.Mongo)); !reflect.DeepEqual(got, []string{"poll-1", "poll-2"}) {
		t.Fatalf("the Mongo background recordings are %v; want [poll-1 poll-2], in recorded order", got)
	}
	var view integrations.MockMemDb = &scopedMockDb{MockMemDb: mm}
	if got := bgNames(integrations.BackgroundRecordingsOf(view, models.Mongo)); !reflect.DeepEqual(got, []string{"poll-1", "poll-2"}) {
		t.Fatalf("a test's view reads %v; want the set's [poll-1 poll-2] whatever it serves", got)
	}

	mm.ResetForReplaySession()
	mm.SeedBackgroundRecordings([]models.BackgroundRecording{bgRec("b-poll", models.Mongo, 9)})
	if got := bgNames(mm.BackgroundRecordings(models.Mongo)); !reflect.DeepEqual(got, []string{"poll-1", "poll-2"}) {
		t.Fatalf("the next set's seed replaced this set's before it was staged: %v", got)
	}
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if got := bgNames(mm.BackgroundRecordings(models.Mongo)); !reflect.DeepEqual(got, []string{"b-poll"}) {
		t.Fatalf("after staging the next set: %v, want [b-poll]", got)
	}

	mm.ResetForReplaySession()
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if got := mm.BackgroundRecordings(models.Mongo); got != nil {
		t.Fatalf("a set that seeded none inherited the previous set's %v", bgNames(got))
	}

	mm.ResetForReplaySession()
	mm.SeedBackgroundRecordings([]models.BackgroundRecording{bgRec("d-poll", models.Mongo, 1)})
	mm.ResetForReplaySession() // d's staging was aborted
	mm.SetMocksWithWindow(nil, nil, models.BaseTime, time.Now())
	if got := mm.BackgroundRecordings(models.Mongo); got != nil {
		t.Fatalf("an aborted set's parked background recordings were installed for the next set: %v", bgNames(got))
	}
}
