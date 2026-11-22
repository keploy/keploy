package log

import (
	"bytes"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"

	"go.uber.org/zap/zapcore"
)

// plantedChildValue is the value the test cutters cut; never a real secret.
const plantedChildValue = "planted-child-s3cr3t"

// encodedOnly is a Redactor with no child-output cut: its pass over log
// lines rewrites the value, which child output must not see.
type encodedOnly struct{}

func (encodedOnly) RedactEntry(*zapcore.Entry) {}
func (encodedOnly) RedactField(*zapcore.Field) {}
func (encodedOnly) RedactEncoded(text string) string {
	return strings.ReplaceAll(text, plantedChildValue, "[LOG-LINE-PASS]")
}

// childCutter cuts plantedChildValue from child output when cuts is set. It
// holds back the end of an unended line that may start the value, or all of
// it when holdAll is set.
type childCutter struct {
	encodedOnly
	cuts    bool
	holdAll bool
}

func (c childCutter) CutsChildOutput() bool { return c.cuts }
func (c childCutter) RedactChildOutput(line string) string {
	return strings.ReplaceAll(line, plantedChildValue, "***")
}
func (c childCutter) HoldChildOutput(text string) int {
	if c.holdAll {
		return len(text)
	}
	for k := len(plantedChildValue) - 1; k > 0; k-- {
		if strings.HasSuffix(text, plantedChildValue[:k]) {
			return k
		}
	}
	return 0
}

func withRedactor(t *testing.T, r Redactor) {
	t.Helper()
	SetRedactor(r)
	t.Cleanup(func() { SetRedactor(nil) })
}

func TestChildOutputIsLeftAsItIsWithoutAChildOutputCut(t *testing.T) {
	line := "DB_PASSWORD=" + plantedChildValue
	for _, tc := range []struct {
		name string
		r    Redactor
	}{
		{"no redactor", nil},
		// keploy's own pass over its log lines is not applied to what an
		// app prints.
		{"a redactor with no child-output cut", encodedOnly{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withRedactor(t, tc.r)
			if CutsChildOutput() {
				t.Fatal("CutsChildOutput is true")
			}
			if got := RedactChildOutput(line); got != line {
				t.Fatalf("RedactChildOutput = %q, want %q", got, line)
			}
		})
	}
}

func TestCutsChildOutputIsTheCuttersOwnAnswer(t *testing.T) {
	withRedactor(t, childCutter{cuts: false})
	if CutsChildOutput() {
		t.Fatal("CutsChildOutput is true for a cutter with nothing to cut")
	}
	withRedactor(t, childCutter{cuts: true})
	if !CutsChildOutput() {
		t.Fatal("CutsChildOutput is false for a cutter with values")
	}
	if got := RedactChildOutput("x " + plantedChildValue); got != "x ***" {
		t.Fatalf("RedactChildOutput = %q", got)
	}
}

func TestACutterWithNothingToCutIsNotAskedToCut(t *testing.T) {
	// It would cut the value: RedactChildOutput must not ask it while its
	// CutsChildOutput is false.
	withRedactor(t, childCutter{cuts: false})
	line := "x " + plantedChildValue
	if got := RedactChildOutput(line); got != line {
		t.Fatalf("RedactChildOutput = %q, want the line as it came", got)
	}
	if got := holdChildOutput(plantedChildValue[:3]); got != 0 {
		t.Fatalf("holdChildOutput = %d, want 0", got)
	}
}

func TestChildOutputWriterCutsAValueSplitAcrossWrites(t *testing.T) {
	withRedactor(t, childCutter{cuts: true})
	var dst bytes.Buffer
	w := NewChildOutputWriter(&dst)
	half := len(plantedChildValue) / 2
	for _, p := range []string{"app-1  | token=" + plantedChildValue[:half], plantedChildValue[half:], " ok\n"} {
		if n, err := w.Write([]byte(p)); err != nil || n != len(p) {
			t.Fatalf("Write(%q) = %d, %v", p, n, err)
		}
	}
	if got, want := dst.String(), "app-1  | token=*** ok\n"; got != want {
		t.Fatalf("wrote %q, want %q", got, want)
	}
}

