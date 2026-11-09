package tools

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"sort"
	"strings"
	"syscall"
	"unicode"

	"github.com/spf13/viper"
	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
	"github.com/zricethezav/gitleaks/v8/report"
	"go.keploy.io/server/v3/pkg"
	"go.keploy.io/server/v3/pkg/platform/yaml/testdb"
	"go.keploy.io/server/v3/utils"
	"go.keploy.io/server/v3/utils/linkout"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

//go:embed custom_gitleaks_rules.toml
var customGitleaksRules string

func (t *Tools) Sanitize(ctx context.Context) error {
	t.logger.Info("Starting sanitize process...")

	// The recorded test sets: what the test store lists in the keploy
	// folder, as keploy test and keploy templatize read them.
	recorded, err := t.testDB.GetAllTestSetIDs(ctx)
	if err != nil {
		t.logger.Error("Failed to get test sets", zap.Error(err))
		return fmt.Errorf("failed to get test sets: %w", err)
	}
	// From CLI: SelectedTests
	testSets := t.extractTestSetIDs()
	if len(testSets) == 0 {
		testSets = recorded
		t.logger.Info("No test sets specified, processing all test sets", zap.Int("count", len(testSets)))
	} else {
		if err := t.refuseUnrecorded(testSets, recorded); err != nil {
			return err
		}
		t.logger.Info("Processing specified test sets", zap.Strings("testSets", testSets))
	}

	var failed []string
	for _, testSetID := range testSets {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			t.logger.Info("Sanitize process cancelled by context")
			return ctx.Err()
		default:
		}

		// keploy/<testSetID>
		testSetDir, err := t.locateTestSetDir(testSetID)
		if err != nil {
			// A recorded set (the testDB listed it) whose folder is not
			// where sanitize looks: left unsanitized, which fails the run.
			t.logger.Error("Could not locate test set directory, so it was not sanitized",
				zap.String("testSetID", testSetID), zap.Error(err))
			failed = append(failed, testSetID)
			continue
		}
		t.logger.Info("Sanitizing test set",
			zap.String("testSetID", testSetID),
			zap.String("dir", testSetDir))

		// if secret.yaml exists in the testSetDir then skip sanitization
		if hasSecretsFile(testSetDir) {
			t.logger.Info("secret.yaml found in the test set directory, skipping sanitization",
				zap.String("testSetID", testSetID),
				zap.String("dir", testSetDir))
			continue
		}

		if err := t.SanitizeTestSetDir(ctx, testSetDir); err != nil {
			t.logger.Error("Sanitize failed for test set",
				zap.String("testSetID", testSetID),
				zap.String("dir", testSetDir),
				zap.Error(err))
			failed = append(failed, testSetID)
			continue
		}
	}

	t.logger.Info("Sanitize process completed")
	// The others were sanitized; these were not, or not entirely, and their
	// tests may still hold secrets. That is a failed run, not a logged aside.
	if len(failed) > 0 {
		return fmt.Errorf("test set(s) %s were not entirely sanitized and may still hold secrets", strings.Join(failed, ", "))
	}
	return nil
}

func (t *Tools) extractTestSetIDs() []string {
	var ids []string
	if t.config == nil || t.config.Test.SelectedTests == nil {
		return ids
	}
	for ts := range t.config.Test.SelectedTests {
		ids = append(ids, ts)
	}
	return ids
}

// keployDir is the keploy folder the run was given (-p, as config.Path), or
// ./keploy where it was given none.
func (t *Tools) keployDir() string {
	if t.config != nil && t.config.Path != "" {
		return t.config.Path
	}
	return filepath.Join(".", "keploy")
}

// locateTestSetDir resolves <keploy folder>/<testSetID>. It looked only in
// ./keploy, whatever -p said: run elsewhere, sanitize skipped every set it
// was asked for and exited 0 with their secrets in place.
func (t *Tools) locateTestSetDir(testSetID string) (string, error) {
	if p := filepath.Join(t.keployDir(), testSetID); isDir(p) {
		return p, nil
	}
	return "", fmt.Errorf("%s not found in %s", testSetID, t.keployDir())
}

