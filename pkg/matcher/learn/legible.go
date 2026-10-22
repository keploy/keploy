package learn

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"strings"
	"sync"
	"unicode/utf8"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/provenswitch"
	"gopkg.in/yaml.v3"
)

// What the learn step learned, legible: a test set's learned files
// (models.LearnedFile, models.LearnedLocalFile in keploy/<set>/) say every
// leaf its mocks learned, with its evidence and how to undo it, for a person
// or an LLM to read. learned.yaml is derived from the mocks and never
// uploaded; learned.local.yaml holds record's full round's words and stays
// on the machine that recorded. Both are written only under the proven
// switch, and only once the project's keploy/.gitignore ignores them
// (EnsureLearnedIgnored): each writer here ensures it first, and writes
// nothing when it cannot.

// SetReader reads the mocks of a test set's directory (keploy/<set>/, or a
// staging copy of it), whatever the store writes them as. The yaml mock store
// registers it (RegisterSetReader); learn reads no mock file itself.
type SetReader func(setDir string) ([]*models.Mock, error)

// LeafReader is, of one kind of mock, its recorded request's leaves by their
// exact place (Leaf.Path), as its learn step reads them: the protocol
// adapter registers it (RegisterLeafReader).
type LeafReader func(m *models.Mock) map[string]Leaf

var (
	readersMu   sync.RWMutex
	setReader   SetReader
	leafReaders = map[models.Kind]LeafReader{}
	// wordsKinds are the kinds whose learn step learns words
	// (RegisterWordsKind).
	wordsKinds = map[models.Kind]bool{}
)

// RegisterSetReader makes f the reader of a test set's mocks.
func RegisterSetReader(f SetReader) {
	readersMu.Lock()
	setReader = f
	readersMu.Unlock()
}

// RegisterLeafReader makes f the reader of the recorded leaves of mocks of
// kind k.
func RegisterLeafReader(k models.Kind, f LeafReader) {
	readersMu.Lock()
	leafReaders[k] = f
	readersMu.Unlock()
}

// RegisterWordsKind says the learn step of mocks of kind k learns a drifted
// leaf by its words (models.WordsEntry): of such a mock, a leaf learned whole
// (noise.req) and a variable bound whole (no words) were learned before
// keploy learned words, and a re-record learns them word by word ("re-record
// to tighten", RetightenLines). Of a kind no adapter registers, keploy learns
// a leaf only whole (design r9 §5: Mongo's and Postgres's learners, which
// learn a leaf only when every word of it changed, learn.WholeDrift), so its
// whole noise is what a record learns today, and a re-record does not
// tighten it. The protocol adapter registers it, as it registers its leaf
// reader: what it declares, never read off a mock.
func RegisterWordsKind(k models.Kind) {
	readersMu.Lock()
	wordsKinds[k] = true
	readersMu.Unlock()
}

// The .gitignore entries that keep every set's learned files, and the files
// being written, out of the project's history.
var learnedIgnore = []string{
	"/*/" + models.LearnedFile,
	"/*/" + models.LearnedLocalFile,
	"/*/" + models.LearnedTempPrefix + "*",
}

// redacted stands in a learned file for a value it may not hold.
const redacted = "(redacted)"

// maxLearnedValue is the most of a value a learned file holds; a longer one
// is cut, at the start of a rune, and the cut marked (cutMark).
const (
	maxLearnedValue = 128
	cutMark         = "…(cut)"
)

// credentialNames are path segments that name a credential (lowercased,
// '-' and '_' dropped): a value at one is never written in a learned file.
var credentialNames = map[string]bool{
	"authorization": true, "proxyauthorization": true, "cookie": true, "setcookie": true,
	"password": true, "passwd": true, "pwd": true, "passphrase": true, "secret": true, "clientsecret": true,
	"token": true, "accesstoken": true, "refreshtoken": true, "idtoken": true, "authtoken": true,
	"apikey": true, "xapikey": true, "session": true, "sessionid": true, "sid": true,
	"csrf": true, "csrftoken": true, "xcsrftoken": true, "xsrftoken": true, "xxsrftoken": true,
	"credential": true, "credentials": true, "privatekey": true, "signature": true,
	"xamzsecuritytoken": true, "otp": true, "pin": true,
}

