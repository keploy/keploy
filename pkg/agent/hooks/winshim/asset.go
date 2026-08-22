//go:build windows && amd64

package winshim

import (
	"bytes"
	"crypto/sha256"
	_ "embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"go.uber.org/zap"
)

// shimAsset is the compiled interception shim that gets injected into the
// application under test.
//
// It is committed as a prebuilt DLL rather than built by cgo because the Windows
// release binaries are cross-compiled and the shim must link MinHook, which is
// C. Rebuild it with pkg/agent/hooks/winshim/shim/build.sh whenever
// keploy_winshim.c changes; .ci/scripts/check-windows-shim-asset.sh enforces in
// CI that the committed asset matches the source.
//
//go:embed assets/keploy_winshim.dll
var shimAsset []byte

// StageShim writes the interception shim into a session directory, together
// with the sidecar file that tells it which control pipe to use, and returns the
// DLL's path.
//
// Both the agent and the client call this. The client needs it because it is the
// process that injects the DLL into the application; the agent needs it because
// it owns the session and must not depend on the client having gone first.
// Writing is idempotent and byte-compares first, so whichever process arrives
// second is a no-op.
func StageShim(logger *zap.Logger, sessionDir, pipeName string) (string, error) {
	if len(shimAsset) == 0 {
		return "", errors.New("keploy was built without the Windows interception shim; rebuild it with pkg/agent/hooks/winshim/shim/build.sh")
	}
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		return "", fmt.Errorf("failed to create the shim directory %s: %w", sessionDir, err)
	}

	final := ShimPath(sessionDir)

	// Skip the rewrite when an identical copy is already in place. The app may
	// be relaunched many times within one replay run (once per test set), and
	// rewriting a DLL that is currently mapped into a live process fails outright
	// on Windows — the file is held with a mapping lock.
	if existing, err := os.ReadFile(final); err != nil || !bytes.Equal(existing, shimAsset) {
		if err := writeAtomically(sessionDir, final, shimAsset); err != nil {
			return "", err
		}
	}

	// The sidecar is how the shim finds the control pipe in every descendant,
	// regardless of what environment block a parent hands its children.
	conf := []byte(pipeName + "\n")
	if existing, err := os.ReadFile(ShimConfPath(sessionDir)); err != nil || !bytes.Equal(existing, conf) {
		if err := writeAtomically(sessionDir, ShimConfPath(sessionDir), conf); err != nil {
			return "", err
		}
	}

	logger.Debug("staged the Windows interception shim",
		zap.String("path", final),
		zap.Int("bytes", len(shimAsset)),
		zap.String("sha256", shimDigest()),
		zap.String("pipe", pipeName))

	return final, nil
}

// writeAtomically writes data to a temporary file in dir and renames it into
// place, so a concurrent run never observes a half-written DLL and refuses to
// load it.
func writeAtomically(dir, final string, data []byte) error {
	tmp, err := os.CreateTemp(dir, filepath.Base(final)+".*")
	if err != nil {
		return fmt.Errorf("failed to stage %s: %w", final, err)
	}
	tmpName := tmp.Name()
	defer func() {
		if _, statErr := os.Stat(tmpName); statErr == nil {
			_ = os.Remove(tmpName)
		}
	}()

	if _, err := tmp.Write(data); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("failed to write %s: %w", final, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("failed to write %s: %w", final, err)
	}
	// Windows rename does not replace an existing file, and the target may be a
	// stale copy from a previous run in a reused session directory.
	if err := os.Remove(final); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("failed to replace %s (is a previous keploy run still holding it?): %w", final, err)
	}
	if err := os.Rename(tmpName, final); err != nil {
		return fmt.Errorf("failed to install %s: %w", final, err)
	}
	return nil
}

// shimDigest returns the hex sha256 of the embedded shim. Logged at startup so a
// bug report can be tied to an exact shim build without shipping symbols.
func shimDigest() string {
	sum := sha256.Sum256(shimAsset)
	return hex.EncodeToString(sum[:])
}