// refuseUnrecorded refuses, naming them, the named test sets that are not
// recorded: not listed by the test store (recorded), as keploy test and
// keploy templatize decide it. A name that is not was skipped with an error
// line and the run exited 0, leaving the set the user meant unsanitized; and
// a folder of the keploy folder is no test set by being a folder (-t reports,
// -t schema, -t . and -t ../elsewhere each passed for one, and exited 0).
//
// Every name here is the command line's (-t): ValidateFlags sets sanitize's
// selection from -t alone, so keploy.yml's test.selectedTests never reaches
// it. By the founder's ruling on P189 such a name is refused, with the proven
// switch on or off, before anything is sanitized.
func (t *Tools) refuseUnrecorded(names, recorded []string) error {
	var unknown []string
	for _, name := range names {
		if !slices.Contains(recorded, name) {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sort.Strings(unknown)
	err := fmt.Errorf("the test set(s) %s named for keploy sanitize are not recorded in %s", strings.Join(unknown, ", "), t.keployDir())
	utils.LogError(t.logger, err, "keploy sanitize names a test set that is not recorded, so it sanitizes none",
		zap.Strings("notRecorded", unknown),
		zap.Strings("recorded", recorded),
		zap.String("next_step", "name only recorded test sets in -t/--test-sets (comma-separated), and point -p at the folder that holds their keploy folder"))
	return err
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func (t *Tools) SanitizeTestSetDir(ctx context.Context, testSetDir string) error {
	// The set's recordings are rewritten where they lie, outside testdb:
	// each is re-keyed once, after (testdb.SetRewrite).
	rw, err := testdb.BeginSetRewrite(t.logger, filepath.Dir(testSetDir), filepath.Base(testSetDir))
	if err != nil {
		return err
	}
	defer rw.Done()
	return t.sanitizeTestSetDir(ctx, testSetDir)
}

func (t *Tools) sanitizeTestSetDir(ctx context.Context, testSetDir string) error {
	set, err := openTestSet(testSetDir)
	if err != nil {
		return err
	}
	defer func() { _ = set.Close() }()

	// Aggregate secrets across ALL files in this test set
	aggSecrets := map[string]string{}

	// keploy/<set>/tests/*.yaml, every one checked before any is rewritten.
	var unsanitized []string
	tests, files, err := testSetFiles(set, testSetDir)
	if err != nil {
		return err
	}
	if tests == nil {
		t.logger.Info("No tests directory found")
		return nil
	}
	defer func() { _ = tests.Close() }()

	if len(files) == 0 {
		t.logger.Info("No files to sanitize")
		return nil
	}

	for _, name := range files {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			t.logger.Info("File sanitization cancelled by context")
			return ctx.Err()
		default:
		}

		f := filepath.Join(testSetDir, "tests", name)
		testName := strings.TrimSuffix(name, filepath.Ext(name)) // e.g. "test-2"

		if err := sanitizeFileIn(tests, name, testName, aggSecrets); err != nil {
			// Continue to next file
			t.logger.Error("Failed to sanitize file", zap.String("file", f), zap.Error(err))
			unsanitized = append(unsanitized, f)
			continue
		}
	}

	// Write keploy/<set>/secret.yaml -- even when a file failed: the files
	// already sanitized need it to be restored.
	secretPath := filepath.Join(testSetDir, secretsFile)
	if err := writeSecretsIn(set, secretsFile, aggSecrets); err != nil {
		return fmt.Errorf("write secret.yaml: %w", err)
	}
	t.logger.Info("Wrote secret.yaml", zap.String("path", secretPath))
	if len(unsanitized) > 0 {
		// A rerun skips a test set that has a secret.yaml, so say how to finish.
		return fmt.Errorf("%d test file(s) could not be sanitized and may still hold secrets: %s; to finish, run `keploy normalize` to restore the test set, fix those files, then sanitize it again", len(unsanitized), strings.Join(unsanitized, ", "))
	}
	return nil
}

type replacement struct {
	old string
	new string
}

// loadCustomRules parses the custom gitleaks configuration from custom_gitleaks_rules.toml
// and returns a Config. The TOML file is embedded at compile time for easy distribution.
func loadCustomRules() (config.Config, error) {
	viper.SetConfigType("toml")
	if err := viper.ReadConfig(strings.NewReader(customGitleaksRules)); err != nil {
		return config.Config{}, fmt.Errorf("failed to read custom rules config: %w", err)
	}

	var viperConfig config.ViperConfig
	if err := viper.Unmarshal(&viperConfig); err != nil {
		return config.Config{}, fmt.Errorf("failed to unmarshal custom rules config: %w", err)
	}

	cfg, err := viperConfig.Translate()
	if err != nil {
		return config.Config{}, fmt.Errorf("failed to translate custom rules config: %w", err)
	}

	return cfg, nil
}

// augmentDetector adds custom rules from custom_gitleaks_rules.toml to an existing Detector.
// This approach allows easy maintenance - just update the TOML file to add/modify rules.
func augmentDetector(det *detect.Detector) (*detect.Detector, error) {
	// Load custom rules from the TOML configuration
	customCfg, err := loadCustomRules()
	if err != nil {
		return nil, fmt.Errorf("failed to load custom rules: %w", err)
	}

	// Start with the base detector's config
	cfg := det.Config
	if cfg.Rules == nil {
		cfg.Rules = make(map[string]config.Rule)
	}
	if cfg.Keywords == nil {
		cfg.Keywords = make(map[string]struct{})
	}

	// Merge custom rules into the base config
	for ruleID, rule := range customCfg.Rules {
		// Add rule to config
		cfg.Rules[ruleID] = rule
		cfg.OrderedRules = append(cfg.OrderedRules, ruleID)

		// Add keywords to the global keyword set
		for _, kw := range rule.Keywords {
			cfg.Keywords[strings.ToLower(kw)] = struct{}{}
		}
	}

	// Ensure stable rule order
	sort.Strings(cfg.OrderedRules)

	// Create new detector with augmented config
	newDet := detect.NewDetector(cfg)

	// Preserve runtime knobs from original detector
	newDet.Redact = det.Redact
	newDet.Verbose = det.Verbose
	newDet.MaxDecodeDepth = det.MaxDecodeDepth
	newDet.MaxArchiveDepth = det.MaxArchiveDepth
	newDet.MaxTargetMegaBytes = det.MaxTargetMegaBytes
	newDet.FollowSymlinks = det.FollowSymlinks
	newDet.NoColor = det.NoColor
	newDet.IgnoreGitleaksAllow = det.IgnoreGitleaksAllow

	return newDet, nil
}

// RedactYAML applies secret detection + redaction to a YAML blob.
// - Populates/extends aggSecrets (shared across files in a test-set)
// - Writes placeholders into the YAML
// - Handles JSON-in-string and curl header blobs
func RedactYAML(yamlBytes []byte, aggSecrets map[string]string, testName string) ([]byte, error) {
	// 1) Detect secrets
	detector, err := detect.NewDetectorDefaultConfig()
	if err != nil {
		return nil, fmt.Errorf("gitleaks: %w", err)
	}
	detector, err = augmentDetector(detector)
	if err != nil {
		return nil, fmt.Errorf("augment gitleaks rules: %w", err)
	}

	findings := detector.DetectString(string(yamlBytes))
	secretSet, secretNames := collectSecrets(findings)

	// 2) Parse YAML
	var root yaml.Node
	if err := yaml.Unmarshal(yamlBytes, &root); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}

	// 3) Redact values; collect per-file replacements
	secretsMap := aggSecrets                      // shared across files
	var repls []replacement                       // oldValue -> placeholder
	headerKeyToPlaceholder := map[string]string{} // per-file

	redactNode(&root, nil, secretSet, secretNames, secretsMap, &repls, headerKeyToPlaceholder, testName)

	// 4) Patch any curl strings using only the mappings we already created
	applyCurlUsingMaps(&root, repls, headerKeyToPlaceholder)

	// 5) Emit YAML
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		_ = enc.Close()
		return nil, fmt.Errorf("encode yaml: %w", err)
	}
	_ = enc.Close()
	return out.Bytes(), nil
}