// credentialParts are what a path segment holding one names a credential
// whatever else it holds ("db_password", "stripe-secret-key").
var credentialParts = []string{"password", "passwd", "secret", "token", "apikey", "credential", "privatekey"}

// credentialSchemes begin a value written as a credential is: an
// Authorization scheme and its parameters.
var credentialSchemes = []string{"bearer ", "basic ", "digest ", "token ", "negotiate ", "ntlm ", "aws4-hmac-sha256 ", "apikey ", "hmac "}

// jwtLike is a JSON Web Token as it is written: three base64url parts, its
// header's first.
var jwtLike = regexp.MustCompile(`eyJ[A-Za-z0-9_-]*\.[A-Za-z0-9_-]+\.[A-Za-z0-9_-]*`)

// credentialPairs are a credential written as a parameter of a longer value.
var credentialPairs = regexp.MustCompile(`(?i)(?:password|passwd|secret|token|api[_-]?key|session)\s*[=:]`)

// RedactLearnedValue is value, at path, as a learned file may hold it:
// redacted when a segment of path names a credential, when the recording's
// obfuscator noise (noise, the mock's value regexes) matches it, or when it
// is written as a credential is; else value, cut to at most 128 bytes at the
// start of a rune, the cut marked.
func RedactLearnedValue(path, value string, noise []string) string {
	for _, seg := range strings.FieldsFunc(strings.ToLower(path), func(r rune) bool {
		return r == '.' || r == '[' || r == ']' || r == '{' || r == '}'
	}) {
		bare := strings.NewReplacer("-", "", "_", "").Replace(seg)
		if credentialNames[bare] {
			return redacted
		}
		for _, part := range credentialParts {
			if strings.Contains(bare, part) {
				return redacted
			}
		}
	}
	for _, n := range noise {
		re, err := regexp.Compile(n)
		if err != nil || re.MatchString(value) {
			// A pattern keploy cannot read may match anything: redacted.
			return redacted
		}
	}
	low := strings.ToLower(value)
	for _, s := range credentialSchemes {
		if strings.HasPrefix(low, s) {
			return redacted
		}
	}
	if jwtLike.MatchString(value) || credentialPairs.MatchString(value) {
		return redacted
	}
	return cutValue(value)
}

// cutValue is value cut to at most maxLearnedValue bytes at the start of a
// rune, the cut marked.
func cutValue(value string) string {
	if len(value) <= maxLearnedValue {
		return value
	}
	cut := maxLearnedValue
	for cut > 0 && !utf8.RuneStart(value[cut]) {
		cut--
	}
	return value[:cut] + cutMark
}

// learnedID is a learned leaf's ID: the hash of its set, its mock, its path
// and its words' digest ("" for a leaf learned whole), stable across runs of
// one recording.
func learnedID(set, mock, path, digest string) string {
	sum := sha256.Sum256([]byte(set + "\x00" + mock + "\x00" + path + "\x00" + digest))
	return hex.EncodeToString(sum[:])[:16]
}

// slotBases are the bases of an entry's slots, in order, each once.
func slotBases(e models.WordsEntry) []models.LearnBasis {
	var out []models.LearnBasis
	seen := map[models.LearnBasis]bool{}
	for _, s := range e.Slots {
		b := models.BasisChanged
		switch s.Kind {
		case models.WordSlotToken:
			b = models.BasisToken
		case models.WordSlotInstant:
			b = models.BasisClockRead
		}
		if !seen[b] {
			seen[b] = true
			out = append(out, b)
		}
	}
	return out
}

