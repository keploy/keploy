//go:build linux

package appns

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"go.keploy.io/server/v3/internal/ownthread"
	"go.uber.org/zap"
	"golang.org/x/sys/unix"
)

// confPath is the file a view replaces; a variable so that a test can name a
// file of its own.
var confPath = "/etc/nsswitch.conf"

// sourceName is the tmpfs the copy lives on: what /proc/self/mountinfo shows
// as the source of the application's /etc/nsswitch.conf.
const sourceName = "keploy-nsswitch"

// fallbackSaid makes the fallback's INFO line once per keploy process: an
// application started again for each test set falls back each time alike.
var fallbackSaid sync.Once

// leftoverSaid makes the INFO line about a hosts line an earlier keploy left
// (sayLeftover) once per keploy process.
var leftoverSaid sync.Once

// Start starts cmd as cmd.Start does, where the host's /etc/nsswitch.conf
// sends glibc's name lookups to another process that resolves them (the rule
// in hosts.go), in a mount namespace of its own, in which /etc/nsswitch.conf
// is the view HostsView makes of it: cmd's processes resolve names by a way
// keploy sees, and no other process sees the view. Where the host's file
// needs none, cmd starts as cmd.Start starts it.
//
// cmd is the same *exec.Cmd, started the same way, by the same os/exec call,
// whichever way it starts: its process group, signals, environment, working
// directory, standard streams and credentials are what the caller set, and so
// is everything the process inherits, its signals' dispositions and limits
// included, as no process runs between keploy and cmd. Go cannot run code in
// the child between its fork and its exec, but the child takes the mount
// namespace of the thread that forks it: the namespace is made on a thread of
// keploy's own (never its main thread), which a goroutine locks and never
// unlocks, that thread starts cmd, and stays, blocked, until the caller calls
// done (once cmd's Wait has returned: the thread that started a process is
// its parent thread, whose exit a parent-death signal would see). The runtime
// then ends it, as it ends every thread whose goroutine exits locked
// (ownthread.Run): no other goroutine ever runs in the namespace, and the
// runtime starts no thread from a locked one.
//
// The namespace is a copy of keploy's, a slave of it: a mount made on the
// host later still reaches cmd's processes (an automount, say), and none of
// theirs reaches the host. Its /etc/nsswitch.conf is a bind mount of a file on
// a read-only tmpfs that only that mount still holds, so nothing of the view
// is ever written to the host's filesystem, a write to it is refused to every
// process in the namespace (one with CAP_SYS_ADMIN there, the application of
// a keploy run as root, could still remount or unmount it), and it is gone
// once the last process in the namespace has exited, however keploy ends:
// there is nothing to restore, after a SIGKILL or a crash, and no other run of
// keploy to clobber.
//
// Making the namespace takes CAP_SYS_ADMIN, which keploy run the usual way,
// as the user (the agent alone is elevated, through sudo), does not have.
// Where it cannot be made, cmd starts as cmd.Start starts it, and keploy says
// so once, at INFO, with what it means and the remedy (sayFallback): the
// host's file is never rewritten in its place. A keploy binary given
// CAP_SYS_ADMIN as a permitted file capability alone (setcap
// cap_sys_admin+p) makes it: keploy raises the capability on the thread that
// makes the view, and on no other (raiseSysAdmin), and the application still
// runs as the user who ran keploy, with no capability: exec gives a program
// without file capabilities of its own none of its parent's.
func Start(logger *zap.Logger, cmd *exec.Cmd) (done func(), err error) {
	done = func() {}
	if cmd.Err != nil {
		return done, cmd.Start() // cmd.Start reports why it cannot run
	}
	host, err := os.ReadFile(confPath)
	if err != nil {
		// None (glibc's default order, dns then files) or none keploy can
		// read, which the application cannot read either.
		return done, cmd.Start()
	}
	v, differs := HostsView(host)
	if !differs {
		if leftByEarlierKeploy(host) && authselectManages() {
			leftoverSaid.Do(func() { sayLeftover(logger) })
		}
		return done, cmd.Start()
	}

	type started struct{ view, start error }
	result := make(chan started, 1)
	release := make(chan struct{})
	ownthread.Run(func() {
		if err := enterView(logger, v.Conf); err != nil {
			result <- started{view: err}
			return
		}
		err := cmd.Start()
		result <- started{start: err}
		if err == nil {
			<-release
		}
	})
	r := <-result
	if r.view != nil {
		fallbackSaid.Do(func() { sayFallback(logger, v, r.view) })
		return done, cmd.Start()
	}
	if r.start != nil {
		return done, r.start
	}
	logger.Debug("the application sees /etc/nsswitch.conf of its own, in a mount namespace of its own; the host's file is untouched",
		zap.String("hosts", v.From), zap.String("view", v.To), zap.Int("pid", cmd.Process.Pid))
	var once sync.Once
	return func() { once.Do(func() { close(release) }) }, nil
}

