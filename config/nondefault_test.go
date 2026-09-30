package config

import (
	"strings"
	"testing"

	yamlLib "gopkg.in/yaml.v3"
)

const defs = `
path: ""
command: ""
port: 0
buildDelay: 30
test:
    delay: 5
    host: localhost
    port: 0
    skipCoverage: false
record:
    recordTimer: 0s
`

// The file a developer opens should hold their decisions, not Keploy's.
func TestStripDefaults_KeepsOnlyWhatDiffers(t *testing.T) {
	got, err := StripDefaults(`
path: ""
command: npm test
port: 0
buildDelay: 30
test:
    delay: 5
    host: localhost
    port: 8080
    skipCoverage: false
record:
    recordTimer: 0s
`, defs)
	if err != nil {
		t.Fatal(err)
	}
	want := "command: npm test\ntest:\n    port: 8080\n"
	if got != want {
		t.Fatalf("stripped config:\n%s\nwant:\n%s", got, want)
	}
}

// A nested mapping keeps its own differing keys and nothing else — one
// changed timeout must not drag its siblings back into the file.
func TestStripDefaults_RecursesIntoMappings(t *testing.T) {
	got, err := StripDefaults("test:\n    delay: 30\n    host: localhost\n    port: 0\n", defs)
	if err != nil {
		t.Fatal(err)
	}
	if got != "test:\n    delay: 30\n" {
		t.Fatalf("got %q", got)
	}
}

// Everything at its default leaves nothing to write: an empty result, which
// the caller renders as a header and a pointer to `keploy config defaults`.
func TestStripDefaults_AllDefaultsIsEmpty(t *testing.T) {
	got, err := StripDefaults(defs, defs)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("expected nothing to write, got:\n%s", got)
	}
}

// A key Keploy's defaults do not mention is the developer's, and survives.
func TestStripDefaults_KeepsUnknownKeys(t *testing.T) {
	got, err := StripDefaults("command: \"\"\nmyOwnKey: 1\nnested:\n    a: b\n", defs)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(got, "myOwnKey: 1") || !strings.Contains(got, "a: b") {
		t.Fatalf("dropped a key Keploy does not own:\n%s", got)
	}
	if strings.Contains(got, "command") {
		t.Fatalf("kept a default:\n%s", got)
	}
}

// Style is not meaning: a value written differently but equal to the default
// is still the default. Sequences compare whole.
func TestStripDefaults_ComparesValuesNotSpelling(t *testing.T) {
	got, err := StripDefaults("port: 0x0\ntest:\n    host: \"localhost\"\n", defs)
	if err != nil {
		t.Fatal(err)
	}
	if got != "" {
		t.Fatalf("a restyled default survived:\n%s", got)
	}
	seq, err := StripDefaults("tags: [a, b]\n", "tags: [a, b]\n")
	if err != nil {
		t.Fatal(err)
	}
	if seq != "" {
		t.Fatalf("an identical sequence survived:\n%s", seq)
	}
	seq2, err := StripDefaults("tags: [a, c]\n", "tags: [a, b]\n")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(seq2, "c") {
		t.Fatalf("a changed sequence was dropped:\n%s", seq2)
	}
}

// A setting that describes files ALREADY ON DISK is written even when it
// equals today's default. The file outlives the default: a release that
// changed storageFormat would otherwise orphan every recording in a repo
// whose keploy.yml never mentioned it, with nothing in the repo changed.
func TestStripDefaults_PinsWhatDescribesFilesOnDisk(t *testing.T) {
	// A setting that NAMES something already on disk is written even at its
	// default, because the artifact outlives the default: a release that
	// changed it would orphan every recording in a repository whose
	// keploy.yml never mentioned it, and nothing in the repo would have
	// changed to explain why.
	//
	// This asserts the RULE, not today's list: every pinned key survives a
	// config that sets it to exactly its default, and a key that is not
	// pinned does not. A list assertion would have to be edited to add a
	// pin, which is how the last omission survived review.
	defaults, err := DefaultsYAML()
	if err != nil {
		t.Fatalf("DefaultsYAML: %v", err)
	}
	got, err := StripDefaults(defaults, defaults)
	if err != nil {
		t.Fatalf("StripDefaults: %v", err)
	}
	for key := range pinned {
		leaf := key[strings.LastIndex(key, ".")+1:]
		if !strings.Contains(got, leaf+":") {
			t.Errorf("%s is pinned but was stripped; a file on disk it names would be orphaned by a changed default\n%s", key, got)
		}
	}
	for _, unpinned := range []string{"proxyPort", "dnsPort", "buildDelay", "mocking"} {
		if strings.Contains(got, unpinned+":") {
			t.Errorf("%s is at its default and not pinned, but was written anyway:\n%s", unpinned, got)
		}
	}
}

