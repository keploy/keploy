package models

import "testing"

// Every registry a read of a mock file derives its mocks through, and the
// declared session calls, start a new generation when they change, and again
// when a registration is removed: a read kept for reuse is then not reused.
func TestEveryRuleChangeStartsANewGeneration(t *testing.T) {
	const kind Kind = "Rules-Gen-Test"
	pred := func(*Mock) bool { return false }
	named := func(*Mock) (string, bool) { return "", false }
	for _, c := range []struct {
		name     string
		register func() (undo func())
	}{
		{"RegisterCarryOver", func() func() { return RegisterCarryOver(kind, pred) }},
		{"RegisterIdleAnswer", func() func() { return RegisterIdleAnswer(kind, pred) }},
		{"RegisterAnswerPlaced", func() func() { return RegisterAnswerPlaced(kind, pred) }},
		{"RegisterStagingNeed", func() func() {
			return RegisterStagingNeed(kind, StagingNeed{Use: StagingWithStartup, Match: pred})
		}},
		{"RegisterSessionReusable", func() func() {
			RegisterSessionReusable(kind, pred)
			// It has no removal of its own (it registers at init): this
			// test's goes through the registry's writer, as every removal does.
			return func() {
				sessionReusableHooks.mu.Lock()
				defer sessionReusableHooks.mu.Unlock()
				sessionReusableHooks.storeLocked(func(next map[Kind][]mockPredicate) { delete(next, kind) })
			}
		}},
		{"RegisterWriteKind", func() func() { return RegisterWriteKind(kind, named) }},
		{"RegisterExactKey", func() func() { return RegisterExactKey(kind, named) }},
		{"RegisterRequestKeys", func() func() { return RegisterRequestKeys(kind, func(*Mock) []string { return nil }) }},
		{"RegisterConcurrentKeys", func() func() { return RegisterConcurrentKeys(kind, ConcurrentKeys{}) }},
		{"RegisterMockDeps", func() func() {
			prev := RegisterMockDeps(kind, func(*Mock) MockDeps { return MockDeps{} })
			return func() { RegisterMockDeps(kind, prev) }
		}},
		{"SetSessionCalls", func() func() {
			SetSessionCalls([]SessionCallRule{{Host: "rules-gen.test"}})
			return func() { SetSessionCalls(nil) }
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			before := MockRulesGeneration()
			undo := c.register()
			registered := MockRulesGeneration()
			if registered == before {
				t.Fatalf("%s left the generation at %d", c.name, before)
			}
			undo()
			if MockRulesGeneration() == registered {
				t.Fatalf("undoing %s left the generation at %d", c.name, registered)
			}
		})
	}
}