// sayFallback says, at INFO, that the application starts without the view v,
// for the reason err, what that means and what to do about it.
func sayFallback(logger *zap.Logger, v View, err error) {
	remedy := "keploy could not make the view: the error says why."
	switch exe, xerr := executable(); {
	case errors.Is(err, unix.EPERM) && os.Geteuid() == 0:
		remedy = "keploy runs as root without CAP_SYS_ADMIN, or under a seccomp profile that refuses unshare (a container's defaults): give keploy CAP_SYS_ADMIN."
	case errors.Is(err, unix.EPERM) && holdsSysAdmin():
		remedy = "keploy holds CAP_SYS_ADMIN (from a file capability, or an ambient one) and still was refused: a seccomp profile or a security module refuses it unshare or mount."
	case errors.Is(err, unix.EPERM) && xerr == nil && hasFileCaps(exe):
		remedy = "keploy's binary " + exe + " has a file capability, but keploy does not hold CAP_SYS_ADMIN from it: the binary is on a nosuid mount, " +
			"keploy runs with no_new_privs, the capability bounding set lacks CAP_SYS_ADMIN (in a container), or the capability is not cap_sys_admin."
	case errors.Is(err, unix.EPERM):
		bin, ownerOnly := "$(command -v keploy)", false
		if xerr == nil {
			bin = shellQuote(exe)
			st, serr := os.Stat(exe)
			ownerOnly = serr == nil && st.Mode().Perm()&0o011 == 0
		}
		steps := "sudo chown root " + bin + " && sudo chmod go-w " + bin
		if ownerOnly {
			// Only its owner may run it, and once root owns it that owner
			// could not: an ACL entry lets that one user (by uid) run it,
			// and no one else, as before. First, so that where setfacl
			// fails (no acl package, a filesystem without ACLs) nothing
			// has changed.
			steps = "sudo setfacl -m u:" + strconv.Itoa(os.Getuid()) + ":rx " + bin + " && " + steps
		}
		remedy = "Making the view takes CAP_SYS_ADMIN, which keploy run as a user does not have. " +
			"To have keploy make it and still run the application as you, with no capability, make the keploy binary root's and writable by root alone, then give it that capability: " +
			steps + " && sudo setcap cap_sys_admin+p " + bin +
			" (a file capability on a binary its user can change is that user's to keep, as a change through a shared writable mapping keeps it). " +
			"Do it again after each update of keploy, which replaces the binary with one anyone may run: restrict it again first, if you had " +
			"(to a group: chgrp and chmod 0750; to you alone: chmod go-rx, then setfacl -m u:" + strconv.Itoa(os.Getuid()) + ":rx, as a chmod go-rx after it empties the ACL's mask, which chmod go-w does not). " +
			"Mind that every user who can run that binary then holds CAP_SYS_ADMIN, which is close to root, in keploy's process " +
			"(keploy makes it effective only on the thread that makes the view): on a machine others share, let only a group you trust run it. " +
			"Running keploy as root (sudo -E keploy ...) makes the view too, but runs the application as root."
	}
	logger.Info("keploy starts the application without a view of /etc/nsswitch.conf of its own. "+
		"This host's hosts line has glibc send the application's DNS lookups to systemd-resolved, which queries from its own process, out of keploy's sight: "+
		"in keploy record they are not recorded as mocks, and in keploy test they resolve live, "+
		"so a name that does not resolve at replay time fails the lookup (keploy answers such a name itself only for a lookup it sees). "+
		"The host's file is left as it is. "+remedy,
		zap.String("hosts", v.From), zap.String("view", v.To), zap.Error(err))
}

// authselectConf, authselectSelected and authselectVendor are the
// nsswitch.conf authselect generates, which /etc/nsswitch.conf links to while
// authselect manages it, the file naming the profile it generates it from,
// and the directory of vendor profiles, which authselect looks a profile's
// name up in before its default ones; variables so that a test can name its
// own.
var (
	authselectConf     = "/etc/authselect/nsswitch.conf"
	authselectSelected = "/etc/authselect/authselect.conf"
	authselectVendor   = "/usr/share/authselect/vendor"
)

// authselectManages reports whether authselect manages the host's
// /etc/nsswitch.conf from one of the default profiles it ships: the file is a
// link to the one authselect generates (earlier keploy versions wrote through
// that link and kept it; an opted-out host's is a file of its own, which its
// administrator may edit, and where authselect apply-changes does nothing),
// and the profile selected is neither a custom one nor a vendor one, whose
// hosts line their authors wrote.
func authselectManages() bool {
	at, err := filepath.EvalSymlinks(confPath)
	if err != nil {
		return false
	}
	generated, err := filepath.EvalSymlinks(authselectConf)
	if err != nil || at != generated {
		return false
	}
	selected, err := os.ReadFile(authselectSelected)
	if err != nil {
		return false
	}
	profile, _, _ := strings.Cut(string(selected), "\n")
	profile = strings.TrimSpace(profile)
	if profile == "" || strings.ContainsRune(profile, '/') { // custom/NAME, or no profile's name
		return false
	}
	_, err = os.Stat(filepath.Join(authselectVendor, profile))
	return errors.Is(err, fs.ErrNotExist)
}

