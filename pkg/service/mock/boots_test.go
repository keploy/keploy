package mock

import (
	"fmt"
	"testing"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

func TestClassifyPutsEachStartupCallWithItsOwnStart(t *testing.T) {
	ms := func(n int) time.Time {
		return time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Millisecond)
	}
	suites := []models.SuiteSpan{
		{Dir: "/r/e2e/orders", Start: ms(0), End: ms(500)},
		{Dir: "/r/e2e/payments", Start: ms(600), End: ms(900)},
	}
	tests := []models.ScopeWindow{
		{Name: "orders.TestCreate", Start: ms(100), End: ms(200), PID: 10},
		{Name: "orders.TestReload", Start: ms(250), End: ms(450), PID: 10},
		{Name: "payments.TestPay", Start: ms(700), End: ms(800), PID: 20},
	}
	starts := []models.ScopeWindow{
		{App: true, Ref: "s1", Name: "shop#1", Dir: "/r/e2e/orders", Program: "shop", N: 1, Ready: ms(60), Worker: 10},
		{App: true, Ref: "s2", Name: "shop#1@orders.TestReload", Dir: "/r/e2e/orders", Program: "shop", Place: "orders.TestReload", N: 1, Ready: ms(320), Worker: 10},
		{App: true, Ref: "s3", Name: "shop#1", Dir: "/r/e2e/payments", Program: "shop", N: 1, Ready: ms(660), Worker: 20},
		{App: true, Ref: "s4", Name: "shop#2@orders.TestReload", Dir: "/r/e2e/orders", Program: "shop", Place: "orders.TestReload", N: 2, Worker: 10},
	}
	mocks := []capturedMock{
		{name: "config-inr", ts: ms(50), ref: "s1"},
		{name: "book", ts: ms(150), ref: "s1"},
		{name: "pen-old", ts: ms(260), ref: "s1"},
		{name: "config-usd", ts: ms(310), ref: "s2"},
		{name: "pen-new", ts: ms(330), ref: "s2"},
		{name: "pg-handshake", ts: ms(340), ref: "s2", boot: true},
		{name: "between", ts: ms(220), ref: "s1"},
		{name: "config-eur", ts: ms(650), ref: "s3"},
		{name: "worker-own", ts: ms(710), pid: 20},
		{name: "go-build", ts: ms(20), pid: 99},
	}
	p := classify(tests, starts, suites, mocks)
	boot := map[string][]string{}
	for _, b := range p.boots {
		var names []string
		for _, m := range b.Mocks {
			names = append(names, m.Name)
		}
		boot[b.Dir+"|"+b.Key] = names
	}
	expect := map[string][]string{
		"/r/e2e/orders|shop#1":                   {"config-inr", "between"},
		"/r/e2e/orders|shop#1@orders.TestReload": {"config-usd", "pg-handshake"},
		"/r/e2e/payments|shop#1":                 {"config-eur"},
		"/r/e2e/orders|shop#2@orders.TestReload": nil,
		"/r/e2e/orders|":                         {"go-build"},
	}
	for k, want := range expect {
		got, ok := boot[k]
		if !ok || len(got) != len(want) {
			t.Fatalf("boot %s = %v (present %v), want %v; all %v", k, got, ok, want, boot)
		}
		for i := range want {
			if got[i] != want[i] {
				t.Fatalf("boot %s = %v, want %v", k, got, want)
			}
		}
	}
	owner := map[string]string{}
	for test, entries := range p.tests {
		for _, e := range entries {
			owner[test+"|"+e.Name] = e.Start
		}
	}
	want := map[string]string{
		"orders.TestCreate|book":      "shop#1",
		"orders.TestReload|pen-old":   "shop#1",
		"orders.TestReload|pen-new":   "shop#1@orders.TestReload",
		"payments.TestPay|worker-own": "",
	}
	if len(owner) != len(want) {
		t.Fatalf("per-test = %v", owner)
	}
	for k, v := range want {
		if got, ok := owner[k]; !ok || got != v {
			t.Fatalf("per-test %s = %q (present %v), want %q", k, got, ok, v)
		}
	}
}

func TestTestAtUsesTheOnlyOpenTestWhenTheCallerHasNone(t *testing.T) {
	at := time.Date(2026, 10, 5, 10, 0, 1, 0, time.UTC)
	tests := []models.ScopeWindow{{Name: "login", Start: at.Add(-time.Second), End: at.Add(time.Second), PID: 30}}
	if got := testAt(tests, 10, 0, at); got != "login" {
		t.Fatalf("testAt = %q", got)
	}
	tests = append(tests, models.ScopeWindow{Name: "other", Start: at.Add(-time.Second), End: at.Add(time.Second), PID: 31})
	if got := testAt(tests, 10, 0, at); got != "" {
		t.Fatalf("two workers with open tests must be ambiguous, got %q", got)
	}
}

// With app starts too, an idle answer (capturedMock.idle: a worker's pop the
// server never answered) is no test's: recorded inside a test's window, it
// goes where a call made outside every window does, to its app start's
// mocks, or to its runner's for a call of no start. A call that is not idle
// stays the test's. So with the calls and the test stamped with the keploy
// instance the runner claimed, as a claimed run records them.
func TestClassifyPutsAnIdleAnswerWhereNoTestOwnsIt(t *testing.T) {
	ms := func(n int) time.Time {
		return time.Date(2026, 10, 5, 10, 0, 0, 0, time.UTC).Add(time.Duration(n) * time.Millisecond)
	}
	names := func(es []models.MockEntry) []string {
		var out []string
		for _, e := range es {
			out = append(out, e.Name)
		}
		return out
	}
	for _, instance := range []uint32{0, 3} {
		t.Run(fmt.Sprintf("instance %d", instance), func(t *testing.T) {
			suites := []models.SuiteSpan{{Dir: "/r", Start: ms(0), End: ms(500)}}
			tests := []models.ScopeWindow{{Name: "TestJob", Start: ms(100), End: ms(200), PID: 10, Instance: instance}}
			starts := []models.ScopeWindow{{App: true, Ref: "s1", Name: "worker#1", Dir: "/r", Program: "worker", N: 1, Ready: ms(50), Worker: 10}}
			mocks := []capturedMock{
				{name: "push", ts: ms(150), pid: 10, instance: instance},
				{name: "app-get", ts: ms(160), ref: "s1", instance: instance},
				{name: "app-pop", ts: ms(190), ref: "s1", instance: instance, idle: true},
				{name: "runner-pop", ts: ms(195), pid: 10, instance: instance, idle: true},
			}
			p := classify(tests, starts, suites, mocks)
			if got := names(p.tests["TestJob"]); len(got) != 2 || got[0] != "push" || got[1] != "app-get" {
				t.Fatalf("TestJob's mocks = %v, want [push app-get]", got)
			}
			boot := map[string][]string{}
			for _, b := range p.boots {
				boot[b.Dir+"|"+b.Key] = names(b.Mocks)
			}
			if got := boot["/r|worker#1"]; len(got) != 1 || got[0] != "app-pop" {
				t.Fatalf("the app start's mocks = %v, want [app-pop]; all %v", got, boot)
			}
			if got := boot["/r|"]; len(got) != 1 || got[0] != "runner-pop" {
				t.Fatalf("the runner's mocks = %v, want [runner-pop]; all %v", got, boot)
			}
		})
	}
}