// collectSecrets returns:
//   - set: the set of secret strings detected by gitleaks
//   - names: a mapping from secret string -> rule-based base name (CamelCase)
func collectSecrets(findings []report.Finding) (map[string]struct{}, map[string]string) {
	set := make(map[string]struct{})
	names := make(map[string]string)

	for _, f := range findings {
		s := strings.TrimSpace(f.Secret)
		if s == "" {
			continue
		}

		set[s] = struct{}{}

		if _, ok := names[s]; !ok {
			base := strings.TrimSpace(f.RuleID)
			if base == "" {
				base = strings.TrimSpace(f.Description)
			}
			if base != "" {
				base = keyToSecretKey(base)
			}
			if base == "" {
				base = "Secret"
			}
			names[s] = base
		}
	}
	return set, names
}

func lastPath(path []string) string {
	if len(path) == 0 {
		return "Value"
	}
	return path[len(path)-1]
}

// testPrefixedBase prefixes the base key with a test name ("test-2" -> "Test2_Base")
func testPrefixedBase(testName, base string) string {
	testName = strings.TrimSpace(testName)
	if testName == "" {
		return base
	}
	tn := keyToSecretKey(testName)
	if tn == "" {
		return base
	}
	if base == "" {
		return tn
	}
	return tn + "_" + base
}

func redactNode(
	n *yaml.Node,
	path []string,
	secretSet map[string]struct{},
	secretNames map[string]string,
	secrets map[string]string, // shared accumulator (key -> value)
	repls *[]replacement,
	headerKeyToPlaceholder map[string]string, // per-file
	testName string,
) {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			redactNode(c, path, secretSet, secretNames, secrets, repls, headerKeyToPlaceholder, testName)
		}

	case yaml.MappingNode:
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			v := n.Content[i+1]
			key := k.Value
			newPath := append(path, key)

			// skip curl here; handled in applyCurlUsingMaps
			if strings.EqualFold(key, "curl") {
				continue
			}

			// gRPC: decoded_data contains protoscope text; we want token-level redaction
			if strings.EqualFold(key, "decoded_data") && v.Kind == yaml.ScalarNode && v.Tag == "!!str" {
				redactGrpcDecodedData(v, key, secretSet, secretNames, secrets, repls, testName)
				continue
			}

			switch v.Kind {
			case yaml.ScalarNode:
				if v.Tag == "!!str" {
					if pkg.LooksLikeJSON(v.Value) {
						changed, newVal, jsonRepls := redactJSONString(v.Value, key, secretSet, secretNames, secrets, testName)
						if changed {
							v.Value = newVal
							*repls = append(*repls, jsonRepls...)
						}
					} else if containsAnySecret(v.Value, secretSet) {
						orig := v.Value
						base := keyToSecretKey(key)
						base = testPrefixedBase(testName, base)

						secKey := uniqueKeyForValue(base, orig, secrets)

						if !looksLikeTemplate(orig) {
							secrets[secKey] = orig // store FULL enclosing value (e.g., "Bearer ...")
						}
						ph := fmt.Sprintf("{{string .secret.%s }}", secKey)
						v.Tag = "!!str"
						v.Style = yaml.DoubleQuotedStyle
						v.Value = ph
						*repls = append(*repls, replacement{old: orig, new: ph})

						if isReqHeaderPath(newPath) {
							headerKeyToPlaceholder[strings.ToLower(key)] = ph
						}
					}
				}
			default:
				redactNode(v, newPath, secretSet, secretNames, secrets, repls, headerKeyToPlaceholder, testName)
			}
		}

	case yaml.ScalarNode:
		if n.Tag == "!!str" && containsAnySecret(n.Value, secretSet) && !looksLikeTemplate(n.Value) {
			orig := n.Value
			base := keyToSecretKey(lastPath(path))
			base = testPrefixedBase(testName, base)
			secKey := uniqueKeyForValue(base, orig, secrets)
			secrets[secKey] = orig
			ph := fmt.Sprintf("{{string .secret.%s }}", secKey)
			n.Style = yaml.DoubleQuotedStyle
			n.Value = ph
			*repls = append(*repls, replacement{old: orig, new: ph})
		}
	}
}

