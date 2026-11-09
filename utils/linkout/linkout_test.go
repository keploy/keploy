package linkout

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"syscall"
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

// chain makes reports/out.yaml in rec the first of n links: each but the
// last leads to the next inside reports (h1.yaml, h2.yaml, ...), and the
// last, reports/h<n-1>.yaml, leads out, to secret.
func chain(t *testing.T, rec, secret string, n int) {
	t.Helper()
	at := filepath.Join(rec, "reports", "out.yaml")
	for i := 1; i < n; i++ {
		next := fmt.Sprintf("h%d.yaml", i)
		symlink(t, next, at)
		at = filepath.Join(rec, "reports", next)
	}
	symlink(t, secret, at)
}

// folderChain makes f0 in rec the first of n folder links, each leading to
// the next (f1, f2, ...), and the last, f<n>, a folder whose out.yaml leads
// out, to secret: f0/out.yaml passes n links, then the link out.
func folderChain(t *testing.T, rec, secret string, n int) {
	t.Helper()
	for i := 0; i < n; i++ {
		symlink(t, fmt.Sprintf("f%d", i+1), filepath.Join(rec, fmt.Sprintf("f%d", i)))
	}
	symlink(t, secret, filepath.Join(rec, fmt.Sprintf("f%d", n), "out.yaml"))
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
		back   bool
		says   []string
	}{
		{"the file, by an absolute path out", func(t *testing.T, rec, secret string) {
			symlink(t, secret, filepath.Join(rec, "reports", "out.yaml"))
		}, "reports/out.yaml", "reports/out.yaml", func(_, secret string) string { return secret }, false, false,
			[]string{`the recording's file "reports/out.yaml" is read only inside its recording`, `: it is a link that leads out of it, to "`, `"; replace the link with what it leads to`}},
		{"the file, by a relative path that climbs out", func(t *testing.T, rec, _ string) {
			symlink(t, filepath.Join("..", "..", "..", "secret.txt"), filepath.Join(rec, "reports", "out.yaml"))
		}, "reports/out.yaml", "reports/out.yaml", func(_, secret string) string { return secret }, false, false,
			[]string{`: it is a link that leads out of it, to "`}},
		{"a folder on the way", func(t *testing.T, rec, secret string) {
			symlink(t, filepath.Dir(secret), filepath.Join(rec, "tests"))
		}, "tests/secret.txt", "tests", func(_, secret string) string { return filepath.Dir(secret) }, false, false,
			[]string{`the recording's file "tests/secret.txt" is read only inside its recording`, `: it leads through "tests", a link that leads out of it, to "`}},
		{"a link inside, to a link out", func(t *testing.T, rec, secret string) {
			symlink(t, "hop.yaml", filepath.Join(rec, "reports", "out.yaml"))
			symlink(t, secret, filepath.Join(rec, "reports", "hop.yaml"))
		}, "reports/out.yaml", "reports/hop.yaml", func(_, secret string) string { return secret }, false, false,
			[]string{`: it leads through "reports/hop.yaml", a link that leads out of it, to "`}},
		{"a link out in a folder a link inside leads to, climbing from where it is", func(t *testing.T, rec, _ string) {
			// a/l is in deep/er: "../../.." from there is the project's
			// folder; from a, as the name reads, it is one above.
			if err := os.MkdirAll(filepath.Join(rec, "deep", "er"), 0o755); err != nil {
				t.Fatal(err)
			}
			symlink(t, filepath.Join("deep", "er"), filepath.Join(rec, "a"))
			symlink(t, filepath.Join("..", "..", "..", "x"), filepath.Join(rec, "deep", "er", "l"))
		}, "a/l", "deep/er/l", func(rec, _ string) string { return filepath.Join(filepath.Dir(rec), "x") }, false, false,
			[]string{`: it leads through "deep/er/l", a link that leads out of it, to "`}},
		{"an absolute path into the recording", func(t *testing.T, rec, _ string) {
			symlink(t, filepath.Join(rec, "reports", "r.yaml"), filepath.Join(rec, "reports", "abs.yaml"))
		}, "reports/abs.yaml", "reports/abs.yaml", func(rec, _ string) string { return filepath.Join(rec, "reports", "r.yaml") }, true, false,
			[]string{`the recording's file "reports/abs.yaml" is read only inside its recording`, `: it is a link by the absolute path "`, `", which keploy does not follow; make the link relative`}},
		{"an absolute path to the recording itself", func(t *testing.T, rec, _ string) {
			symlink(t, rec, filepath.Join(rec, "self"))
		}, "self/reports/r.yaml", "self", func(rec, _ string) string { return rec }, true, false,
			[]string{`: it leads through "self", a link by the absolute path "`}},
		{"a relative link that climbs out of it and back in", func(t *testing.T, rec, _ string) {
			// rec is <tmp>/project/keploy: the link leaves it and comes
			// back by its own name.
			symlink(t, filepath.Join("..", "..", "keploy", "reports", "r.yaml"), filepath.Join(rec, "reports", "l.yaml"))
		}, "reports/l.yaml", "reports/l.yaml", func(rec, _ string) string { return filepath.Join(rec, "reports", "r.yaml") }, false, true,
			[]string{`: it is a link that climbs out of it and back in, to "`, `", which keploy does not follow; make the link lead there without climbing out`}},
		{"seven links inside, then the link out: the eight os.Root follows", func(t *testing.T, rec, secret string) {
			chain(t, rec, secret, 8)
		}, "reports/out.yaml", "reports/h7.yaml", func(_, secret string) string { return secret }, false, false,
			[]string{`: it leads through "reports/h7.yaml", a link that leads out of it, to "`}},
		{"seven folder links inside, then the link out: folder links count too", func(t *testing.T, rec, secret string) {
			folderChain(t, rec, secret, 7)
		}, "f0/out.yaml", "f7/out.yaml", func(_, secret string) string { return secret }, false, false,
			[]string{`: it leads through "f7/out.yaml", a link that leads out of it, to "`}},
		{"a link out where a .. after a folder link leads, not where the name reads", func(t *testing.T, rec, _ string) {
			// a is deep/er, so a/.. is deep, as os.Root walks it; the name
			// read by itself would be l, at the recording's top.
			if err := os.MkdirAll(filepath.Join(rec, "deep", "er"), 0o755); err != nil {
				t.Fatal(err)
			}
			symlink(t, filepath.Join("deep", "er"), filepath.Join(rec, "a"))
			symlink(t, filepath.Join("..", "..", "..", "x"), filepath.Join(rec, "deep", "l"))
		}, "a/../l", "deep/l", func(rec, _ string) string { return filepath.Join(filepath.Dir(filepath.Dir(rec)), "x") }, false, false,
			[]string{`: it leads through "deep/l", a link that leads out of it, to "`}},
		{"a folder that links out by a relative path: where it leads, not the names after it", func(t *testing.T, rec, _ string) {
			symlink(t, filepath.Join("..", ".."), filepath.Join(rec, "tests"))
		}, "tests/secret.txt", "tests", func(_, secret string) string { return filepath.Dir(secret) }, false, false,
			[]string{`: it leads through "tests", a link that leads out of it, to "`}},
		{"a link whose target climbs out after a link in it", func(t *testing.T, rec, _ string) {
			// b is the recording itself, so b/.. is the project's folder,
			// and b/../../x is beside the project, not in it.
			symlink(t, ".", filepath.Join(rec, "b"))
			// Raw: filepath.Join would clean it to ../x.
			symlink(t, "b/../../x", filepath.Join(rec, "l"))
		}, "l", "l", func(rec, _ string) string { return filepath.Join(filepath.Dir(filepath.Dir(rec)), "x") }, false, false,
			[]string{`: it is a link that leads out of it, to "`}},
		{"a relative link to a folder beside it whose name begins with its own", func(t *testing.T, rec, _ string) {
			symlink(t, filepath.Join("..", "keploy2", "x"), filepath.Join(rec, "sib"))
		}, "sib", "sib", func(rec, _ string) string { return filepath.Join(filepath.Dir(rec), "keploy2", "x") }, false, false,
			[]string{`: it is a link that leads out of it, to "`}},
		{"an absolute path into the project, outside the recording", func(t *testing.T, rec, _ string) {
			symlink(t, filepath.Join(filepath.Dir(rec), "x"), filepath.Join(rec, "proj"))
		}, "proj", "proj", func(rec, _ string) string { return filepath.Join(filepath.Dir(rec), "x") }, false, false,
			[]string{`: it is a link that leads out of it, to "`}},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec, secret := linkOutRecording(t)
			c.layout(t, rec, secret)
			file := filepath.FromSlash(c.file)
			got, rootErr := refusedIn(t, rec, file)
			if got == nil {
				t.Fatalf("os.Root's refusal %q is not said as a link out", rootErr)
			}
			want := Error{Refused: refusedFor(rec, file), File: file, Recording: rec, Link: filepath.FromSlash(c.link), Target: c.target(rec, secret), Inside: c.inside, Back: c.back, Err: rootErr}
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
			if !c.inside && (strings.Contains(msg, "by the absolute path") || strings.Contains(msg, "by an absolute path")) {
				t.Errorf("%q calls a link with a relative target absolute", msg)
			}
			if !errors.Is(got, rootErr) {
				t.Errorf("errors.Is does not see os.Root's refusal through %v", got)
			}
		})
	}
}

