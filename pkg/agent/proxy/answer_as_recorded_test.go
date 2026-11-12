package proxy

import (
	"testing"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
)

// The variables the agent hands for a test to answer as recorded, before the
// test's view opens, are its scope's from its first call: another test's
// opening drops them, so its scope answers none so, and the test's next
// opening, with none handed again, answers none so either. A client test's
// cases, given after them, leave them as they are. A hand of none drops what
// was handed before.
func TestVariablesAnsweredAsRecordedAreLeftForTheirOwnTestsScopeOnly(t *testing.T) {
	t.Setenv(provenswitch.Env, "1")
	const slow = "run-appId0-13"
	p, mm, _, open := caseBindsProxy(t)
	answers := func() bool { return mm.vars.AnsweredAsRecorded(slow) }

	if err := p.AnswerAsRecordedAtOpen("test-1", []string{slow}); err != nil {
		t.Fatal(err)
	}
	p.TakeClientCases("test-1", []models.ClientCase{appsCase("post-apps-1", keyAtName)})
	open()
	if !answers() {
		t.Fatalf("test-1's scope does not answer %s as recorded", slow)
	}
	if mm.vars.Unbound("key") == "" {
		t.Fatal("the cases' note was lost beside the answers")
	}
	mm.ServeExactPool(models.PoolKeyNone, nil)
	open()
	if answers() {
		t.Fatal("test-1's next opening, with none handed again, answers as recorded")
	}

	mm.ServeExactPool(models.PoolKeyNone, nil)
	if err := p.AnswerAsRecordedAtOpen("test-1", []string{slow}); err != nil {
		t.Fatal(err)
	}
	mm.OpenExactTest("test-2", nil, nil)
	if answers() {
		t.Fatal("another test's scope answers test-1's variables as recorded")
	}
	mm.ServeExactPool(models.PoolKeyNone, nil)
	open()
	if answers() {
		t.Fatal("test-1's scope, opened after another's took them, answers as recorded")
	}

	mm.ServeExactPool(models.PoolKeyNone, nil)
	if err := p.AnswerAsRecordedAtOpen("test-1", []string{slow}); err != nil {
		t.Fatal(err)
	}
	if err := p.AnswerAsRecordedAtOpen("test-1", nil); err != nil {
		t.Fatal(err)
	}
	open()
	if answers() {
		t.Fatal("a hand of none left what was handed before")
	}

	if err := (&Proxy{}).AnswerAsRecordedAtOpen("test-1", []string{slow}); err == nil {
		t.Fatal("a proxy with no mock manager took the variables")
	}
}
