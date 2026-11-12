package variables

import "testing"

// A scope begun naming the run's variables its test receives only in answers,
// from earlier tests this run did not make first, answers each as recorded
// while it is not bound: once bound (an earlier test of the run made it after
// all), its bound value is answered. A name the scope was not begun with is
// not answered so, and the next scope, however it begins, answers none so. A
// clone answers what its table does.
func TestAScopeAnswersAsRecordedOnlyTheVariablesItBeganWithWhileUnbound(t *testing.T) {
	const made, other, also = "run-appId0-13", "run-appId0-7", "run-appId0-9"
	var tb Table
	tb.KeepRun()
	tb.StartOpeningScope("test-ci", func(uint64) {}, nil, nil, nil, []string{made, also})
	for _, name := range []string{made, also} {
		if !tb.AnsweredAsRecorded(name) {
			t.Fatalf("the scope does not answer %s as recorded", name)
		}
	}
	if tb.AnsweredAsRecorded(other) {
		t.Fatalf("the scope answers %s as recorded, which it was not begun with", other)
	}
	if !tb.Clone().AnsweredAsRecorded(made) {
		t.Fatal("a clone of the scope does not answer as recorded what the scope does")
	}
	if _, ok := tb.Bind(Binding{Name: made, Value: "app-live", Recorded: "app-rec"}); !ok {
		t.Fatalf("%s did not bind", made)
	}
	if tb.AnsweredAsRecorded(made) {
		t.Fatalf("%s is answered as recorded once bound", made)
	}

	begins := []struct {
		name  string
		begin func()
	}{
		{"StartOpeningScope with none", func() { tb.StartOpeningScope("test-next", func(uint64) {}, nil, nil, nil, nil) }},
		{"StartOpeningTest", func() { tb.StartOpeningTest("test-next", func() {}, nil, nil, nil) }},
		{"Start", func() { tb.Start(nil) }},
		{"Reset", tb.Reset},
		{"NewSet", tb.NewSet},
	}
	for _, b := range begins {
		tb.StartOpeningScope("test-ci", func(uint64) {}, nil, nil, nil, []string{also})
		if !tb.AnsweredAsRecorded(also) {
			t.Fatalf("%s: the scope does not answer %s as recorded", b.name, also)
		}
		b.begin()
		if tb.AnsweredAsRecorded(also) {
			t.Fatalf("%s: the next scope answers %s as recorded", b.name, also)
		}
	}
}
