package record

import (
	"context"
	"strings"
	"testing"

	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/pkg/service/replay"
	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// testWrites is a kind of this test's own, so no parser's registration is
// read: each of its mocks is a write of "Test insert keploy.events".
const testWrites models.Kind = "S12TestWrites"

// markingMappingDB is the fake mapping store that keeps the set's mark
// (recordedWithMarker).
type markingMappingDB struct {
	recMappingDB
	marks []string
}

func (d *markingMappingDB) MarkRecordedWith(_ context.Context, testSetID, feature string) error {
	d.mu.Lock()
	defer d.mu.Unlock()
	d.marks = append(d.marks, testSetID+"="+feature)
	return nil
}

// s12Recording is leg (a)'s recording: P, then post-url-1, and
// the consumer's insert mock-6 sent in the gap before post-url-1 and answered
// inside it, every stamp on one clock.
func s12Recording(t *testing.T) *windowMappings {
	t.Helper()
	t.Cleanup(models.RegisterWriteKind(testWrites, func(*models.Mock) (string, bool) { return "Test insert keploy.events", true }))
	return s12Windows()
}

// s12Windows is s12Recording's windows and mocks, testWrites registered.
func s12Windows() *windowMappings {
	const clock = "proxy:run-1"
	w := &windowMappings{}
	p, post := winTest("P", 1000, 1100), winTest("post-url-1", 2000, 2100)
	p.Origin, post.Origin = &models.Origin{Clock: clock}, &models.Origin{Clock: clock}
	w.addTest(p)
	w.addTest(post)
	insert := winMock("mock-6", testWrites, "", 1500)
	insert.Spec.ResTimestampMock = winAt(2050)
	insert.Origin = &models.Origin{Clock: clock, Comm: "sqs", Exe: "./sqs"}
	w.addMock(insert)
	w.addMock(winMock("mock-7", models.HTTP, "", 2010))
	return w
}

// An S12 record (the switch on, S12 serving enabled by a registered func, a
// store that keeps the mark) routes a write in
// flight into a test on its window's clock to the worker scope, at the set's
// start, with the line that says why and how to undo it, and marks the set.
func TestWindowMappingsRouteAnInFlightWriteAndMarkTheSet(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t.Cleanup(replay.RegisterS12Serving(func() bool { return true }))
	db := &markingMappingDB{}
	s12Recording(t).write(context.Background(), zap.NewNop(), db, "test-set-0", nil, true, 0)

	if len(db.batches) != 1 {
		t.Fatalf("batches %v; want one", db.batches)
	}
	for _, e := range db.batches[0]["post-url-1"] {
		if e.Name == "mock-6" {
			t.Fatal("the routed write is still post-url-1's")
		}
	}
	if len(db.worker) != 1 || db.worker[0].Name != "mock-6" || db.worker[0].Place != "" {
		t.Fatalf("worker scope %+v; want mock-6 at the set's start", db.worker)
	}
	reason := db.worker[0].Reason
	for _, want := range []string{"mock-6 (Test insert keploy.events) routed to background at record", "`sqs` (`./sqs`) sent it before post-url-1 began", "keploy's proxy clock (proxy:run-1)", "Undo: move this entry under tests[post-url-1].mocks"} {
		if !strings.Contains(reason, want) {
			t.Errorf("the routing line %q does not say %q", reason, want)
		}
	}
	if len(db.marks) != 1 || db.marks[0] != "test-set-0="+models.RecordedWithS12Routing {
		t.Fatalf("marks %v; want test-set-0 marked %s", db.marks, models.RecordedWithS12Routing)
	}
}

// Any other recording neither routes nor marks: one no registered func
// enables S12 serving for (none registered, or one that says no), one
// without the switch, and one into a store that cannot keep the mark (whose
// routed write today's rules would serve).
func TestWindowMappingsRouteOnlyForASetTheyMark(t *testing.T) {
	for _, tc := range []struct {
		name string
		on   bool
		// enabled is what a registered func says; nil registers none.
		enabled *bool
		store   MappingDb
	}{
		{"S12 serving not registered", true, nil, &markingMappingDB{}},
		{"S12 serving registered off", true, new(false), &markingMappingDB{}},
		{"no switch", false, new(true), &markingMappingDB{}},
		{"a store without the mark", true, new(true), &recMappingDB{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if tc.on {
				t.Setenv(provenswitch.Env, "1")
			} else {
				t.Setenv(provenswitch.Env, "")
			}
			if tc.enabled != nil {
				enabled := *tc.enabled
				t.Cleanup(replay.RegisterS12Serving(func() bool { return enabled }))
			}
			s12Recording(t).write(context.Background(), zap.NewNop(), tc.store, "test-set-0", nil, true, 0)
			var rec *recMappingDB
			switch db := tc.store.(type) {
			case *markingMappingDB:
				rec = &db.recMappingDB
				if len(db.marks) != 0 {
					t.Fatalf("marked %v", db.marks)
				}
			case *recMappingDB:
				rec = db
			}
			owned := false
			for _, e := range rec.batches[0]["post-url-1"] {
				owned = owned || e.Name == "mock-6"
			}
			if !owned {
				t.Fatalf("mock-6 was routed: post-url-1 has %v", rec.batches[0]["post-url-1"])
			}
			for _, e := range rec.worker {
				if e.Reason != "" {
					t.Fatalf("a worker entry has a routing line: %+v", e)
				}
			}
		})
	}
}

// A recording the agent mapped itself is marked all the same, once drained:
// record's judging judges what its background answers.
func TestWindowMappingsMarkASetTheAgentMapped(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t.Cleanup(replay.RegisterS12Serving(func() bool { return true }))
	db := &markingMappingDB{}
	s12Recording(t).write(context.Background(), zap.NewNop(), db, "test-set-0", nil, true, 3)
	if len(db.marks) != 1 || len(db.batches) != 0 {
		t.Fatalf("marks %v, batches %v; want the set marked and nothing derived", db.marks, db.batches)
	}
	db = &markingMappingDB{}
	s12Windows().write(context.Background(), zap.NewNop(), db, "test-set-0", nil, false, 3)
	if len(db.marks) != 0 {
		t.Fatalf("a recording whose drain timed out was marked: %v", db.marks)
	}
}

// The process a routing line names: comm and argv[0], one when they agree,
// and a process keploy could not name; one it would not name because it runs
// in a PID namespace of its own says so, with the kernel pid and the remedy,
// never a name.
func TestProcessOfNamesCommAndArgv0(t *testing.T) {
	for _, tc := range []struct {
		m    models.WindowedMock
		want string
	}{
		{models.WindowedMock{Comm: "sqs", Exe: "./sqs"}, "`sqs` (`./sqs`)"},
		{models.WindowedMock{Comm: "sqs", Exe: "sqs"}, "`sqs`"},
		{models.WindowedMock{Comm: "sqs"}, "`sqs`"},
		{models.WindowedMock{Exe: "./sqs"}, "`./sqs`"},
		{models.WindowedMock{}, "a process keploy could not name"},
	} {
		for _, log := range []bool{true, false} {
			if got := processOf(tc.m, log); got != tc.want {
				t.Errorf("processOf(%+v, %v) = %q; want %q", tc.m, log, got, tc.want)
			}
		}
	}
	// One keploy would not name: why and the remedy, the kernel pid in the
	// record log's line only, never in the committed reason.
	unnamed := models.WindowedMock{Unnamed: models.UnnamedPIDNamespace, PID: 2912345}
	if got, want := processOf(unnamed, true), "a process keploy could not name ("+models.PIDNamespaceReason(2912345)+")"; got != want {
		t.Errorf("the log line names %q; want %q", got, want)
	}
	for log, line := range map[bool]string{true: processOf(unnamed, true), false: processOf(unnamed, false)} {
		for _, want := range []string{"a process keploy could not name (keploy runs in a PID namespace of its own", "--pid=host", "hostPID", "docker and compose modes"} {
			if want == "a process keploy could not name (keploy runs in a PID namespace of its own" && log {
				want = "a process keploy could not name (kernel pid 2912345: keploy runs in a PID namespace of its own"
			}
			if !strings.Contains(line, want) {
				t.Errorf("log=%v: the namespace line %q does not say %q", log, line, want)
			}
		}
		if !log && strings.Contains(line, "2912345") {
			t.Errorf("the committed line %q holds the kernel pid", line)
		}
	}
}

// A routed write whose process keploy would not name, because it runs in a
// PID namespace of its own, says that in its reason line, with the remedy and
// without the kernel pid, which no committed file holds: two
// recordings of one app commit one line, whatever pid its process had. The
// record log's line gives the pid.
func TestARoutedWritesLineSaysKeployRunsInAPIDNamespaceOfItsOwn(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t.Cleanup(replay.RegisterS12Serving(func() bool { return true }))
	t.Cleanup(models.RegisterWriteKind(testWrites, func(*models.Mock) (string, bool) { return "Test insert keploy.events", true }))
	const clock = "proxy:run-1"
	rec := &windowMappings{}
	p, post := winTest("P", 1000, 1100), winTest("post-url-1", 2000, 2100)
	p.Origin, post.Origin = &models.Origin{Clock: clock}, &models.Origin{Clock: clock}
	rec.addTest(p)
	rec.addTest(post)
	insert := winMock("mock-6", testWrites, "", 1500)
	insert.Spec.ResTimestampMock = winAt(2050)
	// As the agent stamps it in a PID namespace of its own: the clock, and
	// why no process is named (starts.Registry.StampOrigin).
	insert.Origin = &models.Origin{Clock: clock, Unnamed: models.UnnamedPIDNamespace, PID: 2912345}
	rec.addMock(insert)
	db := &markingMappingDB{}
	core, logs := observer.New(zapcore.InfoLevel)
	rec.write(context.Background(), zap.New(core), db, "test-set-0", nil, true, 0)
	if len(db.worker) != 1 || db.worker[0].Name != "mock-6" {
		t.Fatalf("worker scope %+v; want mock-6 routed", db.worker)
	}
	reason := db.worker[0].Reason
	for _, want := range []string{"a process keploy could not name (keploy runs in a PID namespace of its own", "--pid=host"} {
		if !strings.Contains(reason, want) {
			t.Fatalf("the line %q does not say %q", reason, want)
		}
	}
	if strings.Contains(reason, "2912345") {
		t.Fatalf("the committed line %q holds the kernel pid", reason)
	}
	routed := logs.FilterMessageSnippet("routed a write").All()
	if len(routed) != 1 || !strings.Contains(routed[0].ContextMap()["process"].(string), "kernel pid 2912345") {
		t.Fatalf("the record log's routing line %+v does not give the kernel pid", routed)
	}
}

// A write routed out of a test the recording did not keep says so, and its
// undo is to delete the entry, never to move it under a test the set has not.
func TestARoutedWritesLineForATestNotKeptUndoesByDeleting(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t.Cleanup(replay.RegisterS12Serving(func() bool { return true }))
	db := &markingMappingDB{}
	s12Recording(t).write(context.Background(), zap.NewNop(), db, "test-set-0", map[string]struct{}{"post-url-1": {}}, true, 0)
	if len(db.worker) != 1 || db.worker[0].Name != "mock-6" {
		t.Fatalf("worker scope %+v; want mock-6 routed", db.worker)
	}
	reason := db.worker[0].Reason
	if !strings.Contains(reason, "Undo: delete this entry: post-url-1 is not in this set") {
		t.Fatalf("the line %q does not undo by deleting the entry", reason)
	}
	if strings.Contains(reason, "tests[post-url-1]") {
		t.Fatalf("the line %q moves the entry under a test the set has not", reason)
	}
}

// A recording made with --sync, or with mapping disabled, derives no test
// windows: it routes nothing and marks no set for S12's serving, which, under
// a build that enables that serving, it says, with how to record one that
// does. A recording that derives windows says nothing of it.
func TestARecordingWithNoWindowsSaysItIsNotMarked(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	t.Cleanup(replay.RegisterS12Serving(func() bool { return true }))
	for _, tc := range []struct {
		name          string
		sync, disable bool
		said          bool
	}{
		{"--sync", true, false, true},
		{"mapping disabled", false, true, true},
		{"windows derived", false, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.InfoLevel)
			cfg := &config.Config{DisableMapping: tc.disable}
			cfg.Record.Synchronous = tc.sync
			r := &Recorder{logger: zap.New(core), config: cfg, mappingDb: &countingMappingDB{}}
			w := r.newWindowMappings()
			if (w == nil) != tc.said {
				t.Fatalf("the collector is %v; want nil only for a recording with no windows", w)
			}
			if n := logs.FilterMessageSnippet("does not route background writes nor mark its test set").Len(); (n == 1) != tc.said {
				t.Fatalf("said %d times; want %v", n, tc.said)
			}
		})
	}
}
