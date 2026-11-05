package models

import (
	"testing"
	"time"
)

// Removing one of two releases registered for a kind keeps the other in
// force, and removing the last leaves the kind with none.
func TestRegisterAnswerReleaseRemovesOnlyItsOwn(t *testing.T) {
	const kind Kind = "answer-release-test"
	first := RegisterAnswerRelease(kind, func(RunPlace, time.Time) bool { return true })
	second := RegisterAnswerRelease(kind, func(RunPlace, time.Time) bool { return false })
	first()
	release, ok := AnswerReleaseOf(kind)
	if !ok {
		t.Fatal("removing the first release removed the second")
	}
	if release(RunPlace{}, time.Time{}) {
		t.Fatal("the release in force is not the one left")
	}
	second()
	if _, ok := AnswerReleaseOf(kind); ok {
		t.Fatal("a kind whose releases were all removed still has one")
	}
}

// A second release registered for one kind keeps every other kind's.
func TestRegisterAnswerReleaseKeepsOtherKinds(t *testing.T) {
	const kind, other Kind = "answer-release-test", "answer-release-other"
	t.Cleanup(RegisterAnswerRelease(other, func(RunPlace, time.Time) bool { return true }))
	t.Cleanup(RegisterAnswerRelease(kind, func(RunPlace, time.Time) bool { return true }))
	t.Cleanup(RegisterAnswerRelease(kind, func(RunPlace, time.Time) bool { return false }))
	if _, ok := AnswerReleaseOf(other); !ok {
		t.Fatal("a second release for one kind removed another kind's")
	}
}