func isReqHeaderPath(path []string) bool {
	if len(path) < 4 {
		return false
	}
	n := len(path)

	// HTTP: ... -> req -> header -> <HeaderName>
	if strings.EqualFold(path[n-3], "req") && strings.EqualFold(path[n-2], "header") {
		return true
	}

	// gRPC Request: ... -> grpcReq -> headers -> ordinary_headers/pseudo_headers -> <HeaderName>
	if len(path) >= 5 && strings.EqualFold(path[n-4], "grpcReq") && strings.EqualFold(path[n-3], "headers") &&
		(strings.EqualFold(path[n-2], "ordinary_headers") || strings.EqualFold(path[n-2], "pseudo_headers")) {
		return true
	}

	// gRPC Response: ... -> grpcResp -> headers -> ordinary_headers/pseudo_headers -> <HeaderName>
	if len(path) >= 5 && strings.EqualFold(path[n-4], "grpcResp") && strings.EqualFold(path[n-3], "headers") &&
		(strings.EqualFold(path[n-2], "ordinary_headers") || strings.EqualFold(path[n-2], "pseudo_headers")) {
		return true
	}

	// gRPC Response Trailers: ... -> grpcResp -> trailers -> ordinary_headers/pseudo_headers -> <HeaderName>
	if len(path) >= 5 && strings.EqualFold(path[n-4], "grpcResp") && strings.EqualFold(path[n-3], "trailers") &&
		(strings.EqualFold(path[n-2], "ordinary_headers") || strings.EqualFold(path[n-2], "pseudo_headers")) {
		return true
	}

	return false
}

func redactJSONString(s string, parentKey string, secretSet map[string]struct{},
	secretNames map[string]string, secrets map[string]string, testName string) (bool, string, []replacement) {

	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return false, s, nil
	}
	var repls []replacement
	changed := redactJSONValue(&v, parentKey, secretSet, secretNames, secrets, &repls, testName)
	if !changed {
		return false, s, nil
	}
	b, err := json.Marshal(v) // compact
	if err != nil {
		return false, s, nil
	}
	return true, string(b), repls
}

func redactJSONValue(v *interface{}, parentKey string, secretSet map[string]struct{},
	secretNames map[string]string, secrets map[string]string, repls *[]replacement, testName string) bool {

	changed := false
	switch x := (*v).(type) {
	case map[string]interface{}:
		for k, vv := range x {
			if redactJSONValue(&vv, k, secretSet, secretNames, secrets, repls, testName) {
				changed = true
			}
			x[k] = vv
		}
	case []interface{}:
		for i := range x {
			if redactJSONValue(&x[i], parentKey, secretSet, secretNames, secrets, repls, testName) {
				changed = true
			}
		}
	case string:
		if containsAnySecret(x, secretSet) {
			orig := x

			// Prefer gitleaks rule-based name if this exact string is a reported secret
			base := ""
			if bn, ok := secretNames[orig]; ok && bn != "" {
				base = bn
			} else {
				base = keyToSecretKey(parentKey)
			}
			base = testPrefixedBase(testName, base)

			secKey := uniqueKeyForValue(base, orig, secrets)

			if !looksLikeTemplate(orig) {
				secrets[secKey] = orig
			}
			placeholder := fmt.Sprintf("{{string .secret.%s }}", secKey)
			*v = placeholder
			*repls = append(*repls, replacement{old: orig, new: placeholder})
			changed = true
		}
	}
	return changed
}

// ----- Curl post-processing -----

func applyCurlUsingMaps(n *yaml.Node, repls []replacement, headerKeyToPlaceholder map[string]string) {
	// dedupe and prefer longer replacements first
	seen := map[string]string{}
	for _, r := range repls {
		if r.old != "" {
			seen[r.old] = r.new
		}
	}
	repls = repls[:0]
	for old, newv := range seen {
		repls = append(repls, replacement{old: old, new: newv})
	}
	sort.Slice(repls, func(i, j int) bool { return len(repls[i].old) > len(repls[j].old) })

	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			applyCurlUsingMaps(c, repls, headerKeyToPlaceholder)
		}
	case yaml.MappingNode:
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			v := n.Content[i+1]
			if strings.EqualFold(k.Value, "curl") && v.Kind == yaml.ScalarNode {
				origStyle := v.Style
				txt := v.Value

				lines := strings.Split(txt, "\n")
				for i := range lines {
					lines[i] = rewriteCurlHeaderLine(lines[i], headerKeyToPlaceholder)
				}
				txt = strings.Join(lines, "\n")

				for _, r := range repls {
					txt = strings.ReplaceAll(txt, r.old, r.new)
				}
				v.Value = txt
				v.Style = origStyle
			} else {
				applyCurlUsingMaps(v, repls, headerKeyToPlaceholder)
			}
		}
	}
}

func rewriteCurlHeaderLine(line string, headerKeyToPlaceholder map[string]string) string {
	if !strings.Contains(line, "--header") && !strings.Contains(line, "-H") {
		return line
	}
	start := strings.Index(line, "--header")
	if start == -1 {
		start = strings.Index(line, "-H")
		if start == -1 {
			return line
		}
	}
	s := line[start:]
	i1s := strings.IndexByte(s, '\'')
	i1d := strings.IndexByte(s, '"')
	if i1s == -1 && i1d == -1 {
		return line
	}
	var q byte
	var i1 int
	switch {
	case i1s == -1:
		q, i1 = '"', start+i1d
	case i1d == -1:
		q, i1 = '\'', start+i1s
	default:
		if i1s < i1d {
			q, i1 = '\'', start+i1s
		} else {
			q, i1 = '"', start+i1d
		}
	}
	i2 := lastUnescapedIndexByte(line, q)
	if i2 == -1 || i2 <= i1 {
		return line
	}

	content := line[i1+1 : i2] // e.g., Authorization: Bearer XXX
	colon := strings.Index(content, ":")
	if colon == -1 {
		return line
	}
	name := strings.TrimSpace(content[:colon])

	ph, ok := headerKeyToPlaceholder[strings.ToLower(name)]
	if !ok {
		return line
	}

	// Check if the value has already been replaced by blanket replacement
	// or if it needs to be replaced with the placeholder
	want := fmt.Sprintf("%s: %s", name, ph)

	// If already correct, return as-is
	if strings.TrimSpace(content) == strings.TrimSpace(want) {
		return line
	}

	// Replace with the header-specific placeholder
	return line[:i1+1] + want + line[i2:]
}

