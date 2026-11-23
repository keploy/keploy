package tls

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// certutilAdds is what a row's certutil answers: the user store's add and the
// machine store's, each its output and error; calls is every add asked, true
// for the user store.
type certutilAdds struct {
	userOut, machineOut []byte
	userErr, machineErr error
	calls               []bool
}

func (c *certutilAdds) add(_ context.Context, user bool, _ string) ([]byte, error) {
	c.calls = append(c.calls, user)
	if user {
		return c.userOut, c.userErr
	}
	return c.machineOut, c.machineErr
}

// swapWindowsStore has installWindowsCA read the user's store through holds
// and find the machine's empty, run certutil through c and see the token as
// elevated, for the row; swapMachineStore then changes the machine's.
func swapWindowsStore(t *testing.T, holds func([]byte) (bool, error), c *certutilAdds, elevated bool) {
	t.Helper()
	h, m, a, e := userRootHoldsFn, machineRootHoldsFn, certutilAddFn, tokenElevatedFn
	t.Cleanup(func() {
		userRootHoldsFn, machineRootHoldsFn, certutilAddFn, tokenElevatedFn = h, m, a, e
		noteUntrustedStore("")
	})
	userRootHoldsFn, machineRootHoldsFn, certutilAddFn = holds, notHeld, c.add
	tokenElevatedFn = func() bool { return elevated }
}

// swapMachineStore has installWindowsCA read the machine's store through
// holds; swapWindowsStore restores it.
func swapMachineStore(holds func([]byte) (bool, error)) { machineRootHoldsFn = holds }

func notHeld([]byte) (bool, error) { return false, nil }

// exitErr is a real *exec.ExitError whose code is not certutil's: the test
// binary's own, refusing a flag it does not know.
func exitErr(t *testing.T) error {
	t.Helper()
	err := exec.Command(os.Args[0], "-test.no-such-flag").Run()
	var e *exec.ExitError
	if !errors.As(err, &e) || uint32(e.ExitCode()) == hrNotSupported {
		t.Fatalf("want an exit error with another code, got %v", err)
	}
	return err
}

const (
	notSupportedOut = "CertUtil: -addstore command FAILED: 0x80070032 (WIN32: 50 ERROR_NOT_SUPPORTED)\r\nCertUtil: The request is not supported.\r\n"
	accessDeniedOut = "CertUtil: -addstore command FAILED: 0x80070005 (WIN32: 5 ERROR_ACCESS_DENIED)\r\nCertUtil: Access is denied.\r\n"
	keptCA          = `C:\Users\runner\AppData\Local\Keploy\ca\ca.crt`
)

func TestAStandardUserWhoseSessionCannotShowTheTrustPromptNeverAsksTheMachineStore(t *testing.T) {
	c := &certutilAdds{userOut: []byte(notSupportedOut), userErr: exitErr(t)}
	swapWindowsStore(t, notHeld, c, false)
	err := installWindowsCA(context.Background(), zap.NewNop(), "x.crt", []byte("der"), keptCA)
	if !errors.Is(err, errNoTrustPrompt) {
		t.Fatalf("installWindowsCA = %v, want errNoTrustPrompt", err)
	}
	if !reflect.DeepEqual(c.calls, []bool{true}) {
		t.Fatalf("certutil adds asked = %v (true: the user store), want the user store's only", c.calls)
	}
}

func TestAStandardUsersOtherUserStoreFailureIsReturnedWithNoMachineStoreAndNoElevation(t *testing.T) {
	c := &certutilAdds{userOut: []byte(accessDeniedOut), userErr: exitErr(t)}
	swapWindowsStore(t, notHeld, c, false)
	core, logs := observer.New(zapcore.DebugLevel)
	err := installWindowsCA(context.Background(), zap.New(core), "x.crt", []byte("der"), keptCA)
	if err != c.userErr {
		t.Fatalf("installWindowsCA = %v, want the user store's error %v", err, c.userErr)
	}
	if !reflect.DeepEqual(c.calls, []bool{true}) {
		t.Fatalf("certutil adds asked = %v, want the user store's only", c.calls)
	}
	var step string
	for _, e := range logs.All() {
		if s, ok := e.ContextMap()["next_step"].(string); ok {
			step = s
		}
	}
	if !strings.Contains(step, keptCA) || strings.Contains(strings.ToLower(step), "elevated") {
		t.Fatalf("next_step = %q, want the kept CA's step and no elevation", step)
	}
}

