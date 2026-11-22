package models

import "testing"

// A parser's idle answers are its own to declare (RegisterIdleAnswer): a mock
// of its kind its predicate accepts is idle, and nothing else is, whatever it
// looks like; unregistering leaves the registry as it was.
func TestAnIdleAnswerIsOnlyWhatItsParserDeclares(t *testing.T) {
	empty := &Mock{Name: "empty-poll", Kind: HTTP}
	full := &Mock{Name: "full-poll", Kind: HTTP}
	other := &Mock{Name: "empty-poll", Kind: Postgres}
	if IsIdleAnswer(empty) {
		t.Fatal("an answer was idle with nothing declared")
	}
	unregister := RegisterIdleAnswer(HTTP, func(m *Mock) bool { return m.Name == "empty-poll" })
	if !IsIdleAnswer(empty) || IsIdleAnswer(full) || IsIdleAnswer(other) || IsIdleAnswer(nil) {
		t.Fatal("IsIdleAnswer read another kind's, another answer's, or a nil mock's as idle, or missed the declared one")
	}
	unregister()
	if IsIdleAnswer(empty) {
		t.Fatal("an answer stayed idle once its declaration was removed")
	}
}