func lastUnescapedIndexByte(s string, ch byte) int {
	for i := len(s) - 1; i >= 0; i-- {
		if s[i] != ch {
			continue
		}
		bs := 0
		for j := i - 1; j >= 0 && s[j] == '\\'; j-- {
			bs++
		}
		if bs%2 == 0 {
			return i
		}
	}
	return -1
}

func containsAnySecret(s string, secretSet map[string]struct{}) bool {
	if s == "" {
		return false
	}
	for secret := range secretSet {
		if secret != "" && strings.Contains(s, secret) {
			return true
		}
	}
	return false
}

func looksLikeTemplate(s string) bool {
	return strings.Contains(s, "{{string .secret.") && strings.Contains(s, "}}")
}

func keyToSecretKey(field string) string {
	field = strings.TrimSpace(field)
	field = strings.TrimPrefix(field, ":")
	parts := splitField(field)
	for i, p := range parts {
		// Title-case each segment
		if p == "" {
			continue
		}
		runes := []rune(strings.ToLower(p))
		runes[0] = unicode.ToUpper(runes[0])
		parts[i] = string(runes)
	}
	key := strings.Join(parts, "")
	if key == "" {
		key = "Secret"
	}
	// A key is an identifier ({{string .secret.KEY}}): it starts with a
	// letter, never a digit.
	if r := []rune(key)[0]; unicode.IsDigit(r) {
		key = "Secret" + key
	}
	return key
}

func splitField(s string) []string {
	var parts []string
	var buf strings.Builder
	lastLower := false
	for _, r := range s {
		if !unicode.IsLetter(r) && !unicode.IsDigit(r) {
			if buf.Len() > 0 {
				parts = append(parts, buf.String())
				buf.Reset()
			}
			lastLower = false
			continue
		}
		if unicode.IsUpper(r) && lastLower {
			parts = append(parts, buf.String())
			buf.Reset()
		}
		buf.WriteRune(r)
		lastLower = unicode.IsLower(r)
	}
	if buf.Len() > 0 {
		parts = append(parts, buf.String())
	}
	return parts
}

// uniqueKeyForValue rules:
//   - If base is unused, use base.
//   - If base exists with SAME value, reuse base.
//   - If base exists with DIFFERENT value, try base_2, base_3...
//     If any suffix already maps to the SAME value, reuse that suffix.
//     Otherwise, return the first free suffix.
func uniqueKeyForValue(base, val string, existing map[string]string) string {
	if v, ok := existing[base]; ok {
		if v == val {
			return base
		}
	} else {
		// base not used yet — claim it
		return base
	}
	for i := 2; ; i++ {
		k := fmt.Sprintf("%s_%d", base, i)
		if v, ok := existing[k]; ok {
			if v == val {
				return k
			}
			continue
		}
		return k // first free slot
	}
}

// --- gRPC decoded_data helpers ---

func isTokenByte(b byte) bool {
	if b >= 'a' && b <= 'z' {
		return true
	}
	if b >= 'A' && b <= 'Z' {
		return true
	}
	if b >= '0' && b <= '9' {
		return true
	}
	switch b {
	case '-', '_', '.', '~',
		':', '/', '?', '#', '[', ']', '@',
		'!', '$', '&', '\'', '(', ')', '*', '+', ',', ';', '=',
		'%':
		return true
	default:
		return false
	}
}

func expandToken(text string, start, end int) (int, int) {
	i := start
	for i > 0 && isTokenByte(text[i-1]) {
		i--
	}
	j := end
	for j < len(text) && isTokenByte(text[j]) {
		j++
	}
	return i, j
}

// redactGrpcDecodedData finds secrets inside a gRPC decoded_data protoscope blob and
// replaces whole "tokens" (e.g., full URLs, full keys) rather than just the small
// substrings that gitleaks reports.
func redactGrpcDecodedData(
	n *yaml.Node,
	key string,
	secretSet map[string]struct{},
	secretNames map[string]string,
	secrets map[string]string,
	repls *[]replacement,
	testName string,
) {
	if n.Kind != yaml.ScalarNode || n.Tag != "!!str" {
		return
	}
	text := n.Value
	if text == "" || len(secretSet) == 0 {
		return
	}

	type cand struct {
		val  string
		base string
	}

	seen := make(map[string]string) // fullToken -> base
	for s := range secretSet {
		if s == "" {
			continue
		}
		idx := strings.Index(text, s)
		for idx != -1 {
			start, end := expandToken(text, idx, idx+len(s))
			if end <= start {
				break
			}
			full := text[start:end]
			if full == "" {
				break
			}
			if _, ok := seen[full]; !ok {
				base := secretNames[s]
				if base == "" {
					base = keyToSecretKey(key)
				}
				base = testPrefixedBase(testName, base)
				seen[full] = base
			}
			next := strings.Index(text[idx+len(s):], s)
			if next == -1 {
				break
			}
			idx += len(s) + next
		}
	}

	if len(seen) == 0 {
		return
	}

	var cands []cand
	for val, base := range seen {
		cands = append(cands, cand{val: val, base: base})
	}

	// Replace longer tokens first
	sort.Slice(cands, func(i, j int) bool {
		return len(cands[i].val) > len(cands[j].val)
	})

	origStyle := n.Style

	for _, c := range cands {
		if !strings.Contains(text, c.val) {
			continue
		}
		secKey := uniqueKeyForValue(c.base, c.val, secrets)
		if !looksLikeTemplate(c.val) {
			secrets[secKey] = c.val
		}
		ph := fmt.Sprintf("{{string .secret.%s }}", secKey)
		text = strings.ReplaceAll(text, c.val, ph)
		*repls = append(*repls, replacement{old: c.val, new: ph})
	}

	n.Value = text
	n.Style = origStyle
}

