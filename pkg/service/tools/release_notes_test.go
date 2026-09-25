package tools

import (
	"strings"
	"testing"

	"go.uber.org/zap"
	"go.uber.org/zap/zapcore"
	"go.uber.org/zap/zaptest/observer"
)

const notes = "## What's new\n- faster replays"

// The release notes are shown after the binary has been replaced, so they
// cannot fail the update: notes that cannot be formatted are shown as they
// came, with a warning that says why. A GLAMOUR_STYLE naming a file that is
// not there is the ordinary way to get there, and it made `keploy update`
// exit 1 over an update that had succeeded.
func TestReleaseNotesThatCannotBeFormattedAreShownAsTheyCame(t *testing.T) {
	t.Setenv("GLAMOUR_STYLE", "/nonexistent/style.json")
	core, logs := observer.New(zapcore.WarnLevel)

	got := releaseNotes(zap.New(core), notes)

	if got != "\n"+notes {
		t.Fatalf("the release notes shown were %q, want them as they came", got)
	}
	warned := logs.FilterMessage("could not format the release notes; showing them unformatted").All()
	if len(warned) != 1 || !strings.Contains(warned[0].ContextMap()["error"].(string), "/nonexistent/style.json") {
		t.Fatalf("the user is not told why the notes are unformatted; logged: %v", logs.All())
	}
}

// ...and notes that can be formatted still are.
func TestReleaseNotesAreFormatted(t *testing.T) {
	t.Setenv("GLAMOUR_STYLE", "dark")
	core, logs := observer.New(zapcore.WarnLevel)

	got := releaseNotes(zap.New(core), notes)

	if !strings.Contains(got, "\x1b[") || !strings.Contains(got, "• ") || !strings.Contains(got, "replays") {
		t.Fatalf("the release notes were not formatted: %q", got)
	}
	if logs.Len() != 0 {
		t.Fatalf("formatting that worked logged: %v", logs.All())
	}
}