func TestAnElevatedRunThatCannotUseTheUserStoreFallsBackToTheMachineStore(t *testing.T) {
	for _, tc := range []struct {
		name       string
		machineErr error
	}{{"the machine store takes it", nil}, {"the machine store refuses it", errors.New("machine refused")}} {
		t.Run(tc.name, func(t *testing.T) {
			c := &certutilAdds{userOut: []byte(notSupportedOut), userErr: exitErr(t), machineErr: tc.machineErr}
			swapWindowsStore(t, notHeld, c, true)
			core, logs := observer.New(zapcore.InfoLevel)
			err := installWindowsCA(context.Background(), zap.New(core), "x.crt", []byte("der"), keptCA)
			if err != tc.machineErr {
				t.Fatalf("installWindowsCA = %v, want the machine store's answer %v", err, tc.machineErr)
			}
			if !reflect.DeepEqual(c.calls, []bool{true, false}) {
				t.Fatalf("certutil adds asked = %v, want the user store's, then the machine store's", c.calls)
			}
			// A CA the machine store took is said in one INFO line naming
			// that store; one it refused is not said to be there.
			var named []observer.LoggedEntry
			for _, e := range logs.All() {
				if strings.Contains(e.Message, `LocalMachine\Root`) {
					named = append(named, e)
				}
			}
			want := 0
			if tc.machineErr == nil {
				want = 1
			}
			if len(named) != want || (want == 1 && named[0].Level != zapcore.InfoLevel) {
				t.Fatalf("lines naming the machine store = %v, want %d at INFO", named, want)
			}
		})
	}
}

// A CA either Windows ROOT store holds, the user's or the machine's (where an
// administrator or a company's group policy puts it for every user), is
// trusted: for a standard user in a session with no desktop, whose
// certutil add would fail for want of the trust prompt, keploy's setup asks
// certutil nothing, warns of nothing and leaves no hint.
func TestACAThatEitherWindowsRootStoreHoldsIsReusedWithNoCertutilWarningOrHint(t *testing.T) {
	held := func([]byte) (bool, error) { return true, nil }
	unread := func([]byte) (bool, error) { return false, errors.New("store unread") }
	for _, tc := range []struct {
		name          string
		user, machine func([]byte) (bool, error)
	}{
		{"the user's store", held, notHeld},
		{"the machine's store, not the user's", notHeld, held},
		{"the machine's store, the user's unread", unread, held},
		{"both stores", held, held},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &certutilAdds{userOut: []byte(notSupportedOut), userErr: exitErr(t)}
			var askedUser, askedMachine []byte
			swapWindowsStore(t, func(der []byte) (bool, error) { askedUser = der; return tc.user(der) }, c, false)
			swapMachineStore(func(der []byte) (bool, error) { askedMachine = der; return tc.machine(der) })
			noteUntrustedStore("a hint from an earlier setup")
			core, logs := observer.New(zapcore.WarnLevel)
			if err := trustOnWindows(context.Background(), zap.New(core), "x.crt", []byte("the CA"), keptCA); err != nil {
				t.Fatalf("trustOnWindows = %v, want nil: a store holds the CA", err)
			}
			if len(c.calls) != 0 {
				t.Fatalf("certutil adds asked = %v, want none: a store holds the CA", c.calls)
			}
			if string(askedUser) != "the CA" || (askedMachine != nil && string(askedMachine) != "the CA") {
				t.Fatalf("stores asked for %q (user) and %q (machine), want the CA", askedUser, askedMachine)
			}
			if logs.Len() != 0 {
				t.Fatalf("warnings = %v, want none: a store holds the CA", logs.All())
			}
			if why := UntrustedStoreWhy(); why != "" {
				t.Fatalf("UntrustedStoreWhy = %q, want \"\": a store holds the CA", why)
			}
		})
	}
}

