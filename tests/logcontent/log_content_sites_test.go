// No log line keploy writes carries an application's content unless
// KEPLOY_LOG_CONTENT says it may, and every place one could is named.
//
// utils/logcontent is the one switch: a value that goes through its helpers
// (or logurl's) is written whole only with the switch on, and otherwise by
// its length, its kind or its shape. A value that reaches a line through
// zap's own constructors, a message, or an error's text is written whole,
// whatever the switch says. Review rounds found such lines one at a time;
// this test finds all of them, by type, from the source (scan_test.go says
// what a site is and which values are safe), so the list of what is left is
// the list a review reads.
//
// Each site is gated (through logcontent or logurl, after which it is no
// site), or it carries no content anyway and is listed in
// testdata/log_content_allowlist.txt: its file, its kind, the expression as
// written (an error with where it came from), how many times that occurs in
// the file, and one line that says why it carries no content. So it is a
// ratchet: a file with more of an expression than listed fails (a new site:
// gate it, or list it with its reason), and so does one with fewer (lower
// the count: the list only ever shrinks). An entry with no reason fails too.
//
// Scope: every package of keploy's module that a binary is built from:
// ./pkg/..., ./utils/..., ./cli/..., ./config/... and main, which hold the
// agent, the proxy and its parsers, record, replay, the matchers, the
// services, the storage of mocks and tests, the agent's client and the CLI.
// That is wider than the packages that touch traffic (the parsers, the
// proxy, record, replay, the matchers, the services and the agent), since a
// value the application sent reaches the stores, the agent's client and
// the CLI's lines too, as an error's text that climbs to them. Excluded,
// and why:
//   - utils/logcontent/...: the gate itself, whose fields are what the
//     switch writes;
//   - tools/..., tests/... and internal/teardowntest: lint tools, test
//     harnesses and a helper of tests, which no binary keploy ships
//     contains;
//   - _test.go files.
//
// TestScanFindsEachShape holds the scan itself to testdata/fixture, one
// line of each shape it reads with the sites it must find there, so a scan
// that stops seeing a shape fails there rather than reading as no content.
//
// Each package is loaded once per platform keploy ships (linux on amd64 and
// arm64, darwin on arm64, windows on amd64) and once with the composelib
// tag, and the scan is their union: a file a build constraint keeps from the
// host's load is still read.
package logcontentsites

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"testing"

	"golang.org/x/tools/go/packages"
)

// scopePatterns are the packages the guard reads, relative to the root.
var scopePatterns = []string{"./pkg/...", "./utils/...", "./cli/...", "./config/...", "."}

// excluded are the files under scope the guard does not read (see the doc).
var excluded = []string{"utils/logcontent/"}

// platforms are the builds the guard loads: every GOOS/GOARCH keploy ships,
// and the composelib tag.
var platforms = []struct{ goos, goarch, tags string }{
	{"linux", "amd64", ""},
	{"linux", "arm64", ""},
	{"darwin", "arm64", ""},
	{"windows", "amd64", ""},
	{"linux", "amd64", "composelib"},
}

const allowlistPath = "testdata/log_content_allowlist.txt"

// contentReason starts the reason of an error text that does carry content
// and keeps it, for the caller that handles the error (a report, a
// verdict, the reply keploy sends): the switch must not change what keploy
// does but what it logs, so the error is left whole, and every line that
// logs it is a site of its own, which gates it. An error text listed with
// any other reason carries no content, and the errors built from it are as
// safe as what they wrap.
const contentReason = "content:"

// constructedReason starts the reason of a site whose value carries no
// content by how it is made, whatever it is made from: a match of keploy's
// placeholder grammar ({{string .id}}), a key of letters, digits and '_'.
// Only such a reason stands for the values the site is made from, which the
// scan then does not judge (scanner.constructed); any other reason is a
// claim about the value the site holds, whose sources are judged where they
// are made. So a reviewer reads the site alone to check one: the reason
// says what of its construction holds the content out.
const constructedReason = "constructed:"

func repoRoot(t *testing.T) string {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	root := filepath.Clean(filepath.Join(wd, "..", ".."))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Fatalf("no go.mod at %s: %v", root, err)
	}
	return root
}

