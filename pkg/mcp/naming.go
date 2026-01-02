package mcp

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	sdkmcp "github.com/modelcontextprotocol/go-sdk/mcp"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

// generateContextualName uses the LLM callback to generate a contextual name for the mock file.
func (s *Server) generateContextualName(ctx context.Context, meta *models.MockMetadata) (string, error) {
	session := s.getActiveSession()
	if session == nil {
		return "", errors.New("no active session for LLM callback")
	}

	// Build summary from metadata
	summary := s.buildMetadataSummary(meta)

	s.logger.Debug("Requesting contextual name from LLM",
		zap.String("serviceName", summary.ServiceName),
		zap.Strings("protocols", summary.Protocols),
		zap.String("endpoints", summary.EndpointsSummary),
	)

	// Create message request for LLM
	prompt := fmt.Sprintf(`Generate a short, descriptive filename (without extension) for a mock file based on this summary:

Service: %s
Protocols: %s
Endpoints: %s

Requirements:
- Use kebab-case (e.g., user-service-postgres-auth)
- Maximum 50 characters
- Include key identifiers (service name, main protocol, key endpoint/action)
- Make it descriptive enough to understand what the mock contains
- Examples: user-service-postgres-auth, payment-api-stripe-checkout, order-service-redis-cache

Respond with ONLY the filename, nothing else.`,
		summary.ServiceName,
		strings.Join(summary.Protocols, ", "),
		summary.EndpointsSummary,
	)

	result, err := session.CreateMessage(ctx, &sdkmcp.CreateMessageParams{
		MaxTokens: 50,
		Messages: []*sdkmcp.SamplingMessage{
			{
				Role: "user",
				Content: &sdkmcp.TextContent{
					Text: prompt,
				},
			},
		},
		ModelPreferences: &sdkmcp.ModelPreferences{
			SpeedPriority: 1.0, // Prefer fast responses for naming
		},
		SystemPrompt: "You are a helpful assistant that generates concise, descriptive filenames for mock files. Always respond with just the filename in kebab-case, no explanation.",
	})
	if err != nil {
		return "", fmt.Errorf("LLM callback failed: %w", err)
	}

	// Extract text from response
	if textContent, ok := result.Content.(*sdkmcp.TextContent); ok {
		name := s.sanitizeFilename(textContent.Text)
		s.logger.Debug("Generated contextual name from LLM", zap.String("name", name))
		return name, nil
	}

	return "", errors.New("unexpected LLM response format")
}

// metadataSummary contains a summary of mock metadata for LLM prompt.
type metadataSummary struct {
	ServiceName      string
	Protocols        []string
	EndpointsSummary string
}

// buildMetadataSummary builds a summary from mock metadata for the LLM prompt.
func (s *Server) buildMetadataSummary(meta *models.MockMetadata) metadataSummary {
	if meta == nil {
		return metadataSummary{ServiceName: "unknown"}
	}

	// Build endpoint summary (limit to 5 most significant)
	var endpoints []string
	seen := make(map[string]bool)
	for _, ep := range meta.Endpoints {
		if len(endpoints) >= 5 {
			break
		}

		var summary string
		if ep.Method != "" && ep.Path != "" {
			summary = fmt.Sprintf("%s %s", ep.Method, ep.Path)
		} else if ep.Path != "" {
			summary = ep.Path
		} else if ep.Host != "" {
			summary = ep.Host
		} else {
			summary = ep.Protocol
		}

		// Deduplicate
		if !seen[summary] {
			seen[summary] = true
			endpoints = append(endpoints, summary)
		}
	}

	endpointsSummary := "none"
	if len(endpoints) > 0 {
		endpointsSummary = strings.Join(endpoints, ", ")
	}

	return metadataSummary{
		ServiceName:      meta.ServiceName,
		Protocols:        meta.Protocols,
		EndpointsSummary: endpointsSummary,
	}
}

// fallbackName generates a deterministic fallback name when LLM callback fails.
func (s *Server) fallbackName(meta *models.MockMetadata) string {
	var parts []string

	// Add service name
	serviceName := "mock"
	if meta != nil && meta.ServiceName != "" {
		serviceName = meta.ServiceName
	}
	parts = append(parts, serviceName)

	// Add primary protocol if available
	if meta != nil && len(meta.Protocols) > 0 {
		parts = append(parts, strings.ToLower(meta.Protocols[0]))
	}

	// Add timestamp
	if meta != nil {
		timestamp := meta.Timestamp.Format("20060102-150405")
		parts = append(parts, timestamp)
	}

	name := strings.Join(parts, "-")
	return s.sanitizeFilename(name)
}

// sanitizeFilename sanitizes a string for use as a filename.
func (s *Server) sanitizeFilename(name string) string {
	// Trim whitespace and newlines
	name = strings.TrimSpace(name)

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
		// Ensure we don't cut in the middle of a word
		if lastHyphen := strings.LastIndex(name, "-"); lastHyphen > 30 {
			name = name[:lastHyphen]
		}
	}

	if name == "" {
		name = "mock"
	}

	return name
}

// renameMockFile renames the mock file with the contextual name.
func (s *Server) renameMockFile(oldPath, newName string) string {
	dir := filepath.Dir(oldPath)
	ext := filepath.Ext(oldPath)

	// Handle directory case (mock directory instead of file)
	if ext == "" {
		// oldPath is a directory, rename the directory
		parentDir := filepath.Dir(oldPath)
		newPath := filepath.Join(parentDir, newName)

		if err := os.Rename(oldPath, newPath); err != nil {
			s.logger.Warn("Failed to rename mock directory",
				zap.String("oldPath", oldPath),
				zap.String("newPath", newPath),
				zap.Error(err),
			)
			return oldPath
		}

		s.logger.Info("Renamed mock directory",
			zap.String("oldPath", oldPath),
			zap.String("newPath", newPath),
		)
		return newPath
	}

	// Handle file case
	newPath := filepath.Join(dir, newName+ext)

	if err := os.Rename(oldPath, newPath); err != nil {
		s.logger.Warn("Failed to rename mock file",
			zap.String("oldPath", oldPath),
			zap.String("newPath", newPath),
			zap.Error(err),
		)
		return oldPath
	}

	s.logger.Info("Renamed mock file",
		zap.String("oldPath", oldPath),
		zap.String("newPath", newPath),
	)
	return newPath
}
