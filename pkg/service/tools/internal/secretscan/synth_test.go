package secretscan

import (
	"encoding/json"
	"fmt"
	"os"
	"reflect"
	"sort"
	"testing"
)

const syntheticGoldenPath = "testdata/synthetic_golden.json"

type goldenFinding struct {
	Rule        string   `json:"rule"`
	Description string   `json:"description"`
	StartLine   int      `json:"startLine"`
	EndLine     int      `json:"endLine"`
	StartColumn int      `json:"startColumn"`
	EndColumn   int      `json:"endColumn"`
	Line        string   `json:"line"`
	Match       string   `json:"match"`
	Secret      string   `json:"secret"`
	Entropy     float32  `json:"entropy"`
	Tags        []string `json:"tags"`
}

type goldenSample struct {
	Case     string          `json:"case"`
	Text     string          `json:"text"`
	Findings []goldenFinding `json:"findings"`
}

func sortGoldenFindings(fs []goldenFinding) {
	sort.Slice(fs, func(i, j int) bool {
		a, b := fs[i], fs[j]
		if a.StartLine != b.StartLine {
			return a.StartLine < b.StartLine
		}
		if a.StartColumn != b.StartColumn {
			return a.StartColumn < b.StartColumn
		}
		return a.Rule < b.Rule
	})
}

// detectNoPanic returns the findings of det in text, or an error where the
// detector panicked.
func detectNoPanic(det *Detector, text string) (fs []Finding, err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("detector panicked: %v", r)
		}
	}()
	return det.DetectString(text), nil
}

// THE DETECTOR TAKES EVERY BRANCH AS GITLEAKS DOES.
// keploy's rules never use secret groups, skip-report rules, AND allowlists
// that allow, path rules that match the empty path, or allowlists that target
// the match or the line with a carriage return, so the corpus rows in
// pkg/service/tools cannot pin those branches. Synthetic configs take them,
// and each text must give every field of every finding as gitleaks v8.28.0
// gave it.
func TestDetector_GivesWhatGitleaksGivesOnSyntheticConfigs(t *testing.T) {
	raw, err := os.ReadFile(syntheticGoldenPath)
	if err != nil {
		t.Fatal(err)
	}
	var golden []goldenSample
	if err := json.Unmarshal(raw, &golden); err != nil {
		t.Fatal(err)
	}

	n := 0
	for _, c := range synthCases {
		cfg, err := synthCaseConfig(c)
		if err != nil {
			t.Fatalf("case %q: %v", c.name, err)
		}
		det, err := NewDetector(cfg)
		if err != nil {
			t.Fatalf("case %q: %v", c.name, err)
		}
		for _, text := range c.texts {
			if n >= len(golden) {
				t.Fatalf("%s has %d samples, fewer than the cases have texts", syntheticGoldenPath, len(golden))
			}
			want := golden[n]
			n++
			if want.Case != c.name || want.Text != text {
				t.Fatalf("%s is out of step with synthCases at sample %d: has %q/%q, want %q/%q; regenerate it",
					syntheticGoldenPath, n-1, want.Case, want.Text, c.name, text)
			}
			fs, err := detectNoPanic(det, text)
			if err != nil {
				t.Errorf("case %q, text %q: %v", c.name, text, err)
				continue
			}
			got := []goldenFinding{}
			for _, f := range fs {
				got = append(got, goldenFinding{f.RuleID, f.Description, f.StartLine, f.EndLine,
					f.StartColumn, f.EndColumn, f.Line, f.Match, f.Secret, f.Entropy, f.Tags})
			}
			sortGoldenFindings(got)
			if !reflect.DeepEqual(got, want.Findings) {
				t.Errorf("case %q, text %q:\n got %+v\nwant %+v", c.name, text, got, want.Findings)
			}
		}
	}
	if n != len(golden) {
		t.Errorf("%s has %d samples, the cases have %d texts", syntheticGoldenPath, len(golden), n)
	}
}

// A MULTI-PART RULE IS REFUSED, NOT HALF-APPLIED.
// gitleaks reports a rule with required rules only where each required rule
// also matches nearby. This package does not implement that, so it must
// refuse such a config rather than report the primary rule alone.
func TestNewDetector_RefusesARuleWithRequiredRules(t *testing.T) {
	cfg, err := synthCaseConfig(synthCase{toml: `
[[rules]]
id = "part"
regex = '''prt=([a-z0-9]+)'''

[[rules]]
id = "whole"
regex = '''whl=([a-z0-9]+)'''
[[rules.required]]
id = "part"
`})
	if err != nil {
		t.Fatal(err)
	}
	if len(cfg.Rules["whole"].RequiredRules) != 1 {
		t.Fatalf("the config did not read the required rule: %+v", cfg.Rules["whole"])
	}
	if det, err := NewDetector(cfg); err == nil {
		t.Fatalf("NewDetector accepted a rule with required rules: %+v", det)
	}

	delete(cfg.Rules, "whole")
	if _, err := NewDetector(cfg); err != nil {
		t.Fatalf("NewDetector refused a config with no required rules: %v", err)
	}
}

// THE DEFAULT RULES LOAD.
// gitleaks' default config must give a detector with its rules and keywords.
// What that detector finds is pinned by the corpus rows in pkg/service/tools.
func TestNewDetectorDefaultConfig_LoadsTheDefaultRules(t *testing.T) {
	det, err := NewDetectorDefaultConfig()
	if err != nil {
		t.Fatal(err)
	}
	if len(det.Config.Rules) < 200 || len(det.Config.Keywords) == 0 {
		t.Fatalf("default config has %d rules and %d keywords", len(det.Config.Rules), len(det.Config.Keywords))
	}
}
