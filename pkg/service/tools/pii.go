// Package tools provides PII (Personally Identifiable Information) detection
// for the Keploy sanitize command. This module extends the existing gitleaks-based
// secret detection with advanced PII validation including Luhn algorithm for
// credit cards and IP address classification.
//
// Implements issue #3400: Automatic PII Detection and Masking
package tools

import (
	"net"
	"regexp"
	"strings"
	"sync"
)

// PIIType represents the category of PII detected
type PIIType string

const (
	PIIEmail      PIIType = "Email"
	PIIPhone      PIIType = "Phone"
	PIIIPv4       PIIType = "Ipv4"
	PIIIPv6       PIIType = "Ipv6"
	PIICreditCard PIIType = "CreditCard"
	PIISSN        PIIType = "Ssn"
	PIIPassport   PIIType = "Passport"
	PIIDOB        PIIType = "Dob"
	PIIName       PIIType = "Name"
	PIIZipCode    PIIType = "ZipCode"
	PIICustom     PIIType = "Custom"
)

// PIIFinding represents a detected PII instance
type PIIFinding struct {
	Type       PIIType
	Value      string
	StartIndex int
	EndIndex   int
	FieldName  string
	Confidence float64 // 0.0 - 1.0, higher is more confident
	Validated  bool    // True if validated (e.g., Luhn check passed)
}

// PIIConfig holds configuration for PII detection
type PIIConfig struct {
	// EnabledCategories specifies which PII types to detect
	// If empty or contains "all", all types are enabled
	EnabledCategories []string

	// SensitiveFields are custom field names to always treat as PII
	SensitiveFields []string

	// ExcludeFields are field names to never treat as PII
	ExcludeFields []string

	// ValidateCreditCards enables Luhn algorithm validation
	ValidateCreditCards bool

	// ExcludePrivateIPs excludes private/local IP addresses
	ExcludePrivateIPs bool
}

// PIIDetector provides configurable PII detection with validation
type PIIDetector struct {
	patterns        map[PIIType]*regexp.Regexp
	sensitiveFields map[string]struct{}
	excludeFields   map[string]struct{}
	enabledTypes    map[PIIType]struct{}
	config          PIIConfig
	mu              sync.RWMutex
}

// Default regex patterns for PII detection
// These are used for standalone detection outside of gitleaks
var defaultPIIPatterns = map[PIIType]string{
	PIIEmail:      `[a-zA-Z0-9._%+\-]+@[a-zA-Z0-9.\-]+\.[a-zA-Z]{2,}`,
	PIIPhone:      `(?:\+?1[-.\s]?)?\(?[2-9]\d{2}\)?[-.\s]?\d{3}[-.\s]?\d{4}`,
	PIIIPv4:       `\b(?:(?:25[0-5]|2[0-4]\d|[01]?\d\d?)\.){3}(?:25[0-5]|2[0-4]\d|[01]?\d\d?)\b`,
	PIIIPv6:       `(?i)(?:[0-9a-f]{1,4}:){7}[0-9a-f]{1,4}`,
	PIICreditCard: `\b(?:4[0-9]{12}(?:[0-9]{3})?|5[1-5][0-9]{14}|3[47][0-9]{13}|6(?:011|5[0-9]{2})[0-9]{12})\b`,
	PIISSN:        `\b[0-9]{3}[-.\s]?[0-9]{2}[-.\s]?[0-9]{4}\b`,
}

// NewPIIDetector creates a new PII detector with the given configuration
func NewPIIDetector(cfg PIIConfig) *PIIDetector {
	d := &PIIDetector{
		patterns:        make(map[PIIType]*regexp.Regexp),
		sensitiveFields: make(map[string]struct{}),
		excludeFields:   make(map[string]struct{}),
		enabledTypes:    make(map[PIIType]struct{}),
		config:          cfg,
	}

	// Compile regex patterns
	for piiType, pattern := range defaultPIIPatterns {
		if re, err := regexp.Compile(pattern); err == nil {
			d.patterns[piiType] = re
		}
	}

	// Set up enabled types
	if len(cfg.EnabledCategories) == 0 || containsIgnoreCase(cfg.EnabledCategories, "all") {
		// Enable all types
		for piiType := range defaultPIIPatterns {
			d.enabledTypes[piiType] = struct{}{}
		}
	} else {
		for _, cat := range cfg.EnabledCategories {
			piiType := categoryToPIIType(cat)
			if piiType != "" {
				d.enabledTypes[piiType] = struct{}{}
			}
		}
	}

	// Set up sensitive fields (case-insensitive)
	for _, field := range cfg.SensitiveFields {
		d.sensitiveFields[strings.ToLower(field)] = struct{}{}
	}

	// Set up exclude fields (case-insensitive)
	for _, field := range cfg.ExcludeFields {
		d.excludeFields[strings.ToLower(field)] = struct{}{}
	}

	return d
}

