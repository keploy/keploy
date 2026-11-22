//go:build linux

package utils

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

// OwnPIDNamespace is the --client-pid-ns value this process says its pid
// namespace by: the inode /proc/self/ns/pid names ("pid:[4026531836]"), in
// decimal, then "@" and this kernel's boot id; the inode alone when the boot
// id cannot be read; "" when the inode cannot be read, so the agent takes the
// number alone, as an older CLI's.
func OwnPIDNamespace() string {
	ino, err := ownPIDNSInode("/proc")
	if err != nil {
		return ""
	}
	boot, _ := bootIDOf("/proc")
	return formatPIDNS(ino, boot)
}

// ClientPIDHere is the pid keploy's CLI said (--client-pid), read in this
// agent's pid namespace beside the namespace the CLI said it runs in
// (--client-pid-ns, OwnPIDNamespace's value):
//   - a CLI in no Linux pid namespace, on another kernel (another boot id,
//     where both are known), or in another namespace (another inode than
//     the agent's own, on any kernel): its pid is not taken. Its number names
//     another process here, or none: a docker agent runs in a pid namespace
//     of its own, which the app's containers join, so a CLI in a CI step
//     container (small pids, like theirs) would otherwise name one of the
//     app's processes, and a CLI on the host one of nothing;
//   - else a process that cannot be the CLI is never taken, whatever else is
//     known: the CLI has its pid in its own namespace, so the process at it
//     must have it as the last of its NSpid, run in the said namespace where
//     its ns/pid can be read, and, where the CLI said the agent's own
//     namespace, be as many namespaces deep as the agent;
//   - the agent's own namespace, on its kernel: the pid is taken;
//   - what the agent cannot compare (a CLI that said no namespace, an older
//     one, or no boot id; a value that does not parse; the agent's own
//     namespace or boot id unreadable; the agent's inode, which the initial
//     namespace has on every kernel, where the kernel is not known) or a
//     process whose status cannot be read: the number alone, as before keploy
//     said the namespace, with Why saying why;
//   - no live process at the pid (it is stat'ed again when its status cannot
//     be read: a process that exits in between): not taken.
func ClientPIDHere(said uint32, saidNS string) ClientPID {
	return ClientPIDIn("/proc", said, saidNS)
}

// clientStatusUnread is called with the /proc dir of the said pid when its
// status cannot be read, before ClientPIDIn stats it again; a test swaps it
// to remove the dir there, as a process that exits in between does.
var clientStatusUnread = func(string) {}

