// Command fetchredirector downloads the prebuilt Windows redirector static
// library that cgo links into the windows/amd64 build.
//
// The artifact is not committed. It is ~20 MB and a committed binary carries no
// provenance — there is no way to tell which redirector commit produced it, and
// the copy this replaced had gone stale without anyone noticing. Instead
// pkg/agent/hooks/windows/redirector.lock pins a release tag and a SHA-256, and
// this fetches exactly that.
//
// Usage:
//
//	go run ./tools/fetchredirector            # fetch if missing or stale
//	go run ./tools/fetchredirector -force     # re-download unconditionally
//
// It is safe to run on any OS and on every build: when the file on disk already
// matches the pinned digest it does nothing, so wiring it into CI costs one
// hash of a local file.
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

const (
	lockPath = "pkg/agent/hooks/windows/redirector.lock"
	libName  = "libwindows_redirector.a"

	// Azure, not the GitHub release, and that is deliberate. This repository is
	// public while keploy/windows-redirector is private, so a release asset
	// there is unreachable for a fork CI run or an outside contributor —
	// exactly the asymmetry that had the 20 MB .a committed here in the first
	// place. keploy.io/ent/dl fronts the same container that serves the
	// enterprise binaries and is world-readable, so this needs no credentials.
	releaseFm = "https://keploy.io/ent/dl/windows-redirector/%s/%s"
)

func main() {
	force := flag.Bool("force", false, "re-download even when the local file already matches the pinned digest")
	root := flag.String("root", ".", "repository root")
	// -lock and -out exist for consumers that link this package as a MODULE
	// rather than building from a checkout of it — keploy/enterprise being the
	// one that matters. Their ${SRCDIR} is the module cache, which is read-only
	// (dr-xr-xr-x), so the .a cannot be placed next to redirector.go the way it
	// can here. They instead read the pin out of the module and drop the .a in a
	// writable directory, then point the linker at it with CGO_LDFLAGS=-L<dir>;
	// the -l:libwindows_redirector.a in redirector.go's #cgo line resolves from
	// there just as well as from ${SRCDIR}.
	//
	// Keeping the pin in the module (rather than duplicating it downstream) is
	// what makes a keploy bump carry the correct redirector version with it,
	// instead of leaving a second version to remember to update.
	lock := flag.String("lock", "", "path to redirector.lock (default: <root>/"+lockPath+")")
	out := flag.String("out", "", "directory to write "+libName+" into (default: alongside the lock file)")
	flag.Parse()

	if err := run(*root, *lock, *out, *force); err != nil {
		fmt.Fprintf(os.Stderr, "fetchredirector: %v\n", err)
		os.Exit(1)
	}
}

func run(root, lock, out string, force bool) error {
	if lock == "" {
		lock = filepath.Join(root, lockPath)
	}
	version, want, err := readLock(lock)
	if err != nil {
		return err
	}
	if out == "" {
		out = filepath.Dir(lock)
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return fmt.Errorf("creating %s: %w", out, err)
	}
	dest := filepath.Join(out, libName)

	if !force {
		switch got, err := digest(dest); {
		case err == nil && got == want:
			fmt.Printf("fetchredirector: %s already at %s (%s)\n", libName, version, short(want))
			return nil
		case err == nil:
			// Present but wrong: either the pin moved or the file was
			// tampered with. Say which digest is on disk so the reader can
			// tell those two apart.
			fmt.Printf("fetchredirector: local %s is %s, want %s — refetching\n", libName, short(got), short(want))
		case !os.IsNotExist(err):
			return fmt.Errorf("checking %s: %w", dest, err)
		}
	}

	url := fmt.Sprintf(releaseFm, version, libName)
	fmt.Printf("fetchredirector: downloading %s\n", url)
	body, err := download(url)
	if err != nil {
		return err
	}

	// Verify BEFORE writing, so a mismatched artifact never lands where cgo
	// could link it.
	sum := sha256.Sum256(body)
	if got := hex.EncodeToString(sum[:]); got != want {
		return fmt.Errorf("checksum mismatch for %s %s:\n  want %s\n  got  %s\nThe release asset does not match %s. Do not link this; either the pin is wrong or the artifact changed.",
			libName, version, want, got, lock)
	}
	// Write to a sibling temp file and rename, so an interrupted run can never
	// leave a truncated .a in place. A short file would self-heal on the next
	// run (the digest check refetches), but in this run's build the linker would
	// hit a corrupt archive and report something far less obvious than "the
	// download was cut off". Same directory, so the rename stays on one
	// filesystem and is atomic.
	tmp, err := os.CreateTemp(filepath.Dir(dest), "."+libName+".tmp*")
	if err != nil {
		return fmt.Errorf("creating temp file next to %s: %w", dest, err)
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(body); err != nil {
		tmp.Close()
		return fmt.Errorf("writing %s: %w", tmp.Name(), err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", tmp.Name(), err)
	}
	if err := os.Chmod(tmp.Name(), 0o644); err != nil {
		return fmt.Errorf("chmod %s: %w", tmp.Name(), err)
	}
	if err := os.Rename(tmp.Name(), dest); err != nil {
		return fmt.Errorf("installing %s: %w", dest, err)
	}
	fmt.Printf("fetchredirector: wrote %s (%d bytes, %s)\n", dest, len(body), short(want))
	return nil
}

// readLock parses the two pinned fields. The format is deliberately trivial —
// "key = value" with # comments — so it needs no dependency and stays readable
// in a diff, which is the point of pinning at all.
func readLock(path string) (version, sha string, err error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", "", fmt.Errorf("reading %s: %w", path, err)
	}
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "version":
			version = strings.TrimSpace(value)
		case "sha256":
			sha = strings.ToLower(strings.TrimSpace(value))
		}
	}
	if version == "" || sha == "" {
		return "", "", fmt.Errorf("%s must set both version and sha256", path)
	}
	if len(sha) != 64 {
		return "", "", fmt.Errorf("%s: sha256 must be 64 hex characters, got %d", path, len(sha))
	}
	return version, sha, nil
}

func download(url string) ([]byte, error) {
	client := &http.Client{Timeout: 5 * time.Minute}
	resp, err := client.Get(url)
	if err != nil {
		return nil, fmt.Errorf("downloading %s: %w", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("downloading %s: HTTP %s (is the release published and the asset attached?)", url, resp.Status)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("reading %s: %w", url, err)
	}
	return body, nil
}

func digest(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer func() { _ = f.Close() }()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

func short(sha string) string {
	if len(sha) < 12 {
		return sha
	}
	return sha[:12]
}
