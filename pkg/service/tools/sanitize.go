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

	"github.com/zricethezav/gitleaks/v8/detect"
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

	for i, testSetID := range testSets {
		// Check for context cancellation
		select {
		case <-ctx.Done():
			t.logger.Info("Sanitize process cancelled by context")
			return ctx.Err()
		default:
		}

		// Progress logging for large test sets
		if len(testSets) > 5 && i%10 == 0 {
			t.logger.Info("Processing test sets", 
				zap.Int("completed", i), 
				zap.Int("total", len(testSets)),
				zap.String("currentSet", testSetID))
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

		if err := t.sanitizeTestSetDir(ctx, testSetDir); err != nil {
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

func (t *Tools) sanitizeTestSetDir(ctx context.Context, testSetDir string) error {
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

	// Process files in batches for better performance with large test sets
	batchSize := 10
	if len(files) < batchSize {
		batchSize = len(files)
	}
	
	// Adjust batch size based on file count for optimal performance
	if len(files) > 50 {
		batchSize = 20
	} else if len(files) < 5 {
		batchSize = len(files)
	}

	for i := 0; i < len(files); i += batchSize {
		end := i + batchSize
		if end > len(files) {
			end = len(files)
		}
		
		batch := files[i:end]
		if err := t.processBatch(ctx, batch, aggSecrets); err != nil {
			return err
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

// processBatch handles a batch of files concurrently to improve performance
func (t *Tools) processBatch(ctx context.Context, files []string, aggSecrets map[string]string) error {
	type fileResult struct {
		path    string
		secrets map[string]string
		err     error
	}

	// Create a shared detector for this batch to avoid repeated initialization
	detector, err := detect.NewDetectorDefaultConfig()
	if err != nil {
		t.logger.Error("Failed to create detector", zap.Error(err))
		// Fallback to sequential processing without shared detector
		for _, f := range files {
			if err := SanitizeFileInPlace(f, aggSecrets); err != nil {
				t.logger.Error("Failed to sanitize file", zap.String("file", f), zap.Error(err))
			}
		}
		return nil
	}

	results := make(chan fileResult, len(files))
	
	// Process files concurrently within the batch
	for _, f := range files {
		go func(filePath string) {
			select {
			case <-ctx.Done():
				results <- fileResult{path: filePath, err: ctx.Err()}
				return
			default:
			}

			// Read and process file independently
			localSecrets := make(map[string]string)
			err := SanitizeFileInPlaceWithDetector(filePath, localSecrets, detector)
			results <- fileResult{path: filePath, secrets: localSecrets, err: err}
		}(f)
	}

	// Collect results and merge secrets
	for i := 0; i < len(files); i++ {
		result := <-results
		if result.err != nil {
			t.logger.Error("Failed to sanitize file", zap.String("file", result.path), zap.Error(result.err))
			continue
		}
		
		// Merge secrets from this file into the aggregate
		for key, value := range result.secrets {
			aggSecrets[key] = value
		}
	}

	return nil
}

type replacement struct {
	old string
	new string
}

// RedactYAML applies secret detection + redaction to a YAML blob.
// - Populates/extends aggSecrets (shared across files in a test-set)
// - Writes placeholders into the YAML
// - Handles JSON-in-string and curl header blobs
func RedactYAML(yamlBytes []byte, aggSecrets map[string]string) ([]byte, error) {
	return RedactYAMLWithDetector(yamlBytes, aggSecrets, nil)
}

// RedactYAMLWithDetector allows reusing a detector instance for better performance
func RedactYAMLWithDetector(yamlBytes []byte, aggSecrets map[string]string, detector *detect.Detector) ([]byte, error) {
	// 1) Detect secrets - reuse detector if provided
	var err error
	if detector == nil {
		detector, err = detect.NewDetectorDefaultConfig()
		if err != nil {
			return nil, fmt.Errorf("gitleaks: %w", err)
		}
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
	// Pre-allocate map with estimated capacity for better performance
	set := make(map[string]struct{}, len(findings))
	for _, f := range findings {
		if s := strings.TrimSpace(f.Secret); s != "" && len(s) > 2 {
			// Skip very short secrets that are likely false positives
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
	if s == "" || len(secretSet) == 0 {
		return false
	}
	
	// Quick check for common template patterns to avoid expensive searches
	if strings.Contains(s, "{{string .secret.") {
		return false
	}
	
	// For performance, limit secret search for very long strings
	if len(s) > 10000 {
		s = s[:10000]
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
	return SanitizeFileInPlaceWithDetector(path, aggSecrets, nil)
}

// SanitizeFileInPlaceWithDetector allows reusing a detector for better performance
func SanitizeFileInPlaceWithDetector(path string, aggSecrets map[string]string, detector *detect.Detector) error {
	raw, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}

	// Skip processing if file is likely already sanitized
	if bytes.Contains(raw, []byte("{{string .secret.")) {
		return nil
	}

	redacted, err := RedactYAMLWithDetector(raw, aggSecrets, detector)
	if err != nil {
		return fmt.Errorf("redact %s: %w", path, err)
	}

	// Only write if content actually changed
	if bytes.Equal(raw, redacted) {
		return nil
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

	if err := os.WriteFile(path, out.Bytes(), 0644); err != nil {
		return fmt.Errorf("write %s: %w", path, err)
	}
	return nil
}

// WriteSecretsYAML writes the aggregated secrets map to secret.yaml with 0644 perms.
func WriteSecretsYAML(path string, secrets map[string]string) error {
	b, err := yaml.Marshal(secrets)
	if err != nil {
		return err
	}
	return os.WriteFile(path, b, 0644)
}
