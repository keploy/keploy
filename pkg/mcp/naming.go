// Package mcp provides contextual naming for mock files based on API descriptions and code context.
package mcp

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"net/url"
	"regexp"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
)

// NamingContext holds information used to generate contextual mock names
type NamingContext struct {
	APIDescription   string            // Description of the API being tested
	HTTPMethod       string            // HTTP method (GET, POST, etc.)
	Endpoint         string            // API endpoint path
	ServiceName      string            // Name of the external service
	OperationType    string            // Type of operation (query, mutation, etc.)
	MockKind         models.Kind       // Kind of mock (HTTP, Postgres, Redis, etc.)
	Metadata         map[string]string // Additional metadata
	Timestamp        time.Time         // When the mock was recorded
	RequestSignature string            // Hash of the request for uniqueness
}

// ContextualNamer generates human-readable, descriptive names for mock files
type ContextualNamer struct {
	prefixMap     map[models.Kind]string
	operationVerbs map[string]string
}

// NewContextualNamer creates a new contextual namer instance
func NewContextualNamer() *ContextualNamer {
	return &ContextualNamer{
		prefixMap: map[models.Kind]string{
			models.HTTP:        "http",
			models.GENERIC:     "generic",
			models.Postgres:    "postgres",
			models.Mongo:       "mongo",
			models.REDIS:       "redis",
			models.MySQL:       "mysql",
			models.GRPC_EXPORT: "grpc",
		},
		operationVerbs: map[string]string{
			"GET":    "fetch",
			"POST":   "create",
			"PUT":    "update",
			"PATCH":  "patch",
			"DELETE": "delete",
			"HEAD":   "check",
			"SELECT": "query",
			"INSERT": "insert",
		},
	}
}

// GenerateName creates a contextual name for a mock based on the provided context
func (cn *ContextualNamer) GenerateName(ctx NamingContext) string {
	parts := make([]string, 0, 5)

	// Add mock kind prefix
	if prefix, ok := cn.prefixMap[ctx.MockKind]; ok {
		parts = append(parts, prefix)
	} else {
		parts = append(parts, "mock")
	}

	// Add service name if available
	if ctx.ServiceName != "" {
		parts = append(parts, cn.sanitizeName(ctx.ServiceName))
	}

	// Add operation verb based on HTTP method or operation type
	verb := cn.getOperationVerb(ctx)
	if verb != "" {
		parts = append(parts, verb)
	}

	// Add resource name from endpoint
	resource := cn.extractResourceName(ctx.Endpoint)
	if resource != "" {
		parts = append(parts, resource)
	}

	// Add API description if available and no resource name
	if resource == "" && ctx.APIDescription != "" {
		desc := cn.sanitizeName(ctx.APIDescription)
		if len(desc) > 30 {
			desc = desc[:30]
		}
		parts = append(parts, desc)
	}

	// Add short hash for uniqueness
	hash := cn.generateShortHash(ctx)
	parts = append(parts, hash)

	return strings.Join(parts, "-")
}

// GenerateTestSetName creates a descriptive name for a test set
func (cn *ContextualNamer) GenerateTestSetName(apiDescription string, timestamp time.Time) string {
	if apiDescription == "" {
		return fmt.Sprintf("test-set-%s", timestamp.Format("20060102-150405"))
	}

	sanitized := cn.sanitizeName(apiDescription)
	if len(sanitized) > 40 {
		sanitized = sanitized[:40]
	}

	return fmt.Sprintf("%s-%s", sanitized, timestamp.Format("20060102"))
}

// AnalyzeEndpoint extracts semantic information from an API endpoint
func (cn *ContextualNamer) AnalyzeEndpoint(endpoint string) EndpointAnalysis {
	analysis := EndpointAnalysis{
		RawPath: endpoint,
	}

	// Parse the endpoint
	parsedURL, err := url.Parse(endpoint)
	if err != nil {
		return analysis
	}

	analysis.Host = parsedURL.Host
	path := parsedURL.Path

	// Extract path segments
	segments := strings.Split(strings.Trim(path, "/"), "/")
	analysis.Segments = segments

	// Identify resource name (usually the last non-ID segment)
	for i := len(segments) - 1; i >= 0; i-- {
		seg := segments[i]
		if !cn.isIDSegment(seg) && seg != "" {
			analysis.ResourceName = seg
			break
		}
	}

	// Identify version
	for _, seg := range segments {
		if cn.isVersionSegment(seg) {
			analysis.Version = seg
			break
		}
	}

	// Check for common patterns
	analysis.IsRESTful = cn.isRESTfulEndpoint(segments)
	analysis.IsGraphQL = strings.Contains(path, "graphql")

	return analysis
}

// EndpointAnalysis contains semantic analysis of an API endpoint
type EndpointAnalysis struct {
	RawPath      string
	Host         string
	Segments     []string
	ResourceName string
	Version      string
	IsRESTful    bool
	IsGraphQL    bool
}

