package tls

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"math/big"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

// legacyCAPEM is the retired static MITM CA, the asset/ca.crt keploy shipped
// until #4682 (its private key was committed beside it). Only its
// certificate is here, for the sweep's rows to find.
const legacyCAPEM = `-----BEGIN CERTIFICATE-----
MIIBczCCARigAwIBAgIUe1yXSfGefdyA09reeQpit2S7TXMwCgYIKoZIzj0EAwIw
FzEVMBMGA1UEAxMMTXkgQ3VzdG9tIENBMB4XDTIzMDQyMDExMDYwMFoXDTMzMDQx
NzExMDYwMFowFzEVMBMGA1UEAxMMTXkgQ3VzdG9tIENBMFkwEwYHKoZIzj0CAQYI
KoZIzj0DAQcDQgAE+QJh3KtWSGRe9NtzZGcJQBcn0ipsjzpo7YlLvbaSFeI62H1b
5EPUjnbDInG4K0nO1Dq/gPtnsVXZcJx06oHj9qNCMEAwDgYDVR0PAQH/BAQDAgEG
MA8GA1UdEwEB/wQFMAMBAf8wHQYDVR0OBBYEFDejUAabmE7NFoKSOo5uHAHMumal
MAoGCCqGSM49BAMCA0kAMEYCIQDkGU6Oku9vLo7fLLquHWXR1rB/1YgWIBghnQXJ
fF5GawIhAK/0BaJCVm6rkRuYCdtahkN1N5DtTPr8AQoDV+MT0UmL
-----END CERTIFICATE-----
`

// legacyCAThumbprint is the retired CA's SHA-1 thumbprint, the name certutil
// is given to remove it.
const legacyCAThumbprint = "e311f6c8d7cd82f8eaeb0698b4be8e4006e78e4e"

func legacyCADER(t *testing.T) []byte {
	t.Helper()
	block, _ := pem.Decode([]byte(legacyCAPEM))
	if block == nil {
		t.Fatal("the retired CA's PEM does not decode")
	}
	return block.Bytes
}