func TestPinned_NamesOnlyArtifactsOnDisk(t *testing.T) {
	// Every pinned key must exist in the defaults -- a typo would pin
	// nothing and no test would notice, because stripping a key that is not
	// there looks exactly like keeping one that is.
	defaults, err := DefaultsYAML()
	if err != nil {
		t.Fatalf("DefaultsYAML: %v", err)
	}
	var doc map[string]interface{}
	if err := yamlLib.Unmarshal([]byte(defaults), &doc); err != nil {
		t.Fatalf("parse defaults: %v", err)
	}
	for key := range pinned {
		cur := doc
		parts := strings.Split(key, ".")
		for i, part := range parts {
			v, ok := cur[part]
			if !ok {
				t.Fatalf("pinned key %q does not exist in the defaults (at %q), so it pins nothing", key, part)
			}
			if i == len(parts)-1 {
				break
			}
			next, ok := v.(map[string]interface{})
			if !ok {
				t.Fatalf("pinned key %q walks through %q, which is not a mapping", key, part)
			}
			cur = next
		}
	}
}

func TestStripDefaults_OneDecisionWritesOneDecision(t *testing.T) {
	full, err := DefaultsYAML()
	if err != nil {
		t.Fatal(err)
	}
	withCmd, err := StripDefaults(strings.Replace(full, `command: ""`, "command: go test ./...", 1), full)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(withCmd, "command: go test ./...") {
		t.Fatalf("the decision is missing:\n%s", withCmd)
	}
	// EXACTLY the decision and the pinned keys that name files on disk --
	// not 251 lines, and not "fewer than five", which a regression writing
	// four wrong lines would also satisfy.
	var kept []string
	for _, l := range strings.Split(strings.TrimSpace(withCmd), "\n") {
		if strings.TrimSpace(l) != "" {
			kept = append(kept, l)
		}
	}
	want := []string{
		"storageFormat: yaml",
		"command: go test ./...",
		"record:",
		"    testCaseNaming: descriptive",
		"mock:",
		"    name: default",
	}
	if len(kept) != len(want) {
		t.Fatalf("a config with one decision wrote %d lines, wanted %d:\n%s", len(kept), len(want), withCmd)
	}
	for i := range want {
		if kept[i] != want[i] {
			t.Fatalf("line %d is %q, wanted %q; the whole file is:\n%s", i+1, kept[i], want[i], withCmd)
		}
	}
}

// DefaultsYAML must describe what the BINARY applies, which is the struct --
// not config/default.go's hand-written document, which omits fields the
// struct has. Every field the document omits looks like a decision the
// developer made and survives the strip: measured, 42 lines against the
// document versus 2 against the struct. Nothing else in the suite can see
// that difference, so this is where it is held.
func TestDefaultsYAML_CoversEveryFieldTheBinaryApplies(t *testing.T) {
	doc, err := DefaultsYAML()
	if err != nil {
		t.Fatal(err)
	}
	full, err := yamlLib.Marshal(New())
	if err != nil {
		t.Fatal(err)
	}
	var all, published map[string]interface{}
	if err := yamlLib.Unmarshal(full, &all); err != nil {
		t.Fatal(err)
	}
	if err := yamlLib.Unmarshal([]byte(doc), &published); err != nil {
		t.Fatal(err)
	}
	for key := range all {
		if key == "agent" { // Keploy manages it; a copy in a repo only goes stale
			continue
		}
		if _, ok := published[key]; !ok {
			t.Errorf("the defaults omit %q, so a config that sets it to the default would keep it forever", key)
		}
	}
	if _, ok := published["agent"]; ok {
		t.Error("the defaults publish the internally-managed agent block")
	}
}

func TestStripDefaults_RefusesMoreThanOneDocument(t *testing.T) {
	// Both helpers rewrite one document. A second one used to be dropped in
	// silence, so a caller got back a config shorter than the one it gave.
	_, err := StripDefaults("command: npm test\n---\ncommand: other\n", "command: \"\"\n")
	if err == nil {
		t.Fatal("a two-document config was accepted; everything after the first --- would be lost")
	}
	if _, err := WithoutKey("a: 1\n---\nb: 2\n", "a"); err == nil {
		t.Fatal("WithoutKey accepted a two-document config")
	}
}

