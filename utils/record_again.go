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

// RecordSetAgainCommand is the shell command that records the test set set
// again in its place, in the run's keploy folder keployDir (the folder that
// holds the sets): keploy record writes every record into a new set, and a
// name already taken gets a -N suffix, so the command first removes the set's
// folder, then records under the set's own name (--metadata name=<set>). It
// records with the application's command app, and with the --path and
// --config-path that name keployDir's parent and configDir (where keploy.yml
// is) when either is not cwd, the folder the command runs from: so keploy
// record reads the keploy.yml and writes the folder the run read. Each word is
// a ShellWord. It is "" when keployDir is empty, when set is not one plain
// folder name of it (empty, ".", "..", or with a path separator), or when
// keploy record would not read name=<set> back as that name, as its
// GetNextTestSetID reads --metadata (TrimSpaces, then ParseMetadata: a name
// with a comma or a space after one, or one it reads as a number, a bool or
// null): keploy then cannot name the set's folder or record into it, and
// a command that removes another folder, or records the set under another
// name, is never printed.
func RecordSetAgainCommand(keployDir, configDir, cwd, set, app string) string {
	if keployDir == "" || set == "" || set == "." || set == ".." || strings.ContainsAny(set, `/\`) {
		return ""
	}
	if meta, err := ParseMetadata(TrimSpaces("name=" + set)); err != nil || meta["name"] != any(set) {
		return ""
	}
	cmd := "rm -rf -- " + ShellWord(filepath.Join(keployDir, set)) + " && keploy record"
	if strings.TrimSpace(app) != "" {
		cmd += " -c " + ShellWord(app)
	}
	if parent := filepath.Dir(filepath.Clean(keployDir)); parent != filepath.Clean(cwd) {
		cmd += " --path " + ShellWord(parent)
	}
	if configDir != "" && filepath.Clean(configDir) != filepath.Clean(cwd) {
		cmd += " --config-path " + ShellWord(configDir)
	}
	return cmd + " --metadata " + ShellWord("name="+set)
}
