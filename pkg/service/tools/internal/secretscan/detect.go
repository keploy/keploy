// Trimmed from github.com/zricethezav/gitleaks/v8 v8.28.0, package detect.
// MIT License, Copyright (c) 2019 Zachary Rice: see LICENSE in this directory.

// Package secretscan finds secrets in a string with gitleaks rules, as
// gitleaks' detector does for an in-memory string. It is gitleaks v8.28.0's
// detect package cut down to that one use, so that keploy links no scanning
// sources (git, archives, directories) and none of their dependencies.
//
// What was cut, and why each cut gives the same findings for a string:
//   - A string has no file path and no commit. A global or rule allowlist that
//     allows by path or commit therefore never allows here, and an AND
//     allowlist that names paths or commits never allows a finding.
//   - No decoding passes (gitleaks' MaxDecodeDepth 0, its library default).
//   - No redaction, size limit, baseline, .gitleaksignore, reporting, logging
//     or concurrency control: all off or unused in a library call.
//   - Multi-part rules (required rules) are refused: NewDetector returns an
//     error for a config that has one, rather than finding something else.
package secretscan

import (
	"fmt"
	"strings"

	ahocorasick "github.com/BobuSumisu/aho-corasick"
	"github.com/spf13/viper"
	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/regexp"
)

const gitleaksAllowSignature = "gitleaks:allow"

var newLineRegexp = regexp.MustCompile("\n")

// Detector finds secrets with the rules of its Config. Config must not be
// changed after NewDetector.
type Detector struct {
	Config config.Config

	// prefilter matches the rules' keywords, so that a rule with keywords
	// runs only on text that holds one of them.
	prefilter ahocorasick.Trie
}

// NewDetector returns a detector for cfg. It refuses a config with a
// multi-part rule (one with required rules), which this package does not
// implement.
func NewDetector(cfg config.Config) (*Detector, error) {
	keywords := make([]string, 0, len(cfg.Keywords))
	for k := range cfg.Keywords {
		keywords = append(keywords, k)
	}
	for id, r := range cfg.Rules {
		if len(r.RequiredRules) > 0 {
			return nil, fmt.Errorf("rule %s has required rules, which the secret detector does not support", id)
		}
	}
	return &Detector{
		Config:    cfg,
		prefilter: *ahocorasick.NewTrieBuilder().AddStrings(keywords).Build(),
	}, nil
}

// NewDetectorDefaultConfig returns a detector with gitleaks' default rules.
// It reads them with a viper instance of its own, where gitleaks used viper's
// global one: keploy's CLI keeps its own config there, with its flags and
// KEPLOY_* environment variables bound, which must neither be replaced by
// the rules nor change them.
func NewDetectorDefaultConfig() (*Detector, error) {
	v := viper.New()
	v.SetConfigType("toml")
	err := v.ReadConfig(strings.NewReader(config.DefaultConfig))
	if err != nil {
		return nil, err
	}
	var vc config.ViperConfig
	err = v.Unmarshal(&vc)
	if err != nil {
		return nil, err
	}
	cfg, err := vc.Translate()
	if err != nil {
		return nil, err
	}
	return NewDetector(cfg)
}

// DetectString returns the findings in content, in no set order.
func (d *Detector) DetectString(content string) []Finding {
	var findings []Finding

	// The keywords in content, for prefiltering rules.
	keywords := make(map[string]bool)
	normalized := strings.ToLower(content)
	for _, m := range d.prefilter.MatchString(normalized) {
		keywords[normalized[m.Pos():int(m.Pos())+len(m.Match())]] = true
	}

	for _, rule := range d.Config.Rules {
		if len(rule.Keywords) == 0 {
			// A rule with no keywords always runs.
			findings = append(findings, d.detectRule(content, rule)...)
			continue
		}
		for _, k := range rule.Keywords {
			if keywords[strings.ToLower(k)] {
				findings = append(findings, d.detectRule(content, rule)...)
				break
			}
		}
	}

	return filter(findings)
}

