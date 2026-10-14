package mock

import (
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
	if got := testAt(tests, 10, at); got != "login" {
		t.Fatalf("testAt = %q", got)
	}
	tests = append(tests, models.ScopeWindow{Name: "other", Start: at.Add(-time.Second), End: at.Add(time.Second), PID: 31})
	if got := testAt(tests, 10, at); got != "" {
		t.Fatalf("two workers with open tests must be ambiguous, got %q", got)
	}
}
