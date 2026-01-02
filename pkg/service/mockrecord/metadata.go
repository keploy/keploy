package mockrecord

import (
	"net/url"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// ExtractMetadata extracts metadata from recorded mocks for contextual naming.
func ExtractMetadata(mocks []*models.Mock, command string) *models.MockMetadata {
	meta := &models.MockMetadata{
		Protocols:   make([]string, 0),
		Endpoints:   make([]models.EndpointInfo, 0),
		ServiceName: extractServiceName(command),
		Timestamp:   time.Now(),
	}

	protocolSet := make(map[string]bool)

	for _, mock := range mocks {
		if mock == nil {
			continue
		}

		// Extract protocol
		protocol := string(mock.Kind)
		if !protocolSet[protocol] {
			protocolSet[protocol] = true
			meta.Protocols = append(meta.Protocols, protocol)
		}

		// Extract endpoint info based on kind
		switch mock.Kind {
		case models.HTTP:
			if mock.Spec.HTTPReq != nil {
				host, path := extractHostAndPath(mock.Spec.HTTPReq.URL)
				meta.Endpoints = append(meta.Endpoints, models.EndpointInfo{
					Protocol: "HTTP",
					Host:     host,
					Path:     path,
					Method:   string(mock.Spec.HTTPReq.Method),
				})
			}

		case models.Postgres:
			host := ""
			if mock.Spec.Metadata != nil {
				host = mock.Spec.Metadata["host"]
			}
			meta.Endpoints = append(meta.Endpoints, models.EndpointInfo{
				Protocol: "Postgres",
				Host:     host,
				Method:   "QUERY",
			})

		case models.MySQL:
			host := ""
			if mock.Spec.Metadata != nil {
				host = mock.Spec.Metadata["host"]
			}
			meta.Endpoints = append(meta.Endpoints, models.EndpointInfo{
				Protocol: "MySQL",
				Host:     host,
				Method:   "QUERY",
			})

		case models.REDIS:
			host := ""
			if mock.Spec.Metadata != nil {
				host = mock.Spec.Metadata["host"]
			}
			meta.Endpoints = append(meta.Endpoints, models.EndpointInfo{
				Protocol: "Redis",
				Host:     host,
				Method:   "COMMAND",
			})

		case models.GRPC_EXPORT:
			if mock.Spec.GRPCReq != nil {
				// Extract method from :path pseudo header
				method := ""
				if mock.Spec.GRPCReq.Headers.PseudoHeaders != nil {
					method = mock.Spec.GRPCReq.Headers.PseudoHeaders[":path"]
				}
				meta.Endpoints = append(meta.Endpoints, models.EndpointInfo{
					Protocol: "gRPC",
					Path:     method,
					Method:   "RPC",
				})
			}

		case models.Mongo:
			host := ""
			if mock.Spec.Metadata != nil {
				host = mock.Spec.Metadata["host"]
			}
			meta.Endpoints = append(meta.Endpoints, models.EndpointInfo{
				Protocol: "Mongo",
				Host:     host,
				Method:   "QUERY",
			})

		case models.GENERIC:
			meta.Endpoints = append(meta.Endpoints, models.EndpointInfo{
				Protocol: "Generic",
				Method:   "CALL",
			})
		}
	}

	return meta
}

// extractServiceName extracts a service name from the command.
func extractServiceName(command string) string {
	// Parse command to extract service name
	// e.g., "go run ./user-service" -> "user-service"
	// e.g., "npm start" -> "npm-app"
	// e.g., "./bin/api-server" -> "api-server"
	// e.g., "python app.py" -> "app"
	// e.g., "java -jar myservice.jar" -> "myservice"

	parts := strings.Fields(command)
	if len(parts) == 0 {
		return "app"
	}

	// Try to find a meaningful name from the end of the command
	for i := len(parts) - 1; i >= 0; i-- {
		part := parts[i]

		// Skip flags
		if strings.HasPrefix(part, "-") {
			continue
		}

		// Skip common commands
		lowerPart := strings.ToLower(part)
		if lowerPart == "go" || lowerPart == "run" || lowerPart == "python" ||
			lowerPart == "node" || lowerPart == "npm" || lowerPart == "java" ||
			lowerPart == "start" || lowerPart == "sh" || lowerPart == "-c" {
			continue
		}

		// Extract base name
		base := filepath.Base(part)

		// Remove common extensions
		base = strings.TrimSuffix(base, ".go")
		base = strings.TrimSuffix(base, ".py")
		base = strings.TrimSuffix(base, ".js")
		base = strings.TrimSuffix(base, ".jar")
		base = strings.TrimSuffix(base, ".exe")

		// Remove leading ./ or ./
		base = strings.TrimPrefix(base, "./")
		base = strings.TrimPrefix(base, ".")

		if base != "" && base != "." && base != "main" {
			return sanitizeForFilename(base)
		}
	}

	// Fallback: use first non-flag argument
	for _, part := range parts {
		if !strings.HasPrefix(part, "-") {
			base := filepath.Base(part)
			if base != "" && base != "." {
				return sanitizeForFilename(base)
			}
		}
	}

	return "app"
}

// extractHostAndPath extracts host and path from a URL string.
func extractHostAndPath(urlStr string) (host, path string) {
	if urlStr == "" {
		return "", ""
	}

	// Try to parse as URL
	parsed, err := url.Parse(urlStr)
	if err == nil && parsed.Host != "" {
		return parsed.Host, parsed.Path
	}

	// If URL parsing fails, try to extract from the string directly
	// Handle cases like "http://localhost:8080/api/users"
	if strings.Contains(urlStr, "://") {
		parts := strings.SplitN(urlStr, "://", 2)
		if len(parts) == 2 {
			remainder := parts[1]
			slashIdx := strings.Index(remainder, "/")
			if slashIdx >= 0 {
				return remainder[:slashIdx], remainder[slashIdx:]
			}
			return remainder, "/"
		}
	}

	// Handle relative paths
	if strings.HasPrefix(urlStr, "/") {
		return "", urlStr
	}

	return "", urlStr
}

// sanitizeForFilename sanitizes a string for use in a filename.
func sanitizeForFilename(name string) string {
	// Convert to lowercase
	name = strings.ToLower(name)

	// Replace spaces and underscores with hyphens
	name = strings.ReplaceAll(name, " ", "-")
	name = strings.ReplaceAll(name, "_", "-")

	// Remove invalid characters (keep only alphanumeric and hyphens)
	reg := regexp.MustCompile(`[^a-z0-9-]`)
	name = reg.ReplaceAllString(name, "")

	// Replace multiple consecutive hyphens with single hyphen
	reg = regexp.MustCompile(`-+`)
	name = reg.ReplaceAllString(name, "-")

	// Trim leading and trailing hyphens
	name = strings.Trim(name, "-")

	// Enforce maximum length
	if len(name) > 50 {
		name = name[:50]
	}

	if name == "" {
		return "app"
	}

	return name
}