// renderSlots is rec with each of e's slots written ⟨changes⟩, "" when the
// entry does not hold of rec (WordsAdmit: its slots cannot be placed).
func renderSlots(rec string, e models.WordsEntry) string {
	if ok, _ := WordsAdmit(rec, rec, e); !ok {
		return ""
	}
	need := map[int]int{}
	for _, s := range e.Slots {
		if s.Kind == models.WordSlotInstant {
			need[layoutNamed(s.Layout)]++
		}
	}
	var tr wtoks
	collapses(rec, need, func(spans []collapsed) bool {
		t := tokenize(rec, spans)
		if layoutHash(t) == e.Layout {
			tr = t
			return true
		}
		return false
	})
	type span struct{ from, to int }
	var spans []span
	for _, s := range e.Slots {
		sg := tr.segs[s.Seg]
		if s.Kind == models.WordSlotRun {
			r := sg.runs[s.Run]
			spans = append(spans, span{r.from, r.to})
			continue
		}
		spans = append(spans, span{sg.from, sg.to})
	}
	sort.Slice(spans, func(i, j int) bool { return spans[i].from < spans[j].from })
	var b strings.Builder
	at := 0
	for _, sp := range spans {
		if sp.from < at {
			continue
		}
		b.WriteString(rec[at:sp.from])
		b.WriteString("⟨changes⟩")
		at = sp.to
	}
	b.WriteString(rec[at:])
	return b.String()
}

// notRead stands for a recorded value Legible could not read: no reader of
// its mock's kind is registered in this keploy.
const notRead = "(not read: keploy reads no leaves of this mock's kind here)"

// notALeaf stands for a recorded value Legible could not read because the
// mock's recorded request, as its kind's reader reads it, holds no leaf at
// the place.
const notALeaf = "(not read: the recorded request holds no leaf at this place)"

// recordedAt is the recorded value at path of a mock whose recorded leaves
// are leaves (nil: its kind has no leaf reader), and whether it was read;
// what stands in a learned file for one that was not (notRead, notALeaf).
func recordedAt(leaves map[string]Leaf, path string) (value string, read bool, unread string) {
	if leaves == nil {
		return "", false, notRead
	}
	l, ok := leaves[path]
	if !ok {
		return "", false, notALeaf
	}
	return l.Value, true, ""
}

// rendered is a learned leaf's Rendered: its path = its recorded value with
// each slot marked, as a learned file may hold it (read: whether the recorded
// value could be read). An entry that does not hold of the recorded value is
// said to be compared whole, and why.
func rendered(path, rec string, read bool, unread string, noise []string, e *models.WordsEntry) string {
	if !read {
		return path + " = " + unread
	}
	if r := RedactLearnedValue(path, rec, noise); r == redacted {
		return path + " = " + redacted
	}
	if e == nil {
		return path + " = ⟨changes⟩"
	}
	if ok, why := WordsAdmit(rec, rec, *e); !ok {
		return path + " = " + cutValue(rec) + " (compared whole: " + why + ")"
	}
	return path + " = " + cutValue(renderSlots(rec, *e))
}

// Legible is every leaf learned onto the mocks of the test set in setDir
// (keploy/<set>/, or a staging copy of it), as learned.yaml says it: each
// leaf's words learned as noise (noise.words), each variable's and echo's
// words, and each leaf learned whole (noise.req), with the recorded value
// redacted (RedactLearnedValue) and no value of record's full round. An entry
// this build cannot read (models.WordsEntry.Unreadable) is listed with no
// slots, its Why saying so, as a leaf compared whole. It reads the mocks
// only, so it says the same of the same mocks wherever it runs.
func Legible(setDir string) ([]models.LearnedLeaf, error) {
	readersMu.RLock()
	read := setReader
	readersMu.RUnlock()
	if read == nil {
		return nil, errors.New("keploy cannot read the test set's mocks: no mock store is registered")
	}
	mocks, err := read(setDir)
	if err != nil {
		return nil, fmt.Errorf("read the mocks of %s: %w", setDir, err)
	}
	set := filepath.Base(filepath.Clean(setDir))
	sort.SliceStable(mocks, func(i, j int) bool { return mocks[i].Name < mocks[j].Name })
	var out []models.LearnedLeaf
	for _, m := range mocks {
		if m == nil {
			continue
		}
		readersMu.RLock()
		leavesOf, words := leafReaders[m.Kind], wordsKinds[m.Kind]
		readersMu.RUnlock()
		var leaves map[string]Leaf
		if leavesOf != nil {
			if leaves = leavesOf(m); leaves == nil {
				// A reader that reads nothing of the mock read it.
				leaves = map[string]Leaf{}
			}
		}
		out = append(out, legibleOf(set, m, leaves, words)...)
	}
	return out, nil
}

