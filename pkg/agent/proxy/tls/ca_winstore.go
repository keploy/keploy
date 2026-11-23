package tls

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sync/atomic"

	"go.uber.org/zap"
)

// errNoTrustPrompt is a user-store add certutil failed with ERROR_NOT_SUPPORTED:
// Windows asks the user, in a dialog, whether to trust a new root in their
// ROOT store, and a session with no desktop to show it in (a CI runner's
// service, an OpenSSH session, a scheduled task) cannot ask.
var errNoTrustPrompt = errors.New("this session has no desktop for Windows' prompt to trust keploy's CA")

// errUserDeclined is a user-store add the user refused: they answered No to
// Windows' dialog asking whether to trust keploy's CA, and certutil failed
// the add with ERROR_CANCELLED; or, in a run whose add could not ask (no
// desktop), a No an earlier run heard (declinedRecord, errDeclinedEarlier). That No is the user's answer for
// every trust store: keploy then adds its CA to none, the machine's
// included, also in a run that could write the machine's.
var errUserDeclined = errors.New("the user answered No to Windows' prompt to trust keploy's CA")

// errDeclinedEarlier is errUserDeclined from a No an earlier run recorded
// beside the kept CA (declinedRecord): this run's add failed without asking.
var errDeclinedEarlier = fmt.Errorf("%w in an earlier run, which this run could not ask again", errUserDeclined)

// winRootStore names a Windows ROOT store keploy changes: the current user's
// own (its .Default physical store, which holds what was added to it, not
// the machine's roots its view takes in), or the machine's
// (LocalMachine\Root), which only an elevated run may change.
type winRootStore int

const (
	userRootStore winRootStore = iota
	machineRootStore
)

func (s winRootStore) String() string {
	if s == machineRootStore {
		return `the machine's Windows ROOT store (LocalMachine\Root)`
	}
	return "the current user's Windows ROOT store"
}

// errRemovalDeclined is a removal from the user's ROOT store the user
// refused: they answered No to Windows' dialog asking whether to remove the
// root.
var errRemovalDeclined = errors.New("the user answered No to Windows' prompt to remove the certificate")

// errRemovalNoPrompt is a removal from the user's ROOT store in a session
// with no desktop for Windows' prompt to remove a root.
var errRemovalNoPrompt = errors.New("this session has no desktop for Windows' prompt to remove the certificate")

// errNoWindowsStore is a Windows store call off Windows
// (ca_winstore_other.go).
var errNoWindowsStore = errors.New("there is no Windows certificate store on this platform")

// hrNotSupported is ERROR_NOT_SUPPORTED as the HRESULT certutil reports it in.
const hrNotSupported = 0x80070032

// hrCancelled is ERROR_CANCELLED as the HRESULT certutil reports it in: the
// user answered No to Windows' dialog.
const hrCancelled = 0x800704C7

// The Windows store, certutil and the process token, as installWindowsCA
// and the retired CA's sweep reach them (ca_winstore_windows.go); rows swap
// them.
var (
	userRootHoldsFn    = userRootHolds
	machineRootHoldsFn = machineRootHolds
	certutilAddFn      = certutilAdd
	tokenElevatedFn    = tokenElevated
	rootStoreCertsFn   = rootStoreCerts
	rootStoreDeleteFn  = rootStoreDelete
)

// trustedWindowsStore names the Windows ROOT store that already holds
// keploy's CA der, "" when none does: the current user's, or the machine's
// (LocalMachine\Root, where an administrator or a company's group policy puts
// a CA every user of the machine trusts). A client that reads the Windows
// store (Go, .NET) trusts a CA in either, so keploy's setup adds it to
// neither again and warns of nothing. The user's view of ROOT takes in the
// machine's on the Windows tested (11, 26200); the machine's own store is
// read as well, so the outcome does not rest on that view. Reading either
// needs no administrator. A store that cannot be read counts as not holding
// the CA: the setup then adds it to the user's, as it would to an empty one.
func trustedWindowsStore(logger *zap.Logger, der []byte) string {
	inUser, userErr := userRootHoldsFn(der)
	if userErr == nil && inUser {
		return "the current user's Windows ROOT store"
	}
	inMachine, machineErr := machineRootHoldsFn(der)
	if machineErr == nil && inMachine {
		return `the machine's Windows ROOT store (LocalMachine\Root)`
	}
	logger.Debug("no Windows ROOT store holds keploy's CA, or one could not be read; adding it to the current user's",
		zap.NamedError("user_store_error", userErr), zap.NamedError("machine_store_error", machineErr))
	return ""
}

// certutilAdd adds the certificate at certPath to the ROOT store, the current
// user's when user is set, else the machine's, and returns certutil's output.
func certutilAdd(ctx context.Context, user bool, certPath string) ([]byte, error) {
	return certutilCmd(ctx, user, certPath).CombinedOutput()
}

