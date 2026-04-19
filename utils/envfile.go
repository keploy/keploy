package utils

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

var validEnvKeyRe = regexp.MustCompile(`^[a-zA-Z_][a-zA-Z0-9_]*$`)

// reservedKeployEnvKeys are env vars Keploy injects for TLS CA interception.
// Allowing users to override them would silently break certificate injection.
var reservedKeployEnvKeys = map[string]struct{}{
	"NODE_EXTRA_CA_CERTS": {},
	"REQUESTS_CA_BUNDLE":  {},
	"SSL_CERT_FILE":       {},
	"CARGO_HTTP_CAINFO":   {},
	"JAVA_TOOL_OPTIONS":   {},
}

// ParseEnvFile reads a .env file and returns its contents as a map.
// It supports:
//   - KEY=VALUE pairs
//   - Lines starting with # are treated as comments and skipped
//   - Empty lines are skipped
//   - Values may be wrapped in single or double quotes (quotes are stripped)
//   - Only the first '=' is used as the delimiter; the rest of the line is the value
func ParseEnvFile(path string) (map[string]string, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, fmt.Errorf("failed to open env file %q: %w. Please verify the file path exists and is readable", path, err)
	}
	defer f.Close()

	envMap := make(map[string]string)
	scanner := bufio.NewScanner(f)
	// Increase the buffer to support large single-line values (e.g., base64 blobs, certificates).
	// The default limit (~64KB) causes bufio.ErrTooLong for such values.
	scanner.Buffer(make([]byte, 64*1024), 10*1024*1024)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())

		// skip empty lines and comments
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		idx := strings.Index(line, "=")
		if idx < 0 {
			// lines without '=' are skipped silently (bare variable exports)
			continue
		}

		key := strings.TrimSpace(line[:idx])
		if key == "" {
			continue
		}

		val := line[idx+1:]

		// strip surrounding quotes (single or double)
		if len(val) >= 2 {
			if (val[0] == '"' && val[len(val)-1] == '"') ||
				(val[0] == '\'' && val[len(val)-1] == '\'') {
				val = val[1 : len(val)-1]
			}
		}

		envMap[key] = val
	}

	if err := scanner.Err(); err != nil {
		return nil, fmt.Errorf("error reading env file %q: %w. Check if the file is corrupted or has encoding issues", path, err)
	}

	return envMap, nil
}

// ResolveEnvVars merges an env file and an inline env map into a single map.
// Inline env values take precedence over values from the env file, matching
// docker-compose semantics.
func ResolveEnvVars(envMap map[string]string, envFilePath string) (map[string]string, error) {
	merged := make(map[string]string)

	if envFilePath != "" {
		fileVars, err := ParseEnvFile(envFilePath)
		if err != nil {
			return nil, err
		}
		for k, v := range fileVars {
			if !validEnvKeyRe.MatchString(k) {
				return nil, fmt.Errorf("invalid environment variable name %q in env file %q: must match [a-zA-Z_][a-zA-Z0-9_]*", k, envFilePath)
			}
			if _, reserved := reservedKeployEnvKeys[k]; reserved {
				return nil, fmt.Errorf("environment variable %q is reserved by Keploy for TLS certificate injection and cannot be overridden via env/envFile", k)
			}
			merged[k] = v
		}
	}

	// inline values override file values
	for k, v := range envMap {
		if !validEnvKeyRe.MatchString(k) {
			return nil, fmt.Errorf("invalid environment variable name %q: must match [a-zA-Z_][a-zA-Z0-9_]*", k)
		}
		if _, reserved := reservedKeployEnvKeys[k]; reserved {
			return nil, fmt.Errorf("environment variable %q is reserved by Keploy for TLS certificate injection and cannot be overridden via env/envFile", k)
		}
		merged[k] = v
	}

	return merged, nil
}

// ResolveEnvFilePath resolves a relative envFile path against the directory
// that contains the keploy config file. If envFile is already absolute, empty,
// or configPath is empty, it is returned unchanged.
func ResolveEnvFilePath(configPath, envFile string) string {
	if envFile == "" || filepath.IsAbs(envFile) || configPath == "" {
		return envFile
	}
	basePath := configPath
	if info, err := os.Stat(basePath); err == nil && !info.IsDir() {
		basePath = filepath.Dir(basePath)
	}
	return filepath.Join(basePath, envFile)
}
