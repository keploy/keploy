package tools

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"runtime"
	"strings"
	"testing"
	"time"

	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
	yamlLib "gopkg.in/yaml.v3"
)

// The file a developer opens holds their decisions. A generated config was
// 251 lines, of which two were theirs -- so the two that mattered were buried,
// and every default became a value the repository appeared to have chosen and
// a release could no longer change.
func TestCreateConfig_WritesOnlyWhatDiffers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keploy.yml")

	cfg := config.New()
	cfg.Command = "npx jest test/checkout.test.js"
	cfg.AppName = "checkout"
	cfg.Test.Delay = 30 // a real decision inside a nested block
	body, err := yamlLib.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := WriteMinimalConfig(zap.NewNop(), path, string(body)); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(written)

	for _, want := range []string{"command: npx jest test/checkout.test.js", "appName: checkout", "delay: 30"} {
		if !strings.Contains(got, want) {
			t.Errorf("the developer's own setting is missing: %q\n%s", want, got)
		}
	}
	// Their neighbours are not: one changed delay must not drag the whole
	// test block back into the file.
	// storageFormat and mock.name are deliberately pinned (they name files
	// already on disk); everything else that was not decided stays out.
	for _, unwanted := range []string{"host: localhost", "proxyPort:", "buildDelay:", "globalNoise:", "dnsPort:"} {
		if strings.Contains(got, unwanted) {
			t.Errorf("a default was written into the file: %q\n%s", unwanted, got)
		}
	}
	// And it says what it is, so a short file does not read as a truncated one.
	if !strings.Contains(got, "keploy config defaults") {
		t.Errorf("the header does not say where the rest of the settings are:\n%s", got)
	}

	// What it wrote must still MEAN the same thing: parsed over the defaults,
	// every value comes back.
	reloaded := config.New()
	if err := yamlLib.Unmarshal(written, reloaded); err != nil {
		t.Fatal(err)
	}
	if reloaded.Command != cfg.Command || reloaded.AppName != cfg.AppName || reloaded.Test.Delay != 30 {
		t.Fatalf("the short file did not round-trip: command=%q appName=%q delay=%d", reloaded.Command, reloaded.AppName, reloaded.Test.Delay)
	}
	if reloaded.Test.Host != cfg.Test.Host || reloaded.BuildDelay != cfg.BuildDelay {
		t.Fatalf("a stripped default did not come back from the defaults: host=%q buildDelay=%v", reloaded.Test.Host, reloaded.BuildDelay)
	}
}

// Nothing decided yet: a header and the pointer to the full set, not a
// hundred lines of values nobody chose.
func TestCreateConfig_NothingDecidedWritesOnlyTheHeader(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keploy.yml")
	if err := WriteMinimalConfig(zap.NewNop(), path, ""); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var values []string
	for _, line := range strings.Split(string(body), "\n") {
		if line == "" || strings.HasPrefix(strings.TrimSpace(line), "#") {
			continue
		}
		values = append(values, line)
	}
	// EXACTLY the settings pinned because they name files already on disk --
	// asserted by name, not by count: swapping a pinned key for an unpinned
	// one keeps the count and changes the meaning.
	want := []string{
		"storageFormat: yaml",
		"record:",
		"    testCaseNaming: descriptive",
		"mock:",
		"    name: default",
	}
	if len(values) != len(want) {
		t.Fatalf("a config nobody has configured wrote %d values, wanted %d:\n%s", len(values), len(want), body)
	}
	for i := range want {
		if values[i] != want[i] {
			t.Fatalf("value %d is %q, wanted %q; the file is:\n%s", i+1, values[i], want[i], body)
		}
	}
	if !strings.Contains(string(body), "keploy config defaults") {
		t.Errorf("an all-defaults file must still say where the defaults are:\n%s", body)
	}
	// ...and the settings a human opens this file to set are named, commented
	// out. Without them the file a new user generates has nothing in it to
	// edit and no hint of what could be.
	for _, hint := range []string{"# command:", "# containerName:"} {
		if !strings.Contains(string(body), hint) {
			t.Errorf("a config with nothing in it offers no starting point (%s missing):\n%s", hint, body)
		}
	}
	// A placeholder is a COMMENT: it must change nothing when loaded.
	var loaded config.Config
	if err := yamlLib.Unmarshal(body, &loaded); err != nil {
		t.Fatalf("the generated config does not parse: %v", err)
	}
	if loaded.Command != "" || loaded.ContainerName != "" {
		t.Errorf("a commented placeholder was loaded as a setting: command=%q containerName=%q", loaded.Command, loaded.ContainerName)
	}
	// keploy.yml names the process Keploy executes.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode&0o022 != 0 {
		t.Errorf("keploy.yml is group/world-writable (%04o): any local user could change the command Keploy runs", mode)
	}
}

