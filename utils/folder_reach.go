package utils

import (
	"fmt"
	"strings"
)

// FolderOutOfReachError is the refusal of a native record or test whose
// keploy folder holds paths the signed-in user cannot read or write (left by
// an older keploy run under sudo), on macOS, where keploy runs as that user
// and never asks for an administrator password. It carries what a caller
// needs to say so as data: the folder, the user and the command that gives
// the folder back (FixCommand), for the person to run in their own terminal
// before running keploy again.
type FolderOutOfReachError struct {
	// Folder is the keploy folder.
	Folder string
	// User is the signed-in user's name, empty when keploy cannot tell.
	User string
	// Paths is how many paths in the folder are out of the user's reach.
	Paths int
}

func (e *FolderOutOfReachError) Error() string {
	return fmt.Sprintf("%d path(s) in the keploy folder %s cannot be read or written by your user, and keploy does not ask for an administrator password on macOS. Give them back to your user in your own terminal, then run keploy again: %s",
		e.Paths, e.Folder, e.FixCommand())
}

// FixCommand is the command that gives the folder back to the user, each
// word quoted for a POSIX shell where it has to be: sudo chown -R <user>
// <folder>, with $USER for a user keploy cannot name.
func (e *FolderOutOfReachError) FixCommand() string {
	username := "$USER"
	if e.User != "" {
		username = shellWord(e.User)
	}
	return "sudo chown -R " + username + " " + shellWord(e.Folder)
}

// shellWord quotes s for a POSIX shell when it holds anything but letters,
// digits and the punctuation a path or user name is safe with unquoted.
func shellWord(s string) string {
	safe := s != ""
	for _, r := range s {
		if !shellSafeRune(r) {
			safe = false
			break
		}
	}
	if safe {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// shellSafeRune reports whether r is a letter, a digit or punctuation a POSIX
// shell leaves alone in an unquoted word.
func shellSafeRune(r rune) bool {
	return r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("/._-+@%:,=", r)
}
