package replayer_test

import (
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/replayer"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.uber.org/zap"
)

// Under the proven switch, a command one more than the open test recorded
// that misses before the background could answer it (the background holds
// only other parameters of the statement) is diagnosed with the background
// hidden: its miss names no background recording as the closest one, and
// counts none among the candidates the scan compared. Diagnosed against the
// background, the report pointed the user at a start-up or worker recording
// that was never the test's, for a test that ran a recorded write twice.
// The miss is still an extra call, recorded once.
func TestACommandOneMoreThanTheTestRecordedIsDiagnosedWithTheBackgroundHidden(t *testing.T) {
	for _, scope := range []string{"pool", "worker"} {
		t.Run(scope, func(t *testing.T) {
			t.Setenv(provenswitch.Env, "1")
			s0, _ := diffWindow(0)
			s1, _ := diffWindow(1)
			prep, _, _ := betweenTestsRecording()
			at := s0.Add(7 * time.Millisecond)
			if scope == "pool" {
				at = diffBase.Add(-500 * time.Microsecond)
			}
			// other is the background's only recording of the statement,
			// with other parameters: it answers no EXECUTE id=1.
			other := diffMock(scope+"-exec-2", "c1", []mysql.Request{executeReq(7, "2")}, at, models.LifetimePerTest, "mocks")
			own := diffMock("t1-exec-1", "c1", []mysql.Request{executeReq(7, "1")}, s1.Add(time.Millisecond), models.LifetimePerTest, "mocks")
			mm := proxy.NewMockManager(nil, nil, zap.NewNop())
			defer mm.Close()
			mm.SetMocksWithWindow(nil, []*models.Mock{prep, other, own}, models.BaseTime, time.Now().Add(time.Hour))
			seedWindows(mm, 2)
			if scope == "pool" {
				mm.ServeExactBackground(models.RecordedWithS12Routing, []string{other.Name}, nil)
			} else {
				mm.ServeExactBackground(models.RecordedWithS12Routing, nil, []string{other.Name})
				mm.ServeExactWorker([]string{other.Name})
			}
			mm.ServeExactTest("T1", []*models.Mock{own})
			if !mm.ExactServing() {
				t.Fatal("precondition: test T1 is not served exactly")
			}
			ctx, dctx := betweenTestsConn()

			if got := matchPayload(t, ctx, executeReq(7, "1"), mm, dctx); got != own.Name {
				t.Fatalf("T1's EXECUTE id=1 was answered with %s; want its own %s", got, own.Name)
			}
			resp, ok, miss, err := replayer.MatchForTest(ctx, zap.NewNop(), executeReq(7, "1"), mm, dctx)
			if ok || err != nil {
				payload := ""
				if resp != nil {
					payload = resp.Payload
				}
				t.Fatalf("T1's second EXECUTE id=1, one more than it recorded, was answered with %q (err %v)", payload, err)
			}
			if !strings.Contains(miss, "extraCall:1") {
				t.Fatalf("the second EXECUTE id=1 missed with %s; want an extra call, recorded once", miss)
			}
			if strings.Contains(miss, other.Name) {
				t.Fatalf("the second EXECUTE id=1 was diagnosed against the background's %s: %s", other.Name, miss)
			}
			// With the background hidden the scan compares two candidates;
			// diagnosed against the background, it counted its recording
			// too: three.
			if !strings.Contains(miss, " candidateCount:2 ") {
				t.Fatalf("the second EXECUTE id=1 counted other than 2 candidates: %s", miss)
			}
		})
	}
}