// Every placeholder must be safe to UNCOMMENT. A commented `test:` block
// offered next to a `test:` the file already had would give the document two
// of that key, and YAML refuses a duplicated key outright -- a hint that
// breaks the file it is helping with.
func TestWriteMinimalConfig_PlaceholdersAreSafeToUncomment(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keploy.yml")
	if err := WriteMinimalConfig(zap.NewNop(), path, ""); err != nil {
		t.Fatal(err)
	}
	body, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var out []string
	uncommented := 0
	for _, line := range strings.Split(string(body), "\n") {
		// A placeholder is a commented SETTING at any depth -- "# key: value"
		// or "#   key:" opening a block. A prose comment is not. Matching
		// only the flat, valued form made this test blind to exactly the
		// shape that can collide.
		if regexp.MustCompile(`^#\s?(\s*)[A-Za-z][\w.-]*:(\s|$)`).MatchString(line) {
			out = append(out, regexp.MustCompile(`^#\s?`).ReplaceAllString(line, ""))
			uncommented++
			continue
		}
		out = append(out, line)
	}
	if uncommented == 0 {
		t.Fatal("no placeholders were offered, so this test is checking nothing")
	}
	var cfg config.Config
	if err := yamlLib.Unmarshal([]byte(strings.Join(out, "\n")), &cfg); err != nil {
		t.Fatalf("uncommenting the %d placeholders Keploy offers makes the config unloadable: %v", uncommented, err)
	}
}

// A config that already names a command needs no placeholders: the file has
// something in it to read, and repeating the key commented out below a real
// value reads as a second, contradictory setting.
func TestWriteMinimalConfig_NoPlaceholdersOverRealSettings(t *testing.T) {
	cfg := config.New()
	cfg.Command = "npm test"
	body, err := yamlLib.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "keploy.yml")
	if err := WriteMinimalConfig(zap.NewNop(), path, string(body)); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(written), "command: npm test") {
		t.Fatalf("the decision is missing:\n%s", written)
	}
	if strings.Contains(string(written), "# command:") {
		t.Errorf("a commented placeholder was written under a command the developer had already set:\n%s", written)
	}
}

