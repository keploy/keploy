package report

import (
	"regexp"
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

var ansiSequence = regexp.MustCompile("\x1b\\[[0-9;]*m")

// A diff's colours are added to its lines and change none of their text, so
// the coloured diff, with the colours taken out, is the diff: whatever the
// length of a line. A body field of more than 64 KiB (an image as base64, a
// long text) is one line of the diff, and the line reader read no line past
// that: the diff ended before the field and left out every difference after it.
func TestApplyCliColorsToDiffKeepsEveryLine(t *testing.T) {
	old := models.IsAnsiDisabled
	models.IsAnsiDisabled = false
	t.Cleanup(func() { models.IsAnsiDisabled = old })

	long := strings.Repeat("x", 70000)
	short := "Path: a\n  Expected: 1\n  Actual: 2\n\nPath: b\n  Expected: x\n  Actual: y"
	for name, diff := range map[string]string{
		"short diff":                   short,
		"long value in the middle":     "Path: a\n  Expected: 1\n  Actual: 2\n\nPath: image\n  Expected: " + long + "\n  Actual: " + long + "y\n\nPath: z\n  Expected: p\n  Actual: q",
		"long value as the last line":  short + "\n\nPath: image\n  Expected: " + long + "\n  Actual: " + long + "y",
		"long value as the first line": "Path: " + long + "\n  Expected: 1\n  Actual: 2\n\n" + short,
		"a line of exactly 64 KiB":     "Path: a\n  Expected: " + strings.Repeat("x", 64*1024-len("  Expected: ")) + "\n  Actual: 2",
	} {
		t.Run(name, func(t *testing.T) {
			got := ansiSequence.ReplaceAllString(applyCliColorsToDiff(diff), "")
			if got != diff {
				t.Fatalf("the coloured diff, without its colours, is %d bytes and the diff %d: it lost lines (it ends with %q)",
					len(got), len(diff), got[max(0, len(got)-40):])
			}
		})
	}
}
