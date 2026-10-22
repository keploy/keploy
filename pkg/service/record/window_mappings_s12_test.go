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

// s12Recording is leg (a)'s recording (S12 v16 §5): P, then post-url-1, and
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
// and a process keploy could not name.
func TestProcessOfNamesCommAndArgv0(t *testing.T) {
	for _, tc := range []struct{ comm, exe, want string }{
		{"sqs", "./sqs", "`sqs` (`./sqs`)"},
		{"sqs", "sqs", "`sqs`"},
		{"sqs", "", "`sqs`"},
		{"", "./sqs", "`./sqs`"},
		{"", "", "a process keploy could not name"},
	} {
		if got := processOf(tc.comm, tc.exe); got != tc.want {
			t.Errorf("processOf(%q, %q) = %q; want %q", tc.comm, tc.exe, got, tc.want)
		}
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