// SanitizeFileInPlace reads a YAML file, redacts secrets, and writes back in-place.
// aggSecrets is a shared map across the entire test-set (key -> original value).
// The file is opened in its folder, opened inside the project
// (utils.OpenRecordingDir), and is never followed if it is a symbolic link.
func SanitizeFileInPlace(path, testName string, aggSecrets map[string]string) error {
	return inFileFolder(path, func(dir *os.Root, name string) error {
		return sanitizeFileIn(dir, name, testName, aggSecrets)
	})
}

// sanitizeFileIn is SanitizeFileInPlace of the file name in the folder dir.
func sanitizeFileIn(dir *os.Root, name, testName string, aggSecrets map[string]string) error {
	path := filepath.Join(dir.Name(), name)
	raw, err := readTestSetFile(dir, name)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	redacted, err := RedactYAML(raw, aggSecrets, testName)
	if err != nil {
		return fmt.Errorf("redact %s: %w", path, err)
	}

	// Normalize YAML formatting
	var root yaml.Node
	if err := yaml.Unmarshal(redacted, &root); err != nil {
		return fmt.Errorf("reparse %s: %w", path, err)
	}
	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)
	if err := enc.Encode(&root); err != nil {
		_ = enc.Close()
		return fmt.Errorf("encode %s: %w", path, err)
	}
	_ = enc.Close()

	if err := writeTestSetFile(dir, name, out.Bytes(), testFileMode, false); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// inFileFolder runs op on the file at path in its folder, opened inside the
// project as a test set's folders are (utils.OpenRecordingDir): a folder on
// the way that is a link out of the project is refused with why.
func inFileFolder(path string, op func(dir *os.Root, name string) error) error {
	dir, err := utils.OpenRecordingDir(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("keploy rewrites %s only inside the project: %w", path, err)
	}
	defer func() { _ = dir.Close() }()
	return op(dir, filepath.Base(path))
}

// WriteSecretsYAML writes the aggregated secrets map to secret.yaml, which only
// its owner may read (secretsFileMode).
// It is opened as SanitizeFileInPlace opens a test file.
func WriteSecretsYAML(path string, secrets map[string]string) error {
	return inFileFolder(path, func(dir *os.Root, name string) error {
		return writeSecretsIn(dir, name, secrets)
	})
}

// writeSecretsIn is WriteSecretsYAML of the file name in the folder dir.
func writeSecretsIn(dir *os.Root, name string, secrets map[string]string) error {
	b, err := yaml.Marshal(secrets)
	if err != nil {
		return err
	}
	return writeTestSetFile(dir, name, b, secretsFileMode, true)
}

// secretsFile is the name of a sanitized test set's secret.yaml.
const secretsFile = "secret.yaml"

// secretsFileMode is secret.yaml's: it holds the secrets taken out of the
// tests, so only its owner reads it. It used to be written 0777, which a
// usual umask makes 0755: every user on the host could read them.
const secretsFileMode = 0o600

// testFileMode is what a test file sanitize creates gets: what keploy creates
// test files with. One it rewrites keeps its own mode.
const testFileMode = 0o644

// A test set is a directory a cloned repository can carry, and sanitize and
// normalize often run under sudo, as recording does. Its files are therefore
// read, rewritten, written and removed only inside the project: the keploy
// folder is opened from the project (utils.OpenRecordingDir), so one the
// repository carries as a link out of the project, by an absolute target or
// by "..", is refused with why, and nothing where it leads is touched. The
// test set, its tests directory and each test file are then opened, each
// inside the one before, and must be what they say, not a symbolic link to
// somewhere else (testSetFiles, openTestSetFile).

// openTestSet opens the test set at testSetDir, <keploy folder>/<set>, inside
// the project (see above). It is refused if it is a symbolic link, even to a
// folder in the keploy folder. A test set that is not there is an error that
// errors.Is fs.ErrNotExist.
func openTestSet(testSetDir string) (*os.Root, error) {
	keploy, err := utils.OpenRecordingDir(filepath.Dir(testSetDir))
	if err != nil {
		return nil, fmt.Errorf("keploy rewrites test set %s only inside the project's keploy folder: %w", testSetDir, err)
	}
	defer func() { _ = keploy.Close() }()
	name := filepath.Base(testSetDir)
	fi, err := keploy.Lstat(name)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		return nil, fmt.Errorf("test set %s is not a directory (a symbolic link?); keploy does not follow it", testSetDir)
	}
	return keploy.OpenRoot(name)
}

// hasSecretsFile reports whether the test set at testSetDir has a
// secret.yaml, a symbolic link there included: it is sanitized. Its folder is
// opened inside the project (see above). One it cannot open or look in has
// none: sanitizing the set opens the same folders, and says why it cannot.
func hasSecretsFile(testSetDir string) bool {
	set, err := openTestSet(testSetDir)
	if err != nil {
		return false
	}
	defer func() { _ = set.Close() }()
	_, err = set.Lstat(secretsFile)
	return err == nil
}

