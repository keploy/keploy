package proxy

import (
	"slices"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// A background series answered past its recorded count in one staging keeps
// answering after the set is staged again: the new staging starts the
// windows' answers afresh, so the HTTP parser's walk of the call starts over
// at the series' first recording too, and its last answers each repeat once
// every recording answered again. A walk that kept the last staging's served
// marks would offer the last recording alone, which answers once and then
// hides until the others answer, which the walk never offers: every later
// probe would miss.
func TestSeriesRepeatsAnswerAfterTheSetIsStagedAgain(t *testing.T) {
	const probePath = "/v1/sys/health"
	mks := func() []*models.Mock {
		return []*models.Mock{
			workerProbe("p1", time.Second, probePath, `{"ok":1}`),
			workerProbe("p2", 2*time.Second, probePath, `{"ok":2}`),
		}
	}
	mm := stagedManager(t, nil, mks())
	mm.ServeExactWorker([]string{"p1", "p2"})
	mm.SetPoolSeries(map[string][]string{"probe/-/1": {"p1", "p2"}})
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.ServeExactTest("TestB", nil)
	var got []string
	for i := 0; i < 3; i++ {
		body, _ := httpMissCall(t, mm, probePath)
		got = append(got, body)
	}
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.SetMocksWithWindow(nil, mks(), models.BaseTime, time.Now())
	mm.ServeExactTest("TestC", nil)
	for i := 0; i < 4; i++ {
		body, r := httpMissCall(t, mm, probePath)
		if r != nil {
			body = "a miss (closest " + r.ClosestMock + ")"
		}
		got = append(got, body)
	}
	want := []string{`{"ok":1}`, `{"ok":2}`, `{"ok":2}`, `{"ok":1}`, `{"ok":2}`, `{"ok":2}`, `{"ok":2}`}
	if !slices.Equal(got, want) {
		t.Fatalf("the probes were answered %q, want %q", got, want)
	}
}