// legibleOf are the learned leaves of mock m of set, its recorded leaves by
// place (leaves; nil when its kind has no leaf reader). words says the learn
// step of m's kind learns words (RegisterWordsKind): only then is a leaf of m
// learned whole, or a variable of m bound whole, one learned before keploy
// learned words (models.BasisBeforeWords), which a re-record tightens.
func legibleOf(set string, m *models.Mock, leaves map[string]Leaf, words bool) []models.LearnedLeaf {
	var out []models.LearnedLeaf
	places := make([]string, 0, len(m.Spec.LearnedWords))
	for p := range m.Spec.LearnedWords {
		places = append(places, p)
	}
	sort.Strings(places)
	for _, p := range places {
		e := m.Spec.LearnedWords[p]
		rec, read, unread := recordedAt(leaves, p)
		why := "record's full round saw only the marked words of " + p + " change, so only they may change, and every other byte of it is still compared. To compare it whole again, delete its entry under noise.words of " + m.Name + "; to stop comparing it, declare it in keploy.yml (test.globalNoise.global.requestbody)."
		slots := append([]models.WordSlot(nil), e.Slots...)
		switch u := e.Unreadable(); {
		case u != "":
			why = p + " is compared whole: its learned words entry is " + u + "."
			slots = nil
		case e.Void():
			why = "record's full round read " + p + " in two ways, so its learned words are void, and it is compared whole. Record again to learn it anew."
		}
		recorded := unread
		if read {
			recorded = RedactLearnedValue(p, rec, m.Noise)
		}
		out = append(out, models.LearnedLeaf{
			ID: learnedID(set, m.Name, p, e.Digest), Mock: m.Name, Path: p,
			Recorded: recorded, Basis: slotBases(e), Rule: models.RuleNoise,
			Why: why, Slots: slots, Rendered: rendered(p, rec, read, unread, m.Noise, &e),
		})
	}
	names := make([]string, 0, len(m.Spec.Variables))
	for n := range m.Spec.Variables {
		names = append(names, n)
	}
	sort.Strings(names)
	for _, n := range names {
		v := m.Spec.Variables[n]
		if v.Bind == "" {
			continue
		}
		p, ok := LeafPath(v.Bind)
		if !ok {
			// A place in the kind's own notation (a Postgres echo's
			// bind.$N): said as the mock spells it.
			p = v.Bind
		}
		rule, bases := models.RuleVariable, []models.LearnBasis{models.BasisEarlier}
		what := "the variable " + n + ", bound here and asserted at its every use"
		if v.Echo {
			rule, bases = models.RuleEcho, []models.LearnBasis{models.BasisEcho}
			what = "the variable " + n + " of this call alone, bound here and answered with this run's value"
		}
		digest := ""
		var slots []models.WordSlot
		why := p + " holds " + what + "; any value may bind it, as keploy binds a leaf of a " + string(m.Kind) + " mock only whole: it learns no words of one yet, so a re-record binds it whole again."
		if words {
			bases = append(bases, models.BasisBeforeWords)
			why = p + " holds " + what + "; any value may bind it, as it was learned before keploy learned words: re-record to tighten."
		}
		if v.Words != nil {
			digest = v.Words.Digest
			slots = append([]models.WordSlot(nil), v.Words.Slots...)
			bases = append(bases[:1], slotBases(*v.Words)...)
			why = p + " holds " + what + "; only its marked words may change where it binds. To bind it as recorded only, delete its words entry under variables." + n + " of " + m.Name + "."
		}
		out = append(out, models.LearnedLeaf{
			ID: learnedID(set, m.Name, p, digest), Mock: m.Name, Variable: n, Path: p,
			Recorded: RedactLearnedValue(p, v.Recorded, m.Noise), Basis: bases, Rule: rule,
			Why: why, Slots: slots, Rendered: rendered(p, v.Recorded, true, "", m.Noise, v.Words),
		})
	}
	whole := make([]string, 0, len(m.Spec.ReqBodyNoise))
	for p := range m.Spec.ReqBodyNoise {
		whole = append(whole, p)
	}
	sort.Strings(whole)
	for _, p := range whole {
		rec, read, unread := recordedAt(leaves, p)
		recorded := unread
		if read {
			recorded = RedactLearnedValue(p, rec, m.Noise)
		}
		// Of a kind keploy learns only whole, a leaf is learned whole only
		// when record's full round saw every word of it change: what a
		// record learns today. Of one it learns words of, a leaf learned
		// whole was learned before keploy learned words.
		basis := models.BasisWhole
		why := p + " is not compared at all, its type still is: keploy learns a leaf of a " + string(m.Kind) + " mock only whole, when record's full round saw every word of it change, and learns no words of one yet, so a re-record learns it whole again. To compare it again, delete it from noise.req of " + m.Name + "."
		if words {
			basis = models.BasisBeforeWords
			why = p + " is not compared at all, its type still is: it was learned whole before keploy learned words. Re-record to tighten; to compare it again now, delete it from noise.req of " + m.Name + "."
		}
		out = append(out, models.LearnedLeaf{
			ID: learnedID(set, m.Name, p, ""), Mock: m.Name, Path: p,
			Recorded: recorded, Basis: []models.LearnBasis{basis}, Rule: models.RuleWholeNoise,
			Why: why, Rendered: p + " = ⟨changes⟩",
		})
	}
	return out
}

