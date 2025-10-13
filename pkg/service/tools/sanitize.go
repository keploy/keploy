package tools

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode"

	"github.com/zricethezav/gitleaks/v8/config"
	"github.com/zricethezav/gitleaks/v8/detect"
	"github.com/zricethezav/gitleaks/v8/regexp"
	"github.com/zricethezav/gitleaks/v8/report"
	"go.uber.org/zap"
	"gopkg.in/yaml.v3"
)

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

		if err := SanitizeFileInPlace(f, aggSecrets); err != nil {
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

// augmentDetector adds extra escape-aware rules to an existing Detector.
func augmentDetector(det *detect.Detector) (*detect.Detector, error) {
	cfg := det.Config // copy current config
	cfg.Extend.UseDefault = true
	if cfg.Rules == nil {
		cfg.Rules = make(map[string]config.Rule)
	}

	// helper to insert a rule into cfg
	add := func(key string, rule config.Rule) {
		cfg.Rules[key] = rule
		cfg.OrderedRules = append(cfg.OrderedRules, key)
		for _, kw := range rule.Keywords {
			if cfg.Keywords == nil {
				cfg.Keywords = make(map[string]struct{})
			}
			cfg.Keywords[strings.ToLower(kw)] = struct{}{}
		}
	}

	// --- JWT in URL/JSON param: handles =, \u003d and &, \u0026, %26
	add("jwt-in-param-escape-aware", config.Rule{
		Description: "JWT in URL/JSON param (handles =, \\u003d and &, \\u0026, %26)",
		Regex:       regexp.MustCompile(`(?i)(?:token|id_token|access_token|auth_token|jwt)(?:=|\\u003d)([A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,})(?:&|\\u0026|%26|["'\s]|$)`),
		Keywords:    []string{"token=", "access_token=", "\\u003d", "\\u0026", "%26"},
		Tags:        []string{"jwt", "escape-aware"},
	})

	// --- Generic JWT fallback
	add("jwt-generic", config.Rule{
		Description: "Generic JWT (three base64url segments)",
		Regex:       regexp.MustCompile(`[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}\.[A-Za-z0-9_-]{10,}`),
		Keywords:    []string{"eyJhbGciOi"},
		Tags:        []string{"jwt"},
	})

	// --- Authorization: Bearer header with escaped separators
	add("auth-bearer-header-escape-aware", config.Rule{
		Description: "Authorization: Bearer <token> (handles :, \\u003a, =, \\u003d)",
		Regex:       regexp.MustCompile(`(?i)authorization(?:\s*(?::|\\u003a|=|\\u003d))\s*bearer\s+([A-Za-z0-9._~-]{20,})`),
		Keywords:    []string{"authorization", "bearer", "\\u003a", "\\u003d"},
		Tags:        []string{"header", "escape-aware"},
	})

	// --- hdnts signed URLs
	add("hdnts-hmac", config.Rule{
		Description: "Signed URL (hdnts) HMAC parameter",
		Regex:       regexp.MustCompile(`(?i)hdnts(?:=|\\u003d)[^"\s]{0,512}?~hmac=([A-Fa-f0-9]{32,64})`),
		Keywords:    []string{"hdnts", "~hmac"},
		Tags:        []string{"signed-url", "hmac"},
	})

	// --- Opaque access tokens in URL params
	add("long-token-in-param-escape-aware", config.Rule{
		Description: "Opaque token in URL/JSON param (handles =, \\u003d and &, \\u0026, %26)",
		Regex:       regexp.MustCompile(`(?i)(?:token|access_token|auth_token|session_token|sig|signature|apikey|api_key)(?:=|\\u003d)([A-Za-z0-9._~%-]{32,})(?:&|\\u0026|%26|["'\s]|$)`),
		Keywords:    []string{"access_token", "auth_token", "apikey", "\\u003d", "\\u0026", "%26"},
		Tags:        []string{"token", "escape-aware"},
	})

	// ensure stable rule order
	sort.Strings(cfg.OrderedRules)

	newDet := detect.NewDetector(cfg)
	// preserve runtime knobs
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
func RedactYAML(yamlBytes []byte, aggSecrets map[string]string) ([]byte, error) {
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
	secretSet := collectSecrets(findings)

	// 2) Parse YAML
	var root yaml.Node
	if err := yaml.Unmarshal(yamlBytes, &root); err != nil {
		return nil, fmt.Errorf("parse yaml: %w", err)
	}

	// 3) Redact values; collect per-file replacements
	secretsMap := aggSecrets                      // shared across files
	var repls []replacement                       // oldValue -> placeholder
	headerKeyToPlaceholder := map[string]string{} // per-file

	redactNode(&root, nil, secretSet, secretsMap, &repls, headerKeyToPlaceholder)

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

func collectSecrets(findings []report.Finding) map[string]struct{} {
	set := make(map[string]struct{})
	for _, f := range findings {
		if s := strings.TrimSpace(f.Secret); s != "" {
			set[s] = struct{}{}
		}
	}
	return set
}

func lastPath(path []string) string {
	if len(path) == 0 {
		return "Value"
	}
	return path[len(path)-1]
}

func redactNode(
	n *yaml.Node,
	path []string,
	secretSet map[string]struct{},
	secrets map[string]string, // shared accumulator (key -> value)
	repls *[]replacement,
	headerKeyToPlaceholder map[string]string, // per-file
) {
	switch n.Kind {
	case yaml.DocumentNode, yaml.SequenceNode:
		for _, c := range n.Content {
			redactNode(c, path, secretSet, secrets, repls, headerKeyToPlaceholder)
		}

	case yaml.MappingNode:
		for i := 0; i < len(n.Content); i += 2 {
			k := n.Content[i]
			v := n.Content[i+1]
			key := k.Value
			newPath := append(path, key)

			if strings.EqualFold(key, "curl") {
				continue
			}

			switch v.Kind {
			case yaml.ScalarNode:
				if v.Tag == "!!str" {
					if looksLikeJSON(v.Value) {
						changed, newVal, jsonRepls := redactJSONString(v.Value, key, secretSet, secrets)
						if changed {
							v.Value = newVal
							*repls = append(*repls, jsonRepls...)
						}
					} else if containsAnySecret(v.Value, secretSet) {
						orig := v.Value
						base := keyToSecretKey(key)
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
				redactNode(v, newPath, secretSet, secrets, repls, headerKeyToPlaceholder)
			}
		}

	case yaml.ScalarNode:
		if n.Tag == "!!str" && containsAnySecret(n.Value, secretSet) && !looksLikeTemplate(n.Value) {
			orig := n.Value
			base := keyToSecretKey(lastPath(path))
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
	// ... spec -> req -> header -> <HeaderName>
	if len(path) < 4 {
		return false
	}
	n := len(path)
	return strings.EqualFold(path[n-3], "req") &&
		strings.EqualFold(path[n-2], "header")
}

// ----- JSON-in-string -----
func looksLikeJSON(s string) bool {
	t := strings.TrimSpace(s)
	return (strings.HasPrefix(t, "{") && strings.Contains(t, "}")) ||
		(strings.HasPrefix(t, "[") && strings.Contains(t, "]"))
}

func redactJSONString(s string, parentKey string, secretSet map[string]struct{},
	secrets map[string]string) (bool, string, []replacement) {

	var v interface{}
	if err := json.Unmarshal([]byte(s), &v); err != nil {
		return false, s, nil
	}
	var repls []replacement
	changed := redactJSONValue(&v, parentKey, secretSet, secrets, &repls)
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
	secrets map[string]string, repls *[]replacement) bool {

	changed := false
	switch x := (*v).(type) {
	case map[string]interface{}:
		for k, vv := range x {
			if redactJSONValue(&vv, k, secretSet, secrets, repls) {
				changed = true
			}
			x[k] = vv
		}
	case []interface{}:
		for i := range x {
			if redactJSONValue(&x[i], parentKey, secretSet, secrets, repls) {
				changed = true
			}
		}
	case string:
		if containsAnySecret(x, secretSet) {
			orig := x
			base := keyToSecretKey(parentKey)
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

	want := fmt.Sprintf("%s: %s", name, ph)
	if strings.TrimSpace(content) == want {
		return line
	}
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

// SanitizeFileInPlace reads a YAML file, redacts secrets, and writes back in-place.
// aggSecrets is a shared map across the entire test-set (key -> original value).
func SanitizeFileInPlace(path string, aggSecrets map[string]string) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	redacted, err := RedactYAML(raw, aggSecrets)
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