// NewDefaultPIIDetector creates a detector with default configuration
func NewDefaultPIIDetector() *PIIDetector {
	return NewPIIDetector(PIIConfig{
		ValidateCreditCards: true,
		ExcludePrivateIPs:   true,
	})
}

// Detect scans text for PII and returns all findings
func (d *PIIDetector) Detect(text string) []PIIFinding {
	d.mu.RLock()
	defer d.mu.RUnlock()

	var findings []PIIFinding
	seen := make(map[string]struct{}) // Deduplicate by value

	for piiType, pattern := range d.patterns {
		// Skip disabled types
		if _, enabled := d.enabledTypes[piiType]; !enabled {
			continue
		}

		matches := pattern.FindAllStringIndex(text, -1)
		for _, match := range matches {
			value := text[match[0]:match[1]]

			// Skip if already found
			if _, exists := seen[value]; exists {
				continue
			}

			// Validate and filter
			finding := d.validateFinding(piiType, value, match[0], match[1])
			if finding != nil {
				findings = append(findings, *finding)
				seen[value] = struct{}{}
			}
		}
	}

	return findings
}

// DetectInField detects PII in a value with field context
func (d *PIIDetector) DetectInField(fieldName, value string) []PIIFinding {
	d.mu.RLock()
	defer d.mu.RUnlock()

	// Check if field is excluded
	if _, excluded := d.excludeFields[strings.ToLower(fieldName)]; excluded {
		return nil
	}

	// Check if field is marked as sensitive
	if _, sensitive := d.sensitiveFields[strings.ToLower(fieldName)]; sensitive {
		return []PIIFinding{{
			Type:       PIICustom,
			Value:      value,
			FieldName:  fieldName,
			Confidence: 1.0,
			Validated:  true,
		}}
	}

	// Run normal detection
	findings := d.Detect(value)
	for i := range findings {
		findings[i].FieldName = fieldName
	}
	return findings
}

// validateFinding validates a finding and returns it if valid, nil otherwise
func (d *PIIDetector) validateFinding(piiType PIIType, value string, start, end int) *PIIFinding {
	confidence := 0.8 // Default confidence
	validated := false

	switch piiType {
	case PIICreditCard:
		if d.config.ValidateCreditCards {
			if !ValidateLuhn(value) {
				return nil // Failed Luhn check
			}
			validated = true
			confidence = 0.95
		}

	case PIIIPv4:
		if d.config.ExcludePrivateIPs && IsPrivateIP(value) {
			return nil // Skip private IPs
		}
		validated = true
		confidence = 0.9

	case PIIIPv6:
		if d.config.ExcludePrivateIPs && IsPrivateIP(value) {
			return nil
		}
		validated = true
		confidence = 0.9

	case PIIEmail:
		// Basic email validation
		if strings.Count(value, "@") == 1 && strings.Contains(value, ".") {
			validated = true
			confidence = 0.95
		}

	case PIIPhone:
		// Clean and validate phone
		cleaned := cleanPhoneNumber(value)
		if len(cleaned) >= 10 && len(cleaned) <= 15 {
			validated = true
			confidence = 0.85
		}

	case PIISSN:
		cleaned := cleanSSN(value)
		if len(cleaned) == 9 {
			// Validate SSN per IRS rules:
			// - Cannot start with 000, 666, or 9xx
			// - Middle 2 digits cannot be 00
			// - Last 4 digits cannot be 0000
			area := cleaned[0:3]
			group := cleaned[3:5]
			serial := cleaned[5:9]
			
			if area == "000" || area == "666" || area[0] == '9' {
				return nil // Invalid SSN
			}
			if group == "00" {
				return nil // Invalid SSN
			}
			if serial == "0000" {
				return nil // Invalid SSN
			}
			validated = true
			confidence = 0.9
		}
	}

	return &PIIFinding{
		Type:       piiType,
		Value:      value,
		StartIndex: start,
		EndIndex:   end,
		Confidence: confidence,
		Validated:  validated,
	}
}