// RetightenLines are the lines a status prints (N6) of a set's learned leaves
// that were learned before keploy learned words (models.BasisBeforeWords: a
// leaf of a kind keploy learns words of, learned whole into noise.req, or a
// variable or echo of one bound whole, with no words), in the leaves' order:
// "<mock> <path>: re-record to tighten (learned whole, before keploy learned
// words)", or "(bound whole, ...)" of a variable or echo. None with the
// proven switch off, or for a set whose latest report does not say proven
// (proven false). A leaf of a kind keploy learns only whole (Mongo,
// Postgres) has none: a re-record learns it whole again.
func RetightenLines(leaves []models.LearnedLeaf, proven bool) []string {
	if !provenswitch.On() || !proven {
		return nil
	}
	var out []string
	for _, l := range leaves {
		if !slices.Contains(l.Basis, models.BasisBeforeWords) {
			continue
		}
		how := "learned whole"
		if l.Rule == models.RuleEcho || l.Rule == models.RuleVariable {
			how = "bound whole"
		}
		out = append(out, l.Mock+" "+l.Path+": re-record to tighten ("+how+", before keploy learned words)")
	}
	return out
}

// EnsureLearnedIgnored makes the project's keploy directory's .gitignore
// (keployDir/.gitignore) ignore every test set's learned files and their
// files being written: it adds each of "/*/learned.yaml",
// "/*/learned.local.yaml" and "/*/.learned-*" it lacks, and keeps every
// other line as it is. It is called under the proven switch, before either
// learned file is written (in cloud mode, before the copy-back); when it
// fails, neither may be written. With the switch off it writes nothing and
// fails, so no learned file is written.
func EnsureLearnedIgnored(keployDir string) error {
	if !provenswitch.On() {
		return errors.New("the learned files are written only under the proven switch")
	}
	path := filepath.Join(keployDir, ".gitignore")
	existing, err := os.ReadFile(path)
	if err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("read %s: %w", path, err)
	}
	have := map[string]bool{}
	for _, l := range strings.Split(string(existing), "\n") {
		have[strings.TrimSpace(l)] = true
	}
	var missing []string
	for _, e := range learnedIgnore {
		if !have[e] {
			missing = append(missing, e)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	if err := os.MkdirAll(keployDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", keployDir, err)
	}
	out := string(existing)
	if out != "" && !strings.HasSuffix(out, "\n") {
		out += "\n"
	}
	out += strings.Join(missing, "\n") + "\n"
	if err := writeReplacing(keployDir, ".gitignore-", path, []byte(out)); err != nil {
		return err
	}
	// Read back: a .gitignore keploy cannot see its lines in ignores nothing.
	back, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read back %s: %w", path, err)
	}
	got := map[string]bool{}
	for _, l := range strings.Split(string(back), "\n") {
		got[strings.TrimSpace(l)] = true
	}
	for _, e := range learnedIgnore {
		if !got[e] {
			return fmt.Errorf("%s does not hold %q after it was written", path, e)
		}
	}
	return nil
}

