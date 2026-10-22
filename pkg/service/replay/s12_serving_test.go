package replay

import (
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

func mustPanicWith(t *testing.T, want string, f func()) {
	t.Helper()
	defer func() {
		t.Helper()
		r := recover()
		if r == nil {
			t.Fatalf("no panic; want one saying %q", want)
		}
		if s, _ := r.(string); !strings.Contains(s, want) {
			t.Fatalf("panic %v; want one saying %q", r, want)
		}
	}()
	f()
}

// S12's serving enablement is one registration: a second panics, as does a
// nil func; S12Serving is what the registered func returns, false with none;
// and unregister removes only its own registration.
func TestRegisterS12ServingIsOneRegistration(t *testing.T) {
	if S12Serving() {
		t.Fatal("S12 serving is on before anything registers")
	}
	on := false
	unregister := RegisterS12Serving(func() bool { return on })
	if S12Serving() {
		t.Fatal("S12 serving is on while the registered func returns false")
	}
	on = true
	if !S12Serving() {
		t.Fatal("S12 serving is off while the registered func returns true")
	}
	mustPanicWith(t, "registered already", func() { RegisterS12Serving(func() bool { return true }) })
	mustPanicWith(t, "nil", func() { RegisterS12Serving(nil) })
	unregister()
	if S12Serving() {
		t.Fatal("unregister left S12 serving on")
	}
	again := RegisterS12Serving(func() bool { return true })
	unregister() // a stale unregister
	if !S12Serving() {
		t.Fatal("a stale unregister removed the next registration")
	}
	again()
	if S12Serving() {
		t.Fatal("the second unregister left S12 serving on")
	}
}

// A drift move given back to its owner says why its owner runs: under a build
// that judges (S12Serving registered), its test was edited or verdicts.yaml
// lost its entry; under one that reads no verdicts.yaml, it runs every test.
func TestAReturnedMoveSaysWhyItsOwnerRuns(t *testing.T) {
	for _, judged := range []bool{false, true} {
		if judged {
			t.Cleanup(RegisterS12Serving(func() bool { return true }))
		}
		core, logs := observer.New(zapcore.WarnLevel)
		r := &Replayer{logger: zap.New(core)}
		r.logReturnedMoves("test-set-0", map[string]string{"mock-6": "post-url-1"})
		all := logs.All()
		if len(all) != 1 {
			t.Fatalf("judged=%v: %d lines; want one", judged, len(all))
		}
		why, _ := all[0].ContextMap()["why"].(string)
		next, _ := all[0].ContextMap()["next_step"].(string)
		want, not := "was edited since the judging", "reads no verdicts.yaml"
		if !judged {
			want, not = not, want
		}
		if !strings.Contains(why, want) || strings.Contains(why, not) || next == "" {
			t.Fatalf("judged=%v: why %q, next_step %q; want %q in why, and a next step", judged, why, next, want)
		}
	}
}
