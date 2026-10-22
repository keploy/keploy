package learn

import (
	"errors"

	"go.keploy.io/server/v3/pkg/models"
)

// What the learn step learned, legible: a test set's learned files
// (models.LearnedFile, models.LearnedLocalFile in keploy/<set>/) say every
// leaf its mocks learned, with its evidence and how to undo it, for a person
// or an LLM to read. learned.yaml is derived from the mocks and never
// uploaded; learned.local.yaml holds record's full round's words and stays
// on the machine that recorded. Both are written only under the proven
// switch, and only once the project's keploy/.gitignore ignores them.
//
// This build declares the contract only: it writes no learned file, and each
// function below refuses.

// errLearnedNotBuilt is what the learned files' functions return while this
// build writes none.
var errLearnedNotBuilt = errors.New("keploy does not write learned files yet")

// redacted stands in a learned file for a value it may not hold.
const redacted = "(redacted)"

// RedactLearnedValue is value, at path, as a learned file may hold it:
// redacted when a segment of path names a credential, when the recording's
// obfuscator noise (noise, the mock's value regexes) matches it, or when it
// is written as a credential is; else value, cut to at most 128 bytes at the
// start of a rune, the cut marked.
//
// This build holds no value: it redacts each.
func RedactLearnedValue(path, value string, noise []string) string {
	return redacted
}

// Legible is every leaf learned onto the mocks of the test set in setDir
// (keploy/<set>/, or a staging copy of it), as learned.yaml says it: each
// leaf's words learned as noise (noise.words), each variable's and echo's
// words, and each leaf learned whole (noise.req), with the recorded value
// redacted (RedactLearnedValue) and no value of record's full round. It reads
// the mocks only, so it says the same of the same mocks wherever it runs.
//
// This build reads no learned leaf: it returns an error.
func Legible(setDir string) ([]models.LearnedLeaf, error) {
	return nil, errLearnedNotBuilt
}

// EnsureLearnedIgnored makes the project's keploy directory's .gitignore
// (keployDir/.gitignore) ignore every test set's learned files and their
// files being written: it adds each of "/*/learned.yaml",
// "/*/learned.local.yaml" and "/*/.learned-*" it lacks, and keeps every
// other line as it is. It is called under the proven switch, before either
// learned file is written (in cloud mode, before the copy-back); when it
// fails, neither may be written.
//
// This build writes no learned file: it returns an error, so none is.
func EnsureLearnedIgnored(keployDir string) error {
	return errLearnedNotBuilt
}

// MergeLocal merges the learned.local.yaml of the test set in srcSetDir (the
// staging copy, in cloud mode) into that of the set in dstSetDir (the
// project's), by ID: an entry of src replaces dst's of the same ID (the
// staging copy wins), and an entry whose ID no leaf learned on dst's mocks
// has any longer is pruned. The file is written whole, under a temporary
// name renamed into place (models.LearnedTempPrefix).
//
// This build writes no learned file: it returns an error, so none is.
func MergeLocal(dstSetDir, srcSetDir string) error {
	return errLearnedNotBuilt
}