// A store that cannot be read counts as not holding the CA: unless the other
// holds it, the setup adds it to the user's store, as to an empty one.
func TestACAThatNoReadableWindowsRootStoreHoldsIsAddedToTheUsers(t *testing.T) {
	unread := func([]byte) (bool, error) { return true, errors.New("store unread") }
	for _, tc := range []struct {
		name          string
		user, machine func([]byte) (bool, error)
	}{
		{"neither store holds it", notHeld, notHeld},
		{"the user's unread, the machine's not holding it", unread, notHeld},
		{"the user's not holding it, the machine's unread", notHeld, unread},
		{"neither store read", unread, unread},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := &certutilAdds{}
			swapWindowsStore(t, tc.user, c, false)
			swapMachineStore(tc.machine)
			if err := installWindowsCA(context.Background(), zap.NewNop(), "x.crt", []byte("der"), keptCA); err != nil {
				t.Fatalf("installWindowsCA = %v, want nil", err)
			}
			if !reflect.DeepEqual(c.calls, []bool{true}) {
				t.Fatalf("certutil adds asked = %v, want the user store's", c.calls)
			}
		})
	}
}

func TestNoTrustPromptIsCertutilsNotSupportedCodeOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{"not supported", notSupportedOut, exitErr(t), true},
		{"access denied", accessDeniedOut, exitErr(t), false},
		{"another failure", "", errors.New("certutil not found"), false},
	} {
		if got := noTrustPrompt([]byte(tc.out), tc.err); got != tc.want {
			t.Errorf("%s: noTrustPrompt = %v, want %v", tc.name, got, tc.want)
		}
	}
}

func TestCertutilAddsToTheUsersStoreOrTheMachines(t *testing.T) {
	user := certutilCmd(context.Background(), true, "x.crt").Args
	machine := certutilCmd(context.Background(), false, "x.crt").Args
	if want := []string{"certutil", "-user", "-addstore", "-f", "ROOT", "x.crt"}; !reflect.DeepEqual(user, want) {
		t.Fatalf("the user store's add = %q, want %q", user, want)
	}
	if want := []string{"certutil", "-addstore", "-f", "ROOT", "x.crt"}; !reflect.DeepEqual(machine, want) {
		t.Fatalf("the machine store's add = %q, want %q", machine, want)
	}
}

func TestASessionWithNoTrustPromptSetsTheCAUpAndNamesTheStep(t *testing.T) {
	c := &certutilAdds{userOut: []byte(notSupportedOut), userErr: exitErr(t)}
	swapWindowsStore(t, notHeld, c, false)
	core, logs := observer.New(zapcore.WarnLevel)
	if err := trustOnWindows(context.Background(), zap.New(core), "x.crt", []byte("der"), keptCA); err != nil {
		t.Fatalf("trustOnWindows = %v, want nil: the app is given the CA where its runtime reads one", err)
	}
	why := UntrustedStoreWhy()
	if !strings.Contains(why, "Go or .NET") || !strings.Contains(why, "certutil -user -addstore ROOT "+keptCA) {
		t.Fatalf("UntrustedStoreWhy = %q, want the runtimes it refuses and the step for the kept CA", why)
	}
	if logs.Len() != 1 || logs.All()[0].ContextMap()["next_step"] != trustStep(keptCA) {
		t.Fatalf("warnings = %v, want one naming the step", logs.All())
	}

	if err := trustOnWindows(context.Background(), zap.NewNop(), "x.crt", []byte("der"), ""); err != nil {
		t.Fatalf("trustOnWindows with no kept CA = %v, want nil", err)
	}
	if why := UntrustedStoreWhy(); !strings.Contains(why, "LOCALAPPDATA") || strings.Contains(why, "certutil") {
		t.Fatalf("UntrustedStoreWhy with no kept CA = %q, want that each run asks again", why)
	}

	// A later setup the store takes clears it.
	c.userErr = nil
	if err := trustOnWindows(context.Background(), zap.NewNop(), "x.crt", []byte("der"), keptCA); err != nil {
		t.Fatalf("trustOnWindows = %v, want nil", err)
	}
	if why := UntrustedStoreWhy(); why != "" {
		t.Fatalf("UntrustedStoreWhy after the store took the CA = %q, want empty", why)
	}

	// Any other failure is the setup's.
	c.userOut, c.userErr = []byte(accessDeniedOut), exitErr(t)
	if err := trustOnWindows(context.Background(), zap.NewNop(), "x.crt", []byte("der"), keptCA); err != c.userErr {
		t.Fatalf("trustOnWindows = %v, want the user store's error", err)
	}
	if why := UntrustedStoreWhy(); why != "" {
		t.Fatalf("UntrustedStoreWhy after a failed setup = %q, want empty", why)
	}
}
