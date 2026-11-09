package linkout

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// linkOutRecording makes a recording at <tmp>/project/keploy holding
// reports/r.yaml, a regular file, and secret.txt beside the project, and
// returns the recording's folder and secret.txt's path.
func linkOutRecording(t *testing.T) (rec, secret string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("symbolic links need privileges on windows")
	}
	base := t.TempDir()
	rec = filepath.Join(base, "project", "keploy")
	if err := os.MkdirAll(filepath.Join(rec, "reports"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(rec, "reports", "r.yaml"), []byte("ok"), 0o644); err != nil {
		t.Fatal(err)
	}
	secret = filepath.Join(base, "secret.txt")
	if err := os.WriteFile(secret, []byte("SECRET"), 0o600); err != nil {
		t.Fatal(err)
	}
	return rec, secret
}

func symlink(t *testing.T, target, at string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(at), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, at); err != nil {
		t.Fatal(err)
	}
}

// refusedIn opens file inside rec as os.Root does and returns its refusal,
// said by Explain.
func refusedIn(t *testing.T, rec, file string) (*Error, error) {
	t.Helper()
	r, err := os.OpenRoot(rec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	_, err = r.Stat(file)
	if err == nil {
		t.Fatalf("os.Root followed %s", file)
	}
	return Explain(r, rec, file, refusedFor(rec, file), err), err
}

// refusedFor is what a reader says for any refusal of file in rec.
func refusedFor(rec, file string) string {
	return fmt.Sprintf("the recording's file %q is read only inside its recording %q", file, rec)
}

// A recording's file that os.Root refuses because it, or a link on its way,
// is a link it does not follow is said plainly: what the caller says for any
// refusal of it, then which link, and where it leads, with os.Root's own
// error kept for errors.Is.
func TestExplainNamesTheLinkAndWhereItLeads(t *testing.T) {
	for _, c := range []struct {
		name string
		// layout makes the links in rec; secret is a file outside it.
		layout     func(t *testing.T, rec, secret string)
		file, link string
		// target is where link leads, from rec and secret.
		target func(rec, secret string) string
		inside bool
		says   []string
	}{
		{"the file, by an absolute path out", func(t *testing.T, rec, secret string) {
			symlink(t, secret, filepath.Join(rec, "reports", "out.yaml"))
		}, "reports/out.yaml", "reports/out.yaml", func(_, secret string) string { return secret }, false,
			[]string{`the recording's file "reports/out.yaml" is read only inside its recording`, `: it is a link that leads out of it, to "`, `"; replace the link with what it leads to`}},
		{"the file, by a relative path that climbs out", func(t *testing.T, rec, _ string) {
			symlink(t, filepath.Join("..", "..", "..", "secret.txt"), filepath.Join(rec, "reports", "out.yaml"))
		}, "reports/out.yaml", "reports/out.yaml", func(_, secret string) string { return secret }, false,
			[]string{`: it is a link that leads out of it, to "`}},
		{"a folder on the way", func(t *testing.T, rec, secret string) {
			symlink(t, filepath.Dir(secret), filepath.Join(rec, "tests"))
		}, "tests/secret.txt", "tests", func(_, secret string) string { return filepath.Dir(secret) }, false,
			[]string{`the recording's file "tests/secret.txt" is read only inside its recording`, `: it leads through "tests", a link that leads out of it, to "`}},
		{"a link inside, to a link out", func(t *testing.T, rec, secret string) {
			symlink(t, "hop.yaml", filepath.Join(rec, "reports", "out.yaml"))
			symlink(t, secret, filepath.Join(rec, "reports", "hop.yaml"))
		}, "reports/out.yaml", "reports/hop.yaml", func(_, secret string) string { return secret }, false,
			[]string{`: it leads through "reports/hop.yaml", a link that leads out of it, to "`}},
		{"a link out in a folder a link inside leads to, climbing from where it is", func(t *testing.T, rec, _ string) {
			// a/l is in deep/er: "../../.." from there is the project's
			// folder; from a, as the name reads, it is one above.
			if err := os.MkdirAll(filepath.Join(rec, "deep", "er"), 0o755); err != nil {
				t.Fatal(err)
			}
			symlink(t, filepath.Join("deep", "er"), filepath.Join(rec, "a"))
			symlink(t, filepath.Join("..", "..", "..", "x"), filepath.Join(rec, "deep", "er", "l"))
		}, "a/l", "deep/er/l", func(rec, _ string) string { return filepath.Join(filepath.Dir(rec), "x") }, false,
			[]string{`: it leads through "deep/er/l", a link that leads out of it, to "`}},
		{"an absolute path into the recording", func(t *testing.T, rec, _ string) {
			symlink(t, filepath.Join(rec, "reports", "r.yaml"), filepath.Join(rec, "reports", "abs.yaml"))
		}, "reports/abs.yaml", "reports/abs.yaml", func(rec, _ string) string { return filepath.Join(rec, "reports", "r.yaml") }, true,
			[]string{`the recording's file "reports/abs.yaml" is read only inside its recording`, `: it is a link by the absolute path "`, `", which keploy does not follow; make the link relative`}},
		{"an absolute path to the recording itself", func(t *testing.T, rec, _ string) {
			symlink(t, rec, filepath.Join(rec, "self"))
		}, "self/reports/r.yaml", "self", func(rec, _ string) string { return rec }, true,
			[]string{`: it leads through "self", a link by the absolute path "`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec, secret := linkOutRecording(t)
			c.layout(t, rec, secret)
			file := filepath.FromSlash(c.file)
			got, rootErr := refusedIn(t, rec, file)
			if got == nil {
				t.Fatalf("os.Root's refusal %q is not said as a link out", rootErr)
			}
			want := Error{Refused: refusedFor(rec, file), File: file, Recording: rec, Link: filepath.FromSlash(c.link), Target: c.target(rec, secret), Inside: c.inside, Err: rootErr}
			if *got != want {
				t.Fatalf("got %+v\nwant %+v", *got, want)
			}
			msg := got.Error()
			if !strings.HasPrefix(msg, want.Refused+": ") {
				t.Errorf("%q does not begin with what any refusal of the file says, %q", msg, want.Refused)
			}
			for _, s := range append(c.says, want.Target, rec) {
				if !strings.Contains(msg, filepath.FromSlash(s)) {
					t.Errorf("%q does not say %q", msg, s)
				}
			}
			if strings.Contains(msg, "escapes") || strings.Contains(msg, "statat") {
				t.Errorf("%q says os.Root's own words", msg)
			}
			if !errors.Is(got, rootErr) {
				t.Errorf("errors.Is does not see os.Root's refusal through %v", got)
			}
		})
	}
}

// Anything else os.Root refuses, or a path that is not there, is not said as
// a link out: Explain is nil, and the caller says it as before.
func TestExplainIsNilForAnythingElse(t *testing.T) {
	rec, secret := linkOutRecording(t)
	symlink(t, secret, filepath.Join(rec, "reports", "out.yaml"))
	symlink(t, "nowhere.yaml", filepath.Join(rec, "reports", "dangling.yaml"))
	symlink(t, "b", filepath.Join(rec, "loop", "a"))
	symlink(t, "a", filepath.Join(rec, "loop", "b"))
	// a/l climbs out as its name reads, but not from deep/er, where it is.
	if err := os.MkdirAll(filepath.Join(rec, "deep", "er"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, filepath.Join("deep", "er"), filepath.Join(rec, "a"))
	symlink(t, filepath.Join("..", "..", "reports", "r.yaml"), filepath.Join(rec, "deep", "er", "l"))
	r, err := os.OpenRoot(rec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	other := errors.New("refused for something else")
	for _, c := range []struct {
		name, file string
		err        error
	}{
		{"no error", "reports/r.yaml", nil},
		{"no error, even at a link out", "reports/out.yaml", nil},
		{"no error, even at a name that leads out", "../../secret.txt", nil},
		{"not there", "reports/none.yaml", os.ErrNotExist},
		{"a regular file", "reports/r.yaml", other},
		{"a link inside to nothing", "reports/dangling.yaml", other},
		{"links that lead to each other", "loop/a", other},
		{"a link that stays inside from where it is", "a/l", other},
		{"a name that only passes by .. and stays inside", "reports/../reports/r.yaml", other},
		{"a name that begins with two dots", "..x", other},
		{"a folder whose name begins with two dots", "..x/r.yaml", other},
	} {
		// Explain returns, even for links that lead to each other: it
		// follows at most maxLinkHops links, as the kernel does.
		said := make(chan *Error, 1)
		go func() { said <- Explain(r, rec, filepath.FromSlash(c.file), refusedFor(rec, c.file), c.err) }()
		select {
		case got := <-said:
			if got != nil {
				t.Errorf("%s: got %v, want nil", c.name, got)
			}
		case <-time.After(30 * time.Second):
			t.Fatalf("%s: Explain did not return in 30 s", c.name)
		}
	}
}

// The working directory as the recording ("") is named ".".
func TestExplainNamesTheWorkingDirectory(t *testing.T) {
	rec, secret := linkOutRecording(t)
	symlink(t, secret, filepath.Join(rec, "out.yaml"))
	t.Chdir(rec)
	r, err := os.OpenRoot(".")
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	_, rootErr := r.Stat("out.yaml")
	got := Explain(r, "", "out.yaml", refusedFor("", "out.yaml"), rootErr)
	if got == nil || got.Recording != "." || got.Target != secret || got.Inside {
		t.Fatalf("got %+v, want the link out of \".\" to %s", got, secret)
	}
}

// A path that os.Root refuses by its own name, one that climbs out by ".."
// or an absolute one, is said plainly too: what the caller says for any
// refusal of it, then where the name leads (or that it is an absolute path
// into the recording, and what to name instead), with os.Root's own error
// kept for errors.Is, and never os.Root's words.
func TestExplainSaysWhereANameLeads(t *testing.T) {
	rec, secret := linkOutRecording(t)
	r, err := os.OpenRoot(rec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	project := filepath.Dir(rec)
	inRec := filepath.Join(rec, "reports", "r.yaml")
	for _, c := range []struct {
		name, file, says string
		target           string
		inside           bool
	}{
		{"up and out", filepath.Join("..", "..", "secret.txt"), "it names %q, a place outside it; name a place inside it", secret, false},
		{"up and out, by the folder above", "..", "it names %q, a place outside it; name a place inside it", project, false},
		{"up and out after a folder", filepath.Join("reports", "..", "..", "x"), "it names %q, a place outside it; name a place inside it", filepath.Join(project, "x"), false},
		{"absolute, outside", secret, "it names %q, a place outside it; name a place inside it", secret, false},
		{"absolute, inside", inRec, "it names %q by an absolute path, which keploy does not open; name it relative to %q", inRec, true},
	} {
		t.Run(c.name, func(t *testing.T) {
			_, rootErr := r.Stat(c.file)
			if rootErr == nil {
				t.Fatalf("os.Root opened %s", c.file)
			}
			got := Explain(r, rec, c.file, refusedFor(rec, c.file), rootErr)
			if got == nil {
				t.Fatalf("Explain(%q) = nil, want it said", c.file)
			}
			want := Error{Refused: refusedFor(rec, c.file), File: c.file, Recording: rec, Target: c.target, Inside: c.inside, Err: rootErr}
			if *got != want {
				t.Fatalf("Explain(%q) = %+v, want %+v", c.file, *got, want)
			}
			says := fmt.Sprintf(c.says, c.target)
			if c.inside {
				says = fmt.Sprintf(c.says, c.target, rec)
			}
			if msg := got.Error(); msg != refusedFor(rec, c.file)+": "+says {
				t.Fatalf("said %q, want %q", msg, refusedFor(rec, c.file)+": "+says)
			}
			if strings.Contains(got.Error(), "escapes") || !errors.Is(got, rootErr) {
				t.Fatalf("said %q (errors.Is os.Root's error: %v), want it plain and os.Root's error kept", got, errors.Is(got, rootErr))
			}
		})
	}
	// A file that is not there is only that, whatever its name.
	if got := Explain(r, rec, "../none", refusedFor(rec, "../none"), os.ErrNotExist); got != nil {
		t.Fatalf("Explain of a file not there = %v, want nil", got)
	}
}
