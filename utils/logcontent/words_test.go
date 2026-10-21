package logcontent

import (
	"errors"
	"fmt"
	"io"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"go.uber.org/zap"
)

// setWords has a test's shapes read formats and texts, and puts keploy's back
// as it ends.
func setWords(t *testing.T, formats, texts []string) {
	t.Helper()
	prev := *loadWords()
	wordsRead = readWords(formats, texts)
	t.Cleanup(func() { wordsRead = prev })
}

// The shape of an error keploy makes keeps keploy's own words, each layer's
// read against the formats keploy makes errors with, and writes every value
// they were written around {}, the application's content among them, which
// no shape carries whatever fills it.
func TestAnErrorsShapeKeepsKeploysWordsAndNoValue(t *testing.T) {
	setWords(t, []string{
		"failed to run test set %s: %w",
		"failed to decode the mock: %w",
		"read the %s header (%d bytes): %w",
		"no mock answered %q",
		"%s: %w",
		"100%% of %d calls failed: %v",
		"lost: %w after %s",
	}, []string{"the mock store is closed"})
	numErr := &strconv.NumError{Func: "Atoi", Num: canary, Err: strconv.ErrSyntax}
	for _, c := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("failed to run test set %s: %w", "test-set-"+canary, fmt.Errorf("failed to decode the mock: %w", numErr)),
			"failed to run test set {}: failed to decode the mock: {}"},
		{fmt.Errorf("failed to run test set %s: %w", "ts", errors.New("the mock store is closed")),
			"failed to run test set {}: the mock store is closed"},
		{fmt.Errorf("failed to decode the mock: %w", io.ErrUnexpectedEOF), "failed to decode the mock: unexpected EOF"},
		{fmt.Errorf("failed to decode the mock: %w", syscall.ECONNRESET), "failed to decode the mock: " + syscall.ECONNRESET.Error()},
		{fmt.Errorf("read the %s header (%d bytes): %w", canary, 12, numErr), "read the {} header ({} bytes): {}"},
		{fmt.Errorf("no mock answered %q", "GET /reset?token="+canary), "no mock answered {}"},
		{fmt.Errorf("100%% of %d calls failed: %v", 3, numErr), "100% of {} calls failed: {}"},
		{fmt.Errorf("lost: %w after %s", numErr, canary), "lost: {} after {}"},
		// The error it wraps quoted, twice: the place that leaves the rest
		// keploy's words is the one read.
		{fmt.Errorf("failed to decode the mock: %w", errors.New("failed to decode the mock: x")), "failed to decode the mock: {}"},
		// A chain whose outermost text no format of keploy's makes, or one
		// that says nothing of what failed ("%s: %w"), has no shape.
		{fmt.Errorf("unexpected %s: %w", canary, numErr), ""},
		{fmt.Errorf("unexpected frame %s", canary), ""},
		{numErr, ""},
		// An error it wraps whose text is empty is found anywhere, and
		// read once at each place.
		{fmt.Errorf("failed to decode the mock: %w", errors.New("")), "failed to decode the mock: {}"},
		{fmt.Errorf("failed to run test set %s: %w", "", emptyErr{}), "failed to run test set {}: {}"},
		{errors.Join(numErr, io.EOF), ""},
		{nil, ""},
	} {
		text := ""
		if c.err != nil {
			text = c.err.Error()
		}
		got := errorShape(c.err, text)
		if got != c.want {
			t.Errorf("errorShape(%v) = %q, want %q", c.err, got, c.want)
		}
		if strings.Contains(got, canary) {
			t.Errorf("the shape of %v carries the canary: %q", c.err, got)
		}
	}
}

