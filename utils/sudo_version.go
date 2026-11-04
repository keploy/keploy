package utils

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"time"
)

// sudoVersionBudget bounds `sudo --version`, which prints and exits without
// asking for a password.
const sudoVersionBudget = 5 * time.Second

// SudoVersion is what `sudo --version` prints on stdout and stderr, or "" if
// there is no sudo on PATH. Classic sudo prints "Sudo version 1.9.15p5" on
// stdout, in the C locale (it translates that line); sudo-rs prints
// "sudo-rs 0.2.5" on stderr.
func SudoVersion(ctx context.Context) string {
	ctx, cancel := context.WithTimeout(ctx, sudoVersionBudget)
	defer cancel()
	cmd := exec.CommandContext(ctx, "sudo", "--version")
	cmd.Env = append(os.Environ(), "LC_ALL=C")
	cmd.WaitDelay = time.Second
	out, _ := cmd.CombinedOutput()
	return string(out)
}

var (
	sudoRsVersionLine = regexp.MustCompile(`(?m)^sudo-rs (\d+)\.(\d+)\.(\d+)`)
	sudoVersionLine   = regexp.MustCompile(`(?m)^Sudo version (\d+)\.(\d+)\.(\d+)`)
)

// SudoListGap is a sudo that would not hand its command the variables a
// --preserve-env=NAME,… list names (SudoPreserveEnvListGap).
type SudoListGap struct {
	// Sudo is the implementation, "sudo" or "sudo-rs", and Version its
	// version, as `sudo --version` said.
	Sudo    string
	Version [3]int
	// Ignored is whether that sudo runs its command without the variables,
	// with only a warning. Otherwise it refuses the whole command as a usage
	// error.
	Ignored bool
	// Since is the first version of that implementation that keeps them.
	Since [3]int
}

// String is the sudo and its version, as "sudo-rs 0.2.5".
func (g SudoListGap) String() string {
	return fmt.Sprintf("%s %s", g.Sudo, versionString(g.Version))
}

// SinceString is Since, as "0.2.7".
func (g SudoListGap) SinceString() string { return versionString(g.Since) }

// SudoPreserveEnvListGap says whether the sudo that printed version (see
// SudoVersion) would not hand its command the variables a
// --preserve-env=NAME,… list names, and which sudo that is; ok is false when
// nothing says it would not.
//
// Classic sudo from 1.8.21 on, and sudo-rs from 0.2.7 on, either keep the
// variables or refuse to run the command at all. They refuse when the sudoers
// rule has neither SETENV nor the variable in env_keep, and that refusal
// already fails loudly. These are the gaps:
//   - sudo-rs before 0.2.7 parses --preserve-env=NAME, warns that it "has not
//     yet been implemented and will be ignored", and runs the command without
//     the variable (sudo-rs's CHANGELOG for 0.2.7, and src/sudo/pipeline.rs up
//     to 0.2.6; seen with Debian trixie's sudo-rs 0.2.5).
//   - sudo before 1.8.21 rejects --preserve-env=NAME as a usage error. The list
//     form arrived in 1.8.21, according to sudo's NEWS.
//
// Any other sudo, and output that names no version, is let through.
func SudoPreserveEnvListGap(version string) (gap SudoListGap, ok bool) {
	if v, ok := parseVersion(sudoRsVersionLine, version); ok && !atLeast(v, sudoRsListSince) {
		return SudoListGap{Sudo: "sudo-rs", Version: v, Ignored: true, Since: sudoRsListSince}, true
	}
	if v, ok := parseVersion(sudoVersionLine, version); ok && !atLeast(v, sudoListSince) {
		return SudoListGap{Sudo: "sudo", Version: v, Since: sudoListSince}, true
	}
	return SudoListGap{}, false
}

// The first versions that keep a --preserve-env=NAME,… list
// (SudoPreserveEnvListGap): one value each, for the check and for what a
// refusal tells the user to upgrade to.
var (
	sudoRsListSince = [3]int{0, 2, 7}
	sudoListSince   = [3]int{1, 8, 21}
)

func parseVersion(line *regexp.Regexp, out string) ([3]int, bool) {
	m := line.FindStringSubmatch(out)
	if m == nil {
		return [3]int{}, false
	}
	var v [3]int
	for i := range v {
		n, err := strconv.Atoi(m[i+1])
		if err != nil {
			return [3]int{}, false
		}
		v[i] = n
	}
	return v, true
}

func atLeast(v, floor [3]int) bool {
	for i := range v {
		if v[i] != floor[i] {
			return v[i] > floor[i]
		}
	}
	return true
}

func versionString(v [3]int) string { return fmt.Sprintf("%d.%d.%d", v[0], v[1], v[2]) }
