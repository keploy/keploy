package tools

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"go.keploy.io/server/v3/config"
	"go.keploy.io/server/v3/pkg/models"
	setfile "go.keploy.io/server/v3/pkg/platform/yaml"
	"go.keploy.io/server/v3/pkg/platform/yaml/configdb/testset"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.uber.org/zap"
	"go.uber.org/zap/zaptest/observer"
)

// setOnDisk is a keploy directory holding one test set, test-set-0, written
// and read by the stores keploy templatize runs with.
type setOnDisk struct {
	dir  string
	db   *testdb.TestYaml
	conf *testset.Db[*models.TestSet]
}

// newSetOnDisk records cases as test-set-0 in a fresh directory, in format,
// with config.yaml holding template (none when nil).
func newSetOnDisk(t *testing.T, format setfile.Format, cases []*models.TestCase, template map[string]interface{}) *setOnDisk {
	t.Helper()
	dir := t.TempDir()
	s := &setOnDisk{dir: dir, db: testdb.NewWithFormat(zap.NewNop(), dir, format), conf: testset.New[*models.TestSet](zap.NewNop(), dir)}
	for _, tc := range cases {
		cp := *tc
		cp.Noise = map[string][]string{}
		require.NoError(t, s.db.UpdateTestCase(context.Background(), &cp, "test-set-0", false))
	}
	if template != nil {
		require.NoError(t, s.conf.Write(context.Background(), "test-set-0", &models.TestSet{Template: template, PreScript: "echo before", AppCommand: "./app"}))
	}
	return s
}

// path is a file of the set.
func (s *setOnDisk) path(name string) string { return filepath.Join(s.dir, "test-set-0", name) }

// files is every file under the set, by its path in it.
func (s *setOnDisk) files(t *testing.T) map[string]string {
	t.Helper()
	out := map[string]string{}
	root := filepath.Join(s.dir, "test-set-0")
	require.NoError(t, filepath.Walk(root, func(p string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() {
			return err
		}
		data, err := os.ReadFile(p)
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(root, p)
		out[filepath.ToSlash(rel)] = string(data)
		return err
	}))
	return out
}

// edit rewrites a file of the set.
func (s *setOnDisk) edit(t *testing.T, name string, change func(string) string) {
	t.Helper()
	data, err := os.ReadFile(s.path(name))
	require.NoError(t, err)
	after := change(string(data))
	require.NotEqual(t, string(data), after, "the edit changed nothing in %s", name)
	require.NoError(t, os.WriteFile(s.path(name), []byte(after), 0o644))
}

// templatize runs keploy templatize over the set as the command does, with
// db as its test case store (the set's own when nil), and returns its error
// and every line it logged.
func (s *setOnDisk) templatize(t *testing.T, db TestDB) (error, *observer.ObservedLogs) {
	t.Helper()
	if db == nil {
		db = s.db
	}
	core, logs := observer.New(zap.InfoLevel)
	cfg := &config.Config{Path: s.dir, Templatize: config.Templatize{TestSets: []string{"test-set-0"}}}
	tools := NewTools(zap.New(core), s.conf, db, nil, nil, cfg)
	return tools.Templatize(context.Background()), logs
}

// createThenRead is a create that answers an id and a read-back by it: a
// plain recording templatize writes into.
func createThenRead() []*models.TestCase {
	js := map[string]string{"Content-Type": "application/json"}
	return []*models.TestCase{
		tcase("test-1", "POST", "http://h/users", js, `{"name":"alice"}`, 201, js, `{"id":"u_REC1"}`),
		tcase("test-2", "GET", "http://h/users/u_REC1", nil, "", 200, js, `{"id":"u_REC1"}`),
	}
}

var heldFileLineRe = regexp.MustCompile(`^test-set-0 holds template placeholders already \((.*) at (\S+) line (\d+)\): keploy templatize writes only into a plain recording, so it changes nothing in this set$`)

// heldLine is the one warning templatize gave for a set it left as it is: the
// placeholder it names, and the line of the file it names.
func heldLine(t *testing.T, s *setOnDisk, logs *observer.ObservedLogs) (text, file, line string) {
	t.Helper()
	warned := logs.FilterMessageSnippet("holds template placeholders already").All()
	require.Len(t, warned, 1, "%v", logs.All())
	assert.Equal(t, heldStep, warned[0].ContextMap()["next_step"])
	m := heldFileLineRe.FindStringSubmatch(warned[0].Message)
	require.NotNil(t, m, warned[0].Message)
	n, err := strconv.Atoi(m[3])
	require.NoError(t, err)
	data, err := os.ReadFile(s.path(filepath.Join("tests", m[2])))
	require.NoError(t, err, "the file the line names")
	lines := strings.Split(string(data), "\n")
	require.True(t, n >= 1 && n <= len(lines), "line %d of %d", n, len(lines))
	return m[1], m[2], lines[n-1]
}