// sayLeftover says, at INFO, that the host's /etc/nsswitch.conf has the hosts
// line an earlier keploy left in a file authselect manages
// (leftByEarlierKeploy, authselectManages), what that has meant for the host,
// and how to put authselect's own line back. keploy does not put it back: it
// never writes the host's file.
func sayLeftover(logger *zap.Logger) {
	logger.Info("This host's /etc/nsswitch.conf, which authselect manages, has the hosts line \""+earlierRewrite+"\": "+
		"no default profile authselect ships writes it, and earlier keploy versions wrote it while keploy test ran, "+
		"leaving it there when such a run was killed or crashed, or when two runs overlapped. "+
		"Unless you wrote it, every process on this host has looked names up through /etc/hosts and DNS alone since, not through authselect's hosts line "+
		"(nss-resolve, myhostname, and any mDNS, libvirt or NIS module the profile adds): where /etc/resolv.conf names systemd-resolved's stub (127.0.0.53, Fedora's default), "+
		"DNS still reaches systemd-resolved; where it names other servers, lookups bypass it. keploy no longer writes this file. "+
		"To put authselect's hosts line back: sudo authselect apply-changes -b (it writes every file authselect manages again from its profile, PAM's included; "+
		"-b keeps a copy of them under /var/lib/authselect/backups). authselect check does not notice the change.",
		zap.String("file", confPath))
}

// executable is keploy's binary, which the fallback's remedy names; a
// variable so that a test can name another.
var executable = os.Executable

