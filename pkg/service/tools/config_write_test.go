package tools

import (
	"context"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

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