// TEMPLATIZE AS THE COMMAND RUNS IT, ON FILES: a plain recording gets its
// placeholders in the test case files and its keys in config.yaml, whose
// other fields stay; nothing else is left in the set. Run again, on its own
// output, it changes no byte and names the first placeholder by file and
// line.
func TestTemplatize_WritesAPlainRecordingOnceAndThenLeavesIt(t *testing.T) {
	s := newSetOnDisk(t, setfile.FormatYAML, createThenRead(), map[string]interface{}{})
	plain := s.files(t)
	err, logs := s.templatize(t, nil)
	require.NoError(t, err)
	assert.Empty(t, logs.FilterMessageSnippet("holds template placeholders already").All())
	first := s.files(t)
	assert.Len(t, first, len(plain), "no file is left beside the set's own: %v", keysOf(first))
	assert.Contains(t, first["tests/test-1.yaml"], `{"id":"{{capture string .id}}"}`)
	assert.Contains(t, first["tests/test-2.yaml"], "http://h/users/{{string .id}}")
	assert.Contains(t, first["tests/test-2.yaml"], `{"id":"{{string .id}}"}`)
	got, readErr := s.conf.ReadForUpdate(context.Background(), "test-set-0")
	require.NoError(t, readErr)
	assert.Equal(t, map[string]interface{}{"id": "u_REC1"}, got.Template)
	assert.Equal(t, "echo before", got.PreScript, "config.yaml keeps its other fields")
	assert.Equal(t, "./app", got.AppCommand)

	err, logs = s.templatize(t, nil)
	require.NoError(t, err)
	assert.Equal(t, first, s.files(t), "a second run changes no byte")
	text, file, line := heldLine(t, s, logs)
	assert.Equal(t, "{{capture string .id}}", text)
	assert.Equal(t, "test-1.yaml", file)
	assert.Contains(t, line, `{"id":"{{capture string .id}}"}`)
}

// lineBefore matches the line before the line that holds next, keeping that
// line's indentation (group 1) and everything from the next line on (group 2).
func lineBefore(next string) *regexp.Regexp {
	return regexp.MustCompile(`(?m)^([ \t]*)[^\n]*\n([ \t]*` + regexp.QuoteMeta(next) + `)`)
}

func keysOf(m map[string]string) []string {
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out
}

