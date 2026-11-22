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

// A trailed test's scope answers as recorded what it began naming so, read
// by test (AnsweredAsRecordedOf): while it runs, and once a later test's
// scope began, from the latest of its ended scopes, whatever the running
// scope began naming; never once the name is bound, for a test whose scope
// has not begun, for no test named, or on a table that keeps no ended
// scopes.
func TestATestsScopeAnswersAsRecordedWhatItBeganWithAfterItEnded(t *testing.T) {
	const made, other = "run-appId0-13", "run-appId0-7"
	var tb Table
	tb.KeepEnded()
	tb.KeepRun()
	if tb.AnsweredAsRecordedOf("test-y", made) {
		t.Fatal("a test whose scope has not begun answers as recorded")
	}
	tb.StartOpeningScope("test-y", func(uint64) {}, nil, nil, nil, []string{made})
	if !tb.AnsweredAsRecordedOf("test-y", made) {
		t.Fatalf("the running scope of test-y does not answer %s as recorded", made)
	}
	if tb.AnsweredAsRecordedOf("", made) || tb.AnsweredAsRecordedOf("test-y", other) {
		t.Fatal("no test named, or a name test-y did not begin with, is answered as recorded")
	}
	tb.StartOpeningScope("test-z", func(uint64) {}, nil, nil, nil, []string{other})
	if !tb.AnsweredAsRecordedOf("test-y", made) {
		t.Fatalf("test-y's ended scope does not answer %s as recorded in test-z's window", made)
	}
	if tb.AnsweredAsRecordedOf("test-y", other) || tb.AnsweredAsRecorded(made) {
		t.Fatal("test-y's ended scope reads test-z's names, or test-z reads test-y's")
	}
	if tb.AnsweredAsRecordedOf("test-z", made) || !tb.AnsweredAsRecordedOf("test-z", other) {
		t.Fatal("test-z's running scope does not answer as recorded what it began with, alone")
	}
	// A third test's window: test-y's ended scope is read, not test-z's,
	// the latest ended one.
	tb.StartOpeningScope("test-w", func(uint64) {}, nil, nil, nil, nil)
	if !tb.AnsweredAsRecordedOf("test-y", made) || tb.AnsweredAsRecordedOf("test-y", other) {
		t.Fatal("test-y's ended scope is not read by its test beside test-z's later one")
	}
	// A scope begun with no test named answers its names as recorded, and
	// is no test's: by test it answers none.
	tb.StartOpeningScope("", func(uint64) {}, nil, nil, nil, []string{other})
	if !tb.AnsweredAsRecorded(other) || tb.AnsweredAsRecordedOf("", other) {
		t.Fatal("a scope begun with no test named is read as a test's")
	}
	// test-y runs again with none named: its latest scope is read.
	tb.StartOpeningScope("test-y", func(uint64) {}, nil, nil, nil, nil)
	tb.StartOpeningScope("test-z", func(uint64) {}, nil, nil, nil, nil)
	if tb.AnsweredAsRecordedOf("test-y", made) {
		t.Fatalf("test-y's latest ended scope, begun with none, answers %s as recorded", made)
	}
	tb.StartOpeningScope("test-y", func(uint64) {}, nil, nil, nil, []string{made})
	tb.Start(nil)
	if !tb.AnsweredAsRecordedOf("test-y", made) {
		t.Fatalf("test-y's ended scope does not answer %s as recorded beside a scope that names no test", made)
	}
	if _, ok := tb.Bind(Binding{Name: made, Value: "app-live", Recorded: "app-rec"}); !ok {
		t.Fatalf("%s did not bind", made)
	}
	if tb.AnsweredAsRecordedOf("test-y", made) {
		t.Fatalf("%s is answered as recorded once bound", made)
	}

	var unkept Table
	unkept.KeepRun()
	unkept.StartOpeningScope("test-y", func(uint64) {}, nil, nil, nil, []string{made})
	if !unkept.AnsweredAsRecordedOf("test-y", made) {
		t.Fatal("the running scope of a table that keeps no ended scopes does not answer as recorded")
	}
	unkept.StartOpeningScope("test-z", func(uint64) {}, nil, nil, nil, nil)
	if unkept.AnsweredAsRecordedOf("test-y", made) {
		t.Fatal("a table that keeps no ended scopes answers as recorded for an ended scope")
	}
	tb.NewSet()
	if tb.AnsweredAsRecordedOf("test-y", made) {
		t.Fatal("a new set answers as recorded for the ended set's scopes")
	}
}