// shellQuote is s as one word of a POSIX shell's command line: as it is,
// where it holds nothing a shell reads specially, else in single quotes.
func shellQuote(s string) string {
	if s != "" && strings.Trim(s, "abcdefghijklmnopqrstuvwxyzABCDEFGHIJKLMNOPQRSTUVWXYZ0123456789_./+-:@%,=") == "" {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// holdsSysAdmin reports whether keploy holds CAP_SYS_ADMIN, permitted.
func holdsSysAdmin() bool {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	return unix.Capget(&hdr, &data[0]) == nil && data[0].Permitted&(uint32(1)<<unix.CAP_SYS_ADMIN) != 0
}

// hasFileCaps reports whether the file at path has a file capability.
func hasFileCaps(path string) bool {
	n, err := unix.Getxattr(path, "security.capability", nil)
	return err == nil && n > 0
}

// enterView moves the calling thread, locked to its goroutine, into a mount
// namespace of its own in which confPath is a read-only bind mount of a file
// holding conf, with the original's mode, owner (where keploy may give it)
// and SELinux label. An error leaves the thread somewhere no process is to be
// started from: its caller discards it.
func enterView(logger *zap.Logger, conf []byte) error {
	lower, err := raiseSysAdmin()
	if err != nil {
		return err
	}
	defer lower()
	if err := unix.Unshare(unix.CLONE_NEWNS); err != nil {
		return fmt.Errorf("make a mount namespace: %w", err)
	}
	if err := unix.Mount("", "/", "", unix.MS_REC|unix.MS_SLAVE, ""); err != nil {
		return fmt.Errorf("make the mount namespace a slave of keploy's: %w", err)
	}
	// The file the view is mounted over (where a symlink, Fedora's, leads),
	// held before anything is mounted, so that the tmpfs below cannot hide
	// it.
	target, err := unix.Open(confPath, unix.O_PATH|unix.O_CLOEXEC, 0)
	if err != nil {
		return fmt.Errorf("open %s: %w", confPath, err)
	}
	defer unix.Close(target)
	held := fmt.Sprintf("/proc/self/fd/%d", target)
	var st unix.Stat_t
	if err := unix.Fstat(target, &st); err != nil {
		return fmt.Errorf("stat %s: %w", confPath, err)
	}
	// The view is written on a tmpfs mounted, in this namespace alone and only
	// until the bind below holds the file, over the directory the path names
	// (/etc): one that is there, as the file is, whatever $TMPDIR says. It is
	// then detached, and the directory, what is mounted under it with it, is
	// the namespace's own again.
	dir := filepath.Dir(confPath)
	const flags = unix.MS_NOSUID | unix.MS_NODEV | unix.MS_NOEXEC
	if err := unix.Mount(sourceName, dir, "tmpfs", flags, "mode=0755,size=1m"); err != nil {
		return fmt.Errorf("mount a tmpfs for the view: %w", err)
	}
	src := filepath.Join(dir, "nsswitch.conf")
	err = writeView(logger, src, held, conf, &st)
	if err == nil {
		// Read-only before the bind, the whole tmpfs (its superblock), so
		// that a write to the view through any mount of it is refused (a
		// process with CAP_SYS_ADMIN in the namespace could still remount or
		// unmount it): a keploy with CAP_SYS_ADMIN alone cannot give the view
		// to root, and the application, which runs as keploy's user, would
		// own it.
		if rerr := unix.Mount("", dir, "", unix.MS_REMOUNT|unix.MS_RDONLY|flags, ""); rerr != nil {
			err = fmt.Errorf("make the view read-only: %w", rerr)
		}
	}
	if err == nil {
		if berr := unix.Mount(src, held, "", unix.MS_BIND, ""); berr != nil {
			err = fmt.Errorf("bind the view over %s: %w", confPath, berr)
		}
	}
	if uerr := unix.Unmount(dir, unix.MNT_DETACH); uerr != nil {
		return fmt.Errorf("detach the view's tmpfs: %w", uerr)
	}
	return err
}

// raiseSysAdmin makes CAP_SYS_ADMIN effective on the calling thread, where
// keploy holds it as permitted alone (a keploy binary given cap_sys_admin+p),
// and returns what puts the thread's effective set back. Capabilities are a
// thread's own: no other thread of keploy's ever has it effective, and this
// one, locked to its goroutine, is discarded with the namespace it made.
// Where keploy holds it effective already (root) or not at all (a user), it
// does nothing: unshare then says whether keploy may.
func raiseSysAdmin() (lower func(), err error) {
	lower = func() {}
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	if err := unix.Capget(&hdr, &data[0]); err != nil {
		return lower, fmt.Errorf("read keploy's capabilities: %w", err)
	}
	const bit = uint32(1) << unix.CAP_SYS_ADMIN // in the first word
	if data[0].Effective&bit != 0 || data[0].Permitted&bit == 0 {
		return lower, nil
	}
	was := data[0].Effective
	data[0].Effective |= bit
	if err := unix.Capset(&hdr, &data[0]); err != nil {
		return lower, fmt.Errorf("make CAP_SYS_ADMIN effective: %w", err)
	}
	return func() {
		data[0].Effective = was
		_ = unix.Capset(&hdr, &data[0])
	}, nil
}

// selinuxXattr is where a file's SELinux label is.
const selinuxXattr = "security.selinux"

// writeView writes conf to src, with the mode (and, where it can, the owner)
// st says and the label the file held at target has.
func writeView(logger *zap.Logger, src, target string, conf []byte, st *unix.Stat_t) error {
	f, err := os.OpenFile(src, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return fmt.Errorf("create the view: %w", err)
	}
	_, werr := f.Write(conf)
	if werr == nil {
		werr = f.Chmod(os.FileMode(st.Mode & 0o777))
	}
	if werr == nil {
		// The owner too, where keploy may give it: not without CAP_CHOWN (a
		// keploy given CAP_SYS_ADMIN alone, whose own the view then is), nor
		// to an owner a user namespace does not map. The view reads the same
		// either way, by the mode above, and no one can write it (enterView).
		if err := f.Chown(int(st.Uid), int(st.Gid)); err != nil {
			logger.Debug("the application's view of /etc/nsswitch.conf is keploy's user's, not the original's owner's", zap.Error(err))
		}
	}
	if werr == nil {
		if err := labelLike(f, target); err != nil {
			logger.Debug("the application's view of /etc/nsswitch.conf keeps its tmpfs's SELinux label, not the original's", zap.Error(err))
		}
	}
	if cerr := f.Close(); werr == nil {
		werr = cerr
	}
	if werr != nil {
		return fmt.Errorf("write the view: %w", werr)
	}
	return nil
}

// labelLike gives f the SELinux label of the file at path, where it has one:
// a file on a tmpfs is labelled for a tmpfs, which a confined domain may not
// be let read where it reads /etc/nsswitch.conf. A host without SELinux has
// no label to copy (nil); one whose policy does not let keploy relabel keeps
// the tmpfs's (the error): the view's content is the same either way.
func labelLike(f *os.File, path string) error {
	size, err := unix.Getxattr(path, selinuxXattr, nil)
	if errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || (err == nil && size == 0) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("read the label of %s: %w", confPath, err)
	}
	label := make([]byte, size)
	n, err := unix.Getxattr(path, selinuxXattr, label)
	if err != nil {
		return fmt.Errorf("read the label of %s: %w", confPath, err)
	}
	if err := unix.Fsetxattr(int(f.Fd()), selinuxXattr, label[:n], 0); err != nil {
		return fmt.Errorf("label the view %q: %w", label[:n], err)
	}
	return nil
}