// testSetFiles is the tests directory of the test set set (at testSetDir),
// opened, and the names of the test files in it, its *.yaml, in name order.
// Every one is checked before any is rewritten: the tests directory and each
// test file must be what they say, not a symlink to somewhere else (tests ->
// /etc/netplan would have had root rewrite every *.yaml there). Checking up
// front means a test set is rewritten whole or not at all. The caller closes
// tests.
//
// Only a tests directory that does not exist means no tests (tests nil). One
// that is there but is not a directory, or cannot be looked at or listed, is
// an error: read as "no tests", sanitize would leave whatever the set holds
// unsanitized and report success.
func testSetFiles(set *os.Root, testSetDir string) (tests *os.Root, files []string, err error) {
	testsDir := filepath.Join(testSetDir, "tests")
	fi, err := set.Lstat("tests")
	switch {
	case errors.Is(err, os.ErrNotExist):
		return nil, nil, nil
	case err != nil:
		return nil, nil, err
	case fi.Mode()&os.ModeSymlink != 0:
		return nil, nil, fmt.Errorf("%s is a symbolic link; keploy does not follow it", testsDir)
	case !fi.IsDir():
		return nil, nil, fmt.Errorf("keploy cannot tell what tests the set holds: %w", &fs.PathError{Op: "lstat", Path: testsDir, Err: syscall.ENOTDIR})
	}
	tests, err = set.OpenRoot("tests")
	if err != nil {
		return nil, nil, fmt.Errorf("read tests dir: %w", err)
	}
	ents, err := readRootDir(tests)
	if err != nil {
		_ = tests.Close()
		return nil, nil, fmt.Errorf("read tests dir: %w", err)
	}
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".yaml") {
			continue
		}
		if !e.Type().IsRegular() {
			_ = tests.Close()
			return nil, nil, fmt.Errorf("%s is not a regular file (a symbolic link?); keploy does not rewrite the test set through it", filepath.Join(testsDir, name))
		}
		files = append(files, name)
	}
	return tests, files, nil
}

// readRootDir is os.ReadDir of the folder dir: its entries, by name.
func readRootDir(dir *os.Root) ([]fs.DirEntry, error) {
	d, err := dir.Open(".")
	if err != nil {
		return nil, err
	}
	defer func() { _ = d.Close() }()
	ents, err := d.ReadDir(-1)
	slices.SortFunc(ents, func(a, b fs.DirEntry) int { return strings.Compare(a.Name(), b.Name()) })
	return ents, err
}

// openTestSetFile opens name, a file in the folder dir of a test set, as
// os.OpenFile does, without following a symbolic link there, not even to a
// file in the folder (openNoFollowIn). name is one path element: anything
// else, "..", or a path, would be opened from dir, and is refused.
func openTestSetFile(dir *os.Root, name string, flag int, perm os.FileMode) (*os.File, error) {
	if name != filepath.Base(name) || name == ".." {
		return nil, &fs.PathError{Op: "open", Path: filepath.Join(dir.Name(), name), Err: fs.ErrInvalid}
	}
	return openNoFollowIn(dir, name, flag, perm)
}

// readTestSetFile reads name, a file of a test set in its folder dir, without
// following a symlink there (openTestSetFile).
func readTestSetFile(dir *os.Root, name string) ([]byte, error) {
	f, err := openTestSetFile(dir, name, os.O_RDONLY, 0)
	if err != nil {
		return nil, noFollowError(dir, name, err)
	}
	defer func() { _ = f.Close() }()
	return io.ReadAll(f)
}

// writeTestSetFile writes data to name, a file of a test set in its folder
// dir, without following a symlink there (openTestSetFile). A new file is
// created with perm and, when root creates it under sudo, belongs to the user
// who ran sudo, as keploy's other outputs do; whether it is new is told by
// the open itself (O_EXCL). With narrow, an existing file more open than perm
// is narrowed to it, through the open file, never by name -- and a file that
// cannot be is not written. Without it, an existing file keeps its mode and
// owner.
func writeTestSetFile(dir *os.Root, name string, data []byte, perm os.FileMode, narrow bool) error {
	path := filepath.Join(dir.Name(), name)
	const flags = os.O_WRONLY | os.O_TRUNC
	f, err := openTestSetFile(dir, name, flags|os.O_CREATE|os.O_EXCL, perm)
	created := err == nil
	if errors.Is(err, os.ErrExist) {
		f, err = openTestSetFile(dir, name, flags, perm)
	}
	if err != nil {
		return noFollowError(dir, name, err)
	}
	if narrow {
		if fi, err := f.Stat(); err == nil && fi.Mode().Perm()&^perm != 0 {
			if err := f.Chmod(fi.Mode().Perm() & perm); err != nil {
				_ = f.Close()
				return fmt.Errorf("%s is readable by others and could not be made %v: %w", path, perm, err)
			}
		}
	}
	if _, err := f.Write(data); err != nil {
		_ = f.Close()
		return err
	}
	if created {
		utils.RestoreFileOwnershipOf(zap.NewNop(), f, path)
	}
	return f.Close()
}

// noFollowError says why name, a file of a test set in its folder dir, was
// not opened (openTestSetFile), plainly, never in os.Root's or the kernel's
// words. A symbolic link there, which an O_NOFOLLOW open refuses even when it
// leads to a file in the folder, is named with where it leads; on a system
// with no O_NOFOLLOW open, where only a link out of the folder is refused, the
// link is said as linkout.Error says it. Anything else is returned as it is.
// errors.Is still sees the open's error (syscall.ELOOP for a link).
func noFollowError(dir *os.Root, name string, err error) error {
	path := filepath.Join(dir.Name(), name)
	refused := fmt.Sprintf("%s is a symbolic link; keploy does not follow it", path)
	if errors.Is(err, syscall.ELOOP) {
		if target, rerr := dir.Readlink(name); rerr == nil {
			return &linkRefusedError{fmt.Sprintf("%s: it leads to %q; replace the link with the file it leads to", refused, target), err}
		}
		return &linkRefusedError{refused + "; replace the link with the file it leads to", err}
	}
	if out := linkout.Explain(dir, dir.Name(), name, fmt.Sprintf("the test set's file %q is rewritten only inside its folder %q", name, dir.Name()), err); out != nil {
		return out
	}
	return err
}