// certutilCmd is certutilAdd's command.
func certutilCmd(ctx context.Context, user bool, certPath string) *exec.Cmd {
	args := []string{"-addstore", "-f", "ROOT", certPath}
	if user {
		args = append([]string{"-user"}, args...)
	}
	return exec.CommandContext(ctx, "certutil", args...)
}

// noTrustPrompt says whether certutil's failed add was ERROR_NOT_SUPPORTED: by
// its exit code, the HRESULT it failed with, or by that code in its output,
// which no locale translates.
func noTrustPrompt(out []byte, err error) bool {
	var exit *exec.ExitError
	if errors.As(err, &exit) && uint32(exit.ExitCode()) == hrNotSupported {
		return true
	}
	return bytes.Contains(out, []byte("0x80070032"))
}

// userDeclined says whether certutil's failed add was ERROR_CANCELLED, the
// user's No to Windows' prompt: by its exit code, the HRESULT it failed with,
// or by that code in its output, which no locale translates (certutil prints
// it in lower case).
func userDeclined(out []byte, err error) bool {
	var exit *exec.ExitError
	if errors.As(err, &exit) && uint32(exit.ExitCode()) == hrCancelled {
		return true
	}
	return bytes.Contains(bytes.ToLower(out), []byte("0x800704c7"))
}

// declinedRecordName is the file, beside the CA keploy keeps for this user,
// that records the user's No to Windows' prompt to trust it. A later run
// whose add cannot ask (no desktop), standard user or elevated, reads it, and
// puts the CA in no store, not the machine's, not a JDK's, as the run that
// heard the No did. A Yes, or the CA found in a Windows ROOT store (the user
// trusted it by hand), removes it.
const declinedRecordName = "trust-declined"

// declinedRecord is the record's path beside the kept CA keptAt, "" when
// there is no place for it: keploy keeps no CA for this user, or keptAt is
// not an absolute path.
func declinedRecord(keptAt string) string {
	if keptAt == "" || !filepath.IsAbs(keptAt) {
		return ""
	}
	return filepath.Join(filepath.Dir(keptAt), declinedRecordName)
}

// noteDeclined records the user's No beside the kept CA keptAt.
func noteDeclined(logger *zap.Logger, keptAt string) {
	p := declinedRecord(keptAt)
	if p == "" {
		return
	}
	if err := os.WriteFile(p, []byte("the user answered No to Windows' prompt to trust keploy's CA\n"), 0600); err != nil {
		logger.Warn("could not record the No to Windows' prompt to trust keploy's CA: a later elevated run with no desktop, which cannot ask, may add keploy's CA to the machine's store (LocalMachine\\Root)", zap.String("path", p), zap.Error(err))
	}
}

// clearDeclined removes the record of a No beside the kept CA keptAt.
func clearDeclined(logger *zap.Logger, keptAt string) {
	p := declinedRecord(keptAt)
	if p == "" {
		return
	}
	if err := os.Remove(p); err != nil && !errors.Is(err, os.ErrNotExist) {
		logger.Debug("could not remove the record of the user's No to Windows' prompt to trust keploy's CA", zap.String("path", p), zap.Error(err))
	}
}

// declinedBefore says whether an earlier run recorded the user's No beside
// the kept CA keptAt.
func declinedBefore(keptAt string) bool {
	p := declinedRecord(keptAt)
	if p == "" {
		return false
	}
	_, err := os.Lstat(p)
	return err == nil
}

// envTrustingClients are the runtimes the app is given keploy's CA for, in
// the env vars the client sets (SetupCaCertEnv): SSL_CERT_FILE takes the
// place of the Windows store for Go from go 1.27 on
// (x509sslcertoverrideplatform). storeOnlyClients read only the Windows
// store, so they trust keploy's CA only when it is there.
const (
	envTrustingClients = "Node, Python, OpenSSL-based clients, Cargo, Java, and Go programs whose go.mod says go 1.27 or later"
	storeOnlyClients   = ".NET, PowerShell, and Go programs whose go.mod says an older go"
)