// load type-checks patterns for one platform, from root.
func load(t *testing.T, root, goos, goarch, tags string, patterns ...string) []*packages.Package {
	t.Helper()
	cfg := &packages.Config{
		Dir:  root,
		Mode: packages.NeedName | packages.NeedFiles | packages.NeedSyntax | packages.NeedTypes | packages.NeedTypesInfo | packages.NeedImports,
		Env:  append(os.Environ(), "GOOS="+goos, "GOARCH="+goarch, "CGO_ENABLED=0"),
	}
	if tags != "" {
		cfg.BuildFlags = []string{"-tags=" + tags}
	}
	pkgs, err := packages.Load(cfg, patterns...)
	if err != nil {
		t.Fatalf("load %s/%s %s: %v", goos, goarch, tags, err)
	}
	var errs []string
	packages.Visit(pkgs, nil, func(p *packages.Package) {
		for _, e := range p.Errors {
			errs = append(errs, e.Error())
		}
	})
	if len(errs) > 0 {
		// A package that does not type-check yields no sites, and no sites
		// reads as no content.
		t.Fatalf("packages did not type-check for %s/%s %s, so the scan would find nothing in them:\n  %s",
			goos, goarch, tags, strings.Join(errs, "\n  "))
	}
	if len(pkgs) == 0 {
		t.Fatalf("no packages loaded for %s/%s", goos, goarch)
	}
	return pkgs
}

// scanScope is every site in scope, across the platforms. listed are the
// error texts the allowlist says carry no content (scanner.listed).
func scanScope(t *testing.T, root string, listed, constructed map[string]bool) ([]site, *scanner) {
	t.Helper()
	s := newScanner(root)
	s.listed, s.constructed = listed, constructed
	s.strictIfaces = os.Getenv("LOG_CONTENT_STRICT_IFACES") == "1" // a measure, for the scan's debugging
	why := map[string]string{}
	for _, p := range platforms {
		s.add(load(t, root, p.goos, p.goarch, p.tags, scopePatterns...), excluded)
		for k, v := range s.explain() {
			why[k] = v
		}
	}
	if out := os.Getenv("LOG_CONTENT_SITES_WHY"); out != "" {
		var b strings.Builder
		for k, v := range why {
			fmt.Fprintf(&b, "%s\t%s\n", k, v)
		}
		if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	return s.list(), s
}

// wordsPath is the file of keploy's error formats and texts, the words
// logcontent keeps of an error's text (utils/logcontent/words.go).
const wordsPath = "utils/logcontent/words_gen.go"

// wordsFile is wordsPath's source for the formats and texts the scan found.
func wordsFile(s *scanner) string {
	list := func(m map[string]bool) string {
		keys := make([]string, 0, len(m))
		for k := range m {
			keys = append(keys, k)
		}
		sort.Strings(keys)
		var b strings.Builder
		for _, k := range keys {
			fmt.Fprintf(&b, "\t%s,\n", strconv.Quote(k))
		}
		return b.String()
	}
	return "// Code generated by tests/logcontent's guard (LOG_CONTENT_SITES_WRITE_ALLOWLIST=1); DO NOT EDIT.\n\n" +
		"package logcontent\n\n" +
		"// errorFormats are the constant formats keploy's code makes errors with\n" +
		"// (fmt.Errorf), and errorTexts the constant texts it makes them of\n" +
		"// (errors.New): the words an error's shape keeps (errorShape).\n" +
		"var errorFormats = []string{\n" + list(s.errFormats) + "}\n\n" +
		"var errorTexts = []string{\n" + list(s.errTexts) + "}\n"
}

// checkWords holds wordsPath to the formats and texts the scan found, or
// writes it (LOG_CONTENT_SITES_WRITE_ALLOWLIST=1). A file behind the source
// carries no content either way, since every word in it is keploy's: an
// error a format added since has {} for a shape until it is written again,
// so it is only said.
func checkWords(t *testing.T, root string, s *scanner) {
	t.Helper()
	path := filepath.Join(root, filepath.FromSlash(wordsPath))
	want := wordsFile(s)
	if os.Getenv("LOG_CONTENT_SITES_WRITE_ALLOWLIST") == "1" {
		if err := os.WriteFile(path, []byte(want), 0o644); err != nil {
			t.Fatal(err)
		}
		return
	}
	have, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(have) != want {
		t.Logf("%s is behind keploy's error formats: the shapes of the errors made since are {} (write it with LOG_CONTENT_SITES_WRITE_ALLOWLIST=1)", wordsPath)
	}
}

// allowed is one line of the allowlist.
type allowed struct {
	count  int
	reason string
}

// readAllowlist reads "file | kind | what | count | reason" lines (# starts
// a comment line).
func readAllowlist(t *testing.T, file string) map[string]allowed {
	t.Helper()
	f, err := os.Open(file)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	out := map[string]allowed{}
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 1<<16), 1<<20)
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if strings.TrimSpace(line) == "" || strings.HasPrefix(line, "#") {
			continue
		}
		parts := strings.Split(line, " | ")
		if len(parts) < 5 {
			t.Fatalf("%s:%d: want \"file | kind | what | count | reason\", got %q", file, n, line)
		}
		c, err := strconv.Atoi(parts[len(parts)-2])
		if err != nil || c <= 0 {
			t.Fatalf("%s:%d: the count %q is not a positive number", file, n, parts[len(parts)-2])
		}
		key := strings.Join(parts[:len(parts)-2], " | ")
		if _, dup := out[key]; dup {
			t.Fatalf("%s:%d: %q is listed twice", file, n, key)
		}
		out[key] = allowed{count: c, reason: strings.TrimSpace(parts[len(parts)-1])}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

