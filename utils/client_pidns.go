package utils

import (
	"strconv"
	"strings"
)

// ClientPIDNSNone is the --client-pid-ns value of a keploy CLI that runs in
// no Linux pid namespace (macOS, Windows): its pid names no process of a
// Linux agent's.
const ClientPIDNSNone = "none"

// ClientPID is a keploy agent's reading of the pid its keploy CLI said it has
// (--client-pid), beside the pid namespace the CLI said it runs in
// (--client-pid-ns).
type ClientPID struct {
	// PID is the CLI's pid in this agent's pid namespace, or 0 when the agent
	// does not take the said pid for the CLI's.
	PID uint32
	// Why says why the said pid is not taken (PID 0), or, for a pid taken by
	// its number alone, why the agent could not compare the namespaces. It is
	// "" for a pid taken after the comparison, and when the CLI said no pid.
	Why string
	// Kind is how the agent decided.
	Kind ClientPIDKind
}

// ClientPIDKind is how a keploy agent decided about the pid its CLI said.
type ClientPIDKind int

const (
	// ClientPIDTaken: the pid is the CLI's, compared in the agent's own pid
	// namespace on its kernel; or the CLI said no pid (PID 0).
	ClientPIDTaken ClientPIDKind = iota
	// ClientPIDGone: no live process has the pid (the CLI may have exited).
	ClientPIDGone
	// ClientPIDElsewhere: the CLI runs in another pid namespace than the
	// agent's, on another kernel, or in none, so its number is no pid here.
	ClientPIDElsewhere
	// ClientPIDByNumber: the pid is taken by its number alone, as before
	// keploy said the namespace: the namespaces cannot be compared (an older
	// CLI that says none, no boot id, a value that does not parse, the
	// agent's own namespace unreadable) or the process's status cannot be
	// read, and nothing that can be read says it is another process.
	ClientPIDByNumber
	// ClientPIDAnother: the CLI runs in the agent's own namespace (its inode
	// and its kernel's boot id were both compared), but the process at its
	// pid is another one (the CLI may have exited and its pid been reused).
	ClientPIDAnother
	// ClientPIDNotTheCLI: the namespaces cannot be compared, but the process
	// at the pid cannot be the CLI: it has another pid in its own namespace,
	// runs in another namespace than the said one, or deeper than the agent.
	ClientPIDNotTheCLI
)

// parseClientPIDNS reads a --client-pid-ns value, "<inode>@<boot_id>": the
// inode of the CLI's /proc/self/ns/pid and its kernel's boot id (an inode
// names one namespace on one kernel only). The boot id may be missing, when
// the CLI could not read it.
func parseClientPIDNS(s string) (ino uint64, boot string, ok bool) {
	inoS, boot, _ := strings.Cut(s, "@")
	ino, err := strconv.ParseUint(inoS, 10, 64)
	if err != nil || ino == 0 || strings.ContainsAny(boot, " \t\n@") {
		return 0, "", false
	}
	return ino, boot, true
}

// pidNSLinkInode is the inode a /proc/<pid>/ns/pid link names
// ("pid:[4026531836]"), or false for a link of another kind.
func pidNSLinkInode(link string) (uint64, bool) {
	rest, ok := strings.CutPrefix(link, "pid:[")
	if !ok || !strings.HasSuffix(rest, "]") {
		return 0, false
	}
	ino, err := strconv.ParseUint(strings.TrimSuffix(rest, "]"), 10, 64)
	if err != nil || ino == 0 {
		return 0, false
	}
	return ino, true
}