// os.Root follows at most eight links in one lookup, the link out among them;
// past that it says too many links (ELOOP), not that a link leads out. So
// does Explain: with eight links inside before the link out, it is nil, and
// the caller says os.Root's ELOOP as before.
func TestExplainStopsWhereOSRootStopsFollowingLinks(t *testing.T) {
	for _, c := range []struct {
		name, file string
		layout     func(t *testing.T, rec, secret string)
	}{
		{"eight links inside, then the link out", "reports/out.yaml", func(t *testing.T, rec, secret string) {
			chain(t, rec, secret, 9)
		}},
		{"eight folder links inside, then the link out", "f0/out.yaml", func(t *testing.T, rec, secret string) {
			folderChain(t, rec, secret, 8)
		}},
		// os.Root follows a before it takes the "..": one link more than
		// the name read by itself, reports/out.yaml, passes.
		{"a folder link, a .. after it, seven links, then the link out", "a/../reports/out.yaml", func(t *testing.T, rec, secret string) {
			if err := os.MkdirAll(filepath.Join(rec, "sub"), 0o755); err != nil {
				t.Fatal(err)
			}
			symlink(t, "sub", filepath.Join(rec, "a"))
			chain(t, rec, secret, 8)
		}},
	} {
		t.Run(c.name, func(t *testing.T) {
			rec, secret := linkOutRecording(t)
			c.layout(t, rec, secret)
			got, rootErr := refusedIn(t, rec, filepath.FromSlash(c.file))
			if !errors.Is(rootErr, syscall.ELOOP) {
				t.Fatalf("os.Root said %v at the ninth link, want ELOOP: its bound is not maxLinks", rootErr)
			}
			if got != nil {
				t.Fatalf("Explain(%v) = %v, want nil: os.Root refused for too many links", rootErr, got)
			}
		})
	}
	// Folders that link to each other: os.Root says ELOOP, and Explain
	// returns, nil, inside a bounded wait.
	rec, _ := linkOutRecording(t)
	symlink(t, "b", filepath.Join(rec, "loop", "a"))
	symlink(t, "a", filepath.Join(rec, "loop", "b"))
	r, err := os.OpenRoot(rec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	file := filepath.Join("loop", "a", "x")
	said := make(chan *Error, 1)
	go func() {
		_, rootErr := r.Stat(file)
		said <- Explain(r, rec, file, refusedFor(rec, file), rootErr)
	}()
	select {
	case got := <-said:
		if got != nil {
			t.Fatalf("folders that link to each other: got %v, want nil", got)
		}
	case <-time.After(30 * time.Second):
		t.Fatal("folders that link to each other: Explain did not return in 30 s")
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
		// os.Root cannot open r.yaml as a folder, so the ".." never climbs.
		{"a name that passes by a file as if it were a folder", "reports/r.yaml/../../../x", other},
		// os.Root stops at none, so the ".." never climbs.
		{"a name that passes by a folder that is not there", "none/../../x", other},
	} {
		// Explain returns, even for links that lead to each other: it
		// follows at most maxLinks links, as os.Root does.
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
	// A name that climbs out of "." and back in by its folder's name.
	back := filepath.Join("..", "keploy", "reports", "r.yaml")
	_, rootErr = r.Stat(back)
	got = Explain(r, "", back, refusedFor("", back), rootErr)
	if got == nil || got.Target != back || got.Inside || !got.Back {
		t.Fatalf("got %+v, want %s said as climbing out of \".\" and back in", got, back)
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
	// a is deep/er and here is the recording itself: a ".." after either is
	// taken where the link leads, as os.Root walks the name.
	if err := os.MkdirAll(filepath.Join(rec, "deep", "er"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, filepath.Join("deep", "er"), filepath.Join(rec, "a"))
	symlink(t, ".", filepath.Join(rec, "here"))
	for _, c := range []struct {
		name, file, says string
		target           string
		inside, back     bool
	}{
		{"up and out", filepath.Join("..", "..", "secret.txt"), "it names %q, a place outside it; name a place inside it", secret, false, false},
		{"up and out, by the folder above", "..", "it names %q, a place outside it; name a place inside it", project, false, false},
		{"up and out after a folder", filepath.Join("reports", "..", "..", "x"), "it names %q, a place outside it; name a place inside it", filepath.Join(project, "x"), false, false},
		{"absolute, outside", secret, "it names %q, a place outside it; name a place inside it", secret, false, false},
		{"absolute, inside", inRec, "it names %q by an absolute path, which keploy does not open; name it relative to %q", inRec, true, false},
		// rec is <tmp>/project/keploy: the name leaves it and comes back
		// by its own name; it is relative, never "an absolute path".
		{"up and back in by the recording's own name", filepath.Join("..", "keploy", "reports", "r.yaml"), "it names %q by a path that climbs out of it and back in, which keploy does not open; name it relative to %q without climbing out", inRec, false, true},
		{"up to a folder beside it whose name begins with its own", filepath.Join("..", "keploy2", "x"), "it names %q, a place outside it; name a place inside it", filepath.Join(project, "keploy2", "x"), false, false},
		// Read by itself a/../../../x is beside the project; from deep/er
		// it is in the project.
		{"up and out after a folder link, from where it leads", "a/../../../x", "it names %q, a place outside it; name a place inside it", filepath.Join(project, "x"), false, false},
		// Read by itself here/../x is x, inside.
		{"up and out after a link to the recording itself", "here/../x", "it names %q, a place outside it; name a place inside it", filepath.Join(project, "x"), false, false},
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
			want := Error{Refused: refusedFor(rec, c.file), File: c.file, Recording: rec, Target: c.target, Inside: c.inside, Back: c.back, Err: rootErr}
			if *got != want {
				t.Fatalf("Explain(%q) = %+v, want %+v", c.file, *got, want)
			}
			says := fmt.Sprintf(c.says, c.target)
			if c.inside || c.back {
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

// parse splits a name, or a link's target put where the link was, as
// os.Root's splitPathInRoot does; on Windows (clean) the whole path cleaned
// first, as os.Root's rootCleanPath does.
func TestParseSplitsAsOSRootDoes(t *testing.T) {
	p, s := []part{{"p", -1}}, []part{{"s", 0}}
	for _, c := range []struct {
		name, s       string
		before, after []part
		clean         bool
		want          []part
		out           bool
	}{
		{"empty names and dots dropped", "a//b/./c/.", nil, nil, false, []part{{"a", 1}, {"b", 1}, {"c", 1}}, false},
		{"a target put where its link was", "../x", p, s, false, []part{{"p", -1}, {"..", 1}, {"x", 1}, {"s", 0}}, false},
		{"a target that is the folder it is in", ".", p, s, false, []part{{"p", -1}, {"s", 0}}, false},
		{"absolute", "/x", nil, nil, false, nil, true},
		{"cleaned: a .. taken before any link", "a/../l", nil, nil, true, []part{{"l", 1}}, false},
		{"cleaned: a target put where its link was", "../x", p, s, true, []part{{"x", 1}, {"s", 1}}, false},
		{"cleaned: a target that climbs out", "../../x", p, nil, true, nil, true},
		{"cleaned: absolute", "/x", p, nil, true, nil, true},
	} {
		got, out := parse(filepath.FromSlash(c.s), 1, c.before, c.after, c.clean)
		if out != c.out || !slices.Equal(got, c.want) {
			t.Errorf("%s: parse(%q) = %v, %v; want %v, %v", c.name, c.s, got, out, c.want, c.out)
		}
	}
}

// On Windows os.Root cleans a name, and each link's target put in its place,
// before it walks it: a ".." is never taken after a link, and a target that
// climbs out is refused as it is put in. Explain walks the same way there
// (cleansFirst); here it is set to show it.
func TestExplainWalksAsOSRootDoesOnWindows(t *testing.T) {
	was := cleansFirst
	cleansFirst = true
	t.Cleanup(func() { cleansFirst = was })
	rec, secret := linkOutRecording(t)
	if err := os.MkdirAll(filepath.Join(rec, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	symlink(t, "sub", filepath.Join(rec, "a"))
	chain(t, rec, secret, 8)
	symlink(t, filepath.Join("..", "..", "x"), filepath.Join(rec, "reports", "up.yaml"))
	r, err := os.OpenRoot(rec)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = r.Close() }()
	refused := errors.New("refused")
	for _, c := range []struct {
		name, file, link, target string
	}{
		// a/.. is cleaned away before the walk: seven links, then the
		// link out, as for reports/out.yaml.
		{"a .. after a folder link, cleaned away", "a/../reports/out.yaml", "reports/h7.yaml", secret},
		{"a target that climbs out, from the folder it is in", "reports/up.yaml", "reports/up.yaml", filepath.Join(filepath.Dir(rec), "x")},
	} {
		file := filepath.FromSlash(c.file)
		got := Explain(r, rec, file, refusedFor(rec, file), refused)
		want := Error{Refused: refusedFor(rec, file), File: file, Recording: rec, Link: filepath.FromSlash(c.link), Target: c.target, Err: refused}
		if got == nil || *got != want {
			t.Errorf("%s: got %+v, want %+v", c.name, got, want)
		}
	}
}