// TEMPLATE TEXT ANYWHERE IN A FILE OF THE SET LEAVES THE SET AS IT IS. The
// rule reads the files, not places: text keploy test reads nowhere (a
// comment, a field it does not know, a plain or a block scalar of one, a key
// of the document, the curl line), text in a name or in a cookie's
// attribute, and text the file writes with an escape of its own syntax
// (YAML's \x7B, a line continued with a backslash, a !!binary value, JSON's
// \u007b) all count as a placeholder in a body does. Round 11 read the places
// keploy test reads, missed one (a Set-Cookie value with no name=), and wrote
// a capture beside the use it had not seen. Each set below is the plain
// recording of TestTemplatize_WritesAPlainRecordingOnceAndThenLeavesIt with
// one such text put in: no byte changes, and the one line names the text, its
// file and its line.
func TestTemplatize_TemplateTextAnywhereInAFileLeavesTheSetAsItIs(t *testing.T) {
	binaryBody := base64.StdEncoding.EncodeToString([]byte(`{"id":"{{capture string .id}}"}`))
	// The response's one header, the line before its body.
	responseHeader := lineBefore(`body: '{"id":"u_REC1"}'`)
	replace := func(old, with string) func(string) string {
		return func(s string) string { return strings.Replace(s, old, with, 1) }
	}
	appendLine := func(line string) func(string) string {
		return func(s string) string { return strings.TrimRight(s, "\n") + "\n" + line + "\n" }
	}
	for _, c := range []struct {
		name   string
		format setfile.Format
		file   string
		change func(string) string
		text   string // the placeholder the line names
		line   string // a text the named line holds
	}{
		{"a comment", setfile.FormatYAML, "tests/test-2.yaml", appendLine("# the id was {{string .id}} before"), "{{string .id}}", "# the id was {{string .id}} before"},
		{"a field keploy does not know", setfile.FormatYAML, "tests/test-1.yaml", appendLine("reviewer_note: 'see {{string .id}}'"), "{{string .id}}", "reviewer_note:"},
		{"a key of the document", setfile.FormatYAML, "tests/test-1.yaml", appendLine("'{{string .id}}': kept"), "{{string .id}}", ": kept"},
		{"the curl line", setfile.FormatYAML, "tests/test-2.yaml", appendLine("curl: curl http://h/users/{{string .id}}"), "{{string .id}}", "curl: curl http://h/users/{{string .id}}"},
		{"a header's name", setfile.FormatYAML, "tests/test-1.yaml", replace("Content-Type: application/json", "'X-{{string .h}}': application/json"), "{{string .h}}", "X-{{string .h}}"},
		{"a Set-Cookie value with no name=", setfile.FormatYAML, "tests/test-1.yaml", func(s string) string {
			return responseHeader.ReplaceAllString(s, "${1}Set-Cookie: '{{string .sid}}; Path=/'\n${2}")
		}, "{{string .sid}}", "Set-Cookie: '{{string .sid}}; Path=/'"},
		{"a cookie's attribute", setfile.FormatYAML, "tests/test-1.yaml", func(s string) string {
			return responseHeader.ReplaceAllString(s, "${1}Set-Cookie: 'sid=abc; Path=/{{string .p}}'\n${2}")
		}, "{{string .p}}", "Set-Cookie: 'sid=abc; Path=/{{string .p}}'"},
		{"a plain scalar", setfile.FormatYAML, "tests/test-2.yaml", appendLine("reviewer_note: read back with {{string .id}}"), "{{string .id}}", "reviewer_note: read back with {{string .id}}"},
		{"a later line of a block scalar", setfile.FormatYAML, "tests/test-2.yaml", appendLine("reviewer_note: |\n  read back\n  with {{string .id}}"), "{{string .id}}", "  with {{string .id}}"},
		{"a header's name written with YAML's \\x escape", setfile.FormatYAML, "tests/test-1.yaml", replace("Content-Type: application/json", `"X-\x7B\x7Bstring .h}}": application/json`), "{{string .h}}", `"X-\x7B\x7Bstring .h}}": application/json`},
		{"written with YAML's \\x escape", setfile.FormatYAML, "tests/test-2.yaml", replace("url: http://h/users/u_REC1", `url: "http://h/users/\x7B\x7Bstring .id}}"`), "{{string .id}}", `url: "http://h/users/\x7B\x7Bstring .id}}"`},
		{"written over a line continued with a backslash", setfile.FormatYAML, "tests/test-2.yaml", replace("url: http://h/users/u_REC1", "url: \"http://h/users/{{string \\\n            .id}}\""), "{{string .id}}", `url: "http://h/users/{{string \`},
		{"written as a !!binary value", setfile.FormatYAML, "tests/test-1.yaml", replace(`body: '{"id":"u_REC1"}'`, "body: !!binary "+binaryBody), "{{capture string .id}}", "body: !!binary " + binaryBody},
		{"written with JSON's \\u escape", setfile.FormatJSON, "tests/test-2.json", replace(`"url": "http://h/users/u_REC1"`, `"url": "http://h/users/\u007b\u007bstring .id\u007d\u007d"`), "{{string .id}}", `"url": "http://h/users/\u007b\u007bstring .id\u007d\u007d"`},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSetOnDisk(t, c.format, createThenRead(), nil)
			s.edit(t, c.file, c.change)
			before := s.files(t)
			err, logs := s.templatize(t, nil)
			require.NoError(t, err)
			assert.Equal(t, before, s.files(t), "no byte of the set changes")
			text, file, line := heldLine(t, s, logs)
			assert.Equal(t, c.text, text)
			assert.Equal(t, filepath.Base(c.file), file)
			assert.Contains(t, line, c.line)
			assert.Len(t, logs.All(), 1, "one line, and nothing else")
		})
	}

	// In config.yaml's template: its place is the key.
	s := newSetOnDisk(t, setfile.FormatYAML, createThenRead(), map[string]interface{}{"greeting": "Hi {{ .Name }}"})
	before := s.files(t)
	err, logs := s.templatize(t, nil)
	require.NoError(t, err)
	assert.Equal(t, before, s.files(t))
	warned := logs.FilterMessageSnippet("holds template placeholders already").All()
	require.Len(t, warned, 1)
	assert.Equal(t, "test-set-0 holds template placeholders already ({{ .Name }} at config.yaml template.greeting): keploy templatize writes only into a plain recording, so it changes nothing in this set", warned[0].Message)
}

// refusingRenames is the set's test case store on a platform that will not
// put one file's new bytes in place (a program holds the file open, or the
// disk fills): ReplaceFiles is the real one, with that rename refused.
type refusingRenames struct {
	*testdb.TestYaml
	refuse func(src string) bool
}

func (r refusingRenames) ReplaceFiles(_ context.Context, files []setfile.File) error {
	return setfile.ReplaceFilesWith(zap.NewNop(), setfile.ReplaceHooks{Rename: func(src, dst string) error {
		if r.refuse(src) {
			return &os.LinkError{Op: "rename", Old: src, New: dst, Err: errors.New("sharing violation")}
		}
		return os.Rename(src, dst)
	}}, files)
}

// A RUN THAT FAILS LEAVES THE SET EXACTLY AS IT WAS, and the next run starts
// from the same plain recording: it never says "nothing to do". Before, the
// test cases were written one by one and config.yaml last, so a config.yaml
// that does not parse, or a write that failed between two files, left
// placeholders in some test cases with no key defined, and the next run took
// the half-written set for one templatize had finished. Now a config.yaml
// that does not read stops the run before anything is written, and the files
// are replaced together or not at all.
func TestTemplatize_AFailedRunLeavesTheSetAsItWas(t *testing.T) {
	clean := newSetOnDisk(t, setfile.FormatYAML, createThenRead(), map[string]interface{}{})
	cleanErr, _ := clean.templatize(t, nil)
	require.NoError(t, cleanErr)
	want := clean.files(t)

	refusing := func(suffix string) func(*setOnDisk) TestDB {
		return func(s *setOnDisk) TestDB {
			return refusingRenames{TestYaml: s.db, refuse: func(src string) bool { return strings.HasSuffix(src, suffix+setfile.ReplaceNewSuffix) }}
		}
	}
	for _, c := range []struct {
		name    string
		breakIt func(*testing.T, *setOnDisk)
		db      func(*setOnDisk) TestDB
		errHas  string
		said    string // the line the run logs for it
		mend    func(*testing.T, *setOnDisk)
	}{
		{"config.yaml does not parse", func(t *testing.T, s *setOnDisk) {
			require.NoError(t, os.WriteFile(s.path("config.yaml"), []byte("template: [unclosed\n"), 0o644))
		}, nil, "config.yaml that could not be parsed", "failed to read the test set's config.yaml; keploy templatize changed nothing in this set", func(t *testing.T, s *setOnDisk) {
			require.NoError(t, s.conf.Write(context.Background(), "test-set-0", &models.TestSet{Template: map[string]interface{}{}, PreScript: "echo before", AppCommand: "./app"}))
		}},
		{"the first test case cannot be put in place", nil, refusing("test-1.yaml"), "failed to put test-1.yaml in place, so every file was put back as it was", "failed to put the templatized files in place", nil},
		{"the second test case cannot be put in place, the first already is", nil, refusing("test-2.yaml"), "failed to put test-2.yaml in place, so every file was put back as it was", "failed to put the templatized files in place", nil},
		{"config.yaml cannot be put in place, every test case already is", nil, refusing("config.yaml"), "failed to put config.yaml in place, so every file was put back as it was", "failed to put the templatized files in place", nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := newSetOnDisk(t, setfile.FormatYAML, createThenRead(), map[string]interface{}{})
			if c.breakIt != nil {
				c.breakIt(t, s)
			}
			before := s.files(t)
			var db TestDB
			if c.db != nil {
				db = c.db(s)
			}
			for run := 1; run <= 2; run++ {
				err, logs := s.templatize(t, db)
				require.Error(t, err, "run %d", run)
				assert.Contains(t, err.Error(), c.errHas, "run %d", run)
				assert.Equal(t, before, s.files(t), "run %d left the set as it was, with no file beside its own", run)
				assert.Empty(t, logs.FilterMessageSnippet("holds template placeholders already").All(), "run %d does not take the set for a templatized one", run)
				assert.Empty(t, logs.FilterMessageSnippet("now captures").All(), "run %d says nothing is captured", run)
				assert.Len(t, logs.FilterMessageSnippet(c.said).All(), 1, "run %d says what stopped it: %v", run, logs.All())
			}
			// What stopped it mended, the run writes what a first run writes.
			if c.mend != nil {
				c.mend(t, s)
			}
			err, _ := s.templatize(t, nil)
			require.NoError(t, err)
			assert.Equal(t, want, s.files(t))
		})
	}
}

// A SET WITHOUT A config.yaml gets one with its test cases, or not at all: a
// run that cannot put it in place leaves the set without one, its test cases
// as recorded.
func TestTemplatize_AConfigIsCreatedWithTheTestCasesOrNotAtAll(t *testing.T) {
	s := newSetOnDisk(t, setfile.FormatYAML, createThenRead(), nil)
	before := s.files(t)
	require.NotContains(t, before, "config.yaml")
	db := refusingRenames{TestYaml: s.db, refuse: func(src string) bool { return strings.HasSuffix(src, "config.yaml"+setfile.ReplaceNewSuffix) }}
	err, _ := s.templatize(t, db)
	require.Error(t, err)
	assert.Equal(t, before, s.files(t))

	err, _ = s.templatize(t, nil)
	require.NoError(t, err)
	after := s.files(t)
	assert.Contains(t, after["config.yaml"], "id: u_REC1")
	assert.Len(t, after, len(before)+1)
}

// A SET WITH SECRETS KEEPS ITS secret.yaml OUT OF GIT, as before: once its
// files are in place, templatize adds /*/secret.yaml to the keploy
// directory's .gitignore. The secrets are read from secret.yaml itself (the
// read that reports a config.yaml it cannot parse does not carry them). A
// set without secrets gets no .gitignore.
func TestTemplatize_ASetWithSecretsKeepsSecretYamlOutOfGit(t *testing.T) {
	s := newSetOnDisk(t, setfile.FormatYAML, createThenRead(), map[string]interface{}{})
	err, _ := s.templatize(t, nil)
	require.NoError(t, err)
	_, statErr := os.Stat(filepath.Join(s.dir, ".gitignore"))
	assert.True(t, os.IsNotExist(statErr), "a set without secrets needs no .gitignore: %v", statErr)

	s = newSetOnDisk(t, setfile.FormatYAML, createThenRead(), map[string]interface{}{})
	require.NoError(t, os.WriteFile(s.path("secret.yaml"), []byte("token: s3cr3t\n"), 0o600))
	err, _ = s.templatize(t, nil)
	require.NoError(t, err)
	assert.Contains(t, s.files(t)["tests/test-1.yaml"], `{"id":"{{capture string .id}}"}`, "the set is templatized")
	ignore, readErr := os.ReadFile(filepath.Join(s.dir, ".gitignore"))
	require.NoError(t, readErr)
	assert.Contains(t, string(ignore), "/*/secret.yaml")
}

// killedRun is what a test panics with to stop keploy templatize where a
// kill would.
type killedRun struct{}

// killingStore is the set's test case store on a platform where the process
// is killed right after the n-th change ReplaceFiles makes on disk (a file
// created, a file written whole, a rename, a removal).
type killingStore struct {
	*testdb.TestYaml
	n int
}

func (k killingStore) ReplaceFiles(_ context.Context, files []setfile.File) error {
	count := 0
	return setfile.ReplaceFilesWith(zap.NewNop(), setfile.ReplaceHooks{Step: func() {
		if count++; count == k.n {
			panic(killedRun{})
		}
	}}, files)
}

// killTemplatize runs keploy templatize over s and kills it right after the
// n-th change it makes on disk. It reports false when the run finished
// before making n changes.
func killTemplatize(t *testing.T, s *setOnDisk, n int) (stopped bool) {
	t.Helper()
	defer func() {
		if r := recover(); r != nil {
			if _, ok := r.(killedRun); !ok {
				panic(r)
			}
			stopped = true
		}
	}()
	err, _ := s.templatize(t, killingStore{TestYaml: s.db, n: n})
	require.NoError(t, err)
	return false
}

// followTheLine does what the error of a run that found a killed run's files
// says, as a person reads it: each "rename A to B" and each "delete A, B"
// after the colon, in order, each file named from the set's directory.
func followTheLine(t *testing.T, s *setOnDisk, msg string) {
	t.Helper()
	var rest string
	for _, lead := range []string{"(or restore them from version control): ", "so the files are as that run wrote them: "} {
		if i := strings.Index(msg, lead); i >= 0 {
			rest = msg[i+len(lead):]
		}
	}
	require.NotEmpty(t, rest, "the line says no steps: %s", msg)
	rest, ok := strings.CutSuffix(rest, "; then run the command again")
	require.True(t, ok, "the line does not end by saying to run again: %s", msg)
	for _, step := range strings.Split(rest, "; ") {
		switch {
		case strings.HasPrefix(step, "rename "):
			from, to, found := strings.Cut(strings.TrimPrefix(step, "rename "), " to ")
			require.True(t, found, step)
			require.NoError(t, os.Rename(s.path(filepath.FromSlash(from)), s.path(filepath.FromSlash(to))), step)
		case strings.HasPrefix(step, "delete "):
			for _, f := range strings.Split(strings.TrimSuffix(strings.TrimPrefix(step, "delete "), ", which that run created"), ", ") {
				require.NoError(t, os.Remove(s.path(filepath.FromSlash(f))), step)
			}
		default:
			t.Fatalf("a step the line says that is neither a rename nor a deletion: %q in %s", step, msg)
		}
	}
}

// WHAT A RUN KILLED ANYWHERE LEFT IS NAMED, AND THE LINE PUTS THE SET IN ONE
// STATE. keploy templatize is killed right after each change its write makes
// on disk, in a set with a config.yaml and in one without (the run creates
// it). The next run changes nothing, names each file the kill left, and says
// the steps; done as written, they leave the set exactly as recorded when the
// kill came before every file was in place, or exactly as a whole run writes
// it when only the copies were left, and the run after that writes the
// templatized set or leaves it. Before, one line said to put back every copy:
// a kill while the copies were removed then put back some test cases and left
// one templatized, and the next run called the set done; a kill while a copy
// was being written put an empty file over a test case, and the set lost it
// without a word.
func TestTemplatize_WhatAKilledRunLeftIsPutInOneStateByTheLineItPrints(t *testing.T) {
	for _, template := range []map[string]interface{}{{}, nil} {
		t.Run(fmt.Sprintf("config.yaml before: %v", template != nil), func(t *testing.T) {
			whole := newSetOnDisk(t, setfile.FormatYAML, createThenRead(), template)
			err, _ := whole.templatize(t, nil)
			require.NoError(t, err)
			written := whole.files(t)
			var putBack, finished int
			n := 1
			for ; ; n++ {
				s := newSetOnDisk(t, setfile.FormatYAML, createThenRead(), template)
				plain := s.files(t)
				require.NotEqual(t, plain, written)
				if !killTemplatize(t, s, n) {
					break
				}
				killed := s.files(t)
				err, logs := s.templatize(t, nil)
				if left, _ := setfile.FindLeftovers(s.path(""), s.path("tests")); len(left.Files) == 0 {
					// Killed after its last change: the run had finished,
					// and this one leaves the set as it wrote it.
					require.NoError(t, err)
					assert.Equal(t, written, s.files(t), "killed after change %d", n)
					continue
				}
				require.ErrorIs(t, err, setfile.ErrReplaceInterrupted, "killed after change %d", n)
				assert.Equal(t, killed, s.files(t), "the run after the kill changes nothing")
				assert.Empty(t, logs.FilterMessageSnippet("holds template placeholders already").All())
				followTheLine(t, s, err.Error())
				want := plain
				if strings.Contains(err.Error(), "it had put every file in place") {
					finished++
					want = written
				} else if killed["tests/test-1.yaml"] == written["tests/test-1.yaml"] {
					putBack++
				}
				require.Equal(t, want, s.files(t), "killed after change %d, the line %q", n, err.Error())
				err, _ = s.templatize(t, nil)
				require.NoError(t, err)
				assert.Equal(t, written, s.files(t), "the run after the steps, killed after change %d", n)
			}
			require.Greater(t, n, 15, "the run made %d changes", n-1)
			require.Positive(t, putBack, "no kill came after a test case was put in place and before the last")
			require.Positive(t, finished, "no kill came while the copies were removed")
		})
	}
}

// filelessStore is a test case store with only the methods TestDB has had
// since before keploy templatize read and replaced files: the shape of
// k8s-proxy's store, which it hands NewTools for keploy normalize.
type filelessStore struct {
	cases   []*models.TestCase
	updated int
}

func (f *filelessStore) GetAllTestSetIDs(context.Context) ([]string, error) {
	return []string{"test-set-0"}, nil
}
func (f *filelessStore) GetTestCases(context.Context, string) ([]*models.TestCase, error) {
	return f.cases, nil
}
func (f *filelessStore) UpdateTestCase(context.Context, *models.TestCase, string, bool) error {
	f.updated++
	return nil
}
func (f *filelessStore) DeleteTests(context.Context, string, []string) error { return nil }
func (f *filelessStore) DeleteTestSet(context.Context, string) error         { return nil }

// filelessConf is a config store with only the methods TestSetConfig has had
// since before keploy templatize replaced config.yaml with the test cases.
type filelessConf struct{ written int }

func (f *filelessConf) Read(context.Context, string) (*models.TestSet, error) {
	return &models.TestSet{}, nil
}
func (f *filelessConf) ReadForUpdate(context.Context, string) (*models.TestSet, error) {
	return &models.TestSet{}, nil
}
func (f *filelessConf) Write(context.Context, string, *models.TestSet) error {
	f.written++
	return nil
}
func (f *filelessConf) ReadSecret(context.Context, string) (map[string]interface{}, error) {
	return nil, nil
}

// A STORE THAT KEEPS NO FILES IS STILL A TestDB, AND A CONFIG STORE THAT KEEPS
// NONE STILL A TestSetConfig: the methods keploy templatize reads and
// replaces files with are their own interfaces (TestSetFiles, ConfigFiles),
// so a program that hands NewTools its own store (k8s-proxy, for keploy
// normalize) builds against this keploy as it did. Templatize over such a
// store changes nothing and says why.
var (
	_ TestDB        = (*filelessStore)(nil)
	_ TestSetConfig = (*filelessConf)(nil)
)

// THE STORES keploy templatize RUNS WITH KEEP FILES. Templatize reaches the
// file methods by assertion, so a store that lost one would build and then
// refuse every set at run time; this fails the build instead.
var (
	_ TestSetFiles = (*testdb.TestYaml)(nil)
	_ ConfigFiles  = (*testset.Db[*models.TestSet])(nil)
)

func TestTemplatize_AStoreThatKeepsNoFilesChangesNothing(t *testing.T) {
	for _, c := range []struct {
		name string
		db   TestDB
		conf TestSetConfig
		says string
	}{
		{"a test case store that keeps no files", &filelessStore{cases: createThenRead()}, &placementConfStore{}, "this test store (*tools.filelessStore) keeps no files"},
		{"a config store that keeps no file", &placementTestDB{written: map[string]models.TestCase{}}, &filelessConf{}, "this config store (*tools.filelessConf) keeps no file"},
		{"no config store", &placementTestDB{written: map[string]models.TestCase{}}, nil, "this config store (<nil>) keeps no file"},
	} {
		t.Run(c.name, func(t *testing.T) {
			core, logs := observer.New(zap.InfoLevel)
			cfg := &config.Config{Templatize: config.Templatize{TestSets: []string{"test-set-0"}}}
			err := NewTools(zap.New(core), c.conf, c.db, nil, nil, cfg).(*Tools).ProcessTestCasesV2(context.Background(), createThenRead(), "test-set-0")
			require.Error(t, err)
			assert.Contains(t, err.Error(), c.says)
			if f, ok := c.db.(*filelessStore); ok {
				assert.Zero(t, f.updated)
			}
			if p, ok := c.db.(*placementTestDB); ok {
				assert.Empty(t, p.written)
			}
			if f, ok := c.conf.(*filelessConf); ok {
				assert.Zero(t, f.written)
			}
			said := logs.FilterMessageSnippet("keploy templatize cannot write this test store, so it changed nothing").All()
			require.Len(t, said, 1, "%v", logs.All())
		})
	}
}

// filelessReports serves one report, as k8s-proxy's report store does.
type filelessReports struct{ report *models.TestReport }

func (f filelessReports) GetAllTestRunIDs(context.Context) ([]string, error) {
	return []string{"test-run-0"}, nil
}
func (f filelessReports) GetReport(context.Context, string, string) (*models.TestReport, error) {
	return f.report, nil
}

// KEPLOY NORMALIZE RUNS WITH NO CONFIG STORE, as k8s-proxy's /test/normalize
// builds it (pkg/service/normalize/normalize.go: tools.NewTools(logger, nil,
// its own test store, its report store, nil, cfg)). It reads the set's
// template values only when there is a store to read them from; before, it
// called Read on the nil store and panicked on every request.
func TestNormalizeTestCases_RunsWithNoConfigStore(t *testing.T) {
	db := &filelessStore{cases: createThenRead()}
	report := &models.TestReport{Tests: []models.TestResult{{TestCaseID: "test-2", Status: models.TestStatusFailed,
		Res: models.HTTPResp{StatusCode: 200, Header: map[string]string{"Content-Type": "application/json"}, Body: `{"id":"u_NEW"}`}}}}
	cfg := config.New()
	tl := NewTools(zap.NewNop(), nil, db, filelessReports{report}, nil, cfg)
	require.NotPanics(t, func() {
		require.NoError(t, tl.NormalizeTestCases(context.Background(), "test-run-0", "test-set-0", nil, nil))
	})
	assert.Equal(t, 1, db.updated, "the failed test case is written with the app's answer")
}

// A SET THAT STOPS keploy templatize NAMES THE SETS IT DID NOT REACH, AND THE
// COMMAND FAILS. test-set-0's config.yaml does not parse, so templatize stops
// there with its one error line; test-set-1, a plain recording after it, is
// left as it is and named in a warning, and Templatize returns the error (the
// command exits non-zero on it). Before, the later sets were skipped without
// a word and the command exited 0.
func TestTemplatize_ASetThatStopsTheRunNamesTheSetsAfterIt(t *testing.T) {
	s := newSetOnDisk(t, setfile.FormatYAML, createThenRead(), map[string]interface{}{})
	for _, tc := range createThenRead() {
		cp := *tc
		cp.Noise = map[string][]string{}
		require.NoError(t, s.db.UpdateTestCase(context.Background(), &cp, "test-set-1", false))
	}
	require.NoError(t, os.WriteFile(s.path("config.yaml"), []byte("template: [unclosed\n"), 0o644))
	second := filepath.Join(s.dir, "test-set-1")
	before, err := os.ReadFile(filepath.Join(second, "tests", "test-1.yaml"))
	require.NoError(t, err)

	core, logs := observer.New(zap.InfoLevel)
	cfg := &config.Config{Path: s.dir, Templatize: config.Templatize{TestSets: []string{"test-set-0", "test-set-1"}}}
	err = NewTools(zap.New(core), s.conf, s.db, nil, nil, cfg).Templatize(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "keploy templatize stopped at test set test-set-0")

	after, readErr := os.ReadFile(filepath.Join(second, "tests", "test-1.yaml"))
	require.NoError(t, readErr)
	assert.Equal(t, string(before), string(after), "the set after it is not templatized")
	stopped := logs.FilterMessageSnippet("keploy templatize stopped at test set test-set-0").All()
	require.Len(t, stopped, 1, "%v", logs.All())
	assert.Equal(t, []interface{}{"test-set-1"}, stopped[0].ContextMap()["notTemplatized"])
	assert.Len(t, logs.FilterLevelExact(zap.ErrorLevel).All(), 1, "the error is logged once: %v", logs.All())
}

// unreadableCases is a test store that cannot read one set's test cases.
type unreadableCases struct {
	filelessStore
	broken string
}

func (u *unreadableCases) GetTestCases(_ context.Context, id string) ([]*models.TestCase, error) {
	if id == u.broken {
		return nil, errors.New("open keploy/" + id + "/tests: permission denied")
	}
	return u.cases, nil
}

// A SET WHOSE TEST CASES CANNOT BE READ STOPS keploy templatize THE SAME WAY:
// its error is logged once, the sets after it are named, and the error is
// returned.
func TestTemplatize_ASetWhoseTestCasesCannotBeReadNamesTheSetsAfterIt(t *testing.T) {
	db := &unreadableCases{filelessStore: filelessStore{cases: createThenRead()}, broken: "test-set-0"}
	core, logs := observer.New(zap.InfoLevel)
	cfg := &config.Config{Templatize: config.Templatize{TestSets: []string{"test-set-0", "test-set-1"}}}
	err := NewTools(zap.New(core), nil, db, nil, nil, cfg).Templatize(context.Background())
	require.Error(t, err)
	assert.Contains(t, err.Error(), "keploy templatize stopped at test set test-set-0")
	assert.Contains(t, err.Error(), "permission denied")
	stopped := logs.FilterMessageSnippet("keploy templatize stopped at test set test-set-0").All()
	require.Len(t, stopped, 1, "%v", logs.All())
	assert.Equal(t, []interface{}{"test-set-1"}, stopped[0].ContextMap()["notTemplatized"])
	assert.Len(t, logs.FilterLevelExact(zap.ErrorLevel).All(), 1, "the error is logged once: %v", logs.All())
	assert.Zero(t, db.updated)
}