// linkRefusedError is noFollowError's refusal of a link: msg says it, and
// errors.Is and errors.As see err, the open's error, through it.
type linkRefusedError struct {
	msg string
	err error
}

func (e *linkRefusedError) Error() string { return e.msg }

func (e *linkRefusedError) Unwrap() error { return e.err }

// DesanitizeFileInPlace reads a sanitized YAML file and replaces placeholders with actual secret values.
// It is opened as SanitizeFileInPlace opens a test file.
func DesanitizeFileInPlace(path string, secrets map[string]string) error {
	return inFileFolder(path, func(dir *os.Root, name string) error {
		return desanitizeFileIn(dir, name, secrets)
	})
}

// desanitizeFileIn is DesanitizeFileInPlace of the file name in the folder dir.
func desanitizeFileIn(dir *os.Root, name string, secrets map[string]string) error {
	path := filepath.Join(dir.Name(), name)
	raw, err := readTestSetFile(dir, name)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	// Replace all placeholders with actual values
	content := string(raw)
	for key, value := range secrets {
		// sanitize only follows this pattern as of now
		placeholder := fmt.Sprintf("{{string .secret.%s }}", key)
		content = strings.ReplaceAll(content, placeholder, value)
	}

	// Normalize YAML formatting
	var root yaml.Node
	err = yaml.Unmarshal([]byte(content), &root)
	if err != nil {
		return fmt.Errorf("parse desanitized yaml %s: %w", path, err)
	}

	var out bytes.Buffer
	enc := yaml.NewEncoder(&out)
	enc.SetIndent(2)

	// to ensure the same formatting as the original file
	err = enc.Encode(&root)
	if err != nil {
		_ = enc.Close()
		return fmt.Errorf("encode desanitized yaml %s: %w", path, err)
	}
	_ = enc.Close()

	err = writeTestSetFile(dir, name, out.Bytes(), testFileMode, false)
	if err != nil {
		return fmt.Errorf("write desanitized %s: %w", path, err)
	}
	return nil
}

// DesanitizeTestSet is a standalone function that desanitizes a test set directory.
// It reads secret.yaml, desanitizes all test files, and removes the secret.yaml file.
// Returns true if desanitization was performed, false if no secret.yaml was found.
func (t *Tools) DesanitizeTestSet(testSetID string, path string) (bool, error) {
	// The set's recordings are rewritten where they lie, outside testdb:
	// each is re-keyed once, after (testdb.SetRewrite).
	rw, err := testdb.BeginSetRewrite(t.logger, path, testSetID)
	if err != nil {
		return false, err
	}
	defer rw.Done()
	return t.desanitizeTestSet(testSetID, path)
}

func (t *Tools) desanitizeTestSet(testSetID string, path string) (bool, error) {
	t.logger.Debug("Desanitizing test set", zap.String("testSetID", testSetID), zap.String("path", path))

	testSetDir := filepath.Join(path, testSetID)
	set, err := openTestSet(testSetDir)
	if errors.Is(err, fs.ErrNotExist) {
		return false, fmt.Errorf("test set directory not found: %s", testSetDir)
	}
	if err != nil {
		return false, err
	}
	defer func() { _ = set.Close() }()

	secretPath := filepath.Join(testSetDir, secretsFile)

	t.logger.Debug("Checking if secret.yaml exists", zap.String("path", secretPath))

	// Check if secret.yaml exists (a symlink there exists too, and is refused
	// by the read below rather than taken for "not sanitized")
	if _, err := set.Lstat(secretsFile); errors.Is(err, fs.ErrNotExist) {
		return false, nil
	}

	// Every test file checked before secret.yaml is read or any is rewritten.
	tests, files, err := testSetFiles(set, testSetDir)
	if err != nil {
		return false, err
	}
	if tests == nil {
		return false, fmt.Errorf("tests directory not found: %s", filepath.Join(testSetDir, "tests"))
	}
	defer func() { _ = tests.Close() }()

	// Read secret.yaml
	secretBytes, err := readTestSetFile(set, secretsFile)
	if err != nil {
		return false, fmt.Errorf("read secret.yaml: %w", err)
	}

	// Parse secrets map
	secrets := make(map[string]string)
	err = yaml.Unmarshal(secretBytes, &secrets)
	if err != nil {
		return false, fmt.Errorf("parse secret.yaml: %w", err)
	}

	// How many, never which: these are the values sanitize took out of the
	// tests so that they would not be kept in the clear.
	t.logger.Debug("Parsed secrets map for desanitization", zap.Int("secrets", len(secrets)))

	// Desanitize each file
	for _, name := range files {
		f := filepath.Join(testSetDir, "tests", name)
		t.logger.Debug("Desanitizing file", zap.String("file", f))

		err = desanitizeFileIn(tests, name, secrets)
		if err != nil {
			return false, fmt.Errorf("failed to desanitize file %s: %w", f, err)
		}
	}

	// Remove secret.yaml after desanitization
	err = set.Remove(secretsFile)
	if err != nil {
		return false, fmt.Errorf("remove secret.yaml: %w", err)
	}

	return true, nil
}