func TestChildOutputWriterKeepsEachLineEndAsItCame(t *testing.T) {
	withRedactor(t, childCutter{cuts: true})
	var dst bytes.Buffer
	w := NewChildOutputWriter(&dst)
	// A progress line ended by '\r' alone is written at once, not held
	// for a '\n' that a redrawing terminal line never gets.
	if _, err := w.Write([]byte("a " + plantedChildValue + "\r")); err != nil {
		t.Fatal(err)
	}
	if got, want := dst.String(), "a ***\r"; got != want {
		t.Fatalf("after a '\\r' line: wrote %q, want %q", got, want)
	}
	if _, err := w.Write([]byte("50%\r\n" + plantedChildValue + "\n\nend\n")); err != nil {
		t.Fatal(err)
	}
	if got, want := dst.String(), "a ***\r50%\r\n***\n\nend\n"; got != want {
		t.Fatalf("wrote %q, want %q", got, want)
	}
}

func TestChildOutputWriterWritesAnUnendedLineAsItComes(t *testing.T) {
	withRedactor(t, childCutter{cuts: true})
	var dst bytes.Buffer
	w := NewChildOutputWriter(&dst)
	write := func(p string) {
		t.Helper()
		if n, err := w.Write([]byte(p)); err != nil || n != len(p) {
			t.Fatalf("Write(%q) = %d, %v", p, n, err)
		}
	}
	// A prompt shows at once: nothing in it can start the value.
	write("[sudo] password for u: ")
	if got, want := dst.String(), "[sudo] password for u: "; got != want {
		t.Fatalf("the prompt came out %q, want %q at once", got, want)
	}
	// What may start the value is held, and only that.
	write("token=" + plantedChildValue[:7])
	if got, want := dst.String(), "[sudo] password for u: token="; got != want {
		t.Fatalf("wrote %q, want %q", got, want)
	}
	write(plantedChildValue[7:] + " rest")
	if got, want := dst.String(), "[sudo] password for u: token=*** rest"; got != want {
		t.Fatalf("wrote %q, want %q", got, want)
	}
	// A start the line does not complete comes out at Flush, as it is.
	write(" " + plantedChildValue[:5])
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if got, want := dst.String(), "[sudo] password for u: token=*** rest "+plantedChildValue[:5]; got != want {
		t.Fatalf("Flush wrote %q, want %q", got, want)
	}
	before := dst.String()
	if err := w.Flush(); err != nil || dst.String() != before {
		t.Fatalf("a second Flush wrote again: %q, %v", dst.String(), err)
	}
}

func TestHoldChildOutputIsKeptWithinTheText(t *testing.T) {
	for _, tc := range []struct {
		hold, want int
	}{{-3, 0}, {2, 2}, {9, 4}} {
		withRedactor(t, fixedHold{childCutter{cuts: true}, tc.hold})
		if got := holdChildOutput("abcd"); got != tc.want {
			t.Fatalf("a hold of %d: holdChildOutput = %d, want %d", tc.hold, got, tc.want)
		}
	}
}

type fixedHold struct {
	childCutter
	n int
}

func (f fixedHold) HoldChildOutput(string) int { return f.n }