// IsSensitiveField checks if a field name indicates sensitive data
func (d *PIIDetector) IsSensitiveField(fieldName string) bool {
	d.mu.RLock()
	defer d.mu.RUnlock()

	lower := strings.ToLower(fieldName)

	// Check custom sensitive fields
	if _, exists := d.sensitiveFields[lower]; exists {
		return true
	}

	// Check common sensitive field patterns
	sensitivePatterns := []string{
		"password", "passwd", "pwd", "secret", "token", "key", "auth",
		"credential", "ssn", "social", "credit", "card", "cvv", "cvc",
		"pin", "dob", "birth", "passport", "license", "tax", "salary",
		"income", "bank", "account", "routing", "swift", "iban",
	}

	for _, pattern := range sensitivePatterns {
		if strings.Contains(lower, pattern) {
			return true
		}
	}

	return false
}

// ValidateLuhn validates a number string using the Luhn algorithm
// Used for credit card validation
func ValidateLuhn(number string) bool {
	// Remove non-digit characters
	var digits []int
	for _, r := range number {
		if r >= '0' && r <= '9' {
			digits = append(digits, int(r-'0'))
		}
	}

	if len(digits) < 13 || len(digits) > 19 {
		return false
	}

	// Luhn algorithm
	sum := 0
	alt := false
	for i := len(digits) - 1; i >= 0; i-- {
		d := digits[i]
		if alt {
			d *= 2
			if d > 9 {
				d -= 9
			}
		}
		sum += d
		alt = !alt
	}

	return sum%10 == 0
}

// IsPrivateIP checks if an IP address is private/local/reserved
func IsPrivateIP(ipStr string) bool {
	ip := net.ParseIP(ipStr)
	if ip == nil {
		return false
	}

	// Check for loopback
	if ip.IsLoopback() {
		return true
	}

	// Check for private ranges
	if ip.IsPrivate() {
		return true
	}

	// Check for link-local
	if ip.IsLinkLocalUnicast() || ip.IsLinkLocalMulticast() {
		return true
	}

	// Check for unspecified (0.0.0.0 or ::)
	if ip.IsUnspecified() {
		return true
	}

	return false
}

// GetPIITypeFromRuleID maps a gitleaks rule ID to a PIIType
func GetPIITypeFromRuleID(ruleID string) PIIType {
	lower := strings.ToLower(ruleID)

	switch {
	case strings.Contains(lower, "email"):
		return PIIEmail
	case strings.Contains(lower, "phone"):
		return PIIPhone
	case strings.Contains(lower, "ipv4"):
		return PIIIPv4
	case strings.Contains(lower, "ipv6"):
		return PIIIPv6
	case strings.Contains(lower, "credit-card") || strings.Contains(lower, "creditcard"):
		return PIICreditCard
	case strings.Contains(lower, "ssn"):
		return PIISSN
	case strings.Contains(lower, "passport"):
		return PIIPassport
	case strings.Contains(lower, "dob") || strings.Contains(lower, "birth"):
		return PIIDOB
	case strings.Contains(lower, "name"):
		return PIIName
	case strings.Contains(lower, "zip"):
		return PIIZipCode
	default:
		return ""
	}
}

// IsPIIRule checks if a gitleaks rule ID is a PII detection rule
func IsPIIRule(ruleID string) bool {
	return strings.HasPrefix(strings.ToLower(ruleID), "pii-")
}

// Helper functions

func containsIgnoreCase(slice []string, target string) bool {
	lower := strings.ToLower(target)
	for _, s := range slice {
		if strings.ToLower(s) == lower {
			return true
		}
	}
	return false
}

func categoryToPIIType(category string) PIIType {
	switch strings.ToLower(category) {
	case "email":
		return PIIEmail
	case "phone":
		return PIIPhone
	case "ip", "ipv4":
		return PIIIPv4
	case "ipv6":
		return PIIIPv6
	case "credit-card", "creditcard", "cc":
		return PIICreditCard
	case "ssn":
		return PIISSN
	case "passport":
		return PIIPassport
	case "dob", "birthday":
		return PIIDOB
	case "name":
		return PIIName
	case "zip", "zipcode":
		return PIIZipCode
	default:
		return ""
	}
}

func cleanPhoneNumber(phone string) string {
	var cleaned strings.Builder
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			cleaned.WriteRune(r)
		}
	}
	return cleaned.String()
}

func cleanSSN(ssn string) string {
	var cleaned strings.Builder
	for _, r := range ssn {
		if r >= '0' && r <= '9' {
			cleaned.WriteRune(r)
		}
	}
	return cleaned.String()
}
