package utils

import (
	"context"
	"errors"
	"os"
	"strconv"
	"testing"
)

// A closed / non-TTY stdin yields io.EOF. Three contracts have been tried:
//
//  1. return the raw error -- callers surfaced it as "failed to ask for
//     confirmation", a hard failure with a message about nothing;
//  2. return (false, nil) -- indistinguishable from a human answering "no",
//     which is how `keploy config --generate` came to write nothing in CI and
//     exit 0, reporting a decision nobody made;
//  3. return (false, ErrNoAnswer) -- the safe default AND the fact that
//     nobody was there, so a caller can decline quietly or refuse out loud.
//
// This pins the third. A TTY check is not a substitute for it: CI runners
// that allocate a pty (docker -t, Jenkins, GitLab) look interactive and still
// deliver EOF.
func TestAskForConfirmation_NobodyThereIsNotADecline(t *testing.T) {
	r, w, err := os.Pipe()
	if err != nil {
		t.Fatalf("failed to create pipe: %v", err)
	}
	// Closing the write end immediately makes every read return io.EOF, which
	// is what a closed or redirected-from-/dev/null stdin looks like.
	if err := w.Close(); err != nil {
		t.Fatalf("failed to close pipe writer: %v", err)
	}

	orig := os.Stdin
	os.Stdin = r
	t.Cleanup(func() {
		os.Stdin = orig
		_ = r.Close()
	})

	override, err := AskForConfirmation(context.Background(), "Config file already exists. Do you want to override it?")
	if !errors.Is(err, ErrNoAnswer) {
		t.Fatalf("AskForConfirmation returned %v; an EOF is nobody answering, not somebody declining", err)
	}
	if override {
		t.Error("AskForConfirmation returned true on EOF; the safe default is still false")
	}
}

// An answer typed without a trailing newline is still an answer.
//
// bufio's ReadString returns what it read AND io.EOF when the input ends
// without the delimiter -- `printf 'yes' | keploy config --generate`, or a
// heredoc with no final newline. Reporting only the error made that "there
// was nobody to answer", so the command refused and told the user to pass
// --force, over a question they had just said yes to.
func TestAskForConfirmation_AnAnswerWithoutANewline(t *testing.T) {
	for _, tc := range []struct {
		in    string
		want  bool
		noOne bool
	}{
		{in: "yes", want: true},
		{in: "y", want: true},
		{in: "n", want: false},
		{in: "yes\n", want: true},
		{in: "", noOne: true},
		{in: "   ", noOne: true},
	} {
		t.Run(strconv.Quote(tc.in), func(t *testing.T) {
			r, w, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			if _, err := w.WriteString(tc.in); err != nil {
				t.Fatal(err)
			}
			if err := w.Close(); err != nil {
				t.Fatal(err)
			}
			orig := os.Stdin
			os.Stdin = r
			t.Cleanup(func() { os.Stdin = orig; _ = r.Close() })

			got, err := AskForConfirmation(context.Background(), "override?")
			if tc.noOne {
				if !errors.Is(err, ErrNoAnswer) {
					t.Fatalf("%q should be nobody answering; got (%v, %v)", tc.in, got, err)
				}
				return
			}
			if err != nil {
				t.Fatalf("%q was read as an error: %v", tc.in, err)
			}
			if got != tc.want {
				t.Errorf("%q answered %v, want %v", tc.in, got, tc.want)
			}
		})
	}
}