// A format of no words of its own ("%s: %w", "%w: %s") makes a shape only
// through what it holds: the shape of the error it wraps, or of the text it
// formats last, read against the formats of words of their own. One that would
// write no letter is none.
func TestAShapeOfNoWordsIsWhatItHolds(t *testing.T) {
	setWords(t, []string{"%s: %w", "%w: %s", "%v", "the %s of the %s response is %d bytes"},
		[]string{"mysql: the exchange is framed but cannot be recorded"})
	notRecorded := errors.New("mysql: the exchange is framed but cannot be recorded")
	for _, c := range []struct {
		err  error
		want string
	}{
		{fmt.Errorf("%s: %w", canary, io.EOF), "{}: EOF"},
		{fmt.Errorf("%s: %w", canary, &strconv.NumError{Func: "Atoi", Num: canary, Err: strconv.ErrSyntax}), ""},
		{fmt.Errorf("%w: %s", notRecorded, fmt.Sprintf("the %s of the %s response is %d bytes", canary, "COM_QUERY", 3)),
			"mysql: the exchange is framed but cannot be recorded: the {} of the {} response is {} bytes"},
		{fmt.Errorf("%w: %s", notRecorded, canary), "mysql: the exchange is framed but cannot be recorded: {}"},
		// A text formatted last is read against the formats of words of
		// their own alone: "%v" would read it whole again.
		{fmt.Errorf("%w: %s", io.EOF, canary+": "+canary), "EOF: {}"},
	} {
		got := errorShape(c.err, c.err.Error())
		if got != c.want {
			t.Errorf("errorShape(%v) = %q, want %q", c.err, got, c.want)
		}
		if strings.Contains(got, canary) {
			t.Errorf("the shape of %v carries the canary: %q", c.err, got)
		}
	}
}

// However many formats a text could be read against, a shape reads a bounded
// number of texts: a line's encoding never stalls on its error.
func TestAShapeReadsABoundedNumberOfTexts(t *testing.T) {
	var formats []string
	for i := 0; i < 200; i++ {
		formats = append(formats, fmt.Sprintf("a%d: %%s", i), "a: %s", "a: %s: %s", "%s: a: %s")
	}
	setWords(t, formats, nil)
	text := strings.Repeat("a: ", 2000) + canary
	done := make(chan string, 1)
	go func() { done <- errorShape(errors.New(text), text) }()
	select {
	case got := <-done:
		if strings.Contains(got, canary) {
			t.Fatalf("the shape carries the canary: %q", got)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("the shape of a text many formats can read did not end within 10s")
	}
}

// Error logs an error's shape with its kind and its length, with the switch
// off; with it on, the error whole, as zap.Error does.
func TestErrorLogsTheShapeOfAnErrorKeployMade(t *testing.T) {
	setWords(t, []string{"failed to decode the mock: %w"}, nil)
	err := fmt.Errorf("failed to decode the mock: %w", &strconv.NumError{Func: "Atoi", Num: canary, Err: strconv.ErrSyntax})
	t.Cleanup(Set(false))
	lines := observed(t, func(l *zap.Logger) { l.Error("failed to mock the outgoing message", Error(err)) })
	want := fmt.Sprintf(`"error": {"kind": "*strconv.NumError", "len": %d, "shape": "failed to decode the mock: {}"}`, len(err.Error()))
	if !strings.Contains(lines[0], want) || carries(lines[0]) {
		t.Fatalf("the line is %s, want %s and no canary", lines[0], want)
	}
}

func TestParseFormat(t *testing.T) {
	for _, c := range []struct {
		in     string
		chunks []string
		wrap   int
		ok     bool
	}{
		{"failed: %w", []string{"failed: ", ""}, 0, true},
		{"%-8s|%5.2f|%w|%%", []string{"", "|", "|", "|%"}, 2, true},
		{"no verbs", []string{"no verbs"}, -1, true},
		{"%w and %w", nil, 0, false},
		{"%[1]s", nil, 0, false},
		{"%*d", nil, 0, false},
		{"trailing %", nil, 0, false},
	} {
		f, ok := parseFormat(c.in)
		if ok != c.ok || (ok && (strings.Join(f.chunks, "|") != strings.Join(c.chunks, "|") || len(f.chunks) != len(c.chunks) || f.wrap != c.wrap)) {
			t.Errorf("parseFormat(%q) = %q %d %v, want %q %d %v", c.in, f.chunks, f.wrap, ok, c.chunks, c.wrap, c.ok)
		}
	}
}

// emptyErr is an error whose text is empty.
type emptyErr struct{}

func (emptyErr) Error() string { return "" }

// panicky is an error whose Error panics once its chain is read.
type panicky struct{ calls *int }

func (p panicky) Error() string {
	*p.calls++
	if *p.calls > 1 {
		panic("read twice")
	}
	return "x"
}

// A chain whose Error panics as its shape is read leaves the line without a
// shape, and the line is logged.
func TestAShapeThatPanicsIsNone(t *testing.T) {
	setWords(t, []string{"failed to decode the mock: %w"}, nil)
	calls := 0
	err := fmt.Errorf("failed to decode the mock: %w", panicky{&calls})
	if got := errorShape(err, "failed to decode the mock: x"); got != "" {
		t.Fatalf("errorShape = %q, want none", got)
	}
}
