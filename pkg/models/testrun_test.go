package models

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"gopkg.in/yaml.v3"
)

// What keploy cannot capture at all is written as cannot_capture in a test's
// failure info, in YAML and JSON: not_judged would read as the test's whole
// not-judged state (NotJudged), which covers more than this one reason.
func TestCannotCaptureIsWrittenAsCannotCapture(t *testing.T) {
	fi := FailureInfo{CannotCapture: CaptureLosses{{Kind: CaptureLossUnixSocket, Count: 1, What: "/run/app/db.sock"}}}
	y, err := yaml.Marshal(fi)
	if err != nil {
		t.Fatal(err)
	}
	j, err := json.Marshal(fi)
	if err != nil {
		t.Fatal(err)
	}
	for format, b := range map[string]string{"yaml": string(y), "json": string(j)} {
		if !strings.Contains(b, "cannot_capture") || strings.Contains(b, "not_judged") {
			t.Fatalf("%s: %s, want the losses under cannot_capture", format, b)
		}
	}
	var back FailureInfo
	if err := yaml.Unmarshal(y, &back); err != nil || len(back.CannotCapture) != 1 || back.CannotCapture[0].What != "/run/app/db.sock" {
		t.Fatalf("read back: %+v, %v", back, err)
	}
}

// MappingStale is a demoted wrong answer's reason for having no verdict: it
// counts on an OBSOLETE row only. A row a before-test-result hook relabelled
// since (PASSED, FAILED) says its own verdict, and nothing marks it not judged.
func TestMappingStaleCountsOnAnObsoleteRowOnly(t *testing.T) {
	stale := FailureInfo{MappingStale: "not judged: stale mapping"}
	if why := (TestResult{Status: TestStatusObsolete, FailureInfo: stale}).VerdictUnknown(); len(why) != 1 {
		t.Fatalf("an OBSOLETE row with MappingStale: VerdictUnknown %v, want its reason", why)
	}
	for _, st := range []TestStatus{TestStatusPassed, TestStatusFailed} {
		if why := (TestResult{Status: st, FailureInfo: stale}).VerdictUnknown(); len(why) != 0 {
			t.Fatalf("a %s row left with MappingStale: VerdictUnknown %v, want none", st, why)
		}
	}
	unread := FailureInfo{MappingStale: "not judged: stale mapping", TestMocksUnread: true}
	if why := (TestResult{Status: TestStatusPassed, FailureInfo: unread}).VerdictUnknown(); len(why) != 1 {
		t.Fatalf("a PASSED row with another reason too: VerdictUnknown %v, want that reason only", why)
	}
}

// A ConcurrentNote's Clone shares no list with it: the replay fills a copy
// of a parser's note, and the parser's is left as it was.
func TestConcurrentNoteCloneSharesNothing(t *testing.T) {
	n := &ConcurrentNote{With: []string{"a"}, MayBe: []string{"b"}, Class: []string{"c"}, Places: []string{"bind.$1"}, Unkept: 1, Case: ConcurrentCaseNotOwn}
	c := n.Clone()
	if !reflect.DeepEqual(c, n) {
		t.Fatalf("clone %+v of %+v", c, n)
	}
	c.With[0], c.MayBe[0], c.Class[0], c.Places[0] = "x", "x", "x", "x"
	if n.With[0] != "a" || n.MayBe[0] != "b" || n.Class[0] != "c" || n.Places[0] != "bind.$1" {
		t.Fatalf("the clone shares a list: %+v", n)
	}
	if (*ConcurrentNote)(nil).Clone() != nil {
		t.Fatal("a nil note clones to one")
	}
}

// A test whose learning record's full round did not keep (its agent did not
// say it read the test case's request) has no verdict, saying why, unless it
// failed: what was not kept can only add to why it failed.
func TestALearningNotKeptLeavesATestWithNoVerdict(t *testing.T) {
	why := "keploy did not keep what its agent learned on the mocks this test consumed (mock-1 body.at (noise))"
	fi := FailureInfo{LearningNotKept: why}
	if got := fi.NotJudged(); len(got) != 1 || got[0] != why {
		t.Fatalf("NotJudged %v; want the reason", got)
	}
	if got := (TestResult{Status: TestStatusPassed, FailureInfo: fi}).VerdictUnknown(); len(got) != 1 || got[0] != why {
		t.Fatalf("a passed row: VerdictUnknown %v; want the reason", got)
	}
	if got := (TestResult{Status: TestStatusFailed, FailureInfo: fi}).VerdictUnknown(); got != nil {
		t.Fatalf("a failed row: VerdictUnknown %v; want none", got)
	}
	if got := (FailureInfo{}).NotJudged(); got != nil {
		t.Fatalf("no reason: NotJudged %v", got)
	}
}
