package replay

import (
	"context"
	"net/url"
	"strconv"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// ownListenerNote is what the replay says of a set's recordings of the app's
// calls to its own port.
const ownListenerNote = "this test set holds recordings of calls the app made to its own port"

// calledAt is httpAt, recorded as a call to dest (its destAddr).
func calledAt(name string, ms int, dest string) *models.Mock {
	m := httpAt(name, ms)
	m.Spec.Metadata = map[string]string{"destAddr": dest}
	return m
}

func TestOwnListenerMockReadsWhereTheRecordingWent(t *testing.T) {
	ports := map[uint16]bool{6100: true, 65535: true}
	withHost := func(dest, host string) *models.Mock {
		m := &models.Mock{Kind: models.HTTP, Spec: models.MockSpec{HTTPReq: &models.HTTPReq{Header: map[string]string{"Host": host}}}}
		if dest != "" {
			m.Spec.Metadata = map[string]string{"destAddr": dest}
		}
		return m
	}
	for _, tc := range []struct {
		name string
		m    *models.Mock
		want bool
	}{
		{"loopback v4 at the app's port", calledAt("m", 0, "127.0.0.1:6100"), true},
		{"another loopback v4 address", calledAt("m", 0, "127.0.0.2:6100"), true},
		{"loopback v6", calledAt("m", 0, "[::1]:6100"), true},
		{"unspecified v4", calledAt("m", 0, "0.0.0.0:6100"), true},
		{"unspecified v6", calledAt("m", 0, "[::]:6100"), true},
		{"localhost", calledAt("m", 0, "localhost:6100"), true},
		{"localhost in capitals", calledAt("m", 0, "LOCALHOST:6100"), true},
		{"destAddr padded", calledAt("m", 0, " 127.0.0.1:6100 "), true},
		{"another local service", calledAt("m", 0, "127.0.0.1:5432"), false},
		{"another host at the app's port", calledAt("m", 0, "10.0.0.5:6100"), false},
		{"a service by name at the app's port", calledAt("m", 0, "db:6100"), false},
		{"no port", calledAt("m", 0, "127.0.0.1"), false},
		{"a port out of range", calledAt("m", 0, "127.0.0.1:70000"), false},
		{"no destination", &models.Mock{Kind: models.HTTP}, false},
		{"nil", nil, false},
		{"only a Host, at the app's port", withHost("", "localhost:6100"), true},
		{"only a Host, at another port", withHost("", "localhost:5432"), false},
		{"destAddr decides over the Host", withHost("10.0.0.5:80", "localhost:6100"), false},
		{"destAddr decides for the Host", withHost("127.0.0.1:6100", "api.example.com"), true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := ownListenerMock(tc.m, ports); got != tc.want {
				t.Fatalf("ownListenerMock = %v, want %v", got, tc.want)
			}
		})
	}
	// A payload left on disk keeps the Host it was recorded with.
	m := withHost("", "localhost:6100")
	m.LeavePayloadOnDisk(nil, models.PayloadAt{Name: "m"})
	if !ownListenerMock(m, ports) {
		t.Fatal("a recording whose payload is on disk, with only its Host, is not recognized")
	}
}

func TestOwnListenerPortsAreTheTestCasesAppPorts(t *testing.T) {
	got := ownListenerPorts([]*models.TestCase{{AppPort: 6100}, {AppPort: 0}, {AppPort: 8080}, {AppPort: 6100}})
	if len(got) != 2 || !got[6100] || !got[8080] {
		t.Fatalf("ports %v, want 6100 and 8080 only", got)
	}
	// A test case recorded with no port names none: not port 0.
	if ownListenerMock(calledAt("m", 0, "127.0.0.1:0"), ownListenerPorts([]*models.TestCase{{}})) {
		t.Fatal("a test case with no AppPort made port 0 the app's")
	}
}