// CreateConfig writes the document it is GIVEN. The strip belongs to the two
// places that generate a config for a human to read, not here: one caller
// reads keploy.yml into a zero-valued struct, edits a field and writes it
// back, so a document that omitted the defaults would come back with every
// default replaced by a Go zero -- proxyPort 0, mocking off. Found in review;
// this is the guard.
func TestCreateConfig_WritesTheDocumentItIsGiven(t *testing.T) {
	tools := &Tools{logger: zap.NewNop()}
	path := filepath.Join(t.TempDir(), "keploy.yml")

	cfg := config.New()
	cfg.Command = "npm test"
	body, err := yamlLib.Marshal(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if err := tools.CreateConfig(context.Background(), path, string(body)); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// Every default the caller handed over is still there: a consumer that
	// reads this file back gets what it wrote, not a zero value.
	for _, want := range []string{"command: npm test", "proxyPort:", "buildDelay:", "host: localhost"} {
		if !strings.Contains(string(written), want) {
			t.Errorf("CreateConfig dropped %q from the document it was given:\n%s", want, written)
		}
	}
	var back config.Config
	if err := yamlLib.Unmarshal(written, &back); err != nil {
		t.Fatal(err)
	}
	if back.ProxyPort != cfg.ProxyPort || back.Test.Host != cfg.Test.Host || back.BuildDelay != cfg.BuildDelay {
		t.Fatalf("a zero-valued read of the written file lost settings: proxyPort=%d host=%q buildDelay=%v",
			back.ProxyPort, back.Test.Host, back.BuildDelay)
	}
	// ...and at 0644. This is the only test that exercises the ordinary
	// write path, and it never looked: the mode could go back to 0777 with
	// the whole suite green, on the file that names the process Keploy runs.
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode&0o022 != 0 {
		t.Errorf("keploy.yml is group/world-writable (%04o): any local user could change the command Keploy runs", mode)
	}
}

// A file of nothing but comments -- the ordinary shape now -- must not crash
// the CLI when a caller reads it back and hands it over.
func TestCreateConfig_CommentsOnlyDocument(t *testing.T) {
	tools := &Tools{logger: zap.NewNop()}
	path := filepath.Join(t.TempDir(), "keploy.yml")
	if err := tools.CreateConfig(context.Background(), path, "# nothing but a comment\n"); err != nil {
		t.Fatal(err)
	}
	written, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	// The same file every other path writes: the caller's document, the
	// version header and the guide. This branch used to write the raw bytes
	// and nothing else, so a config that happened to be all comments came
	// out in a different shape from every other one.
	if !strings.Contains(string(written), "# nothing but a comment") {
		t.Errorf("the document it was given is missing:\n%s", written)
	}
	if !strings.Contains(string(written), "keploy.io/docs") {
		t.Errorf("a comments-only document was written without the guide every other path adds:\n%s", written)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode&0o022 != 0 {
		t.Errorf("keploy.yml is group/world-writable (%04o)", mode)
	}
}

// The mode is fixed on a file that ALREADY EXISTS, which is the only case
// that matters: os.WriteFile leaves an existing file's mode alone, and every
// other test here writes into a fresh t.TempDir() where 0644 comes for free.
// The upgrade path this hardening targets is a keploy.yml left 0777 by the
// previous release.
func TestWriteMinimalConfig_TakesTheWriteBitsOffAnExistingFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keploy.yml")
	if err := os.WriteFile(path, []byte("command: npm test\n"), 0o777); err != nil {
		t.Fatal(err)
	}
	// CHMOD, not the WriteFile mode: the process umask (022 by default)
	// strips the very bits this test is about, so the fixture was never
	// world-writable and the assertion could not fail.
	if err := os.Chmod(path, 0o777); err != nil {
		t.Fatal(err)
	}
	if err := WriteMinimalConfig(zap.NewNop(), path, ""); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode&0o022 != 0 {
		t.Errorf("a world-writable keploy.yml stayed world-writable (%04o): any local user could change the command Keploy runs", mode)
	}
}

// ...and a config somebody deliberately RESTRICTED is not widened. keploy.yml
// can carry mongoPassword; forcing an exact 0644 made it world-readable.
func TestWriteMinimalConfig_DoesNotWidenARestrictedFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "keploy.yml")
	if err := os.WriteFile(path, []byte("command: npm test\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := WriteMinimalConfig(zap.NewNop(), path, ""); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Errorf("a config the user restricted to 0600 was widened to %04o", mode)
	}
	// ...and one whose owner bits are unusual keeps them. Forcing an exact
	// 0644 would both widen this and take the execute bit off, which is the
	// user's business and not Keploy's.
	// 0776: the dangerous bits are there AND the owner bits differ from
	// 0644's, so clearing and forcing give different answers. A file whose
	// group/other bits are already safe never reaches the chmod at all.
	other := filepath.Join(t.TempDir(), "keploy.yml")
	if err := os.WriteFile(other, []byte("command: npm test\n"), 0o776); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(other, 0o776); err != nil {
		t.Fatal(err)
	}
	if err := WriteMinimalConfig(zap.NewNop(), other, ""); err != nil {
		t.Fatal(err)
	}
	oInfo, err := os.Stat(other)
	if err != nil {
		t.Fatal(err)
	}
	if mode := oInfo.Mode().Perm(); mode != 0o754 {
		t.Errorf("a 0776 config became %04o; only the group and world WRITE bits should have gone, not the rest", mode)
	}
}

