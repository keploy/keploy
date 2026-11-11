package utils

import (
	"path/filepath"
	"strings"
)

// ShellWord is s as a POSIX shell reads it as one word: as it stands when it
// is one plain word (letters, digits and /:._-+=,@), else in single quotes,
// each ' in it written '\” . Inside single quotes a shell expands nothing,
// so $, backticks and backslashes stay as they are.
func ShellWord(s string) string {
	plain := s != "" && !strings.ContainsFunc(s, func(r rune) bool {
		return !strings.ContainsRune("/:._-+=,@", r) && (r < 'a' || r > 'z') && (r < 'A' || r > 'Z') && (r < '0' || r > '9')
	})
	if plain {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// ReplacedDir is the folder, beside the keploy folder (so no keploy command
// reads it as a set), that RecordSetAgain moves a set's recordings into
// before it records the set again.
const ReplacedDir = "keploy-replaced"

// RecordAgain is how a next step records a test set again in its place
// (RecordSetAgain): the shell command, and the folder it first moves the
// set's recordings into, which stays until the user deletes it (Replaced is
// "" for a command that records the set in place without moving it). Command
// is "" when keploy cannot print one.
type RecordAgain struct {
	Command  string
	Replaced string
}

// Text is a as a next step says it: the command, then where it moves the
// set's recordings and that they are deleted once the set recorded again
// passes. "" for no command.
func (a RecordAgain) Text() string {
	if a.Command == "" || a.Replaced == "" {
		return a.Command
	}
	return a.Command + " (it first moves the set's recordings out of the keploy folder, into " + a.Replaced + "; delete that folder once the set recorded again passes)"
}

// RecordSetAgain is the shell command that records the test set set again
// in its place, in the run's keploy folder keployDir (the folder that holds
// the sets), for the run whose id is run (as its reports name it). keploy record
// writes every record into a new set, and a name already taken gets a -N
// suffix, so the command first moves the set's folder out of keployDir, to
// <set> in the new folder <set>-<run> of ReplacedDir beside it (Replaced),
// then records under the set's own name (--metadata name=<set>). Nothing is
// removed: if recording fails, the old recordings are there to move back.
// mkdir makes <set>-<run> only where nothing has that name, so a command run
// twice, or a folder already there, stops it, loudly, before it moves
// anything: no earlier copy is overwritten or nested into. It records with
// the application's command app, and with the --path and --config-path that
// name keployDir's parent and configDir (where keploy.yml is) when either is
// not cwd, the folder the command runs from: so keploy record reads the
// keploy.yml and writes the folder the run read. Each word is a ShellWord.
// It is empty when keployDir is empty, when run is empty or has a path
// separator, when set is not one plain folder name of it (empty, ".", "..",
// or with a path separator), or when keploy record would not read name=<set>
// back as that name, as its GetNextTestSetID reads --metadata (TrimSpaces,
// then ParseMetadata: a name with a comma or a space after one, or one it
// reads as a number, a bool or null): keploy then cannot name the set's
// folder or record into it, and a command that moves another folder, or
// records the set under another name, is never printed.
func RecordSetAgain(keployDir, configDir, cwd, set, run, app string) RecordAgain {
	if keployDir == "" || run == "" || strings.ContainsAny(run, `/\`) || set == "" || set == "." || set == ".." || strings.ContainsAny(set, `/\`) {
		return RecordAgain{}
	}
	if meta, err := ParseMetadata(TrimSpaces("name=" + set)); err != nil || meta["name"] != any(set) {
		return RecordAgain{}
	}
	parent := filepath.Dir(filepath.Clean(keployDir))
	replaced := filepath.Join(parent, ReplacedDir)
	moved := filepath.Join(replaced, set+"-"+run)
	cmd := "mkdir -p -- " + ShellWord(replaced) + " && mkdir -- " + ShellWord(moved) +
		" && mv -- " + ShellWord(filepath.Join(keployDir, set)) + " " + ShellWord(filepath.Join(moved, set)) + " && keploy record"
	if strings.TrimSpace(app) != "" {
		cmd += " -c " + ShellWord(app)
	}
	if parent != filepath.Clean(cwd) {
		cmd += " --path " + ShellWord(parent)
	}
	if configDir != "" && filepath.Clean(configDir) != filepath.Clean(cwd) {
		cmd += " --config-path " + ShellWord(configDir)
	}
	return RecordAgain{Command: cmd + " --metadata " + ShellWord("name="+set), Replaced: moved}
}