func TestMarkOwnListenerMocksMarksTheAppsSelfCallsAndSaysSoOnce(t *testing.T) {
	core, logs := observer.New(zap.InfoLevel)
	r := &Replayer{logger: zap.New(core)}
	cases := []*models.TestCase{{AppPort: 6100}}
	self, other := calledAt("mock-self", 0, "127.0.0.1:6100"), calledAt("mock-db", 0, "127.0.0.1:5432")
	again := calledAt("mock-self", 0, "127.0.0.1:6100")
	if n := r.markOwnListenerMocks("set", cases, []*models.Mock{self, other}, []*models.Mock{again}); n != 1 {
		t.Fatalf("marked %d, want 1 (one name, in both pools)", n)
	}
	if !self.OwnListenerCall() || !again.OwnListenerCall() || other.OwnListenerCall() {
		t.Fatalf("marks: self %v, again %v, other %v; want true, true, false", self.OwnListenerCall(), again.OwnListenerCall(), other.OwnListenerCall())
	}
	said := logs.FilterMessageSnippet(ownListenerNote).All()
	if len(said) != 1 {
		t.Fatalf("said %d time(s), want once", len(said))
	}
	if c := said[0].ContextMap()["count"]; c != int64(1) {
		t.Fatalf("count %v, want 1", c)
	}
	// None marked: nothing said.
	if n := r.markOwnListenerMocks("set", cases, []*models.Mock{calledAt("mock-db", 0, "127.0.0.1:5432")}); n != 0 {
		t.Fatalf("marked %d, want 0", n)
	}
	if n := len(logs.FilterMessageSnippet(ownListenerNote).All()); n != 1 {
		t.Fatalf("a set with no self-call recording was said of too (%d)", n)
	}
}

// appPortOf sets each of h's test cases' AppPort to the port they are sent
// to, as keploy record writes it, and returns it.
func appPortOf(t *testing.T, h *prRun) string {
	t.Helper()
	db, ok := h.replayer.testDB.(*prTestDB)
	if !ok || len(db.cases) == 0 {
		t.Fatal("the harness has no test cases")
	}
	u, err := url.Parse(db.cases[0].HTTPReq.URL)
	if err != nil {
		t.Fatal(err)
	}
	n, err := strconv.ParseUint(u.Port(), 10, 16)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range db.cases {
		tc.AppPort = uint16(n)
	}
	return u.Port()
}

// A recording of a call the app made to its own port, which a keploy before
// this one recorded and which keploy now passes through to the app, is no
// call the app must make again: a set whose app made every other call passes,
// though the recording holds a self-call at its boot and one inside test-1,
// nothing names either as a call it never made, and the replay says so once.
// A recorded call to another local service the app did not make still fails
// test-1. Under the switch, whether keploy record wrote the set's
// mappings.yaml (the self-calls in the worker scope and test-1's mapping) or
// the replay derives it from the windows; with the switch off, under
// --assert-dependencies, with test-1's mapping listing the one in its window.
func TestARecordedCallToTheAppsOwnPortIsNoCallTheVerdictRequires(t *testing.T) {
	for _, tc := range []struct {
		name     string
		on       bool
		recorded bool
	}{
		{"switch on, mapped as record wrote it", true, true},
		{"switch on, mapped from the windows", true, false},
		{"switch off, --assert-dependencies", false, true},
	} {
		for _, own := range []bool{true, false} {
			name := tc.name + ", a self-call"
			if !own {
				name = tc.name + ", another local service"
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
				inTest := "127.0.0.1:" + port
				if !own {
					inTest = "127.0.0.1:1"
				}
				h.mocks.filtered = []*models.Mock{calledAt("mock-self-boot", 500, "localhost:"+port), httpAt("mock-a", 1010), calledAt("mock-in-test", 1020, inTest)}
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
				wantSaid := 2
				if !own {
					wantSaid = 1
				}
				if c := logs.FilterMessageSnippet(ownListenerNote).All(); len(c) != 1 || c[0].ContextMap()["count"] != int64(wantSaid) {
					t.Fatalf("the replay's note: %d line(s) %v; want one, count %d", len(c), c, wantSaid)
				}
				if !own {
					// With the switch off, --assert-dependencies fails the
					// test without naming the call under UnconsumedMocks.
					named := !tc.on || (len(got.FailureInfo.UnconsumedMocks) == 1 && got.FailureInfo.UnconsumedMocks[0].Name == "mock-in-test")
					if set == models.TestSetStatusPassed || got.Status == models.TestStatusPassed || !named {
						t.Fatalf("set %s, test-1 %s, unconsumed %+v; want test-1 failed for mock-in-test", set, got.Status, got.FailureInfo.UnconsumedMocks)
					}
					return
				}
				if set != models.TestSetStatusPassed {
					t.Fatalf("set %s, want PASSED (worker-scope call named %d time(s))", set, workerNamed(logs))
				}
				if got.Status != models.TestStatusPassed || got.FailureInfo.UnconsumedMocks != nil {
					t.Fatalf("test-1: %s, unconsumed %+v; want PASSED, none", got.Status, got.FailureInfo.UnconsumedMocks)
				}
				if n := workerNamed(logs); n != 0 {
					t.Fatalf("the set's end named a worker-scope call never made %d time(s)", n)
				}
				// The report's expected side, where a mapping names the
				// self-call, lists the call made and not that one.
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
