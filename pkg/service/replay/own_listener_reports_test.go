package replay

import (
	"context"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

const ownListenerReportNote = "the application called its own listener at an address this test set's recordings hold calls to"

// reportingInstrumentation is an agent that names, from its n-th read on
// (at, counted by reads), the destinations it passed through as the app's own
// listener.
type reportingInstrumentation struct {
	Instrumentation
	reads *atomic.Int32
	at    int32
	dests []string
}

func (i reportingInstrumentation) OwnListenerCalls() []string {
	if i.reads.Load() < i.at {
		return nil
	}
	return i.dests
}

// reportingAt is an agent that names dests from the start.
func reportingAt(dests ...string) reportingInstrumentation {
	return reportingInstrumentation{reads: &atomic.Int32{}, dests: dests}
}

// The set's recordings are found by where they were made, in the agent's
// spelling: a destAddr, else an HTTP recording's Host, when it is an IP and a
// port. A destination the agent names marks each recording made there, once;
// one it does not name marks nothing.
func TestOwnListenerReportsMarkTheRecordingsMadeWhereTheAgentSaid(t *testing.T) {
	byHost := &models.Mock{Name: "by-host", Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Header: map[string]string{"Host": "172.18.0.5:6100"}}}}
	mapped := calledAt("mapped", 1, "[::ffff:172.18.0.5]:6100")
	padded := calledAt("padded", 2, " 172.18.0.5:6100 ")
	other := calledAt("other", 3, "172.18.0.7:5432")
	named := calledAt("named", 4, "app:6100")
	early := calledAt("early", 5, "127.0.0.1:6100")
	early.MarkOwnListenerCall()
	again := calledAt("mapped", 1, "[::ffff:172.18.0.5]:6100")

	var o ownListenerReports
	func() {
		defer func() {
			if p := recover(); p != nil {
				t.Fatalf("indexing a pool that holds a nil recording panicked: %v", p)
			}
		}()
		o = newOwnListenerReports([]*models.Mock{byHost, nil, mapped, other}, []*models.Mock{padded, named, early, again})
	}()
	if !o.marked["early"] || len(o.marked) != 1 {
		t.Fatalf("marked at the start %v; want only the recording marked at load", o.marked)
	}
	if got := o.apply([]string{"10.0.0.1:80"}); len(got) != 0 {
		t.Fatalf("a destination no recording was made at marked %v", got)
	}
	if got := o.apply([]string{"172.18.0.5:6100"}); !reflect.DeepEqual(got, []string{"by-host", "mapped", "padded"}) {
		t.Fatalf("the app's own address marked %v; want by-host, mapped and padded, once each, sorted", got)
	}
	if got := o.apply([]string{"172.18.0.5:6100", "127.0.0.1:6100"}); len(got) != 0 {
		t.Fatalf("a second report of the same destinations marked %v again", got)
	}
	for _, n := range []string{"other", "named"} {
		if o.marked[n] {
			t.Fatalf("%s is marked; a recording at another service, or at a name, is judged as before", n)
		}
	}
	// The zero value marks nothing.
	if got := (ownListenerReports{}).apply([]string{"172.18.0.5:6100"}); len(got) != 0 {
		t.Fatalf("an empty index marked %v", got)
	}
}

// What the agent names leaves a recording made there out of what a test
// requires (the set's reusableMockNames and unservedMockNames), in the
// plan the set's end reads, and is said once, at INFO; an agent that names
// nothing (one released before it names any) changes nothing, and says
// nothing.
func TestTheAgentsReportLeavesTheAppsOwnCallsOutOfWhatATestRequires(t *testing.T) {
	for _, tc := range []struct {
		name string
		inst Instrumentation
		want []string
	}{
		{"an agent that reports", reportingAt("172.18.0.5:6100"), []string{"self"}},
		{"an agent that reports none", reportingAt(), nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			core, logs := observer.New(zapcore.InfoLevel)
			r := &Replayer{logger: zap.New(core), instrumentation: tc.inst}
			o := newOwnListenerReports([]*models.Mock{calledAt("self", 1, "172.18.0.5:6100"), calledAt("db", 2, "172.18.0.7:5432")})
			p := newExactPlan(nil, nil)
			p.ownListener = o.marked
			reusable, unserved := map[string]bool{}, map[string]bool{"old": true}
			got := r.applyOwnListenerReport("ts", o, reusable, unserved)
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("marked %v; want %v", got, tc.want)
			}
			for _, n := range tc.want {
				if !reusable[n] || !unserved[n] || !p.ownListener[n] {
					t.Fatalf("%s: reusable %v, unserved %v, plan %v; want it in each", n, reusable, unserved, p.ownListener)
				}
			}
			if reusable["db"] || unserved["db"] || p.ownListener["db"] || !unserved["old"] {
				t.Fatalf("reusable %v, unserved %v, plan %v; want the other service's recording judged as before", reusable, unserved, p.ownListener)
			}
			if n := logs.FilterMessageSnippet(ownListenerReportNote).Len(); n != len(tc.want) {
				t.Fatalf("said it %d times; want %d", n, len(tc.want))
			}
			// The same report again says nothing more.
			r.applyOwnListenerReport("ts", o, reusable, unserved)
			if n := logs.FilterMessageSnippet(ownListenerReportNote).Len(); n != len(tc.want) {
				t.Fatalf("a repeated report was said again: %d times", n)
			}
		})
	}
}