// A link that resolves INSIDE the directory being written is an ordinary
// monorepo layout. Refusing it took the generator away from those repos; the
// thing that must never happen is writing through a link to somewhere else.
func TestWriteMinimalConfig_SymlinkRules(t *testing.T) {
	dir := t.TempDir()
	inner := filepath.Join(dir, "config")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	real := filepath.Join(inner, "keploy.yml")
	if err := os.WriteFile(real, []byte("command: old\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "keploy.yml")
	if err := os.Symlink(real, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := WriteMinimalConfig(zap.NewNop(), link, ""); err != nil {
		t.Fatalf("a link inside the same directory was refused: %v", err)
	}
	body, err := os.ReadFile(real)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(body), "Generated by Keploy") {
		t.Errorf("the link was accepted but nothing was written through it:\n%s", body)
	}

	// ...and one pointing out of the directory is refused, dangling or not.
	outside := filepath.Join(t.TempDir(), "elsewhere.yml")
	away := filepath.Join(dir, "away.yml")
	if err := os.Symlink(outside, away); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := WriteMinimalConfig(zap.NewNop(), away, ""); err == nil {
		t.Error("a link pointing outside the directory was written through")
	}
	if _, err := os.Stat(outside); err == nil {
		t.Error("a file was created outside the directory the user named")
	}
}

// The link a developer COMMITS is dangling by definition: the file it points
// at is the one the generator has not written yet. EvalSymlinks fails outright
// on such a link, and folding that failure into the containment test collapsed
// "dangling but inside" into "points outside" -- refusing the very monorepo
// layout the containment rule exists to allow, and telling the developer their
// link pointed somewhere it did not.
func TestWriteMinimalConfig_DanglingLinkInsideIsWrittenThrough(t *testing.T) {
	dir := t.TempDir()
	inner := filepath.Join(dir, "config")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(inner, "keploy.yml") // deliberately absent
	link := filepath.Join(dir, "keploy.yml")
	if err := os.Symlink(target, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := WriteMinimalConfig(zap.NewNop(), link, ""); err != nil {
		t.Fatalf("a dangling link inside the directory was refused: %v", err)
	}
	body, err := os.ReadFile(target)
	if err != nil {
		t.Fatalf("nothing was written through the link: %v", err)
	}
	if !strings.Contains(string(body), "Generated by Keploy") {
		t.Errorf("what came through the link is not a generated config:\n%s", body)
	}
}

// ...and a RELATIVE dangling link, which is what `ln -s config/keploy.yml`
// actually produces.
func TestWriteMinimalConfig_RelativeDanglingLinkInsideIsWrittenThrough(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "keploy.yml")
	if err := os.Symlink(filepath.Join("config", "keploy.yml"), link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := WriteMinimalConfig(zap.NewNop(), link, ""); err != nil {
		t.Fatalf("a relative dangling link inside the directory was refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "config", "keploy.yml")); err != nil {
		t.Fatalf("nothing was written through the relative link: %v", err)
	}
}

// The containment rule is the security boundary, and allowing a missing
// target must not weaken it. A dangling link pointing OUT is still refused --
// including one reached through a chain, and one expressed relatively.
func TestWriteMinimalConfig_DanglingLinkOutsideIsStillRefused(t *testing.T) {
	outer := t.TempDir()
	dir := filepath.Join(outer, "project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	cases := map[string]string{
		"absolute": filepath.Join(outer, "escaped.yml"),
		"relative": filepath.Join("..", "escaped-rel.yml"),
	}
	for name, dest := range cases {
		t.Run(name, func(t *testing.T) {
			link := filepath.Join(dir, "keploy.yml")
			_ = os.Remove(link)
			if err := os.Symlink(dest, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			if err := WriteMinimalConfig(zap.NewNop(), link, ""); err == nil {
				t.Fatal("a dangling link pointing outside the directory was written through")
			}
			abs := dest
			if !filepath.IsAbs(abs) {
				abs = filepath.Join(dir, dest)
			}
			if _, err := os.Stat(abs); err == nil {
				t.Fatalf("a file was created outside the directory the user named: %s", abs)
			}
		})
	}

	// A CHAIN that ends outside: the first hop looks local, so following only
	// one hop would report it as contained.
	hop := filepath.Join(dir, "hop.yml")
	if err := os.Symlink(filepath.Join(outer, "chained.yml"), hop); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	chained := filepath.Join(dir, "chain.yml")
	if err := os.Symlink(hop, chained); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := WriteMinimalConfig(zap.NewNop(), chained, ""); err == nil {
		t.Error("a chain of links ending outside the directory was written through")
	}
	if _, err := os.Stat(filepath.Join(outer, "chained.yml")); err == nil {
		t.Error("a file was created outside the directory via a chain of links")
	}
}

// A cycle has no target at all. It must be refused, and it must not spin.
func TestWriteMinimalConfig_LinkCycleIsRefused(t *testing.T) {
	dir := t.TempDir()
	a := filepath.Join(dir, "keploy.yml")
	b := filepath.Join(dir, "b.yml")
	if err := os.Symlink(b, a); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(a, b); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	done := make(chan error, 1)
	go func() { done <- WriteMinimalConfig(zap.NewNop(), a, "") }()
	select {
	case err := <-done:
		if err == nil {
			t.Error("a link cycle was accepted")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("a link cycle hung instead of being refused")
	}
}

// ".." after a symlinked DIRECTORY is the escape a lexical resolver cannot
// see. filepath.Join and filepath.Abs both Clean, collapsing ".." before the
// component in front of it has been resolved; the kernel resolves the link
// first and then takes ".." of the REAL parent. So "a/sub/../b.yml", where
// a/sub links out of the project, computes as inside it and is written
// outside it -- with no prompt and exit 0.
func TestWriteMinimalConfig_DotDotAfterASymlinkedDirectoryIsRefused(t *testing.T) {
	for _, form := range []string{"relative", "absolute"} {
		t.Run(form, func(t *testing.T) {
			outer := t.TempDir()
			dir := filepath.Join(outer, "project")
			if err := os.MkdirAll(filepath.Join(dir, "a"), 0o755); err != nil {
				t.Fatal(err)
			}
			// project/a/sub -> outer  (a directory outside the project)
			if err := os.Symlink(outer, filepath.Join(dir, "a", "sub")); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			precious := filepath.Join(outer, "b.yml")
			if err := os.WriteFile(precious, []byte("PRECIOUS\n"), 0o644); err != nil {
				t.Fatal(err)
			}
			// Built by hand, NOT with filepath.Join: Join cleans, so it
			// would collapse the ".." before the link is even created and
			// the test would exercise a path the kernel never sees.
			dest := "a/sub/../b.yml"
			if form == "absolute" {
				dest = dir + string(os.PathSeparator) + dest
			}
			link := filepath.Join(dir, "keploy.yml")
			if err := os.Symlink(dest, link); err != nil {
				t.Skipf("symlinks unavailable: %v", err)
			}
			err := WriteMinimalConfig(zap.NewNop(), link, "")
			body, rerr := os.ReadFile(precious)
			if rerr != nil {
				t.Fatalf("the file outside the project is gone: %v", rerr)
			}
			if string(body) != "PRECIOUS\n" {
				t.Fatalf("a file outside %s was overwritten through a link:\n%s", dir, body)
			}
			if err == nil {
				t.Fatal("the write was allowed through a link that resolves outside the directory")
			}
		})
	}
}

// ...and the same shape pointing back INSIDE still works, so the refusal
// above is about where the path lands and not about ".." being present.
func TestWriteMinimalConfig_DotDotThatLandsInsideIsAllowed(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "a", "b"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("a/b/../keploy.generated.yml", filepath.Join(dir, "keploy.yml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := WriteMinimalConfig(zap.NewNop(), filepath.Join(dir, "keploy.yml"), ""); err != nil {
		t.Fatalf("a link that resolves inside the directory was refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "a", "keploy.generated.yml")); err != nil {
		t.Fatalf("nothing was written through the link: %v", err)
	}
}

// The directory is compared by IDENTITY, not by spelling. On a
// case-insensitive volume -- the macOS default -- two spellings of one
// directory compared unequal as strings, and a link squarely inside it was
// refused with a message saying it pointed outside.
func TestWriteMinimalConfig_ADifferentSpellingOfTheSameDirectory(t *testing.T) {
	dir := t.TempDir()
	inner := filepath.Join(dir, "Config")
	if err := os.MkdirAll(inner, 0o755); err != nil {
		t.Fatal(err)
	}
	// Reach the same directory by a second name, via a link. On any
	// filesystem this is one directory with two spellings.
	if err := os.Symlink(inner, filepath.Join(dir, "cfg")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := os.Symlink(filepath.Join(dir, "cfg", "keploy.yml"), filepath.Join(dir, "keploy.yml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := WriteMinimalConfig(zap.NewNop(), filepath.Join(dir, "keploy.yml"), ""); err != nil {
		t.Fatalf("a link reaching a directory inside by another name was refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(inner, "keploy.yml")); err != nil {
		t.Fatalf("nothing was written through the link: %v", err)
	}
}

// The path that was CHECKED has to be the path that is opened.
//
// When the config does not exist yet -- the ordinary first run -- the check
// resolves to the leaf's own path, and a write follows whatever is at that
// path by the time it runs. A link planted in between sent the generated
// config wherever it pointed: roughly one attempt in six against a process
// doing nothing more exotic than `ln -sf` in a loop.
//
// This covers the CLI's window -- between the resolve that consent was asked
// about and the call to the writer. The writer's own, narrower window, between
// its resolve and its open, has no test seam; O_NOFOLLOW closes that one, and
// TestConfigWriteRefusesASymlink below pins the flag itself.
func TestWriteMinimalConfig_DoesNotFollowALinkPlantedAfterTheCheck(t *testing.T) {
	outer := t.TempDir()
	dir := filepath.Join(outer, "project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outer, "victim.yml")
	if err := os.WriteFile(victim, []byte("PRECIOUS\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "keploy.yml")

	// The check runs against an empty directory and returns the leaf path...
	resolved, err := ResolveConfigTarget(target)
	if err != nil {
		t.Fatal(err)
	}
	// Compared by name, not by string: the parent is resolved, so on darwin
	// this comes back under /private/var where the fixture said /var.
	if filepath.Base(resolved) != "keploy.yml" || filepath.Base(filepath.Dir(resolved)) != "project" {
		t.Fatalf("a missing leaf resolved to %q, which is not the file that was named", resolved)
	}
	// ...and by the time the write happens, that path is a link out.
	if err := os.Symlink(victim, target); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	err = WriteMinimalConfig(zap.NewNop(), resolved, "")
	body, rerr := os.ReadFile(victim)
	if rerr != nil {
		t.Fatalf("the file outside the project is gone: %v", rerr)
	}
	if string(body) != "PRECIOUS\n" {
		t.Fatalf("the write followed a link planted after the check:\n%s", body)
	}
	if err == nil {
		t.Fatal("writing through a link planted after the check was reported as success")
	}
}

// Containment is decided by inode, not by spelling.
//
// A string prefix is both too strict and too loose: on a case-insensitive
// volume two spellings of ONE directory compare unequal, and a link squarely
// inside was refused with a message saying it pointed outside.
func TestWriteMinimalConfig_ContainmentIsByIdentityNotSpelling(t *testing.T) {
	root := t.TempDir()
	lower := filepath.Join(root, "project")
	if err := os.MkdirAll(lower, 0o755); err != nil {
		t.Fatal(err)
	}
	// Does this volume treat the two spellings as one directory? On a
	// case-sensitive one they are genuinely different and there is nothing
	// here to test.
	upper := filepath.Join(root, "PROJECT")
	if _, err := os.Stat(upper); err != nil {
		t.Skipf("case-sensitive filesystem: %q and %q are different directories", lower, upper)
	}
	// The link sits at the directory named by --path and points at a file
	// inside it, spelled the OTHER way. One directory, two spellings: the
	// string prefix the inode check replaces reads the second as an escape.
	if err := os.MkdirAll(filepath.Join(lower, "config"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(lower, "config", "keploy.yml"), filepath.Join(upper, "keploy.yml")); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	if err := WriteMinimalConfig(zap.NewNop(), filepath.Join(upper, "keploy.yml"), ""); err != nil {
		t.Fatalf("a link inside the named directory, spelled differently, was refused: %v", err)
	}
	if _, err := os.Stat(filepath.Join(lower, "config", "keploy.yml")); err != nil {
		t.Fatalf("nothing was written: %v", err)
	}
}

// A path that is not a symlink must not be refused as one. The resolve step
// fails for an unreadable or non-directory parent too, and saying "symbolic
// link" there names something that is not in the path -- while displacing the
// message that says to check the directory.
func TestResolveConfigTarget_SaysWhatIsActuallyWrong(t *testing.T) {
	dir := t.TempDir()
	notADir := filepath.Join(dir, "notadir")
	if err := os.WriteFile(notADir, []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err := ResolveConfigTarget(filepath.Join(notADir, "keploy.yml"))
	if err == nil {
		t.Fatal("a config path under a regular file was accepted")
	}
	if strings.Contains(err.Error(), "symbolic link") {
		t.Errorf("no symlink is involved, but the refusal names one: %v", err)
	}
	if !strings.Contains(err.Error(), "directory") {
		t.Errorf("the refusal does not point at the directory: %v", err)
	}
}

// O_NOFOLLOW is what stops the writer's own check-then-open window, which no
// test can drive without a race. Pinned directly: if the platform constant
// ever becomes 0 on a system that has the flag, this says so.
func TestConfigWriteRefusesASymlink(t *testing.T) {
	// Gated on the PLATFORM, never on the constant: skipping when oNoFollow
	// is 0 would skip on exactly the regression this exists to catch.
	if runtime.GOOS == "windows" {
		t.Skip("windows has no O_NOFOLLOW")
	}
	dir := t.TempDir()
	victim := filepath.Join(dir, "victim.yml")
	if err := os.WriteFile(victim, []byte("PRECIOUS\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(dir, "link.yml")
	if err := os.Symlink(victim, link); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
	f, err := os.OpenFile(link, os.O_WRONLY|os.O_CREATE|os.O_TRUNC|oNoFollow, 0644)
	if err == nil {
		_ = f.Close()
		t.Fatal("the config write opened a symlink")
	}
	if body, _ := os.ReadFile(victim); string(body) != "PRECIOUS\n" {
		t.Fatalf("the file behind the link was truncated:\n%s", body)
	}
}

// The CLI resolves once to ask for consent, and WriteMinimalConfig resolves
// again before it writes. Resolving an already-resolved path has to give the
// same answer, or the file the user agreed to is not the file that is opened.
// It is not obviously true: the second call measures containment against the
// RESOLVED path's own parent, which is a different directory whenever a link
// points into a subdirectory.
func TestResolveConfigTargetIsIdempotent(t *testing.T) {
	cases := map[string]func(t *testing.T) string{
		"plain": func(t *testing.T) string {
			return filepath.Join(t.TempDir(), "keploy.yml")
		},
		"link into a subdirectory": func(t *testing.T) string {
			d := t.TempDir()
			mustDir(t, filepath.Join(d, "config"))
			mustLink(t, filepath.Join("config", "keploy.yml"), filepath.Join(d, "keploy.yml"))
			return filepath.Join(d, "keploy.yml")
		},
		"link to an existing file in a subdirectory": func(t *testing.T) string {
			d := t.TempDir()
			mustDir(t, filepath.Join(d, "config"))
			if err := os.WriteFile(filepath.Join(d, "config", "keploy.yml"), []byte("x"), 0o644); err != nil {
				t.Fatal(err)
			}
			mustLink(t, filepath.Join("config", "keploy.yml"), filepath.Join(d, "keploy.yml"))
			return filepath.Join(d, "keploy.yml")
		},
		"chain through two subdirectories": func(t *testing.T) string {
			d := t.TempDir()
			mustDir(t, filepath.Join(d, "a", "b"))
			mustLink(t, filepath.Join("a", "hop.yml"), filepath.Join(d, "keploy.yml"))
			mustLink(t, filepath.Join("b", "keploy.yml"), filepath.Join(d, "a", "hop.yml"))
			return filepath.Join(d, "keploy.yml")
		},
	}
	for name, mk := range cases {
		t.Run(name, func(t *testing.T) {
			p := mk(t)
			one, err := ResolveConfigTarget(p)
			if err != nil {
				t.Fatalf("first resolve refused: %v", err)
			}
			two, err := ResolveConfigTarget(one)
			if err != nil {
				t.Fatalf("resolving the resolved path refused it: %v", err)
			}
			if one != two {
				t.Errorf("not idempotent: %q then %q", one, two)
			}
		})
	}
}

func mustDir(t *testing.T, p string) {
	t.Helper()
	if err := os.MkdirAll(p, 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustLink(t *testing.T, target, at string) {
	t.Helper()
	if err := os.Symlink(target, at); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}
}

// Nothing outside the directory may be TOUCHED -- not its bytes, not its
// mode, not its owner.
//
// The window is between WriteMinimalConfig's own resolve and the syscalls
// that follow it, which nothing can step into deterministically, so this
// drives the race the way it happens and asserts what must hold however it
// lands. Checking only the file's CONTENT is what let the second half of this
// through: the write refused the link, and then chmod and chown re-resolved
// the same path by name a moment later and followed it. Under sudo that
// second one hands a root-owned file to the invoking user.
//
// Without the protections this fails within a few hundred attempts; with
// them it cannot fail at all, so there is nothing here to flake.
func TestWriteMinimalConfig_NeverWritesOutsideUnderARace(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("windows has no O_NOFOLLOW")
	}
	outer := t.TempDir()
	dir := filepath.Join(outer, "project")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	victim := filepath.Join(outer, "victim.yml")
	const precious = "PRECIOUS\n"
	if err := os.WriteFile(victim, []byte(precious), 0o644); err != nil {
		t.Fatal(err)
	}
	// Group- and world-writable, which is exactly what restrictConfig exists
	// to narrow -- so if it ever reaches this file, the mode changes and the
	// assertion below sees it.
	if err := os.Chmod(victim, 0o666); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(victim)
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(dir, "keploy.yml")
	if err := os.Symlink(victim, target); err != nil {
		t.Skipf("symlinks unavailable: %v", err)
	}

	// One flipper, doing exactly what `ln -sf` does in a loop.
	stop := make(chan struct{})
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			select {
			case <-stop:
				return
			default:
			}
			_ = os.Remove(target)
			_ = os.Symlink(victim, target)
		}
	}()
	fail := func(format string, args ...interface{}) {
		close(stop)
		<-done
		t.Fatalf(format, args...)
	}
	for i := 0; i < 400; i++ {
		_ = WriteMinimalConfig(zap.NewNop(), target, "")
		body, rerr := os.ReadFile(victim)
		if rerr != nil {
			fail("attempt %d: the file outside the directory is gone: %v", i, rerr)
		}
		if string(body) != precious {
			fail("attempt %d wrote through a link to a file outside the directory:\n%s", i, body)
		}
		now, serr := os.Stat(victim)
		if serr != nil {
			fail("attempt %d: %v", i, serr)
		}
		if now.Mode().Perm() != before.Mode().Perm() {
			fail("attempt %d chmod'd a file outside the directory: %v -> %v", i, before.Mode().Perm(), now.Mode().Perm())
		}
		if !os.SameFile(before, now) {
			fail("attempt %d replaced the file outside the directory", i)
		}
	}
	close(stop)
	<-done
}
