package models

import (
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// testWords are words that place a loss in a test, as whole words: a loss
// no test carried says none of them (RunText).
var testWords = regexp.MustCompile(`\b(this test|before it|one before|the test it|while a test|test's calls)\b`)

// captureLossKinds are every CaptureLossKind constant this package declares,
// read from its source, so that a kind added later is held to RunText's
// promise without a list here to forget.
func captureLossKinds(t *testing.T) []CaptureLossKind {
	t.Helper()
	files, err := filepath.Glob("*.go")
	if err != nil {
		t.Fatal(err)
	}
	var out []CaptureLossKind
	fset := token.NewFileSet()
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		src, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		f, err := parser.ParseFile(fset, name, src, 0)
		if err != nil {
			t.Fatal(err)
		}
		for _, d := range f.Decls {
			g, ok := d.(*ast.GenDecl)
			if !ok || g.Tok != token.CONST {
				continue
			}
			for _, s := range g.Specs {
				v := s.(*ast.ValueSpec)
				if id, ok := v.Type.(*ast.Ident); !ok || id.Name != "CaptureLossKind" {
					continue
				}
				for _, val := range v.Values {
					lit, ok := val.(*ast.BasicLit)
					if !ok || lit.Kind != token.STRING {
						t.Fatalf("%s: a CaptureLossKind that is not a string literal", name)
					}
					out = append(out, CaptureLossKind(strings.Trim(lit.Value, "\"")))
				}
			}
		}
	}
	if len(out) < 28 {
		t.Fatalf("read %d CaptureLossKind constants, want every one (28 or more)", len(out))
	}
	return out
}

// Every kind's run-level sentence says what was lost, and how many times
// where its test's sentence does, with no word that places it in a test:
// a loss no test carried is the run's. Before RunText, keploy mock record
// and keploy record said a loss after their last test with Text, which
// says "while this test or one before it ran".
func TestEveryKindsRunSentenceNamesNoTest(t *testing.T) {
	for _, k := range captureLossKinds(t) {
		l := CaptureLosses{{Kind: k, Count: 37, What: "to db:5432", Err: -7}}
		got := l.RunText()
		if got == "" {
			t.Errorf("%s: no run sentence", k)
		}
		if w := testWords.FindString(got); w != "" {
			t.Errorf("%s: its run sentence says %q: %s", k, w, got)
		}
		if strings.Contains(l.Text(), "37") && !strings.Contains(got, "37") {
			t.Errorf("%s: its run sentence lost the count its test's has: %s", k, got)
		}
		if strings.HasPrefix(got, "capture incomplete: ") || strings.HasPrefix(got, "not judged: ") {
			t.Errorf("%s: its run sentence carries a test reason's head: %s", k, got)
		}
		if !k.pidMap() && !strings.Contains(got, "(to db:5432)") {
			t.Errorf("%s: its run sentence does not say what the loss was: %s", k, got)
		}
	}
}

// A kind this build does not know says its kind and count, never a test's
// words: fail-closed for a kind an agent newer than this build sends.
func TestAnUnknownKindsRunSentenceSaysItsKindAndCount(t *testing.T) {
	got := CaptureLosses{{Kind: "kind_from_a_newer_agent", Count: 4}}.RunText()
	if got != "4 capture loss(es) of kind kind_from_a_newer_agent" {
		t.Fatalf("got %q", got)
	}
}