func TestStripDefaults_KnowsWhatItCompares(t *testing.T) {
	// Values are compared as YAML DECODES them, which makes 0x0 and 0 the
	// same port and "localhost" and localhost the same host. It does not
	// make 60s and 1m0s the same duration: those decode to two different
	// strings, and Keploy parses them later. The cost is a config that
	// restates a default in another spelling keeps that line -- visible, and
	// not wrong. Stated here so nobody reads the comparison as semantic.
	got, err := StripDefaults("test:\n  healthPollTimeout: 60s\n", "test:\n  healthPollTimeout: 1m0s\n")
	if err != nil {
		t.Fatalf("StripDefaults: %v", err)
	}
	if !strings.Contains(got, "healthPollTimeout") {
		t.Fatalf("a differently-spelled duration was dropped as a default; the comparison is by decoded value, not by meaning:\n%s", got)
	}
}

func TestDefaultsYAML_IsWhatTheBinaryApplies(t *testing.T) {
	// The document must not merely LIST every setting; each value must be
	// the value the binary uses. Two of them were not: `path` printed "" for
	// a flag that defaults to ".", and report.format printed "" for a flag
	// that defaults to "text" -- so `keploy config defaults -o keploy.yml`
	// changed the behaviour of the directory it was run in.
	doc, err := DefaultsYAML()
	if err != nil {
		t.Fatalf("DefaultsYAML: %v", err)
	}
	var loaded Config
	if err := yamlLib.Unmarshal([]byte(doc), &loaded); err != nil {
		t.Fatalf("the defaults Keploy prints do not load back: %v", err)
	}
	want := New()
	// agent is deliberately absent: Keploy resolves it per run.
	loaded.Agent = want.Agent
	// Compared as documents, not as structs: a nil slice and an empty one
	// are the same config, and reporting them as a difference would make
	// this test noise rather than a guard.
	gotDoc, err := yamlLib.Marshal(&loaded)
	if err != nil {
		t.Fatalf("marshal loaded: %v", err)
	}
	wantDoc, err := yamlLib.Marshal(want)
	if err != nil {
		t.Fatalf("marshal wanted: %v", err)
	}
	if string(gotDoc) != string(wantDoc) {
		gotLines, wantLines := strings.Split(string(gotDoc), "\n"), strings.Split(string(wantDoc), "\n")
		for i := range wantLines {
			if i >= len(gotLines) || gotLines[i] != wantLines[i] {
				g := "(end of document)"
				if i < len(gotLines) {
					g = gotLines[i]
				}
				t.Fatalf("the defaults Keploy PRINTS are not the defaults Keploy APPLIES, from line %d:\n  printed: %s\n  applied: %s", i+1, g, wantLines[i])
			}
		}
		t.Fatalf("the printed defaults carry %d lines the binary does not apply", len(gotLines)-len(wantLines))
	}
}

func TestDefaultsYAML_ListsEverySettingAtEveryDepth(t *testing.T) {
	// The old check walked top-level keys only, so dropping record.metadata
	// or the whole record.recordBuffer block passed.
	full, err := yamlLib.Marshal(New())
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	stripped, err := WithoutKey(string(full), "agent")
	if err != nil {
		t.Fatalf("WithoutKey: %v", err)
	}
	doc, err := DefaultsYAML()
	if err != nil {
		t.Fatalf("DefaultsYAML: %v", err)
	}
	want := paths(t, stripped)
	got := paths(t, doc)
	for p := range want {
		if _, ok := got[p]; !ok {
			t.Errorf("%s is a setting the binary applies and the defaults do not list", p)
		}
	}
	for p := range got {
		if _, ok := want[p]; !ok {
			t.Errorf("%s is listed in the defaults but is not a setting the binary has", p)
		}
	}
}

func TestDefaultsYAML_KeepsTheDocumentation(t *testing.T) {
	// The generated config stopped carrying the sixty lines that explain the
	// non-obvious knobs. This command is now the only place they are
	// reachable, so it has to carry them.
	doc, err := DefaultsYAML()
	if err != nil {
		t.Fatalf("DefaultsYAML: %v", err)
	}
	for _, explained := range []string{"strictMockWindow", "upstreamTls", "mysqlPorts", "disableMysqlAutoDetect"} {
		if !strings.Contains(doc, explained) {
			t.Fatalf("%s is missing from the defaults entirely", explained)
		}
	}
	if n := strings.Count(doc, "#"); n < 30 {
		t.Errorf("the defaults carry only %d comment lines; the documentation for the settings they list is gone", n)
	}
}

