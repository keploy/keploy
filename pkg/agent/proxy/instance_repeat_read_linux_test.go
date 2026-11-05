package proxy

import (
	"context"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A replay of more than one test worker serves each worker's tests from a
// manager of its own (ServeInstance). The session's header noise reaches
// every worker's manager, one served before the session began and one
// served after: a read recorded three times from three pages (Referer, which
// keploy.yml names noise) is one read there too, answered a fourth time with
// nothing charged, as on the run's own manager
// (TestARepeatedReadIsProvedUnderTheSessionsHeaderNoise).
func TestEveryTestWorkerProvesARepeatedReadUnderTheSessionsHeaderNoise(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	p, _ := storeProxy(t, ipv4Dest(t, "127.0.0.1:50211"))
	ctx := context.Background()
	if err := p.SetMocks(ctx, nil, nil); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ServeInstance(1); err != nil {
		t.Fatal(err)
	}
	referer := models.OutgoingOptions{DisableMysqlAutoDetect: true, Mocking: true, NoiseConfig: map[string]map[string][]string{"header": {"Referer": {}}}}
	if err := p.Mock(ctx, referer); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ServeInstance(2); err != nil {
		t.Fatal(err)
	}
	four := []string{fromPage("a"), fromPage("b"), fromPage("c"), fromPage("d")}
	for _, inst := range []uint32{1, 2} {
		wm := instanceMgrOf(t, p, inst)
		mapping := pageReads()
		wm.SetMocksWithWindow(nil, mapping, models.BaseTime, time.Now().Add(time.Hour))
		wm.ServeExactPool("k", nil)
		wm.ServeExactTest("t", mapping)
		var missed []int
		for i, req := range four {
			if r := sendForMissLogged(t, ctx, zap.NewNop(), wm, referer, nil, req); r != nil {
				missed = append(missed, i)
			}
		}
		test, _, unmade, ok := wm.ExactTestOutcome()
		if !ok || test != "t" {
			t.Fatalf("worker %d is not serving t exactly at its end: %q %v", inst, test, ok)
		}
		if len(missed) != 0 || len(unmade) != 0 {
			t.Errorf("worker %d, under keploy.yml's Referer noise: missed %v, unmade %v; want none", inst, missed, unmade)
		}
	}
}