// A PID map's run sentence is said from its record alone: the map (its
// kind), the inserts it refused (Count), why (Err), and its cause's remedy,
// never its What, which only repeats them. Two maps with two causes each say
// their own; one remedy two maps share is said once.
func TestAPIDMapsRunSentenceSaysItsMapCountCauseAndRemedyFromItsRecord(t *testing.T) {
	target := CaptureLoss{Kind: CaptureLossPIDMap, Count: 1179, Err: -linuxE2BIG, What: "capture incomplete: whatever the words were"}
	got := CaptureLosses{target}.RunText()
	want := "the agent's PID map target_namespace_pids refused 1179 insert(s) (E2BIG: the map had no room for it, usually because it is full), so at least one process was not intercepted: its calls were neither recorded nor mocked and went to the real destination. What to do: " + pidMapRemedy
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}

	excluded := CaptureLoss{Kind: CaptureLossPIDMapExcluded, Count: 2, Err: -linuxEBUSY, What: "excluded_pids, E2BIG: the map had no room for it, usually because it is full"}
	got = CaptureLosses{target, excluded}.RunText()
	for _, w := range []string{"target_namespace_pids refused 1179 insert(s) (E2BIG", "excluded_pids refused 2 insert(s) (EBUSY: its bucket lock", pidMapRemedy, pidMapLockRemedy} {
		if strings.Count(got, w) != 1 {
			t.Errorf("want %q once in %q", w, got)
		}
	}
	if strings.Contains(got, "whatever the words were") || strings.Contains(got, "excluded_pids, E2BIG") {
		t.Errorf("a PID map's What was said: %s", got)
	}

	// The same cause in both maps: its remedy once.
	excluded.Err = -linuxE2BIG
	if got = (CaptureLosses{target, excluded}).RunText(); strings.Count(got, pidMapRemedy) != 1 {
		t.Errorf("want the shared remedy once: %s", got)
	}

	// An agent that did not say why: no cause, and the full map's remedy.
	got = CaptureLosses{{Kind: CaptureLossPIDMap, Count: 3}}.RunText()
	if strings.Contains(got, "E2BIG") || !strings.HasSuffix(got, ". What to do: "+pidMapRemedy) {
		t.Errorf("got %q", got)
	}
	got = CaptureLosses{{Kind: CaptureLossPIDMap, Count: 3, Err: -linuxENOMEM}}.RunText()
	if !strings.Contains(got, "(ENOMEM:") || !strings.HasSuffix(got, ". What to do: "+pidMapMemoryRemedy) {
		t.Errorf("got %q", got)
	}
}

// Each kind keploy names a remedy for says it after every loss, once; a kind
// whose sentence says what to do, or that keploy names none for, adds none.
// What the loss was is shaped as a report names it.
func TestARunSentenceSaysEachRemedyOnceAfterItsLosses(t *testing.T) {
	got := CaptureLosses{
		{Kind: CaptureLossUnixSocket, Count: 2},
		{Kind: CaptureLossServerFirst, Count: 1, What: "to mail:25"},
		{Kind: CaptureLossKernelSkip, Count: 5},
		{Kind: CaptureLossTLSAbort, Count: 1},
	}.RunText()
	want := "the application called a Unix socket keploy cannot capture (2 time(s)); " +
		captureLossText(CaptureLoss{Kind: CaptureLossServerFirst, Count: 1}) + " (to mail:25); " +
		"during its recording the kernel skipped 5 capture run(s) of keploy's programs that see the application's connections (a run of a tracing program is skipped on a CPU where another run of it was preempted); " +
		captureLossText(CaptureLoss{Kind: CaptureLossTLSAbort, Count: 1}) +
		". What to do: reach that dependency over a TCP address (mocked, or listed in mock.real); and list the server-first destination in keploy.yml mock.real, to reach it for real by design; and " + KernelSkipRemedy + "; and " + trustKeployCARemedy
	if got != want {
		t.Fatalf("got  %q\nwant %q", got, want)
	}

	// Two losses with one remedy: it is said once.
	got = CaptureLosses{{Kind: CaptureLossKernelSkip, Count: 1}, {Kind: CaptureLossKernelSkipCall, Count: 1}}.RunText()
	if strings.Count(got, KernelSkipRemedy) != 1 {
		t.Errorf("want the shared remedy once: %s", got)
	}

	// A sentence that says what to do itself, or a kind with no remedy:
	// nothing after it.
	for _, k := range []CaptureLossKind{CaptureLossTLSAbandoned, CaptureLossUDPReply, CaptureLossAppSubtreeUntracked, CaptureLossParserStall, CaptureLossDropped} {
		if got := (CaptureLosses{{Kind: k, Count: 1}}).RunText(); strings.Contains(got, "What to do") {
			t.Errorf("%s: %s", k, got)
		}
	}

	// What the loss was, shaped: no query value.
	got = CaptureLosses{{Kind: CaptureLossAppSubtreeUntracked, Count: 1, What: "/bin/app?token=s3cret"}}.RunText()
	if strings.Contains(got, "s3cret") || !strings.Contains(got, "token={}") {
		t.Errorf("What not shaped: %s", got)
	}

	if got := CaptureLosses(nil).RunText(); got != "" {
		t.Errorf("no loss: %q", got)
	}
}
