package replay

import (
	"strings"
	"testing"
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