// trustOnWindows puts keploy's CA in the user's Windows trust store
// (installWindowsCA). A session that cannot show Windows' trust prompt
// (errNoTrustPrompt) is not a failed setup: the app keploy starts is given
// keploy's CA where its runtime reads one (SetupCaCertEnv:
// envTrustingClients), and a client that reads only the Windows store
// (storeOnlyClients) rejects keploy's certificate, each such handshake charged
// as a capture loss that names the one step, with no administrator, that
// makes it trusted (UntrustedStoreWhy). keptAt is the certificate keploy keeps
// across runs, "" when it keeps none.
//
// A user who answered No to Windows' prompt (errUserDeclined), in this run
// or, recorded, in an earlier one this run could not ask again
// (errDeclinedEarlier), gets the same run, and declined is true: keploy's CA
// then goes in no trust store, so the caller adds it to no JDK keystore
// either. The warning says what does not work and how to opt in.
func trustOnWindows(ctx context.Context, logger *zap.Logger, certPath string, der []byte, keptAt string) (declined bool, err error) {
	err = installWindowsCA(ctx, logger, certPath, der, keptAt)
	switch {
	case errors.Is(err, errDeclinedEarlier):
		noteUntrustedStore("keploy's CA is not in this user's Windows trust store, the only one " + storeOnlyClients + " read, because Windows' prompt to trust it was answered No in an earlier run, and this run could not ask again: " + declinedEarlierStep(keptAt))
		logger.Warn("Windows' prompt to trust keploy's CA was answered No in an earlier run, and this run could not ask again, so keploy added its CA to no trust store (not yours, not the machine's, not a JDK's). The app is given keploy's CA where its runtime reads one ("+envTrustingClients+"); a client that reads only the Windows store ("+storeOnlyClients+", among them) rejects keploy's certificate, so its HTTPS calls fail and are neither recorded nor mocked",
			zap.String("record_of_the_no", declinedRecord(keptAt)), zap.String("next_step", declinedEarlierStep(keptAt)))
		return true, nil
	case errors.Is(err, errUserDeclined):
		noteUntrustedStore("keploy's CA is not in this user's Windows trust store, the only one " + storeOnlyClients + " read, because Windows' prompt to trust it was answered No: " + optInStep(keptAt))
		logger.Warn("Windows' prompt to trust keploy's CA was answered No, so keploy added it to no trust store (not yours, not the machine's, not a JDK's). The app is given keploy's CA where its runtime reads one ("+envTrustingClients+"); a client that reads only the Windows store ("+storeOnlyClients+", among them) rejects keploy's certificate, so its HTTPS calls fail and are neither recorded nor mocked",
			zap.String("next_step", optInStep(keptAt)))
		return true, nil
	case errors.Is(err, errNoTrustPrompt):
		noteUntrustedStore("keploy's CA is not in this user's Windows trust store, the only one " + storeOnlyClients + " read, because this session has no desktop for Windows' prompt to trust it: " + trustStep(keptAt))
		logger.Warn("this session cannot show Windows' prompt to trust keploy's CA: the app is given keploy's CA where its runtime reads one ("+envTrustingClients+"), and a TLS call of a client that reads only the Windows store ("+storeOnlyClients+", among them) is refused as a capture loss",
			zap.String("next_step", trustStep(keptAt)))
		return false, nil
	default:
		noteUntrustedStore("")
		return false, err
	}
}

// optInStep is how a user who answered No to Windows' prompt to trust
// keploy's CA opts in later, with no administrator.
func optInStep(keptAt string) string {
	if keptAt == "" {
		return "to have those calls trusted, recorded and mocked, run keploy again and answer Yes to Windows' prompt to trust its CA (no administrator is needed); keploy keeps no CA for this user (LOCALAPPDATA is not set), so each run asks again"
	}
	return "to have those calls trusted, recorded and mocked, run keploy again and answer Yes to Windows' prompt to trust its CA, or run 'certutil -user -addstore ROOT \"" + keptAt + "\"' (no administrator is needed for either). keploy keeps that CA, so later runs reuse it with no prompt"
}

// declinedEarlierStep is how a user whose No an earlier run recorded beside
// the kept CA keptAt opts in, from a run that cannot ask: where Windows can
// ask, or by dropping the record.
func declinedEarlierStep(keptAt string) string {
	return "to have those calls trusted, recorded and mocked, trust keploy's CA from a session signed in to the desktop: run keploy there and answer Yes to Windows' prompt, or run 'certutil -user -addstore ROOT \"" + keptAt + "\"' there (no administrator is needed for either); keploy then finds it in your store and drops the record of the No. Or delete '" + declinedRecord(keptAt) + "' to let an elevated run with no desktop add it to the machine's store (LocalMachine\\Root), which every user of this machine trusts"
}

// trustStep is the one step, with no administrator, that puts keploy's CA in
// the user's Windows trust store.
func trustStep(keptAt string) string {
	if keptAt == "" {
		return "run keploy from a session signed in to the desktop and accept Windows' prompt to trust its CA (no administrator is needed); keploy keeps no CA for this user (LOCALAPPDATA is not set), so each run asks again"
	}
	return "trust keploy's CA once, with no administrator, from a session signed in to the desktop: run keploy there and accept Windows' prompt, or run 'certutil -user -addstore ROOT \"" + keptAt + "\"' there. keploy keeps that CA, so later runs, with a desktop or not, reuse it with no prompt"
}

// untrustedStore is UntrustedStoreWhy's answer, set by each Windows CA setup.
var untrustedStore atomic.Pointer[string]

func noteUntrustedStore(why string) { untrustedStore.Store(&why) }

// UntrustedStoreWhy is why a client that reads only the Windows trust store
// rejects keploy's certificate in this run, with the step that fixes it; ""
// when that store holds keploy's CA, or off Windows.
func UntrustedStoreWhy() string {
	if p := untrustedStore.Load(); p != nil {
		return *p
	}
	return ""
}