// An agent released before it names the calls it passes through as the
// app's own marks nothing: each recording is judged as before.
func TestAnOlderAgentsAnswerMarksNoRecordingAsTheAppsOwnCall(t *testing.T) {
	core, logs := observer.New(zapcore.InfoLevel)
	r := &Replayer{logger: zap.New(core), instrumentation: struct{ Instrumentation }{}}
	o := newOwnListenerReports([]*models.Mock{calledAt("self", 1, "172.18.0.5:6100")})
	reusable := map[string]bool{}
	if got := r.applyOwnListenerReport("ts", o, reusable); len(got) != 0 || len(reusable) != 0 || len(o.marked) != 0 || logs.Len() != 0 {
		t.Fatalf("marked %v, reusable %v, marks %v, said %d line(s); want nothing", got, reusable, o.marked, logs.Len())
	}
}

// A worker-scope recording of the app's call to its own listener at its
// container's address, which no load-time check can tell, is required by the
// set's end until the agent names that address: the wait then ends at that
// read, though the call was never consumed. A worker recording at an address
// the agent never names stays required.
func TestTheSetsEndStopsRequiringAWorkerCallTheAgentPassedThroughAsTheAppsOwn(t *testing.T) {
	self := calledAt("w-self", 1, "172.18.0.5:6100")
	dep := calledAt("w-dep", 2, "172.18.0.7:5432")
	for _, tc := range []struct {
		name  string
		dests []string
		want  []string
	}{
		{"reported at the second read", []string{"172.18.0.5:6100"}, nil},
		{"never reported", nil, []string{"w-self"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			o := newOwnListenerReports([]*models.Mock{self, dep})
			p := newExactPlan(nil, []models.MockEntry{{Name: "w-self"}, {Name: "w-dep"}}, []*models.Mock{self, dep})
			p.ownListener = o.marked
			p.bootSeen = true
			reads := &atomic.Int32{}
			inst := reportingInstrumentation{reads: reads, at: 2, dests: tc.dests}
			r := &Replayer{logger: zap.NewNop(), instrumentation: inst, hookImpl: nthRead{n: reads, at: 1}}
			consumed := map[string]models.MockState{"w-dep": {Name: "w-dep"}}
			if got := p.unansweredWorker(consumed, false); !reflect.DeepEqual(got, []string{"w-self"}) {
				t.Fatalf("before any report the set's end requires %v; want w-self", got)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
			defer cancel()
			missing, err := r.awaitWorkerScope(ctx, p, consumed, func(read []models.MockState) {
				r.applyOwnListenerReport("ts", o)
				for _, m := range read {
					consumed[m.Name] = m
				}
			})
			if err != nil || !reflect.DeepEqual(missing, tc.want) {
				t.Fatalf("the set's end required %v (%v) after %d reads; want %v", missing, err, reads.Load(), tc.want)
			}
		})
	}
}

// A grouped recording the agent reports, after the shares were planned, as
// the app's call to its own listener charges no test of its group at the
// set's end: no test made it from a mock. Unreported, it charges the group.
func TestSettleSharesNeverChargesARecordingReportedMidSetAsTheAppsOwnCall(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	registerShareKeys(t)
	for _, tc := range []struct {
		name  string
		dests []string
		want  []string
	}{
		{"reported", []string{"172.18.0.5:6100"}, nil},
		{"not reported", nil, []string{"a", "b"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ca := shMock("ca", 100, "INSERT", "bind.$1=a", "ok")
			self := shMock("self", 200, "INSERT", "bind.$1=health", "ok")
			self.Spec.Metadata["destAddr"] = "172.18.0.5:6100"
			s := newShareSet(t, []models.ConcurrencyGroup{{ID: "g1", Tests: []string{"a", "b"}}},
				map[string][]models.MockEntry{"a": {shEntry(ca, "g1"), shEntry(self, "g1")}, "b": {}}, nil, nil, ca, self)
			o := newOwnListenerReports([]*models.Mock{ca, self})
			s.p.ownListener = o.marked
			s.run("a", used("ca"))
			o.apply(tc.dests)
			s.run("b")
			if charged := s.settle(t); !reflect.DeepEqual(charged, tc.want) {
				t.Fatalf("charged %v; want %v", charged, tc.want)
			}
		})
	}
}

// A recording of the app's call to its own listener at its container's
// address, at boot and inside test-1, is no call the set requires once the
// agent names that address as one it passed through to the app: the set
// passes, and the replay says so once. An agent that names nothing leaves
// both judged as before: test-1 fails for its recorded call. Under the
// switch, whether keploy record wrote the set's mappings.yaml or the replay
// derives it from the windows; with the switch off, under
// --assert-dependencies.
func TestARecordingAtAnAddressTheAgentReportsAsTheAppsOwnIsNoCallTheVerdictRequires(t *testing.T) {
	for _, tc := range []struct {
		name     string
		on       bool
		recorded bool
	}{
		{"switch on, mapped as record wrote it", true, true},
		{"switch on, mapped from the windows", true, false},
		{"switch off, --assert-dependencies", false, true},
	} {
		for _, reported := range []bool{true, false} {
			name := tc.name + ", reported"
			if !reported {
				name = tc.name + ", not reported"
			}
			t.Run(name, func(t *testing.T) {
				setSwitch(t, tc.on)
				fastWorkerWait(t)
				h := newPartialRunHarness(t, 1, 0)
				pinWindows(h)
				port := appPortOf(t, h)
				h.replayer.config.Command = "./app"
				h.replayer.config.Test.AssertDependencies = !tc.on
				core, logs := observer.New(zap.InfoLevel)
				h.replayer.logger = zap.New(core)
				own := "172.18.0.5:" + port
				var dests []string
				if reported {
					dests = []string{own}
				}
				h.replayer.instrumentation = reportingInstrumentation{Instrumentation: h.replayer.instrumentation, reads: &atomic.Int32{}, dests: dests}
				h.mocks.filtered = []*models.Mock{calledAt("mock-self-boot", 500, own), httpAt("mock-a", 1010), calledAt("mock-in-test", 1020, own)}
				var recorded map[string][]models.MockEntry
				if tc.recorded {
					recorded = map[string][]models.MockEntry{"test-1": {
						{Name: "mock-a", Kind: string(models.HTTP)},
						{Name: "mock-in-test", Kind: string(models.HTTP)},
					}}
				}
				db := mappingFile(t, h, recorded)
				if tc.recorded && tc.on {
					if err := db.UpsertWorker(context.Background(), "test-set-0", []models.MockEntry{{Name: "mock-self-boot", Kind: string(models.HTTP)}}); err != nil {
						t.Fatal(err)
					}
				}
				made := models.MockState{Name: "mock-a", Kind: models.HTTP, Usage: models.Deleted, Lifetime: models.LifetimePerTest,
					ReqTimestampMock: models.FormatMockTimestamp(attrAt(1010)), ResTimestampMock: models.FormatMockTimestamp(attrAt(1011))}
				h.replayer.hookImpl = prHooks{consumedFor: consumedByWindow(h, map[string][]models.MockState{"test-1": {made}})}
				set := h.run(t)
				got := resultOf(t, h, "test-1")
				if n := logs.FilterMessageSnippet(ownListenerNote).Len(); n != 0 {
					t.Fatalf("the load-time note was said %d time(s); a container address is no loopback one", n)
				}
				if !reported {
					if n := logs.FilterMessageSnippet(ownListenerReportNote).Len(); n != 0 {
						t.Fatalf("an agent that named nothing was said to have: %d line(s)", n)
					}
					named := !tc.on || (len(got.FailureInfo.UnconsumedMocks) == 1 && got.FailureInfo.UnconsumedMocks[0].Name == "mock-in-test")
					if set == models.TestSetStatusPassed || got.Status == models.TestStatusPassed || !named {
						t.Fatalf("set %s, test-1 %s, unconsumed %+v; want test-1 failed for mock-in-test", set, got.Status, got.FailureInfo.UnconsumedMocks)
					}
					return
				}
				if c := logs.FilterMessageSnippet(ownListenerReportNote).All(); len(c) != 1 || c[0].ContextMap()["count"] != int64(2) {
					t.Fatalf("the replay's note: %d line(s) %v; want one, count 2", len(c), c)
				}
				if set != models.TestSetStatusPassed {
					t.Fatalf("set %s, want PASSED (worker-scope call named %d time(s))", set, workerNamed(logs))
				}
				if got.Status != models.TestStatusPassed || got.FailureInfo.UnconsumedMocks != nil {
					t.Fatalf("test-1: %s, unconsumed %+v; want PASSED, none", got.Status, got.FailureInfo.UnconsumedMocks)
				}
				if tc.recorded {
					if got.MockMismatches == nil {
						t.Fatal("test-1 has no expected-vs-consumed report")
					}
					var expected []string
					for _, m := range got.MockMismatches.ExpectedMocks {
						expected = append(expected, m.Name)
					}
					if len(expected) != 1 || expected[0] != "mock-a" {
						t.Fatalf("test-1's report expects %v; want [mock-a], not the self-call", expected)
					}
				}
			})
		}
	}
}