// writeReplacing writes data to path through a temporary file in dir named
// with prefix, renamed into place once whole.
func writeReplacing(dir, prefix, path string, data []byte) error {
	var rnd [8]byte
	if _, err := rand.Read(rnd[:]); err != nil {
		return fmt.Errorf("name a temporary file: %w", err)
	}
	tmp := filepath.Join(dir, prefix+hex.EncodeToString(rnd[:]))
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", tmp, err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("rename %s into place: %w", path, err)
	}
	return nil
}

// learnedHeader is the comment a learned file starts with: what it is and how
// to undo what it says, for a person or an LLM reading it.
func learnedHeader(name string) string {
	what := "what keploy learned onto this test set's mocks in record's full round, derived from the mocks"
	if name == models.LearnedLocalFile {
		what = "the values record's full round saw at the leaves keploy learned onto this test set's mocks; it stays on this machine"
	}
	return "# " + what + ".\n" +
		"# keploy rewrites it, never uploads it, and git ignores it (keploy/.gitignore).\n" +
		"# Each leaf says what changed, on what evidence, and how to undo it: delete its words entry\n" +
		"# (noise.words, or variables.<name>.words) from the mock, and the leaf is compared whole\n" +
		"# (red on every run it changes); or excuse the field in keploy.yml's test.globalNoise.\n"
}

// readLearned reads the learned leaves of a learned file (a YAML list of
// models.LearnedLeaf); none when it does not exist.
func readLearned(path string) ([]models.LearnedLeaf, error) {
	b, err := os.ReadFile(path)
	if os.IsNotExist(err) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var doc models.LearnedDocument
	if err := yaml.Unmarshal(b, &doc); err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	return doc.Learned, nil
}

// writeLearned writes leaves to the learned file name of setDir, whole,
// through a temporary file renamed into place (models.LearnedTempPrefix). No
// leaf removes the file: a learned file never says what the mocks no longer
// hold.
func writeLearned(setDir, name string, leaves []models.LearnedLeaf) error {
	path := filepath.Join(setDir, name)
	if len(leaves) == 0 {
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return err
		}
		return nil
	}
	b, err := yaml.Marshal(models.LearnedDocument{Learned: leaves})
	if err != nil {
		return err
	}
	return writeReplacing(setDir, models.LearnedTempPrefix, path, append([]byte(learnedHeader(name)), b...))
}

// learnedFilesExist reports whether setDir holds either learned file.
func learnedFilesExist(setDir string) bool {
	for _, name := range []string{models.LearnedFile, models.LearnedLocalFile} {
		if _, err := os.Lstat(filepath.Join(setDir, name)); err == nil {
			return true
		}
	}
	return false
}

// WriteLegible writes learned.yaml of the test set in setDir under the proven
// switch: every leaf its mocks learned (Legible), once the project's
// keploy/.gitignore (setDir's parent) ignores the learned files
// (EnsureLearnedIgnored; when it cannot, nothing is written). A set whose
// mocks learned nothing has none, and one left from an earlier recording is
// removed; the .gitignore is then left as it is. With the switch off it
// writes nothing.
func WriteLegible(setDir string) error {
	if !provenswitch.On() {
		return nil
	}
	leaves, err := Legible(setDir)
	if err != nil {
		return err
	}
	if len(leaves) == 0 && !learnedFilesExist(setDir) {
		return nil
	}
	if err := EnsureLearnedIgnored(filepath.Dir(filepath.Clean(setDir))); err != nil {
		return fmt.Errorf("keep the learned files out of git: %w", err)
	}
	return writeLearned(setDir, models.LearnedFile, leaves)
}

// LocalValue is record's full round's value of one learned leaf, as the
// agent saw it (models.MockState.LearnedF): learned.local.yaml's F.
type LocalValue struct {
	Mock, Path, F string
}