func TestChildOutputWriterWritesOutALineThatOutgrowsItsHold(t *testing.T) {
	withRedactor(t, childCutter{cuts: true, holdAll: true})
	var dst bytes.Buffer
	w := NewChildOutputWriter(&dst)
	long := strings.Repeat("x", maxHeldLine-1)
	if n, err := w.Write([]byte(long)); err != nil || n != len(long) {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if dst.Len() != 0 {
		t.Fatalf("wrote %d bytes of an unended line it may hold", dst.Len())
	}
	if n, err := w.Write([]byte("x" + strings.Repeat("y", 10))); err != nil || n != 11 {
		t.Fatalf("Write = %d, %v", n, err)
	}
	if dst.Len() != maxHeldLine {
		t.Fatalf("wrote %d bytes of an unended line, want the %d it holds at most", dst.Len(), maxHeldLine)
	}
	if _, err := w.Write([]byte(" " + plantedChildValue + "\n")); err != nil {
		t.Fatal(err)
	}
	if got, want := dst.String()[maxHeldLine:], "yyyyyyyyyy ***\n"; got != want {
		t.Fatalf("the rest of the line came out %q, want %q", got, want)
	}
}

// A value that starts in the last bytes a full unended line holds is still
// cut: the cap writes the line out but for the end that may start it.
func TestChildOutputWriterCutsAValueAcrossItsHoldsCap(t *testing.T) {
	withRedactor(t, childCutter{cuts: true})
	var dst bytes.Buffer
	w := NewChildOutputWriter(&dst)
	long := strings.Repeat("x", maxHeldLine-5)
	for _, piece := range []string{long + plantedChildValue, "\n"} {
		if n, err := w.Write([]byte(piece)); err != nil || n != len(piece) {
			t.Fatalf("Write = %d, %v", n, err)
		}
	}
	if got, want := dst.String(), long+"***\n"; got != want {
		t.Fatalf("the line came out with %q at its end, want %q", got[len(long):], want[len(long):])
	}
}

type failingWriter struct{ after int }

func (f *failingWriter) Write(p []byte) (int, error) {
	if f.after == 0 {
		return 0, errors.New("console closed")
	}
	f.after--
	return len(p), nil
}

// The count on an error is of the bytes taken, the piece whose write failed
// (dropped) and what is held included, so a caller that writes the rest
// again repeats none of it.
func TestChildOutputWriterCountsWhatItTookBeforeAnError(t *testing.T) {
	withRedactor(t, childCutter{cuts: true})
	w := NewChildOutputWriter(&failingWriter{after: 1})
	n, err := w.Write([]byte("one\ntwo\nthree\n"))
	if err == nil {
		t.Fatal("no error from a failing console")
	}
	if want := len("one\ntwo\n"); n != want {
		t.Fatalf("Write took %d bytes, want %d", n, want)
	}
	var dst bytes.Buffer
	w = NewChildOutputWriter(&failingWriter{after: 0})
	in := "x " + plantedChildValue[:5]
	if n, err := w.Write([]byte(in)); err == nil || n != len(in) {
		t.Fatalf("Write = %d, %v; want all %d taken, with the console's error", n, err, len(in))
	}
	w.dst = &dst
	if err := w.Flush(); err != nil {
		t.Fatal(err)
	}
	if got, want := dst.String(), plantedChildValue[:5]; got != want {
		t.Fatalf("the held end came out %q, want %q once", got, want)
	}
}

func TestChildOutputWriterKeepsConcurrentLinesWhole(t *testing.T) {
	withRedactor(t, childCutter{cuts: true})
	var dst bytes.Buffer
	w := NewChildOutputWriter(&dst)
	var wg sync.WaitGroup
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func(g int) {
			defer wg.Done()
			for i := 0; i < 50; i++ {
				_, _ = fmt.Fprintf(w, "g%d-%d %s\n", g, i, plantedChildValue)
			}
		}(g)
	}
	wg.Wait()
	lines := strings.Split(strings.TrimSuffix(dst.String(), "\n"), "\n")
	if len(lines) != 400 {
		t.Fatalf("got %d lines, want 400", len(lines))
	}
	for _, l := range lines {
		var g, i int
		if _, err := fmt.Sscanf(l, "g%d-%d ***", &g, &i); err != nil || strings.Contains(l, plantedChildValue) {
			t.Fatalf("line %q is not one whole cut line", l)
		}
	}
}
