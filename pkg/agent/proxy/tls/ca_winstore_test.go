package tls

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
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
	// windowsKeptCA is where keploy keeps a Windows user's CA. Rows that
	// reach the record of a No (declinedRecord) keep theirs in a directory of
	// their own (tempKeptCA), never in a real user's.
	windowsKeptCA = `C:\Users\runner\AppData\Local\Keploy\ca\ca.crt`
)

func TestAStandardUserWhoseSessionCannotShowTheTrustPromptNeverAsksTheMachineStore(t *testing.T) {
	keptCA := tempKeptCA(t)
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
	keptCA := tempKeptCA(t)
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
			keptCA := tempKeptCA(t)
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
			keptCA := tempKeptCA(t)
			c := &certutilAdds{userOut: []byte(notSupportedOut), userErr: exitErr(t)}
			var askedUser, askedMachine []byte
			swapWindowsStore(t, func(der []byte) (bool, error) { askedUser = der; return tc.user(der) }, c, false)
			swapMachineStore(func(der []byte) (bool, error) { askedMachine = der; return tc.machine(der) })
			noteUntrustedStore("a hint from an earlier setup")
			core, logs := observer.New(zapcore.WarnLevel)
			if _, err := trustOnWindows(context.Background(), zap.New(core), "x.crt", []byte("the CA"), keptCA); err != nil {
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
			keptCA := tempKeptCA(t)
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
	keptCA := tempKeptCA(t)
	c := &certutilAdds{userOut: []byte(notSupportedOut), userErr: exitErr(t)}
	swapWindowsStore(t, notHeld, c, false)
	core, logs := observer.New(zapcore.WarnLevel)
	if _, err := trustOnWindows(context.Background(), zap.New(core), "x.crt", []byte("der"), keptCA); err != nil {
		t.Fatalf("trustOnWindows = %v, want nil: the app is given the CA where its runtime reads one", err)
	}
	why := UntrustedStoreWhy()
	if !strings.Contains(why, storeOnlyClients) || !strings.Contains(why, `certutil -user -addstore ROOT "`+keptCA+`"`) {
		t.Fatalf("UntrustedStoreWhy = %q, want the runtimes it refuses and the step for the kept CA", why)
	}
	if logs.Len() != 1 || logs.All()[0].ContextMap()["next_step"] != trustStep(keptCA) {
		t.Fatalf("warnings = %v, want one naming the step", logs.All())
	}

	if _, err := trustOnWindows(context.Background(), zap.NewNop(), "x.crt", []byte("der"), ""); err != nil {
		t.Fatalf("trustOnWindows with no kept CA = %v, want nil", err)
	}
	if why := UntrustedStoreWhy(); !strings.Contains(why, "LOCALAPPDATA") || strings.Contains(why, "certutil") {
		t.Fatalf("UntrustedStoreWhy with no kept CA = %q, want that each run asks again", why)
	}

	// A later setup the store takes clears it.
	c.userErr = nil
	if _, err := trustOnWindows(context.Background(), zap.NewNop(), "x.crt", []byte("der"), keptCA); err != nil {
		t.Fatalf("trustOnWindows = %v, want nil", err)
	}
	if why := UntrustedStoreWhy(); why != "" {
		t.Fatalf("UntrustedStoreWhy after the store took the CA = %q, want empty", why)
	}

	// Any other failure is the setup's.
	c.userOut, c.userErr = []byte(accessDeniedOut), exitErr(t)
	if _, err := trustOnWindows(context.Background(), zap.NewNop(), "x.crt", []byte("der"), keptCA); err != c.userErr {
		t.Fatalf("trustOnWindows = %v, want the user store's error", err)
	}
	if why := UntrustedStoreWhy(); why != "" {
		t.Fatalf("UntrustedStoreWhy after a failed setup = %q, want empty", why)
	}
}

// cancelledOut is certutil's answer when the user answers No to Windows'
// prompt to trust the CA it adds to their ROOT store.
const cancelledOut = "CertUtil: -addstore command FAILED: 0x800704c7 (WIN32: 1223 ERROR_CANCELLED)\r\nCertUtil: The operation was canceled by the user.\r\n"

// A No to Windows' prompt to trust keploy's CA is the user's answer for every
// trust store: an elevated run, which could write the machine's store and ask
// nobody, adds the CA there no more than a standard user's run does.
func TestANoToTheTrustPromptAddsTheCAToNoStoreElevatedOrNot(t *testing.T) {
	for _, elevated := range []bool{true, false} {
		t.Run(map[bool]string{true: "elevated", false: "standard user"}[elevated], func(t *testing.T) {
			keptCA := tempKeptCA(t)
			c := &certutilAdds{userOut: []byte(cancelledOut), userErr: exitErr(t)}
			swapWindowsStore(t, notHeld, c, elevated)
			err := installWindowsCA(context.Background(), zap.NewNop(), "x.crt", []byte("der"), keptCA)
			if !errors.Is(err, errUserDeclined) {
				t.Fatalf("installWindowsCA = %v, want errUserDeclined", err)
			}
			if !reflect.DeepEqual(c.calls, []bool{true}) {
				t.Fatalf("certutil adds asked = %v (true: the user store), want only the user store's, the add the user said No to: no machine-store write", c.calls)
			}
		})
	}
}

// A Yes puts keploy's CA in the store Windows asked about, the user's, and in
// no other: an elevated run writes the machine's store only when the user's
// cannot take the CA at all (TestAnElevatedRunThatCannotUseTheUserStoreFallsBackToTheMachineStore).
func TestAYesToTheTrustPromptPutsTheCAInTheUsersStoreOnlyElevatedOrNot(t *testing.T) {
	for _, elevated := range []bool{true, false} {
		t.Run(map[bool]string{true: "elevated", false: "standard user"}[elevated], func(t *testing.T) {
			keptCA := tempKeptCA(t)
			c := &certutilAdds{}
			swapWindowsStore(t, notHeld, c, elevated)
			if err := installWindowsCA(context.Background(), zap.NewNop(), "x.crt", []byte("der"), keptCA); err != nil {
				t.Fatalf("installWindowsCA = %v, want nil", err)
			}
			if !reflect.DeepEqual(c.calls, []bool{true}) {
				t.Fatalf("certutil adds asked = %v, want the user store's only", c.calls)
			}
		})
	}
}

func TestUserDeclinedIsCertutilsCancelledCodeOnly(t *testing.T) {
	for _, tc := range []struct {
		name string
		out  string
		err  error
		want bool
	}{
		{"cancelled", cancelledOut, exitErr(t), true},
		{"cancelled, the code in upper case", strings.ToUpper(cancelledOut), exitErr(t), true},
		{"not supported", notSupportedOut, exitErr(t), false},
		{"access denied", accessDeniedOut, exitErr(t), false},
		{"another failure", "", errors.New("certutil not found"), false},
	} {
		if got := userDeclined([]byte(tc.out), tc.err); got != tc.want {
			t.Errorf("%s: userDeclined = %v, want %v", tc.name, got, tc.want)
		}
	}
	if noTrustPrompt([]byte(cancelledOut), exitErr(t)) {
		t.Error("noTrustPrompt(certutil's cancelled answer) = true, want false: a No is not a session with no desktop")
	}
}

// After a No the run goes on, the app given keploy's CA where its runtime
// reads one; the warning and each lost handshake's reason
// (UntrustedStoreWhy) say what does not work and how to opt in.
func TestANoToTheTrustPromptGoesOnSayingWhatDoesNotWorkAndHowToOptIn(t *testing.T) {
	for _, elevated := range []bool{true, false} {
		t.Run(map[bool]string{true: "elevated", false: "standard user"}[elevated], func(t *testing.T) {
			kept := tempKeptCA(t)
			c := &certutilAdds{userOut: []byte(cancelledOut), userErr: exitErr(t)}
			swapWindowsStore(t, notHeld, c, elevated)
			core, logs := observer.New(zapcore.WarnLevel)
			declined, err := trustOnWindows(context.Background(), zap.New(core), "x.crt", []byte("der"), kept)
			if err != nil || !declined {
				t.Fatalf("trustOnWindows = %v, %v; want declined, nil: the run goes on", declined, err)
			}
			if !reflect.DeepEqual(c.calls, []bool{true}) {
				t.Fatalf("certutil adds asked = %v, want the user store's only", c.calls)
			}
			why := UntrustedStoreWhy()
			if !strings.Contains(why, "answered No") || !strings.Contains(why, storeOnlyClients) || !strings.Contains(why, `certutil -user -addstore ROOT "`+kept+`"`) {
				t.Fatalf("UntrustedStoreWhy = %q, want the No, the runtimes it refuses and the opt-in step for the kept CA", why)
			}
			if logs.Len() != 1 {
				t.Fatalf("warnings = %v, want one", logs.All())
			}
			w := logs.All()[0]
			if w.ContextMap()["next_step"] != optInStep(kept) || !strings.Contains(w.Message, "no trust store") ||
				!strings.Contains(w.Message, storeOnlyClients) || !strings.Contains(w.Message, "HTTPS calls fail") ||
				!strings.Contains(w.Message, "go 1.27 or later") {
				t.Fatalf("warning = %q %v, want what does not work and the opt-in step", w.Message, w.ContextMap())
			}
		})
	}
	c := &certutilAdds{userOut: []byte(cancelledOut), userErr: exitErr(t)}
	swapWindowsStore(t, notHeld, c, true)
	if declined, err := trustOnWindows(context.Background(), zap.NewNop(), "x.crt", []byte("der"), ""); err != nil || !declined {
		t.Fatalf("trustOnWindows with no kept CA = %v, %v; want declined, nil", declined, err)
	}
	if why := UntrustedStoreWhy(); !strings.Contains(why, "LOCALAPPDATA") || strings.Contains(why, "certutil") {
		t.Fatalf("UntrustedStoreWhy with no kept CA = %q, want that each run asks again", why)
	}
	// A session with no desktop is not a No.
	c.userOut = []byte(notSupportedOut)
	swapWindowsStore(t, notHeld, c, false)
	if declined, err := trustOnWindows(context.Background(), zap.NewNop(), "x.crt", []byte("der"), tempKeptCA(t)); err != nil || declined {
		t.Fatalf("trustOnWindows with no desktop = %v, %v; want not declined, nil", declined, err)
	}
}

// swapJavaImport has setupNativeWindows import into a JDK keystore through
// a recorder, for the row, and returns how many imports were asked.
func swapJavaImport(t *testing.T) *int {
	t.Helper()
	asked := new(int)
	real := installJavaCAForHomeFn
	installJavaCAForHomeFn = func(context.Context, *zap.Logger, string, string, string) (javaInstall, error) {
		*asked++
		return javaInstall{}, nil
	}
	t.Cleanup(func() { installJavaCAForHomeFn = real })
	return asked
}

// The JDK's keystore is a trust store too, read by every program of that
// JDK: after a No keploy imports its CA into none, and the run's CA setup is
// still over (the env vars, and Java's truststore the client points
// JAVA_TOOL_OPTIONS at). After a Yes it imports it, as before.
func TestANoToTheTrustPromptImportsTheCAIntoNoJDKKeystore(t *testing.T) {
	for _, tc := range []struct {
		name       string
		userOut    string
		declined   bool
		wantImport int
	}{
		{"No", cancelledOut, true, 0},
		{"Yes", "", false, 1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, k := range []string{"NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "SSL_CERT_FILE", "CARGO_HTTP_CAINFO"} {
				t.Setenv(k, "")
			}
			ResetCAReadyForTest()
			t.Cleanup(ResetCAReadyForTest)
			t.Cleanup(func() { recordNativeInstall(nil) })
			c := &certutilAdds{}
			if tc.declined {
				c.userOut, c.userErr = []byte(tc.userOut), exitErr(t)
			}
			swapWindowsStore(t, notHeld, c, true)
			imports := swapJavaImport(t)
			ca, err := generateRunCA()
			if err != nil {
				t.Fatal(err)
			}
			if err := setupNativeWindows(context.Background(), zap.NewNop(), ca, ephemeralStore{}, "", runAlias(ca)); err != nil {
				t.Fatalf("setupNativeWindows = %v, want nil", err)
			}
			if *imports != tc.wantImport {
				t.Fatalf("JDK keystore imports asked = %d, want %d", *imports, tc.wantImport)
			}
			if !reflect.DeepEqual(c.calls, []bool{true}) {
				t.Fatalf("certutil adds asked = %v, want the user store's only", c.calls)
			}
			select {
			case <-CAReady():
			default:
				t.Fatal("the CA setup is not over: the run must go on")
			}
		})
	}
}