// ClientPIDIn is ClientPIDHere over the /proc at proc.
func ClientPIDIn(proc string, said uint32, saidNS string) ClientPID {
	if said == 0 {
		return ClientPID{}
	}
	if saidNS == ClientPIDNSNone {
		return ClientPID{Kind: ClientPIDElsewhere, Why: fmt.Sprintf("keploy's CLI runs in no Linux pid namespace (macOS or Windows), so its pid %d names no process of this agent's", said)}
	}
	// The namespaces first: a CLI elsewhere said a number of another
	// namespace, whatever process has it here.
	ino, boot, parsed := parseClientPIDNS(saidNS)
	kernel := "" // why the CLI's kernel is not known to be the agent's, or ""
	ownIno, ownErr := ownPIDNSInode(proc)
	if parsed {
		elsewhere := func(how string) ClientPID {
			return ClientPID{Kind: ClientPIDElsewhere, Why: fmt.Sprintf("keploy's CLI runs %s, so its pid %d names another process here, or none", how, said)}
		}
		if boot == "" {
			kernel = "keploy's CLI did not say its kernel's boot id"
		} else if ownBoot, err := bootIDOf(proc); err != nil {
			kernel = "this agent cannot read its kernel's boot id: " + err.Error()
		} else if ownBoot != boot {
			return elsewhere(fmt.Sprintf("on another kernel (boot id %s) than this agent (%s)", boot, ownBoot))
		}
		if ownErr == nil && ownIno != ino {
			return elsewhere(fmt.Sprintf("in another pid namespace (%d) than this agent (%d)", ino, ownIno))
		}
	}
	gone := ClientPID{Kind: ClientPIDGone, Why: fmt.Sprintf("no process has the pid %d keploy's CLI said (the CLI may have exited)", said)}
	dir := filepath.Join(proc, strconv.FormatUint(uint64(said), 10))
	if _, err := os.Stat(dir); err != nil {
		return gone
	}
	nspids, statusErr := readNSpids(filepath.Join(dir, "status"))
	last, unread := said, ""
	if statusErr != nil {
		clientStatusUnread(dir)
		if _, err := os.Stat(dir); err != nil {
			return gone
		}
		unread = fmt.Sprintf("the status of pid %d cannot be read (%v)", said, statusErr)
	} else {
		last = nspids[len(nspids)-1]
	}
	byNumber := func(cannot string) ClientPID {
		why := cannot + fmt.Sprintf(", so the agent takes the CLI's pid %d by its number alone", said)
		if unread != "" && unread != cannot {
			why += " (" + unread + ")"
		}
		return ClientPID{PID: said, Kind: ClientPIDByNumber, Why: why}
	}
	otherPID := fmt.Sprintf("pid %d here is another process than keploy's CLI: it has pid %d in its own pid namespace", said, last)
	if saidNS == "" {
		if last != said {
			return ClientPID{Kind: ClientPIDNotTheCLI, Why: otherPID}
		}
		return byNumber("keploy's CLI did not say its pid namespace (an older keploy CLI)")
	}
	if !parsed {
		if last != said {
			return ClientPID{Kind: ClientPIDNotTheCLI, Why: otherPID}
		}
		return byNumber(fmt.Sprintf("keploy's CLI said a pid namespace that does not parse (%q)", saidNS))
	}
	// A process that is not the CLI: ClientPIDAnother where the CLI is known
	// to run in the agent's namespace (its inode, on its kernel), else
	// ClientPIDNotTheCLI.
	another := ClientPIDNotTheCLI
	if ownErr == nil && kernel == "" {
		another = ClientPIDAnother
	}
	if last != said {
		return ClientPID{Kind: another, Why: otherPID}
	}
	if link, err := os.Readlink(filepath.Join(dir, "ns", "pid")); err == nil {
		if theirs, ok := pidNSLinkInode(link); ok && theirs != ino {
			return ClientPID{Kind: another, Why: fmt.Sprintf("pid %d here runs in another pid namespace (%d) than the one keploy's CLI said (%d): it is another process", said, theirs, ino)}
		}
	}
	if ownErr != nil {
		return byNumber("this agent cannot read its own pid namespace: " + ownErr.Error())
	}
	if statusErr == nil {
		if self, err := readNSpids(filepath.Join(proc, "self", "status")); err == nil && len(self) != len(nspids) {
			return ClientPID{Kind: another, Why: fmt.Sprintf("pid %d here runs %d pid namespaces deep, this agent %d, so it is not in this agent's namespace, which keploy's CLI said: it is another process", said, len(nspids), len(self))}
		}
	}
	if kernel != "" {
		return byNumber(fmt.Sprintf("keploy's CLI said this agent's pid namespace inode (%d), but %s", ino, kernel))
	}
	if unread != "" {
		return byNumber(unread)
	}
	return ClientPID{PID: said}
}

// ownPIDNSInode is the inode of the pid namespace of the process reading
// the /proc at proc.
func ownPIDNSInode(proc string) (uint64, error) {
	link, err := os.Readlink(filepath.Join(proc, "self", "ns", "pid"))
	if err != nil {
		return 0, err
	}
	ino, ok := pidNSLinkInode(link)
	if !ok {
		return 0, fmt.Errorf("%s/self/ns/pid names no pid namespace (%q)", proc, link)
	}
	return ino, nil
}

// bootIDOf is the boot id of the kernel of the /proc at proc.
func bootIDOf(proc string) (string, error) {
	b, err := os.ReadFile(filepath.Join(proc, "sys", "kernel", "random", "boot_id"))
	if err != nil {
		return "", err
	}
	boot := strings.TrimSpace(string(b))
	if boot == "" {
		return "", fmt.Errorf("%s/sys/kernel/random/boot_id is empty", proc)
	}
	return boot, nil
}

// formatPIDNS is a --client-pid-ns value: "<inode>@<boot_id>", or the inode
// alone without a boot id.
func formatPIDNS(ino uint64, boot string) string {
	s := strconv.FormatUint(ino, 10)
	if boot != "" {
		s += "@" + boot
	}
	return s
}

// readNSpids is the NSpid line of a /proc/<pid>/status: the process's pid
// in each pid namespace from the one that /proc was mounted in down to its
// own (the last). A list it returns has at least one pid.
func readNSpids(status string) ([]uint32, error) {
	b, err := os.ReadFile(status)
	if err != nil {
		return nil, err
	}
	for _, line := range strings.Split(string(b), "\n") {
		rest, ok := strings.CutPrefix(line, "NSpid:")
		if !ok {
			continue
		}
		var out []uint32
		for _, f := range strings.Fields(rest) {
			n, err := strconv.ParseUint(f, 10, 32)
			if err != nil {
				return nil, fmt.Errorf("%s: NSpid %q does not parse", status, strings.TrimSpace(rest))
			}
			out = append(out, uint32(n))
		}
		if len(out) == 0 {
			return nil, fmt.Errorf("%s: NSpid is empty", status)
		}
		return out, nil
	}
	return nil, fmt.Errorf("%s has no NSpid line", status)
}
