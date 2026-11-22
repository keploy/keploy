package models

import (
	"strings"
	"testing"
)

// A parser that panicked is said as itself, a bug in keploy whose call was
// neither answered nor recorded, and fails the tests it taints: it is no
// call keploy cannot capture at all.
func TestAParserPanicIsSaidAndJudged(t *testing.T) {
	lost := CaptureLosses{{Kind: CaptureLossParserPanic, Count: 2, What: "the http parser, on a connection to 10.0.0.5:80"}}
	if text := lost.Text(); !strings.Contains(text, "parser panicked 2 time(s)") || !strings.Contains(text, "a bug in keploy") {
		t.Fatalf("Text = %q", text)
	}
	if judged, notJudged := lost.Split(); len(judged) != 1 || len(notJudged) != 0 {
		t.Fatalf("Split = %+v, %+v; want it judged", judged, notJudged)
	}
}
