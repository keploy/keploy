package tools

import (
	"bytes"
	"context"
	_ "embed"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/spf13/viper"
	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
	"github.com/zricethezav/gitleaks/v8/report"
	"go.keploy.io/server/v3/pkg"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

//go:embed custom_gitleaks_rules.toml
var customGitleaksRules string

func (t *Tools) Sanitize(ctx context.Context) error {
	t.logger.Info("Starting sanitize process...")

	// From CLI: SelectedTests
	testSets := t.extractTestSetIDs()
	if len(testSets) == 0 {
		var err error
		testSets, err = t.testDB.GetAllTestSetIDs(ctx)
		if err != nil {
			t.logger.Error("Failed to get test sets", zap.Error(err))
			return fmt.Errorf("failed to get test sets: %w", err)
		}
		t.logger.Info("No test sets specified, processing all test sets", zap.Int("count", len(testSets)))
	} else {
		t.logger.Info("Processing specified test sets", zap.Strings("testSets", testSets))
	}

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
			t.logger.Error("Could not locate test set directory; skipping",
				zap.String("testSetID", testSetID), zap.Error(err))
			continue
		}
		t.logger.Info("Sanitizing test set",
			zap.String("testSetID", testSetID),
			zap.String("dir", testSetDir))

		// if secret.yaml exists in the testSetDir then skip sanitization
		if _, err := os.Stat(filepath.Join(testSetDir, "secret.yaml")); err == nil {
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
			continue
		}
	}

	t.logger.Info("Sanitize process completed")
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

// locateTestSetDir resolves ./keploy/<testSetID> at the current working directory
func (t *Tools) locateTestSetDir(testSetID string) (string, error) {
	if p := filepath.Join(".", "keploy", testSetID); isDir(p) {
		return p, nil
	}
	return "", fmt.Errorf("keploy/%s not found in current directory", testSetID)
}

func isDir(p string) bool {
	fi, err := os.Stat(p)
	return err == nil && fi.IsDir()
}

func (t *Tools) SanitizeTestSetDir(ctx context.Context, testSetDir string) error {
	// Aggregate secrets across ALL files in this test set
	aggSecrets := map[string]string{}

	testsDir := filepath.Join(testSetDir, "tests")
	var files []string

	// Prefer keploy/<set>/tests/*.yaml
	if isDir(testsDir) {
		ents, err := os.ReadDir(testsDir)
		if err != nil {
			return fmt.Errorf("read tests dir: %w", err)
		}
		for _, e := range ents {
			if e.IsDir() {
				continue
			}
			name := e.Name()
			if !strings.HasSuffix(strings.ToLower(name), ".yaml") {
				continue
			}
			files = append(files, filepath.Join(testsDir, name))
		}
	} else {
		t.logger.Info("No tests directory found")
		return nil
	}

	if len(files) == 0 {
		t.logger.Info("No files to sanitize")
		return nil
	}

	for _, f := range files {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			t.logger.Info("File sanitization cancelled by context")
			return ctx.Err()
		default:
		}

		testName := strings.TrimSuffix(filepath.Base(f), filepath.Ext(f)) // e.g. "test-2"

		if err := SanitizeFileInPlace(f, testName, aggSecrets); err != nil {
			// Continue to next file
			t.logger.Error("Failed to sanitize file", zap.String("file", f), zap.Error(err))
			continue
		}
	}

	// Write keploy/<set>/secret.yaml
	secretPath := filepath.Join(testSetDir, "secret.yaml")
	if err := WriteSecretsYAML(secretPath, aggSecrets); err != nil {
		return fmt.Errorf("write secret.yaml: %w", err)
	}
	t.logger.Info("Wrote secret.yaml", zap.String("path", secretPath))
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
func SanitizeFileInPlace(path, testName string, aggSecrets map[string]string) error {
	raw, err := os.ReadFile(path)
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

	if err := os.WriteFile(path, out.Bytes(), 0777); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// WriteSecretsYAML writes the aggregated secrets map to secret.yaml with 0777 perms.
func WriteSecretsYAML(path string, secrets map[string]string) error {
	b, err := yaml.Marshal(secrets)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0777)
}

// DesanitizeFileInPlace reads a sanitized YAML file and replaces placeholders with actual secret values.
func DesanitizeFileInPlace(path string, secrets map[string]string) error {
	raw, err := os.ReadFile(path)
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

	err = os.WriteFile(path, out.Bytes(), 0777)
	if err != nil {
		return fmt.Errorf("write desanitized %s: %w", path, err)
	}
	return nil
}

// DesanitizeTestSet is a standalone function that desanitizes a test set directory.
// It reads secret.yaml, desanitizes all test files, and removes the secret.yaml file.
// Returns true if desanitization was performed, false if no secret.yaml was found.
func (t *Tools) DesanitizeTestSet(testSetID string, path string) (bool, error) {
	t.logger.Debug("Desanitizing test set", zap.String("testSetID", testSetID), zap.String("path", path))

	testSetDir := filepath.Join(path, testSetID)
	if !isDir(testSetDir) {
		return false, fmt.Errorf("test set directory not found: %s", testSetDir)
	}

	secretPath := filepath.Join(testSetDir, "secret.yaml")

	t.logger.Debug("Checking if secret.yaml exists", zap.String("path", secretPath))

	// Check if secret.yaml exists
	if _, err := os.Stat(secretPath); os.IsNotExist(err) {
		return false, nil
	}

	// Read secret.yaml
	secretBytes, err := os.ReadFile(secretPath)
	if err != nil {
		return false, fmt.Errorf("read secret.yaml: %w", err)
	}

	// Parse secrets map
	secrets := make(map[string]string)
	err = yaml.Unmarshal(secretBytes, &secrets)
	if err != nil {
		return false, fmt.Errorf("parse secret.yaml: %w", err)
	}

	t.logger.Debug("Parsed secrets map for desanitization", zap.Any("secrets", secrets))

	// Get all test files
	testsDir := filepath.Join(testSetDir, "tests")
	if !isDir(testsDir) {
		return false, fmt.Errorf("tests directory not found: %s", testsDir)
	}

	ents, err := os.ReadDir(testsDir)
	if err != nil {
		return false, fmt.Errorf("read tests dir: %w", err)
	}

	var files []string
	for _, e := range ents {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(strings.ToLower(name), ".yaml") {
			continue
		}
		files = append(files, filepath.Join(testsDir, name))
	}

	// Desanitize each file
	for _, f := range files {
		t.logger.Debug("Desanitizing file", zap.String("file", f))

		err = DesanitizeFileInPlace(f, secrets)
		if err != nil {
			return false, fmt.Errorf("failed to desanitize file %s: %w", f, err)
		}
	}

	// Remove secret.yaml after desanitization
	err = os.Remove(secretPath)
	if err != nil {
		return false, fmt.Errorf("remove secret.yaml: %w", err)
	}

	return true, nil
}
