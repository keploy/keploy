package proxy

import (
	"context"
	"os"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// A test worker's client of a port the recording never saw that gives up
// waiting for its greeting after the worker began its next test is marked in
// the round of the worker's own manager it arrived in: its miss names the
// worker's test it was made in, never the run's test, nor the worker's next.
func TestAWorkersSilentClientIsMarkedInItsWorkersRound(t *testing.T) {
	t.Setenv("KEPLOY_PROVEN", "1")
	p := testProxy(t)
	p.errChannel = make(chan error, 4)
	p.mockManager = NewMockManager(NewTreeDb(customComparator), NewTreeDb(customComparator), zap.NewNop())
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	p.StartErrorDrain(ctx)
	recordedMySQL(p, "127.0.0.1:3306", "")
	s, err := p.ServeInstance(7)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(p.endInstances)
	w := s.(workerMisses)
	m7 := instanceMgrOf(t, p, 7)
	run := p.getMockManager()
	run.ServeExactPool(models.PoolKeyNone, nil)
	run.ServeExactTest("R1", nil)
	m7.ServeExactPool(models.PoolKeyNone, nil)
	m7.ServeExactTest("W1", nil)
	w.ContinueTestErrorCapture()

	client := newSilentClient()
	done := make(chan *mysqlProbe, 1)
	go func() {
		probe, _ := p.probeMysql(ctx, client, "10.9.9.9:13306", nil, 13306, models.MODE_TEST,
			models.OutgoingOptions{SrcPid: uint32(os.Getpid()), SrcInstance: 7}, zap.NewNop())
		done <- probe
	}()
	if probe := readOrVerdict(t, done, client); probe != nil {
		t.Fatalf("the probe decided (reason %q) before the client did anything", probe.Reason)
	}
	// Worker 7's test ends and its next begins before the client gives up.
	m7.ServeExactPool(models.PoolKeyNone, nil)
	m7.ServeExactTest("W2", nil)
	_ = client.Close()
	if probe := awaitEvent(t, done, "the probe's verdict once the client left"); probe.IsMySQL {
		t.Fatalf("reason %q: an endpoint the recording cannot be tied to was served as MySQL", probe.Reason)
	}
	got, err := w.GetMockErrors(ctx)
	if err != nil {
		t.Fatalf("worker 7's read: %v", err)
	}
	if len(got) != 1 || !got[0].Marked.Known || got[0].Marked.Test != "W1" || got[0].Kind != models.MissInTest {
		t.Fatalf("worker 7's read %+v; want its one miss, in a test, marked in W1's view, the worker's test it arrived in", got)
	}
}
