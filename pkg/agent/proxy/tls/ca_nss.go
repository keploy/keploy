package tls

import (
	"bytes"
	"context"
	"errors"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"

	"go.uber.org/zap"
)

// Chromium-based browsers (Chrome, Chromium, the Chromium Playwright and
// Puppeteer drive, Electron apps) and other NSS clients on Linux do not read
// the system trust store: they read the NSS shared database in the home of
// the user they run as, $HOME/.pki/nssdb. Without keploy's CA there such a
// client rejects keploy's certificate, and its HTTPS calls are neither
// recorded nor mocked. So a native Linux run adds its CA to that database
// too, beside the system store, NODE_EXTRA_CA_CERTS and the Java truststore,
// under the run's own nickname, and removes it at teardown.
//
// The database is written with NSS's own certutil (Debian/Ubuntu package
// libnss3-tools, Fedora/RHEL nss-tools). Where certutil is missing keploy
// warns once, naming the package, and the run goes on: every other client
// still trusts the CA through the stores above.

// nssTrust trusts the certificate as a CA that issues TLS server
// certificates: the trust NSS clients need to accept keploy's leaf
// certificates, and nothing more (no e-mail or code signing).
const nssTrust = "C,,"

// nssMissingWarned keeps the missing-certutil warning to one per agent
// process: each run's setup would otherwise repeat it.
var nssMissingWarned atomic.Bool

// nssDBDir is the NSS shared database of the user the application runs as:
// the HOME of the app's process when its PID is known, else the home of the
// user who ran keploy with sudo, else the agent's own home. "" when none is
// known.
func nssDBDir(appPID int) string {
	home := ""
	if appPID > 0 {
		// #nosec G304 -- the path is built from an int PID.
		environ, _ := os.ReadFile("/proc/" + strconv.Itoa(appPID) + "/environ")
		home = environValue(environ, "HOME")
	}
	if home == "" {
		if name := os.Getenv("SUDO_USER"); name != "" {
			if u, err := user.Lookup(name); err == nil {
				home = u.HomeDir
			}
		}
	}
	if home == "" {
		home, _ = os.UserHomeDir()
	}
	if home == "" {
		return ""
	}
	return filepath.Join(home, ".pki", "nssdb")
}

// environValue is key's value in a /proc/<pid>/environ image (NUL-separated
// KEY=VALUE entries), "" when it is not set.
func environValue(environ []byte, key string) string {
	prefix := []byte(key + "=")
	for _, kv := range bytes.Split(environ, []byte{0}) {
		if bytes.HasPrefix(kv, prefix) {
			return string(kv[len(prefix):])
		}
	}
	return ""
}

// installNSSCA adds the CA in certPath to the NSS database dir under alias.
// It creates the database when there is none (Chromium would create it at its
// first start, without the CA), first removes the CAs earlier keploy runs
// left there, and leaves what it writes owned by the owner of the database's
// home. It never fails the CA setup: a database keploy cannot write is
// warned about, with what to do.
func installNSSCA(ctx context.Context, logger *zap.Logger, dir, certPath, alias string) {
	if dir == "" {
		logger.Warn("keploy found no home directory for the application's user, so it did not add its CA to the NSS database Chromium-based browsers read ($HOME/.pki/nssdb); a browser there rejects keploy's certificate and its HTTPS calls are neither recorded nor mocked. Set HOME for the application")
		return
	}
	certutil, err := exec.LookPath("certutil")
	if err != nil {
		if nssMissingWarned.CompareAndSwap(false, true) {
			logger.Warn("keploy could not add its CA to the NSS database Chromium-based browsers read: certutil is not installed. Install it (Debian/Ubuntu: apt-get install libnss3-tools; Fedora/RHEL: dnf install nss-tools); until then a browser rejects keploy's certificate and its HTTPS calls are neither recorded nor mocked",
				zap.String("nssdb", dir))
		}
		return
	}
	db := "sql:" + dir
	if _, err := os.Stat(filepath.Join(dir, "cert9.db")); errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			logger.Warn("keploy could not create the NSS database Chromium-based browsers read, so a browser rejects keploy's certificate", zap.String("nssdb", dir), zap.Error(err))
			return
		}
		if out, err := exec.CommandContext(ctx, certutil, "-N", "--empty-password", "-d", db).CombinedOutput(); err != nil {
			logger.Warn("keploy could not create the NSS database Chromium-based browsers read, so a browser rejects keploy's certificate", zap.String("nssdb", dir), zap.ByteString("certutil", out), zap.Error(err))
			giveToHomeOwner(logger, dir)
			return
		}
	}
	sweepNSSLeftovers(ctx, logger, certutil, db, alias)
	out, err := exec.CommandContext(ctx, certutil, "-A", "-d", db, "-n", alias, "-t", nssTrust, "-i", certPath).CombinedOutput()
	giveToHomeOwner(logger, dir)
	if err != nil {
		logger.Warn("keploy could not add its CA to the NSS database Chromium-based browsers read, so a browser rejects keploy's certificate and its HTTPS calls are neither recorded nor mocked", zap.String("nssdb", dir), zap.ByteString("certutil", out), zap.Error(err))
		return
	}
	logger.Debug("added this run's CA to the NSS database", zap.String("nssdb", dir), zap.String("nickname", alias))
}