// paths returns every leaf and mapping path in a YAML document, dotted.
func paths(t *testing.T, doc string) map[string]bool {
	t.Helper()
	var m map[string]interface{}
	if err := yamlLib.Unmarshal([]byte(doc), &m); err != nil {
		t.Fatalf("parse: %v", err)
	}
	out := map[string]bool{}
	var walk func(prefix string, v map[string]interface{})
	walk = func(prefix string, v map[string]interface{}) {
		for k, val := range v {
			p := k
			if prefix != "" {
				p = prefix + "." + k
			}
			out[p] = true
			if child, ok := val.(map[string]interface{}); ok && len(child) > 0 {
				walk(p, child)
			}
		}
	}
	walk("", m)
	return out
}

func TestStripDefaults_PinnedKeepsTheUserValue(t *testing.T) {
	// The pinned keys are written even at their default. What must never
	// happen is writing the DEFAULT over a value the user chose: a repo that
	// records in json, into a named set, under sequential filenames would
	// have its config silently rewritten to yaml/default/descriptive --
	// orphaning exactly the artifacts the pinning exists to protect. Every
	// test here compared the defaults against themselves, so it could not
	// tell "kept the user's value" from "substituted the default".
	defaults, err := DefaultsYAML()
	if err != nil {
		t.Fatal(err)
	}
	mine := strings.NewReplacer(
		"storageFormat: yaml", "storageFormat: json",
		"name: default", "name: checkout-suite",
		"testCaseNaming: descriptive", "testCaseNaming: sequential",
	).Replace(defaults)
	if mine == defaults {
		t.Fatal("the fixture changed nothing; the defaults no longer contain the values this test edits")
	}
	got, err := StripDefaults(mine, defaults)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"storageFormat: json", "name: checkout-suite", "testCaseNaming: sequential"} {
		if !strings.Contains(got, want) {
			t.Errorf("a pinned setting lost the value the repository chose (%q):\n%s", want, got)
		}
	}
	for _, gone := range []string{"storageFormat: yaml", "name: default", "testCaseNaming: descriptive"} {
		if strings.Contains(got, gone) {
			t.Errorf("a pinned setting was written back at its DEFAULT, over the value the repository chose (%q):\n%s", gone, got)
		}
	}
}

func TestDefaultsYAML_CommentsLandOnTheSettingTheyExplain(t *testing.T) {
	// The documentation is transplanted from a second document by key path.
	// A transplant that is off by one attaches every explanation to the
	// following setting -- the document still has its sixty comment lines,
	// every substring check still passes, and every knob is described by
	// somebody else's paragraph.
	//
	// Each phrase below is unique to one comment block in config/default.go
	// and belongs to the setting named beside it.
	doc, err := DefaultsYAML()
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []struct{ phrase, key string }{
		{"KEPLOY_STRICT_MOCK_WINDOW", "strictMockWindow"},
		{"pins extra ports to the MySQL parser", "mysqlPorts"},
		{"hang its handshake", "disableMysqlAutoDetect"},
		{"authenticates the REAL upstream", "upstreamTls"},
	} {
		lines := strings.Split(doc, "\n")
		found := -1
		for i, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "#") && strings.Contains(line, want.phrase) {
				if found >= 0 {
					t.Fatalf("%q appears in more than one comment; pick a phrase unique to one block", want.phrase)
				}
				found = i
			}
		}
		if found < 0 {
			t.Errorf("no comment in the defaults carries %q, so the explanation of %s is gone", want.phrase, want.key)
			continue
		}
		landed := ""
		for _, line := range lines[found:] {
			trimmed := strings.TrimSpace(line)
			if trimmed == "" || strings.HasPrefix(trimmed, "#") {
				continue
			}
			if i := strings.Index(trimmed, ":"); i > 0 {
				landed = strings.TrimSpace(trimmed[:i])
			}
			break
		}
		if landed != want.key {
			t.Errorf("the comment carrying %q heads %q, not the %q it explains", want.phrase, landed, want.key)
		}
	}
}