// writeAllowlist writes what the scan found, keeping each reason the list
// has: to take counts down after gating sites, and to list new sites, which
// it writes with no reason, so the test fails until each has one
// (LOG_CONTENT_SITES_WRITE_ALLOWLIST=1). Check that the diff only lowers
// counts, or adds the entries you mean to.
func writeAllowlist(t *testing.T, file string, got map[string]int, prev map[string]allowed) {
	t.Helper()
	var b strings.Builder
	b.WriteString("# The places a value reaches a log line, or an error's text, without\n")
	b.WriteString("# logcontent or logurl, that carry no customer content: see\n")
	b.WriteString("# log_content_sites_test.go. file | kind | what | count | reason.\n")
	b.WriteString("# Only ever lower a count; a new site is gated, or listed with its reason.\n")
	keys := make([]string, 0, len(got))
	for k := range got {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		fmt.Fprintf(&b, "%s | %d | %s\n", k, got[k], prev[k].reason)
	}
	if err := os.WriteFile(file, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
}

func TestNoLogLineCarriesContentUngated(t *testing.T) {
	if testing.Short() {
		t.Skip("loads and type-checks every package of the module, five times")
	}
	root := repoRoot(t)
	allowFile := filepath.Join(root, "tests", "logcontent", allowlistPath)
	var prev map[string]allowed
	if _, err := os.Stat(allowFile); err == nil {
		prev = readAllowlist(t, allowFile)
	}
	listed, constructed := map[string]bool{}, map[string]bool{}
	for k, a := range prev {
		if a.reason != "" && !strings.HasPrefix(a.reason, contentReason) &&
			(strings.Contains(k, " | "+kindErrorf+" | ") || strings.Contains(k, " | "+kindErrorsNew+" | ")) {
			listed[k] = true
		}
		if strings.HasPrefix(a.reason, constructedReason) {
			constructed[k] = true
		}
	}
	all, scan := scanScope(t, root, listed, constructed)
	checkWords(t, root, scan)
	var sites []site
	for _, s := range all {
		if s.kind == kindOvergated {
			// No entry excuses one: the error carries no content, and the
			// line withholds keploy's own words from a user for nothing.
			t.Errorf("%s: an error whose origin carries no content is logged by its kind: log it whole (zap.Error, zap.NamedError, utils.LogError)", s)
			continue
		}
		sites = append(sites, s)
	}
	got := map[string]int{}
	where := map[string][]string{}
	for _, s := range sites {
		got[s.key()]++
		where[s.key()] = append(where[s.key()], s.String())
	}
	if out := os.Getenv("LOG_CONTENT_SITES_DUMP"); out != "" {
		var b strings.Builder
		for _, s := range sites {
			fmt.Fprintf(&b, "%s:%d:%d\t%s\t%s\t%s\n", s.file, s.line, s.col, s.kind, s.what, s.via)
		}
		if err := os.WriteFile(out, []byte(b.String()), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	if os.Getenv("LOG_CONTENT_SITES_WRITE_ALLOWLIST") == "1" {
		writeAllowlist(t, allowFile, got, prev)
	}
	allow := readAllowlist(t, allowFile)
	keys := map[string]bool{}
	for k := range got {
		keys[k] = true
	}
	for k := range allow {
		keys[k] = true
	}
	sorted := make([]string, 0, len(keys))
	for k := range keys {
		sorted = append(sorted, k)
	}
	sort.Strings(sorted)
	var news, lower, noReason int
	for _, k := range sorted {
		have, may := got[k], allow[k]
		switch {
		case have > may.count:
			news++
			t.Errorf("%d new site(s) of %q (%d listed): gate the value through logcontent or logurl, or list it in %s with why it carries no content\n\t%s",
				have-may.count, k, may.count, allowlistPath, strings.Join(where[k], "\n\t"))
		case have < may.count:
			lower++
			t.Errorf("%q occurs %d time(s) and %s lists %d: lower its count to %d (or drop the line at 0)", k, have, allowlistPath, may.count, have)
		}
		if have > 0 && may.count > 0 && may.reason == "" {
			noReason++
			t.Errorf("%q is listed with no reason: say why it carries no customer content", k)
		}
	}
	if t.Failed() {
		t.Logf("%d site(s) in scope; %d new, %d to lower, %d with no reason", len(sites), news, lower, noReason)
	}
}

// wantRe is a fixture line's "want:" comment: the kinds of the sites on it.
var wantRe = regexp.MustCompile(`// want: (.*)$`)

// TestScanFindsEachShape holds the scan to testdata/fixture, which logs a
// value the application sent every way the guard reads, gated and not: each
// line's sites are the kinds its "want:" comment names, and a line with none
// has no site. A scan that misses a shape would read as no content in every
// package of the module, and its allowlist would pass.
func TestScanFindsEachShape(t *testing.T) {
	root := repoRoot(t)
	const dir = "tests/logcontent/testdata/fixture"
	s := newScanner(root)
	s.add(load(t, root, runtime.GOOS, runtime.GOARCH, "", "./"+dir, "./"+dir+"/sentinel"), nil)
	got := map[int][]string{}
	for _, st := range s.list() {
		if st.file != dir+"/fixture.go" {
			t.Fatalf("a site outside the fixture: %s", st)
		}
		got[st.line] = append(got[st.line], st.kind)
	}
	src, err := os.ReadFile(filepath.Join(root, dir, "fixture.go"))
	if err != nil {
		t.Fatal(err)
	}
	want := map[int][]string{}
	for i, line := range strings.Split(string(src), "\n") {
		if m := wantRe.FindStringSubmatch(line); m != nil {
			want[i+1] = strings.Fields(strings.ReplaceAll(m[1], ",", " "))
		}
	}
	if len(want) == 0 {
		t.Fatal("the fixture names no site")
	}
	lines := map[int]bool{}
	for l := range got {
		lines[l] = true
	}
	for l := range want {
		lines[l] = true
	}
	for l := range lines {
		g, w := append([]string(nil), got[l]...), append([]string(nil), want[l]...)
		sort.Strings(g)
		sort.Strings(w)
		if strings.Join(g, " ") != strings.Join(w, " ") {
			t.Errorf("fixture.go:%d: sites %v, want %v", l, g, w)
		}
	}
}

// A site listed as constructed (constructedReason) stands for what its value
// is made from, which the scan then does not judge: the calls that give the
// function that makes it a value the application sent are no sites.
func TestAConstructedSiteStopsTheScanAtItsValue(t *testing.T) {
	root := repoRoot(t)
	const dir = "tests/logcontent/testdata/fixture"
	scan := func(constructed map[string]bool) map[string]bool {
		s := newScanner(root)
		s.constructed = constructed
		s.add(load(t, root, runtime.GOOS, runtime.GOARCH, "", "./"+dir, "./"+dir+"/sentinel"), nil)
		out := map[string]bool{}
		for _, st := range s.list() {
			out[st.key()] = true
		}
		return out
	}
	const write = dir + "/fixture.go | field | set tests/logcontent/testdata/fixture.shaped.Text(s[:min(3, len(s))])"
	const call = dir + "/fixture.go | field | shape(s)"
	before := scan(nil)
	if !before[write] || !before[call] {
		t.Fatalf("precondition: the write and the call are sites: %v %v", before[write], before[call])
	}
	after := scan(map[string]bool{write: true})
	if !after[write] {
		t.Fatal("the constructed write is no site any more: it is still one, listed")
	}
	if after[call] {
		t.Fatal("a call of the function that makes a constructed value is still judged")
	}
}
