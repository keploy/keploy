package tls

import (
	"bytes"
	"context"
	"errors"
	"os/exec"
	"sync/atomic"

	"go.uber.org/zap"
)

// errNoTrustPrompt is a user-store add certutil failed with ERROR_NOT_SUPPORTED:
// Windows asks the user, in a dialog, whether to trust a new root in their
// ROOT store, and a session with no desktop to show it in (a CI runner's
// service, an OpenSSH session, a scheduled task) cannot ask.
var errNoTrustPrompt = errors.New("this session has no desktop for Windows' prompt to trust keploy's CA")

// hrNotSupported is ERROR_NOT_SUPPORTED as the HRESULT certutil reports it in.
const hrNotSupported = 0x80070032

// The Windows store, certutil and the process token, as installWindowsCA
// reaches them (ca_winstore_windows.go); rows swap them.
var (
	userRootHoldsFn    = userRootHolds
	machineRootHoldsFn = machineRootHolds
	certutilAddFn      = certutilAdd
	tokenElevatedFn    = tokenElevated
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

// trustOnWindows puts keploy's CA in the user's Windows trust store
// (installWindowsCA). A session that cannot show Windows' trust prompt
// (errNoTrustPrompt) is not a failed setup: the app keploy starts is given
// keploy's CA where its runtime reads one (SetupCaCertEnv: Node, Python,
// OpenSSL and curl, Cargo and Java), and a client that reads only the Windows
// store (Go, .NET) rejects keploy's certificate, each such handshake charged
// as a capture loss that names the one step, with no administrator, that
// makes it trusted (UntrustedStoreWhy). keptAt is the certificate keploy keeps
// across runs, "" when it keeps none.
func trustOnWindows(ctx context.Context, logger *zap.Logger, certPath string, der []byte, keptAt string) error {
	err := installWindowsCA(ctx, logger, certPath, der, keptAt)
	if !errors.Is(err, errNoTrustPrompt) {
		noteUntrustedStore("")
		return err
	}
	noteUntrustedStore("keploy's CA is not in this user's Windows trust store, the only one a Go or .NET client reads, because this session has no desktop for Windows' prompt to trust it: " + trustStep(keptAt))
	logger.Warn("this session cannot show Windows' prompt to trust keploy's CA: the app is given keploy's CA where its runtime reads one (Node, Python, OpenSSL and curl, Cargo and Java), and a TLS call of a Go or .NET client, which reads only the Windows store, is refused as a capture loss",
		zap.String("next_step", trustStep(keptAt)))
	return nil
}

// trustStep is the one step, with no administrator, that puts keploy's CA in
// the user's Windows trust store.
func trustStep(keptAt string) string {
	if keptAt == "" {
		return "run keploy from a session signed in to the desktop and accept Windows' prompt to trust its CA (no administrator is needed); keploy keeps no CA for this user (LOCALAPPDATA is not set), so each run asks again"
	}
	return "trust keploy's CA once, with no administrator, from a session signed in to the desktop: run keploy there and accept Windows' prompt, or run 'certutil -user -addstore ROOT " + keptAt + "' there. keploy keeps that CA, so later runs, with a desktop or not, reuse it with no prompt"
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