// detectRule returns the findings of rule r in raw.
func (d *Detector) detectRule(raw string, r config.Rule) []Finding {
	var findings []Finding

	// A skip-report rule reports only as a part of a multi-part rule.
	if r.SkipReport {
		return findings
	}

	if r.Path != nil {
		// The text scanned has no path: a path rule tests the empty path.
		if r.Regex == nil {
			// A path-only rule.
			if r.Path.MatchString("") {
				return append(findings, Finding{
					RuleID:      r.RuleID,
					Description: r.Description,
					Match:       "file detected: ",
					Tags:        r.Tags,
				})
			}
		} else if !r.Path.MatchString("") {
			// A rule with a path and a regex needs both to match.
			return findings
		}
	}

	if r.Regex == nil {
		return findings
	}

	matches := r.Regex.FindAllStringIndex(raw, -1)
	if len(matches) == 0 {
		return findings
	}

	newlineIndices := newLineRegexp.FindAllStringIndex(raw, -1)

	for _, matchIndex := range matches {
		secret := strings.Trim(raw[matchIndex[0]:matchIndex[1]], "\n")

		// Drop a newline the regex matched after the secret
		// (gitleaks issue 1352).
		matchIndex[1] = matchIndex[0] + len(secret)

		// The location is that of the match, not of the secret, which
		// differs when the rule has a secret group.
		loc := location(newlineIndices, raw, matchIndex)
		if matchIndex[1] > loc.endLineIndex {
			loc.endLineIndex = matchIndex[1]
		}

		finding := Finding{
			RuleID:      r.RuleID,
			Description: r.Description,
			StartLine:   loc.startLine,
			EndLine:     loc.endLine,
			StartColumn: loc.startColumn,
			EndColumn:   loc.endColumn,
			Line:        raw[loc.startLineIndex:loc.endLineIndex],
			Match:       secret,
			Secret:      secret,
			Tags:        r.Tags,
		}
		if strings.Contains(finding.Line, gitleaksAllowSignature) {
			continue
		}

		// The secret is the rule's secret group when it has one, else the
		// first non-empty capture group, if the regex has any.
		groups := r.Regex.FindStringSubmatch(finding.Secret)
		if len(groups) >= 2 {
			if r.SecretGroup > 0 {
				if len(groups) <= r.SecretGroup {
					// Config validation should prevent this.
					continue
				}
				finding.Secret = groups[r.SecretGroup]
			} else {
				for _, s := range groups[1:] {
					if len(s) > 0 {
						finding.Secret = s
						break
					}
				}
			}
		}

		entropy := shannonEntropy(finding.Secret)
		finding.Entropy = float32(entropy)
		if r.Entropy != 0.0 && entropy <= r.Entropy {
			// The entropy is too low.
			continue
		}

		if checkFindingAllowed(finding, d.Config.Allowlists) {
			continue
		}
		if checkFindingAllowed(finding, r.Allowlists) {
			continue
		}
		findings = append(findings, finding)
	}
	return findings
}

// checkFindingAllowed reports whether one of allowlists allows finding.
func checkFindingAllowed(finding Finding, allowlists []*config.Allowlist) bool {
	for _, a := range allowlists {
		target := finding.Secret
		switch a.RegexTarget {
		case "match":
			target = finding.Match
		case "line":
			target = finding.Line
		}

		regexAllowed := a.RegexAllowed(target)
		containsStopword, _ := a.ContainsStopWord(finding.Secret)

		var isAllowed bool
		if a.MatchCondition == config.AllowlistMatchAnd {
			// Every part the allowlist names must allow. The text has no
			// commit and no path, so an allowlist naming either never
			// allows.
			isAllowed = len(a.Commits) == 0 && len(a.Paths) == 0 &&
				(len(a.Regexes) == 0 || regexAllowed) &&
				(len(a.StopWords) == 0 || containsStopword)
		} else {
			isAllowed = regexAllowed || containsStopword
		}
		if isAllowed {
			return true
		}
	}
	return false
}