// getOperationVerb returns a verb describing the operation
func (cn *ContextualNamer) getOperationVerb(ctx NamingContext) string {
	if ctx.HTTPMethod != "" {
		if verb, ok := cn.operationVerbs[strings.ToUpper(ctx.HTTPMethod)]; ok {
			return verb
		}
	}
	if ctx.OperationType != "" {
		if verb, ok := cn.operationVerbs[strings.ToUpper(ctx.OperationType)]; ok {
			return verb
		}
	}
	return ""
}

// extractResourceName extracts the main resource name from an endpoint
func (cn *ContextualNamer) extractResourceName(endpoint string) string {
	if endpoint == "" {
		return ""
	}

	// Remove query parameters
	if idx := strings.Index(endpoint, "?"); idx != -1 {
		endpoint = endpoint[:idx]
	}

	// Split path and find resource
	segments := strings.Split(strings.Trim(endpoint, "/"), "/")
	
	for i := len(segments) - 1; i >= 0; i-- {
		seg := segments[i]
		if !cn.isIDSegment(seg) && !cn.isVersionSegment(seg) && seg != "" {
			return cn.sanitizeName(seg)
		}
	}

	return ""
}

// isIDSegment checks if a path segment looks like an ID
func (cn *ContextualNamer) isIDSegment(segment string) bool {
	// Check for UUID pattern
	uuidPattern := regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)
	if uuidPattern.MatchString(segment) {
		return true
	}

	// Check for numeric ID
	numericPattern := regexp.MustCompile(`^\d+$`)
	if numericPattern.MatchString(segment) {
		return true
	}

	// Check for MongoDB ObjectID pattern
	objectIDPattern := regexp.MustCompile(`^[0-9a-fA-F]{24}$`)
	if objectIDPattern.MatchString(segment) {
		return true
	}

	return false
}

// isVersionSegment checks if a segment is an API version
func (cn *ContextualNamer) isVersionSegment(segment string) bool {
	versionPattern := regexp.MustCompile(`^v\d+(\.\d+)?$`)
	return versionPattern.MatchString(strings.ToLower(segment))
}

// isRESTfulEndpoint checks if the endpoint follows REST conventions
func (cn *ContextualNamer) isRESTfulEndpoint(segments []string) bool {
	if len(segments) == 0 {
		return false
	}

	// Common REST patterns: /api/v1/resources, /v1/resources/{id}
	hasVersion := false
	hasResource := false

	for _, seg := range segments {
		if cn.isVersionSegment(seg) || seg == "api" {
			hasVersion = true
		}
		if !cn.isIDSegment(seg) && !cn.isVersionSegment(seg) && seg != "api" && seg != "" {
			hasResource = true
		}
	}

	return hasVersion || hasResource
}

// sanitizeName converts a string to a valid file name component
func (cn *ContextualNamer) sanitizeName(name string) string {
	// Convert to lowercase
	name = strings.ToLower(name)

	// Replace spaces and special characters with hyphens
	reg := regexp.MustCompile(`[^a-z0-9]+`)
	name = reg.ReplaceAllString(name, "-")

	// Remove leading/trailing hyphens
	name = strings.Trim(name, "-")

	return name
}

// generateShortHash creates a short hash for uniqueness
func (cn *ContextualNamer) generateShortHash(ctx NamingContext) string {
	data := fmt.Sprintf("%s|%s|%s|%s|%d",
		ctx.HTTPMethod,
		ctx.Endpoint,
		ctx.ServiceName,
		ctx.RequestSignature,
		ctx.Timestamp.UnixNano(),
	)

	hash := sha256.Sum256([]byte(data))
	return hex.EncodeToString(hash[:])[:8]
}

// GenerateMockNameFromHTTP creates a contextual name from HTTP mock data
func (cn *ContextualNamer) GenerateMockNameFromHTTP(method, endpoint, serviceName, apiDesc string) string {
	ctx := NamingContext{
		APIDescription: apiDesc,
		HTTPMethod:     method,
		Endpoint:       endpoint,
		ServiceName:    serviceName,
		MockKind:       models.HTTP,
		Timestamp:      time.Now(),
	}
	return cn.GenerateName(ctx)
}

// GenerateMockNameFromDB creates a contextual name from database mock data
func (cn *ContextualNamer) GenerateMockNameFromDB(kind models.Kind, operation, tableName, dbName string) string {
	ctx := NamingContext{
		OperationType: operation,
		ServiceName:   dbName,
		MockKind:      kind,
		Timestamp:     time.Now(),
	}

	if tableName != "" {
		ctx.Endpoint = "/" + tableName
	}

	return cn.GenerateName(ctx)
}

// GenerateMockNameFromGeneric creates a contextual name for generic mocks
func (cn *ContextualNamer) GenerateMockNameFromGeneric(serviceName, description string) string {
	ctx := NamingContext{
		APIDescription: description,
		ServiceName:    serviceName,
		MockKind:       models.GENERIC,
		Timestamp:      time.Now(),
	}
	return cn.GenerateName(ctx)
}