// WriteLearned writes the learned files of the test set in setDir under the
// proven switch: learned.yaml whole from the set's mocks (Legible), and into
// learned.local.yaml, by ID, each learned leaf of this run with record's
// full round's value (local; of two for one leaf, the least), the earlier
// entries of leaves the mocks still learn kept, and every other pruned. The
// project's keploy/.gitignore (setDir's parent) is ensured first
// (EnsureLearnedIgnored); when it cannot be, neither file is written. With
// the switch off it writes nothing.
func WriteLearned(setDir string, local []LocalValue) error {
	if !provenswitch.On() {
		return nil
	}
	leaves, err := Legible(setDir)
	if err != nil {
		return err
	}
	if len(leaves) == 0 && !learnedFilesExist(setDir) {
		// Nothing learned, and nothing to prune: no file, and the
		// .gitignore is left as it is.
		return nil
	}
	if err := EnsureLearnedIgnored(filepath.Dir(filepath.Clean(setDir))); err != nil {
		return fmt.Errorf("keep the learned files out of git: %w", err)
	}
	if err := writeLearned(setDir, models.LearnedFile, leaves); err != nil {
		return err
	}
	// A leaf of which local holds more than one value (a mock the run
	// answered more than once; an earlier call's place a later call bound
	// too) keeps the least: the file is a function of the values, never of
	// the order they come in (a map's, MockState by mock).
	f := map[string]string{}
	for _, v := range local {
		k := v.Mock + "\x00" + v.Path
		if cur, ok := f[k]; !ok || v.F < cur {
			f[k] = v.F
		}
	}
	var fresh []models.LearnedLeaf
	for _, l := range leaves {
		v, ok := f[l.Mock+"\x00"+l.Path]
		if !ok {
			continue
		}
		l.F = RedactLearnedValue(l.Path, v, nil)
		if l.Recorded == redacted {
			l.F = redacted
		}
		fresh = append(fresh, l)
	}
	return mergeLocalInto(setDir, leaves, fresh)
}

// mergeLocalInto writes learned.local.yaml of setDir: of each leaf of current
// (the leaves the set's mocks learn now), in its order, the value record's
// full round saw there, from's entry of its ID winning over the file's. An
// entry whose ID no leaf of current has is pruned, and a leaf with neither is
// left out.
func mergeLocalInto(setDir string, current, from []models.LearnedLeaf) error {
	have, err := readLearned(filepath.Join(setDir, models.LearnedLocalFile))
	if err != nil {
		return err
	}
	byID := map[string]models.LearnedLeaf{}
	for _, l := range have {
		byID[l.ID] = l
	}
	for _, l := range from {
		byID[l.ID] = l
	}
	var out []models.LearnedLeaf
	for _, l := range current {
		e, ok := byID[l.ID]
		if !ok {
			continue
		}
		l.F = e.F
		out = append(out, l)
		delete(byID, l.ID)
	}
	return writeLearned(setDir, models.LearnedLocalFile, out)
}

// MergeLocal merges the learned.local.yaml of the test set in srcSetDir (the
// staging copy, in cloud mode) into that of the set in dstSetDir (the
// project's), by ID: an entry of src replaces dst's of the same ID (the
// staging copy wins), and an entry whose ID no leaf the staging copy's mocks
// learn has any longer is pruned (the staging copy holds the mocks the set
// now has; the project's folder holds none in cloud mode). The file is
// written whole, under a temporary name renamed into place
// (models.LearnedTempPrefix), and only once the project's keploy/.gitignore
// (dstSetDir's parent) ignores it; with nothing to merge and no file to
// prune, nothing is touched. With the proven switch off nothing is written.
func MergeLocal(dstSetDir, srcSetDir string) error {
	if !provenswitch.On() {
		return nil
	}
	src, err := readLearned(filepath.Join(srcSetDir, models.LearnedLocalFile))
	if err != nil {
		return err
	}
	if len(src) == 0 && !learnedFilesExist(dstSetDir) {
		return nil
	}
	current, err := Legible(srcSetDir)
	if err != nil {
		return err
	}
	if err := EnsureLearnedIgnored(filepath.Dir(filepath.Clean(dstSetDir))); err != nil {
		return fmt.Errorf("keep the learned files out of git: %w", err)
	}
	if err := os.MkdirAll(dstSetDir, 0o755); err != nil {
		return fmt.Errorf("create %s: %w", dstSetDir, err)
	}
	return mergeLocalInto(dstSetDir, current, src)
}
