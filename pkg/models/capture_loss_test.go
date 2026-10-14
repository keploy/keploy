package models

import (
	"strings"
	"testing"
)

func TestCaptureLossesSinceIsWhatGrew(t *testing.T) {
	base := CaptureLosses{{Kind: CaptureLossPIDMap, Count: 4}}
	if got := (CaptureLosses{{Kind: CaptureLossPIDMap, Count: 4}}).Since(base); len(got) != 0 {
		t.Fatalf("nothing grew: %+v", got)
	}
	got := (CaptureLosses{{Kind: CaptureLossPIDMap, Count: 7, What: "x"}}).Since(base)
	if len(got) != 1 || got[0].Count != 3 || got[0].What != "x" {
		t.Fatalf("grew by 3: %+v", got)
	}
	// A kind base never saw is taken whole; so is a count below base's (an
	// agent restarted under the run).
	if got := (CaptureLosses{{Kind: "other", Count: 2}}).Since(base); len(got) != 1 || got[0].Count != 2 {
		t.Fatalf("a new kind: %+v", got)
	}
	if got := (CaptureLosses{{Kind: CaptureLossPIDMap, Count: 1}}).Since(base); len(got) != 1 || got[0].Count != 1 {
		t.Fatalf("a restarted agent's count: %+v", got)
	}
	if got := CaptureLosses(nil).Since(base); len(got) != 0 {
		t.Fatalf("no counts: %+v", got)
	}
}

func TestCaptureLossesTextSaysWhatWasNotCaptured(t *testing.T) {
	if CaptureLosses(nil).Text() != "" {
		t.Fatal("no losses, no text")
	}
	s := (CaptureLosses{{Kind: CaptureLossPIDMap, Count: 2, What: "target_namespace_pids"}}).Text()
	for _, want := range []string{"capture incomplete: ", "refused 2 insert(s)", "(target_namespace_pids)"} {
		if !strings.Contains(s, want) {
			t.Fatalf("%q lacks %q", s, want)
		}
	}
}

func TestScopeEvidenceWithACaptureLossIsNotEmpty(t *testing.T) {
	if !(&ScopeEvidence{}).Empty() {
		t.Fatal("empty evidence")
	}
	if (&ScopeEvidence{CaptureIncomplete: CaptureLosses{{Kind: CaptureLossPIDMap, Count: 1}}}).Empty() {
		t.Fatal("a capture loss is evidence")
	}
}
