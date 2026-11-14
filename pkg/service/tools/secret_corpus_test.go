package tools

import (
	"compress/gzip"
	"encoding/base64"
	"encoding/json"
	"os"
	"slices"
	"sort"
	"testing"
)

// secretCorpusPath holds 10,477 samples and what gitleaks v8.28.0's detector
// (its default rules with custom_gitleaks_rules.toml merged in, as keploy
// sanitize builds it) finds in each one: rule id, match, secret and position.
// The samples are every true- and false-positive example gitleaks' rule
// generator validates its rules with, examples generated from the regex of each
// rule that has none (every custom rule, and two dropbox rules), and every
// string of 8 bytes or more in keploy's sanitizer tests. A recorded file holds
// many values side by side, so 145 more samples join those, seeded: several
// matches of one rule where a skipped match (low entropy, a global or rule
// allowlist, a gitleaks:allow line) comes before a reported one, twelve
// matches of one rule, and files of 60 values. Three more are non-ASCII
// secrets of the curl rule. gitleaks divides their character counts by their
// length in bytes, so two fall just under the rule's entropy bound and are
// skipped, and one is reported.
//
// Texts, matches and secrets are stored XOR 0x5a and base64, so that the
// repository and the module zip carry no plain token-shaped strings for a
// secret scanner to flag.
const secretCorpusPath = "testdata/secret_corpus.json.gz"

type corpusFinding struct {
	Rule   string `json:"rule"`
	Match  string `json:"match"`
	Secret string `json:"secret"`
	Line   [2]int `json:"line"`
	Col    [2]int `json:"col"`
}

type corpusSample struct {
	Src      string          `json:"src"`
	Text     string          `json:"text"`
	Findings []corpusFinding `json:"findings"`
}

func unmask(t *testing.T, s string) string {
	t.Helper()
	b, err := base64.StdEncoding.DecodeString(s)
	if err != nil {
		t.Fatalf("corpus value %q is not base64: %v", s, err)
	}
	for i := range b {
		b[i] ^= 0x5a
	}
	return string(b)
}

func loadSecretCorpus(t *testing.T) []corpusSample {
	t.Helper()
	f, err := os.Open(secretCorpusPath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = f.Close() }()
	zr, err := gzip.NewReader(f)
	if err != nil {
		t.Fatal(err)
	}
	var samples []corpusSample
	if err := json.NewDecoder(zr).Decode(&samples); err != nil {
		t.Fatal(err)
	}
	for i := range samples {
		s := &samples[i]
		s.Text = unmask(t, s.Text)
		for j := range s.Findings {
			s.Findings[j].Match = unmask(t, s.Findings[j].Match)
			s.Findings[j].Secret = unmask(t, s.Findings[j].Secret)
		}
		sortCorpusFindings(s.Findings)
	}
	return samples
}

func sortCorpusFindings(fs []corpusFinding) {
	sort.Slice(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		switch {
		case a.Line != b.Line:
			return a.Line[0] < b.Line[0] || (a.Line[0] == b.Line[0] && a.Line[1] < b.Line[1])
		case a.Col != b.Col:
			return a.Col[0] < b.Col[0] || (a.Col[0] == b.Col[0] && a.Col[1] < b.Col[1])
		case a.Rule != b.Rule:
			return a.Rule < b.Rule
		case a.Secret != b.Secret:
			return a.Secret < b.Secret
		}
		return a.Match < b.Match
	})
}

// THE SECRET DETECTOR FINDS EXACTLY WHAT GITLEAKS FINDS.
// keploy sanitize replaces each secret the detector reports, so a finding lost
// leaves a secret in a recording, and a finding gained or moved redacts a value
// that is not one. Over the corpus, each sample must give the same findings as
// gitleaks v8.28.0 gave: the same rule ids, matches, secrets, lines and columns.
func TestSecretDetector_FindsWhatGitleaksFindsOnTheCorpus(t *testing.T) {
	samples := loadSecretCorpus(t)
	det, err := newSecretDetector()
	if err != nil {
		t.Fatal(err)
	}
	bad := 0
	for _, s := range samples {
		var got []corpusFinding
		for _, f := range det.DetectString(s.Text) {
			got = append(got, corpusFinding{Rule: f.RuleID, Match: f.Match, Secret: f.Secret,
				Line: [2]int{f.StartLine, f.EndLine}, Col: [2]int{f.StartColumn, f.EndColumn}})
		}
		sortCorpusFindings(got)
		if slices.Equal(got, s.Findings) {
			continue
		}
		bad++
		if bad <= 20 {
			t.Errorf("sample from %s, text %q:\n got %+v\nwant %+v", s.Src, s.Text, got, s.Findings)
		}
	}
	if bad > 0 {
		t.Errorf("%d of %d samples gave other findings than gitleaks", bad, len(samples))
	}
}

// THE CORPUS PINS EVERY RULE.
// A rule the corpus never fires could be dropped or changed with every sample
// still passing. Every rule of the detector that matches content must have at
// least one finding in the corpus. A rule with a path never fires here: a
// sanitized test file is scanned as a string, which has no path.
func TestSecretCorpus_HasAFindingForEveryContentRule(t *testing.T) {
	samples := loadSecretCorpus(t)
	det, err := newSecretDetector()
	if err != nil {
		t.Fatal(err)
	}
	found := map[string]int{}
	for _, s := range samples {
		for _, f := range s.Findings {
			found[f.Rule]++
		}
	}
	content := 0
	for id, r := range det.Config.Rules {
		if r.Regex == nil || r.Path != nil {
			if found[id] > 0 {
				t.Errorf("rule %s has a path or no regex, yet the corpus has %d findings for it", id, found[id])
			}
			continue
		}
		content++
		if found[id] == 0 {
			t.Errorf("rule %s has no finding in %s: add samples for it", id, secretCorpusPath)
		}
	}
	if content < 200 {
		t.Errorf("the detector has %d content rules; gitleaks' default rules alone have more than 200", content)
	}
}
