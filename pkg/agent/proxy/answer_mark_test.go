package proxy

import (
	"context"
	"net/http"
	"testing"
	"time"

	httpint "go.keploy.io/server/v3/pkg/agent/proxy/integrations/http"
	"go.keploy.io/server/v3/pkg/matcher/learn"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// An answer keploy gives a call in record's full round by pairing it with
// another test's recording (models.AnswerProvisional) is listed under the
// test whose read takes it, as a miss is, and judging reads it there: it
// moves the recording to the set's background on that test's account. So
// its report carries the round the call was marked in (where the HTTP
// parser's learn step first read the round), and the proxy places it by
// that mark (models.UnmatchedCall.Marked), as a miss's: read after test-1's
// install, test-0's pairing still names test-0, never test-1. Unmarked, a
// test's end placed it by when keploy decided it.
func TestAPairingIsMarkedInTheTestThatMadeItsCall(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	p := startedDrainProxy(t, 100)
	p.learnTracker = learn.NewTracker()
	start := time.Now().Add(time.Second)
	// test-0 recorded a call of another method and path: the learn step
	// serves it nothing of GET /b, which only the pairing answers.
	own0 := r3Mock("mock-own-0", start.Add(100*time.Millisecond), "PUT", "/orders/7", `{"qty":2}`, `{"test":0}`)
	own1 := r3Mock("mock-own-1", start.Add(1100*time.Millisecond), "GET", "/b", "", `{"test":1}`)
	mm := NewMockManager(nil, nil, zap.NewNop())
	t.Cleanup(mm.Close)
	mm.SeedRecordedWindows([]models.TestWindow{
		{TestCase: "test-0", Start: start, End: start.Add(900 * time.Millisecond)},
		{TestCase: "test-1", Start: start.Add(time.Second), End: start.Add(1900 * time.Millisecond)},
	})
	mm.SetMocksWithWindow(nil, []*models.Mock{own0, own1}, models.BaseTime, start.Add(time.Hour))
	mm.ServeExactBackground(models.RecordedWithS12Routing, nil, nil)
	mm.ServeExactWorker(nil)
	p.setMockManager(mm)
	t.Cleanup(func() { p.setMockManager(nil) })
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.SetPairing(true)
	h := httpint.New(zap.NewNop())
	opts := models.OutgoingOptions{Learn: true}
	mm.ServeExactTest("test-0", []*models.Mock{roundMock(mm, "mock-own-0")})
	p.ContinueTestErrorCapture()
	if code, body := r3SendVia(t, context.Background(), h, p, mm, opts, httpGet("/b")); code != http.StatusOK || body != `{"test":1}` {
		t.Fatalf("test-0's GET /b got %d %s; want test-1's recording, paired (this case's premise)", code, body)
	}
	// test-0 ends and test-1 begins before the answer is read.
	mm.ServeExactPool(models.PoolKeyNone, nil)
	mm.ServeExactTest("test-1", []*models.Mock{roundMock(mm, "mock-own-1")})
	calls, err := p.GetMockErrors(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(calls) != 1 || calls[0].Answer != models.AnswerProvisional || !calls[0].NotOwned || calls[0].ClosestMock != "mock-own-1" {
		t.Fatalf("reported %+v; want one pairing of GET /b with mock-own-1 (this case's premise)", calls)
	}
	if m := calls[0].Marked; !m.Known || m.Test != "test-0" {
		t.Fatalf("WRONG TEST: test-0's pairing is marked %+v; want known, made in test-0's view, so a read after test-1's install lists it under test-0", m)
	}
	if calls[0].Kind != models.MissInTest {
		t.Fatalf("test-0's pairing is of kind %q; want %q, where its call was marked", calls[0].Kind, models.MissInTest)
	}
}
