package record

import (
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/stretchr/testify/assert"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A revoke names a saved test case by the name it was saved under, or, for
// one the agent sent with no name (the async capture names none; the client
// names it as it saves it), by its request time (models.RevokeNameByRequest):
// that names every saved test case whose window starts then, each deleted
// once. A revoke that names none saved deletes nothing, and a name that only
// looks like a request time is read as a saved name first.
func TestARevokeFindsATestCaseSentWithNoNameByItsRequestTime(t *testing.T) {
	at := time.Date(2026, 11, 8, 8, 27, 56, 322384000, time.UTC)
	other := at.Add(time.Millisecond)
	literal := models.RevokeNameByRequest(at.Add(time.Hour))
	inserted := map[string]struct{}{"test-1": {}, "post-customers-939": {}, "post-customers-940": {}, "get-healthz-1": {}, literal: {}}
	byRequest := map[int64][]string{
		at.UnixNano():    {"post-customers-939", "post-customers-940"},
		other.UnixNano(): {"get-healthz-1"},
	}
	got := revokedToDelete([]string{
		"test-1",
		models.RevokeNameByRequest(at),
		models.RevokeNameByRequest(at),
		"post-customers-939",
		models.RevokeNameByRequest(at.Add(time.Second)),
		"never-saved",
		literal,
	}, inserted, byRequest)
	slices.Sort(got)
	want := []string{literal, "post-customers-939", "post-customers-940", "test-1"}
	slices.Sort(want)
	if !slices.Equal(got, want) {
		t.Fatalf("deletes %v, want %v", got, want)
	}
	if req, ok := models.RequestOfRevokeName(models.RevokeNameByRequest(at)); !ok || req != at.UnixNano() {
		t.Fatalf("a revoke name by request time reads back as %d, %v", req, ok)
	}
	for _, n := range []string{"test-1", "1794130763404555477", models.RevokeByRequestPrefix, models.RevokeByRequestPrefix + "x1"} {
		if _, ok := models.RequestOfRevokeName(n); ok {
			t.Fatalf("%q is read as a revoke name by request time", n)
		}
	}
}

// The recording deletes, at its end, a test case the agent sent with no name
// and the store named as it saved it, when a revoke names it by its request
// time, and only that one.
func TestStart_AgentRevokeByRequestTimeDeletesTheTestCaseTheStoreNamed(t *testing.T) {
	run := startRecording(t, &recMockDB{unencodable: map[string]bool{}}, BaseRecordHooks{}, zap.NewNop(), func(r *Recorder) {
		r.testDB.(*recTestDB).autoName = "post-customers"
	})
	at := time.Now().Add(-time.Minute)
	for i, req := range []time.Time{at, at.Add(time.Millisecond)} {
		select {
		case run.f.incoming <- &models.TestCase{Kind: models.HTTP, HTTPReq: models.HTTPReq{Timestamp: req}, HTTPResp: models.HTTPResp{Timestamp: req.Add(time.Millisecond)}}:
		case <-time.After(hangGuard):
			t.Fatalf("recorder never consumed nameless test case %d", i+1)
		}
		waitForPersistedTestCase(t, run.testDB, fmt.Sprintf("post-customers-%d", i+1))
	}
	run.frame(t, revokeFrame("revoke-frame", models.RevokeNameByRequest(at)))
	run.stop(t)
	assert.Equal(t, []string{"post-customers-1"}, run.deleted())
}
