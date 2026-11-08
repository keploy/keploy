package proxy_test

import (
	"testing"

	"go.keploy.io/server/v3/pkg/agent/proxy"
	agentsvc "go.keploy.io/server/v3/pkg/service/agent"
)

// keploy test reads each set's end with the switch on and off, and a call a
// parser still holds for one of the set's tests is filed as a miss at that
// read (MockManager.AtSetEnd, Proxy.FileAtSetEnd). A replay test worker's own
// store (Proxy.ServeInstance) takes no such registration, and no set's end
// read asks it: a call held there would be filed only when its connection
// ends, after the set's last read, and a connection that outlives the set
// would leave it unreported there. No run reaches that store while keploy's
// agent takes one test worker at a time: MaxWorkers is the one place the
// number is set, and a claim that declares more is refused at once.
//
// This row holds the two together. It fails the moment a build lets
// MaxWorkers take more than one test worker while a worker's store still
// files nothing at the set's end, and it fails when a worker's store starts
// to file there, so that whoever builds that also builds the rest, named
// below, before this row lets a second worker run.
func TestSeveralTestWorkersRunOnlyWhereTheirStoresFileTheSetsEnd(t *testing.T) {
	const build = "(1) a test worker's manager takes its session's word that each set's end is read " +
		"(ServeInstance sets notAtSetEnd, and Proxy.Mock tells the run's manager alone, SetReadsSetEnds); " +
		"(2) Proxy.FileAtSetEnd runs each serving worker manager's registrations, not only the run's; " +
		"(3) the agent's read of a set's end (models.SetEndRead) takes the misses filed in each worker's " +
		"misses window (instance_misses.go), not only the run's (Proxy.GetMockErrors)"
	if n := agentsvc.MaxWorkers(); n > 1 {
		t.Fatalf("keploy's agent now takes %d test workers at once (MaxWorkers), but a replay test worker's own store "+
			"files no call it holds at a set's end: a Kafka poll waiting there would be filed only when its connection "+
			"ends, after the set's last read. Before more than one test worker runs, build all three, with rows: %s", n, build)
	}
	if proxy.WorkerStoresFileTheSetsEnd(t) {
		t.Fatalf("a replay test worker's own store now files a call it holds at the set's end. Check that all three "+
			"are built, with rows, then make this row ask for them where it lets MaxWorkers take more than one: %s", build)
	}
}