// sweepNSSLeftovers removes from the database the CAs of earlier keploy runs
// a crash left there (their keys died with those runs, so they are inert),
// and nothing else.
func sweepNSSLeftovers(ctx context.Context, logger *zap.Logger, certutil, db, alias string) {
	out, err := exec.CommandContext(ctx, certutil, "-L", "-d", db).Output()
	if err != nil {
		logger.Debug("could not list the NSS database to remove earlier runs' CAs", zap.String("nssdb", db), zap.Error(err))
		return
	}
	for _, nick := range nssLeftovers(out, alias) {
		if err := exec.CommandContext(ctx, certutil, "-D", "-d", db, "-n", nick).Run(); err != nil {
			logger.Debug("could not remove an earlier run's CA from the NSS database", zap.String("nickname", nick), zap.Error(err))
		}
	}
}

// nssLeftovers is every nickname of `certutil -L` output that is an earlier
// keploy run's CA: named keploy-mitm-<run id> (runAlias), and not this run's.
func nssLeftovers(list []byte, alias string) []string {
	var nicks []string
	for _, line := range strings.Split(string(list), "\n") {
		f := strings.Fields(line)
		if len(f) < 2 || !strings.HasPrefix(f[0], "keploy-mitm-") || f[0] == alias {
			continue
		}
		nicks = append(nicks, f[0])
	}
	return nicks
}

// removeNSSCA removes this run's CA from the NSS database it was added to.
func removeNSSCA(ctx context.Context, logger *zap.Logger, dir, alias string) {
	if dir == "" || alias == "" {
		return
	}
	certutil, err := exec.LookPath("certutil")
	if err != nil {
		return // nothing was added: installNSSCA warned
	}
	if out, err := exec.CommandContext(ctx, certutil, "-D", "-d", "sql:"+dir, "-n", alias).CombinedOutput(); err != nil {
		logger.Debug("could not remove this run's CA from the NSS database", zap.String("nssdb", dir), zap.ByteString("certutil", out), zap.Error(err))
	}
}

// giveToHomeOwner hands the database directory, its parent (.pki) and the
// database's files to the owner of the home they are in, when this process
// (keploy under sudo) made them its own: a database the browser's user cannot
// write is one Chromium cannot open.
func giveToHomeOwner(logger *zap.Logger, dir string) {
	home := filepath.Dir(filepath.Dir(dir))
	paths := []string{filepath.Dir(dir), dir}
	for _, name := range []string{"cert9.db", "key4.db", "pkcs11.txt"} {
		paths = append(paths, filepath.Join(dir, name))
	}
	for _, p := range paths {
		if err := chownToOwnerOf(home, p); err != nil {
			logger.Warn("keploy could not give the NSS database back to its home's owner; a browser run as that user may not open it", zap.String("path", p), zap.Error(err))
		}
	}
}

// needsChown reports whether a file owned by fileUID, in a home owned by
// homeUID, is to be handed to homeUID by a process running as euid: only what
// this process owns, and only when the home's owner is another user.
func needsChown(fileUID, euid, homeUID int) bool {
	return fileUID == euid && homeUID != euid
}