// tempKeptCA is a kept CA's path in a directory of the row's own, where the
// record of a No (declinedRecord) goes.
func tempKeptCA(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), caCertFileName)
}

// A No outlives the run that heard it: a later elevated run that cannot ask
// (no desktop) puts the CA in the machine's store no more than that run did.
// A Yes clears it, and so does the CA found in a store (the user trusted it
// by hand); after that, such a run falls back to the machine's store again.
func TestANoIsRecordedSoALaterElevatedRunThatCannotAskWritesNoStore(t *testing.T) {
	held := func([]byte) (bool, error) { return true, nil }
	for _, tc := range []struct {
		name  string
		clear func(t *testing.T, kept string)
	}{
		{"a later Yes clears it", func(t *testing.T, kept string) {
			swapWindowsStore(t, notHeld, &certutilAdds{}, false)
			if err := installWindowsCA(context.Background(), zap.NewNop(), "x.crt", []byte("der"), kept); err != nil {
				t.Fatalf("installWindowsCA after a Yes = %v, want nil", err)
			}
		}},
		{"the CA found in a store clears it", func(t *testing.T, kept string) {
			swapWindowsStore(t, held, &certutilAdds{}, false)
			if err := installWindowsCA(context.Background(), zap.NewNop(), "x.crt", []byte("der"), kept); err != nil {
				t.Fatalf("installWindowsCA with the CA in a store = %v, want nil", err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			kept := tempKeptCA(t)
			// The No, in a standard user's run with a desktop.
			swapWindowsStore(t, notHeld, &certutilAdds{userOut: []byte(cancelledOut), userErr: exitErr(t)}, false)
			if err := installWindowsCA(context.Background(), zap.NewNop(), "x.crt", []byte("der"), kept); !errors.Is(err, errUserDeclined) {
				t.Fatalf("installWindowsCA = %v, want errUserDeclined", err)
			}
			if _, err := os.Stat(filepath.Join(filepath.Dir(kept), declinedRecordName)); err != nil {
				t.Fatalf("the No is not recorded beside the kept CA: %v", err)
			}
			// A later elevated run with no desktop.
			noDesktop := &certutilAdds{userOut: []byte(notSupportedOut), userErr: exitErr(t)}
			swapWindowsStore(t, notHeld, noDesktop, true)
			core, logs := observer.New(zapcore.WarnLevel)
			declined, err := trustOnWindows(context.Background(), zap.New(core), "x.crt", []byte("der"), kept)
			if err != nil || !declined {
				t.Fatalf("trustOnWindows after a recorded No = %v, %v; want declined, nil", declined, err)
			}
			if !reflect.DeepEqual(noDesktop.calls, []bool{true}) {
				t.Fatalf("certutil adds asked = %v, want the user store's only: no machine-store write after a recorded No", noDesktop.calls)
			}
			// It says the No is an earlier run's, where it is recorded, and
			// steps that work from a run that cannot ask.
			record := filepath.Join(filepath.Dir(kept), declinedRecordName)
			if logs.Len() != 1 {
				t.Fatalf("warnings = %v, want one", logs.All())
			}
			w := logs.All()[0]
			if !strings.Contains(w.Message, "earlier run") || w.ContextMap()["record_of_the_no"] != record || w.ContextMap()["next_step"] != declinedEarlierStep(kept) {
				t.Fatalf("warning = %q %v, want the earlier run's No, its record and the steps for a run that cannot ask", w.Message, w.ContextMap())
			}
			if step := declinedEarlierStep(kept); !strings.Contains(step, "session signed in to the desktop") || !strings.Contains(step, record) {
				t.Fatalf("declinedEarlierStep = %q, want the desktop session's step and the record to delete", step)
			}
			if why := UntrustedStoreWhy(); !strings.Contains(why, "earlier run") {
				t.Fatalf("UntrustedStoreWhy = %q, want the earlier run's No", why)
			}

			tc.clear(t, kept)
			if declinedBefore(kept) {
				t.Fatal("the record of the No is still there")
			}
			noDesktop = &certutilAdds{userOut: []byte(notSupportedOut), userErr: exitErr(t)}
			swapWindowsStore(t, notHeld, noDesktop, true)
			if err := installWindowsCA(context.Background(), zap.NewNop(), "x.crt", []byte("der"), kept); err != nil {
				t.Fatalf("installWindowsCA = %v, want nil: the machine store takes it", err)
			}
			if !reflect.DeepEqual(noDesktop.calls, []bool{true, false}) {
				t.Fatalf("certutil adds asked = %v, want the user store's, then the machine store's", noDesktop.calls)
			}
		})
	}
}

// With no kept CA, or a kept CA's path that names no directory here, a No is
// recorded nowhere.
func TestANoIsRecordedOnlyBesideAKeptCA(t *testing.T) {
	for _, kept := range []string{"", "relative/ca.crt", windowsKeptCA} {
		if filepath.IsAbs(kept) {
			continue // windowsKeptCA is absolute on Windows
		}
		if p := declinedRecord(kept); p != "" {
			t.Errorf("declinedRecord(%q) = %q, want none", kept, p)
		}
	}
	kept := tempKeptCA(t)
	if want := filepath.Join(filepath.Dir(kept), declinedRecordName); declinedRecord(kept) != want {
		t.Fatalf("declinedRecord(%q) = %q, want %q", kept, declinedRecord(kept), want)
	}
}

// The machine store is the fallback only for an elevated run whose user store
// could not ask (no desktop): a failure keploy cannot name could be a No in a
// form it does not know, so it is the setup's, as for a standard user.
func TestAnElevatedRunFallsBackToTheMachineStoreOnlyWhenTheUserStoreCouldNotAsk(t *testing.T) {
	c := &certutilAdds{userOut: []byte(accessDeniedOut), userErr: exitErr(t)}
	swapWindowsStore(t, notHeld, c, true)
	err := installWindowsCA(context.Background(), zap.NewNop(), "x.crt", []byte("der"), tempKeptCA(t))
	if err != c.userErr {
		t.Fatalf("installWindowsCA = %v, want the user store's error %v", err, c.userErr)
	}
	if !reflect.DeepEqual(c.calls, []bool{true}) {
		t.Fatalf("certutil adds asked = %v, want the user store's only: no machine-store write for a failure keploy cannot name", c.calls)
	}
}

// The setup records a No beside the CA its store keeps, and a later standard
// user's run that cannot ask (no desktop) holds to it: no JDK keystore gets
// the CA, and the warning names the earlier run's No.
func TestANoRecordedBesideTheKeptCAHoldsForAStandardUsersRunThatCannotAsk(t *testing.T) {
	for _, k := range []string{"NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "SSL_CERT_FILE", "CARGO_HTTP_CAINFO"} {
		t.Setenv(k, "")
	}
	ResetCAReadyForTest()
	t.Cleanup(ResetCAReadyForTest)
	t.Cleanup(func() { recordNativeInstall(nil) })
	store := &fileStore{dir: t.TempDir()}
	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	imports := swapJavaImport(t)

	swapWindowsStore(t, notHeld, &certutilAdds{userOut: []byte(cancelledOut), userErr: exitErr(t)}, false)
	if err := setupNativeWindows(context.Background(), zap.NewNop(), ca, store, "", runAlias(ca)); err != nil {
		t.Fatalf("setupNativeWindows after a No = %v, want nil", err)
	}
	if _, err := os.Stat(filepath.Join(store.dir, declinedRecordName)); err != nil {
		t.Fatalf("the No is not recorded beside the kept CA in %s: %v", store.dir, err)
	}

	ResetCAReadyForTest()
	noDesktop := &certutilAdds{userOut: []byte(notSupportedOut), userErr: exitErr(t)}
	swapWindowsStore(t, notHeld, noDesktop, false)
	core, logs := observer.New(zapcore.WarnLevel)
	if err := setupNativeWindows(context.Background(), zap.New(core), ca, store, "", runAlias(ca)); err != nil {
		t.Fatalf("setupNativeWindows with no desktop = %v, want nil", err)
	}
	if *imports != 0 {
		t.Fatalf("JDK keystore imports asked = %d, want none after a No", *imports)
	}
	if !reflect.DeepEqual(noDesktop.calls, []bool{true}) {
		t.Fatalf("certutil adds asked = %v, want the user store's only", noDesktop.calls)
	}
	if logs.Len() != 1 || !strings.Contains(logs.All()[0].Message, "earlier run") {
		t.Fatalf("warnings = %v, want one naming the earlier run's No", logs.All())
	}
	select {
	case <-CAReady():
	default:
		t.Fatal("the CA setup is not over: the run must go on")
	}
}

// A recorded No answers only a run that could not ask: any other failure of
// the user store's add is the setup's, said as it is, and no store, a JDK's
// included, gets the CA.
func TestARecordedNoDoesNotHideAnotherFailureOfTheUserStore(t *testing.T) {
	for _, k := range []string{"NODE_EXTRA_CA_CERTS", "REQUESTS_CA_BUNDLE", "SSL_CERT_FILE", "CARGO_HTTP_CAINFO"} {
		t.Setenv(k, "")
	}
	ResetCAReadyForTest()
	t.Cleanup(ResetCAReadyForTest)
	t.Cleanup(func() { recordNativeInstall(nil) })
	store := &fileStore{dir: t.TempDir()}
	if err := os.WriteFile(filepath.Join(store.dir, declinedRecordName), []byte("an earlier No\n"), 0600); err != nil {
		t.Fatal(err)
	}
	ca, err := generateRunCA()
	if err != nil {
		t.Fatal(err)
	}
	imports := swapJavaImport(t)
	for _, elevated := range []bool{true, false} {
		c := &certutilAdds{userOut: []byte(accessDeniedOut), userErr: exitErr(t)}
		swapWindowsStore(t, notHeld, c, elevated)
		if err := setupNativeWindows(context.Background(), zap.NewNop(), ca, store, "", runAlias(ca)); err != c.userErr {
			t.Fatalf("elevated=%v: setupNativeWindows = %v, want the user store's error %v", elevated, err, c.userErr)
		}
		if !reflect.DeepEqual(c.calls, []bool{true}) {
			t.Fatalf("elevated=%v: certutil adds asked = %v, want the user store's only", elevated, c.calls)
		}
	}
	if *imports != 0 {
		t.Fatalf("JDK keystore imports asked = %d, want none: the setup failed", *imports)
	}
}