// selfSignedDER is a fresh self-signed CA certificate with subject cn.
func selfSignedDER(t *testing.T, cn string) []byte {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{SerialNumber: big.NewInt(time.Now().UnixNano()), Subject: pkix.Name{CommonName: cn},
		NotBefore: time.Now().Add(-time.Hour), NotAfter: time.Now().Add(time.Hour), IsCA: true, BasicConstraintsValid: true}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// rootStores is a row's Windows ROOT stores: what each holds, what reading
// or removing from each answers, and every removal asked.
type rootStores struct {
	held      map[winRootStore][][]byte
	readErr   map[winRootStore]error
	deleteErr map[winRootStore]error
	deletes   []storeDelete
}

type storeDelete struct {
	store winRootStore
	der   []byte
}

func (r *rootStores) certs(s winRootStore) ([][]byte, error) {
	if err := r.readErr[s]; err != nil {
		return nil, err
	}
	return append([][]byte(nil), r.held[s]...), nil
}

// delete answers as rootStoreDelete does: whether s held der, and why its
// removal failed, when it did.
func (r *rootStores) delete(s winRootStore, der []byte) (bool, error) {
	r.deletes = append(r.deletes, storeDelete{s, der})
	for i, h := range r.held[s] {
		if bytes.Equal(h, der) {
			if err := r.deleteErr[s]; err != nil {
				return true, err
			}
			r.held[s] = append(r.held[s][:i:i], r.held[s][i+1:]...)
			return true, nil
		}
	}
	return false, r.deleteErr[s]
}

// swapRootStores has the sweep reach the Windows ROOT stores through r and
// see the token as elevated or not, for the row.
func swapRootStores(t *testing.T, r *rootStores, elevated bool) {
	t.Helper()
	c, d, e := rootStoreCertsFn, rootStoreDeleteFn, tokenElevatedFn
	t.Cleanup(func() { rootStoreCertsFn, rootStoreDeleteFn, tokenElevatedFn = c, d, e })
	rootStoreCertsFn, rootStoreDeleteFn = r.certs, r.delete
	tokenElevatedFn = func() bool { return elevated }
}

func mustLegacyFP(t *testing.T) [32]byte {
	t.Helper()
	fp, ok := legacyCAFingerprint()
	if !ok {
		t.Fatal("legacy CA fingerprint constant failed to parse")
	}
	return fp
}

func TestTheRetiredCAPEMIsTheOneTheSweepLooksFor(t *testing.T) {
	der := legacyCADER(t)
	if sha256.Sum256(der) != mustLegacyFP(t) {
		t.Fatal("legacyCAPEM's SHA-256 is not legacyCASHA256Hex")
	}
	if step := legacyWindowsRootStep(userRootStore, der); !strings.Contains(step, legacyCAThumbprint) {
		t.Fatalf("the user store's step = %q, want the thumbprint %s", step, legacyCAThumbprint)
	}
}

// Only the retired CA itself is ever matched: its exact subject AND its
// fingerprint.
func TestIsLegacyWindowsRootNeedsTheSubjectAndTheFingerprint(t *testing.T) {
	fp := mustLegacyFP(t)
	legacy := legacyCADER(t)
	sameSubject := selfSignedDER(t, "My Custom CA")
	otherSubject := selfSignedDER(t, "My Custom CA 2")
	garbage := []byte("not a certificate")
	for _, tc := range []struct {
		name string
		der  []byte
		fp   [32]byte
		want bool
	}{
		{"the retired CA", legacy, fp, true},
		{"its subject, another fingerprint", sameSubject, fp, false},
		{"another subject, whose own fingerprint is looked for", otherSubject, sha256.Sum256(otherSubject), false},
		{"its subject, whose own fingerprint is looked for", sameSubject, sha256.Sum256(sameSubject), true},
		{"no certificate, whose own fingerprint is looked for", garbage, sha256.Sum256(garbage), false},
	} {
		if got := isLegacyWindowsRoot(tc.der, tc.fp); got != tc.want {
			t.Errorf("%s: isLegacyWindowsRoot = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// An elevated run removes the retired CA from both ROOT stores, and nothing
// else: a certificate with its subject and another fingerprint stays, as does
// every other root. Each removal is said at INFO, naming its store.
func TestAnElevatedSweepRemovesOnlyTheRetiredCAFromBothWindowsRootStores(t *testing.T) {
	legacy := legacyCADER(t)
	sameSubject := selfSignedDER(t, "My Custom CA")
	other := selfSignedDER(t, "Some Root")
	r := &rootStores{held: map[winRootStore][][]byte{
		userRootStore:    {sameSubject, legacy, other},
		machineRootStore: {other, legacy, sameSubject},
	}}
	swapRootStores(t, r, true)
	core, logs := observer.New(zapcore.DebugLevel)
	sweepLegacyWindowsRoots(zap.New(core), mustLegacyFP(t))

	want := []storeDelete{{userRootStore, legacy}, {machineRootStore, legacy}}
	if !reflect.DeepEqual(r.deletes, want) {
		t.Fatalf("removals asked = %v, want the retired CA from the user's store, then the machine's", r.deletes)
	}
	for s, rest := range map[winRootStore][][]byte{userRootStore: {sameSubject, other}, machineRootStore: {other, sameSubject}} {
		if !reflect.DeepEqual(r.held[s], rest) {
			t.Fatalf("%s holds %d certificates after the sweep, want the %d that are not the retired CA", s, len(r.held[s]), len(rest))
		}
	}
	var infos []string
	for _, e := range logs.All() {
		if e.Level == zapcore.WarnLevel {
			t.Fatalf("warning %q, want none", e.Message)
		}
		if e.Level == zapcore.InfoLevel {
			infos = append(infos, e.Message)
			if strings.HasPrefix(e.Message, "removed ") && (e.ContextMap()["sha256"] != legacyCASHA256Hex || e.ContextMap()["subject"] != legacyCASubject) {
				t.Fatalf("INFO line %q %v, want the removed CA's subject and fingerprint", e.Message, e.ContextMap())
			}
		}
	}
	// The user's store asks the user: said before its removal, which is said
	// after; the machine's asks nobody.
	if len(infos) != 3 || !strings.Contains(infos[0], "answer Yes") ||
		!strings.HasPrefix(infos[1], "removed ") || !strings.Contains(infos[1], userRootStore.String()) ||
		!strings.HasPrefix(infos[2], "removed ") || !strings.Contains(infos[2], `LocalMachine\Root`) {
		t.Fatalf("INFO lines = %q, want the user store's prompt explained, then one removal per store, naming it", infos)
	}
}

// A run that is not elevated cannot change the machine's store: it removes
// the retired CA from the user's, leaves the machine's alone and says, at
// WARN, that the machine's still trusts it and how an administrator removes
// it.
func TestASweepThatIsNotElevatedLeavesTheMachineStoreAndSaysHowToCleanIt(t *testing.T) {
	legacy := legacyCADER(t)
	r := &rootStores{held: map[winRootStore][][]byte{
		userRootStore:    {legacy},
		machineRootStore: {legacy},
	}}
	swapRootStores(t, r, false)
	core, logs := observer.New(zapcore.WarnLevel)
	sweepLegacyWindowsRoots(zap.New(core), mustLegacyFP(t))

	if want := []storeDelete{{userRootStore, legacy}}; !reflect.DeepEqual(r.deletes, want) {
		t.Fatalf("removals asked = %v, want only the user store's", r.deletes)
	}
	if len(r.held[machineRootStore]) != 1 {
		t.Fatal("the machine's store lost its certificate in a run that is not elevated")
	}
	if logs.Len() != 1 {
		t.Fatalf("warnings = %v, want one, for the machine's store", logs.All())
	}
	w := logs.All()[0]
	step, _ := w.ContextMap()["next_step"].(string)
	if !strings.Contains(w.Message, `LocalMachine\Root`) || !strings.Contains(w.Message, "not elevated") ||
		!strings.Contains(step, "administrator") || !strings.Contains(step, "certutil -delstore ROOT "+legacyCAThumbprint) {
		t.Fatalf("warning = %q, next_step %q; want the machine's store, the reason and the administrator's certutil step", w.Message, step)
	}
}

// A store that holds only a certificate with the retired CA's subject and
// another fingerprint is left alone, said nowhere.
func TestASameSubjectCertificateWithAnotherFingerprintIsNeverRemoved(t *testing.T) {
	sameSubject := selfSignedDER(t, "My Custom CA")
	for _, elevated := range []bool{true, false} {
		r := &rootStores{held: map[winRootStore][][]byte{
			userRootStore:    {sameSubject},
			machineRootStore: {sameSubject},
		}}
		swapRootStores(t, r, elevated)
		core, logs := observer.New(zapcore.InfoLevel)
		sweepLegacyWindowsRoots(zap.New(core), mustLegacyFP(t))
		if len(r.deletes) != 0 || logs.Len() != 0 {
			t.Fatalf("elevated=%v: removals %v and lines %v, want none", elevated, r.deletes, logs.All())
		}
	}
}

// Removing a root from the user's store asks the user. A No, a session with
// no desktop to ask in, or another failure leaves the CA there, said at WARN
// with the step that removes it, and the machine's store is still swept.
func TestAUserStoreRemovalWindowsDidNotAllowIsSaidWithItsStep(t *testing.T) {
	legacy := legacyCADER(t)
	for _, tc := range []struct {
		name, want string
		err        error
	}{
		{"No", "answered No", errRemovalDeclined},
		{"no desktop", "no desktop", errRemovalNoPrompt},
		{"another failure", "could not remove", errors.New("store busy")},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := &rootStores{
				held:      map[winRootStore][][]byte{userRootStore: {legacy}, machineRootStore: {legacy}},
				deleteErr: map[winRootStore]error{userRootStore: tc.err},
			}
			swapRootStores(t, r, true)
			core, logs := observer.New(zapcore.InfoLevel)
			sweepLegacyWindowsRoots(zap.New(core), mustLegacyFP(t))
			if want := []storeDelete{{userRootStore, legacy}, {machineRootStore, legacy}}; !reflect.DeepEqual(r.deletes, want) {
				t.Fatalf("removals asked = %v, want the user store's, then the machine store's", r.deletes)
			}
			var warns, infos []observer.LoggedEntry
			for _, e := range logs.All() {
				switch e.Level {
				case zapcore.WarnLevel:
					warns = append(warns, e)
				case zapcore.InfoLevel:
					if strings.HasPrefix(e.Message, "removed ") {
						infos = append(infos, e)
					}
				}
			}
			if len(warns) != 1 || !strings.Contains(warns[0].Message, tc.want) {
				t.Fatalf("warnings = %v, want one saying %q", warns, tc.want)
			}
			step, _ := warns[0].ContextMap()["next_step"].(string)
			if !strings.Contains(step, "certutil -user -delstore ROOT "+legacyCAThumbprint) {
				t.Fatalf("next_step = %q, want the user store's certutil step", step)
			}
			if len(infos) != 1 || !strings.Contains(infos[0].Message, `LocalMachine\Root`) {
				t.Fatalf("INFO lines = %v, want the machine store's removal only", infos)
			}
		})
	}
}

// A store that cannot be read is skipped; the other is still swept.
func TestAnUnreadableWindowsRootStoreDoesNotStopTheSweep(t *testing.T) {
	legacy := legacyCADER(t)
	r := &rootStores{
		held:    map[winRootStore][][]byte{machineRootStore: {legacy}},
		readErr: map[winRootStore]error{userRootStore: errors.New("store unread")},
	}
	swapRootStores(t, r, true)
	sweepLegacyWindowsRoots(zap.NewNop(), mustLegacyFP(t))
	if want := []storeDelete{{machineRootStore, legacy}}; !reflect.DeepEqual(r.deletes, want) {
		t.Fatalf("removals asked = %v, want the machine store's", r.deletes)
	}
}

// Off Windows there is no ROOT store: the sweep reads none, removes nothing
// and says nothing.
func TestOffWindowsTheWindowsRootSweepDoesNothing(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("off Windows only")
	}
	d := rootStoreDeleteFn
	t.Cleanup(func() { rootStoreDeleteFn = d })
	rootStoreDeleteFn = func(s winRootStore, _ []byte) (bool, error) {
		t.Errorf("a removal from %s was asked off Windows", s)
		return false, nil
	}
	core, logs := observer.New(zapcore.DebugLevel)
	sweepLegacyWindowsRoots(zap.New(core), mustLegacyFP(t))
	if logs.Len() != 0 {
		t.Fatalf("lines = %v, want none", logs.All())
	}
}

// The Windows stores are swept where the system store is: in SweepLegacyCA,
// which the agent's start runs before it installs this run's CA, and in
// keploy ca clean (SweepLegacyCAAggressive).
func TestSweepLegacyCASweepsTheWindowsRootStores(t *testing.T) {
	legacy := legacyCADER(t)
	old := caStorePath
	caStorePath = []string{t.TempDir() + "/"}
	t.Cleanup(func() { caStorePath = old })
	// The leftover temp files are swept from the temp directory: TMPDIR's,
	// or TMP's and TEMP's on Windows.
	t.Setenv("TMPDIR", t.TempDir())
	t.Setenv("TMP", t.TempDir())
	t.Setenv("TEMP", t.TempDir())
	t.Setenv("JAVA_HOME", t.TempDir())
	t.Setenv("PATH", t.TempDir())
	for name, sweep := range map[string]func(){
		"SweepLegacyCA":           func() { SweepLegacyCA(context.Background(), zap.NewNop(), false) },
		"SweepLegacyCAAggressive": func() { SweepLegacyCAAggressive(context.Background(), zap.NewNop(), false) },
	} {
		r := &rootStores{held: map[winRootStore][][]byte{userRootStore: {legacy}, machineRootStore: {legacy}}}
		swapRootStores(t, r, true)
		sweep()
		if want := []storeDelete{{userRootStore, legacy}, {machineRootStore, legacy}}; !reflect.DeepEqual(r.deletes, want) {
			t.Fatalf("%s: removals asked = %v, want the retired CA from both stores", name, r.deletes)
		}
	}
}
