package secure

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"strings"
	"syscall"
	"time"

	"go.keploy.io/server/v2/config"
	"go.keploy.io/server/v2/pkg/service/testsuite"
	"go.uber.org/zap"
)

type SecurityCheck struct {
	ID          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description"`
	Severity    string `json:"severity"`        // "CRITICAL", "HIGH", "MEDIUM", "LOW"
	Type        string `json:"type"`            // "header", "body", "cookie", "url"
	Target      string `json:"target"`          // "request", "response" - where to apply the check
	Key         string `json:"key"`             // Header name, JSON path, regex pattern, etc.
	Value       string `json:"value,omitempty"` // Expected value or pattern to match
	Operation   string `json:"operation"`       // "exists", "equals", "contains", "regex", "not_exists"
	Status      string `json:"status"`          // "enabled", "disabled"
}

type SecurityResult struct {
	CheckID        string `json:"check_id"`
	CheckName      string `json:"check_name"`
	Status         string `json:"status"` // "passed", "failed", "warning"
	Severity       string `json:"severity"`
	Description    string `json:"description"`
	Details        string `json:"details,omitempty"`
	Recommendation string `json:"recommendation,omitempty"`
	// Step information
	StepName   string `json:"step_name"`
	StepMethod string `json:"step_method"`
	StepURL    string `json:"step_url"`
	StatusCode int    `json:"status_code,omitempty"`
	Target     string `json:"target"` // "request" or "response" - where the check was applied
}

type StepSecurityResults struct {
	StepName   string           `json:"step_name"`
	StepMethod string           `json:"step_method"`
	StepURL    string           `json:"step_url"`
	Results    []SecurityResult `json:"results"`
	Passed     int              `json:"passed"`
	Failed     int              `json:"failed"`
	Warnings   int              `json:"warnings"`
}

type SecurityReport struct {
	TestSuite   string                `json:"test_suite"`
	Timestamp   string                `json:"timestamp"`
	TotalChecks int                   `json:"total_checks"`
	Passed      int                   `json:"passed"`
	Failed      int                   `json:"failed"`
	Warnings    int                   `json:"warnings"`
	Steps       []StepSecurityResults `json:"steps"`
	Summary     map[string]int        `json:"summary"` // severity -> count
}

type StepRequest struct {
	Method  string
	Headers http.Header
	Body    string
}

type StepResponse struct {
	StatusCode int
	Headers    http.Header
	Body       string
}

type Step struct {
	Endpoint     string
	StepName     string
	StepRequest  StepRequest
	StepResponse StepResponse
}

// Built-in security checks
// Only 10 Checks, commneted checks are for code testing purpose.
var BuiltInSecurityChecks = []SecurityCheck{
	{
		ID:          "https-enforcement",
		Name:        "HTTPS Enforcement",
		Description: "Check if Strict-Transport-Security header is present",
		Severity:    "HIGH",
		Type:        "header",
		Target:      "response",
		Key:         "Strict-Transport-Security",
		Operation:   "exists",
		Status:      "enabled",
	},
	{
		ID:          "x-content-type-options",
		Name:        "X-Content-Type-Options",
		Description: "Check for X-Content-Type-Options nosniff header",
		Severity:    "HIGH",
		Type:        "header",
		Target:      "response",
		Key:         "X-Content-Type-Options",
		Value:       "nosniff",
		Operation:   "equals",
		Status:      "enabled",
	},
	{
		ID:          "x-frame-options",
		Name:        "X-Frame-Options",
		Description: "Check for X-Frame-Options header to prevent clickjacking",
		Severity:    "HIGH",
		Type:        "header",
		Target:      "response",
		Key:         "X-Frame-Options",
		Operation:   "exists",
		Status:      "enabled",
	},
	// {
	// 	ID:          "content-security-policy",
	// 	Name:        "Content Security Policy",
	// 	Description: "Check for Content-Security-Policy header",
	// 	Severity:    "HIGH",
	// 	Type:        "header",
	// 	Target:      "response",
	// 	Key:         "Content-Security-Policy",
	// 	Operation:   "exists",
	// 	Status:      "enabled",
	// },
	// {
	// 	ID:          "email-exposure",
	// 	Name:        "Email Exposure",
	// 	Description: "Check for email addresses in response body",
	// 	Severity:    "CRITICAL",
	// 	Type:        "body",
	// 	Target:      "response",
	// 	Key:         ".+@.+\\..+",
	// 	Operation:   "regex",
	// 	Status:      "enabled",
	// },
	// {
	// 	ID:          "credit-card-exposure",
	// 	Name:        "Credit Card Exposure",
	// 	Description: "Check for credit card numbers in response body",
	// 	Severity:    "CRITICAL",
	// 	Type:        "body",
	// 	Target:      "response",
	// 	Key:         "\\b(?:\\d[ -]*?){13,16}\\b",
	// 	Operation:   "regex",
	// 	Status:      "enabled",
	// },
	// {
	// 	ID:          "api-key-exposure",
	// 	Name:        "API Key Exposure",
	// 	Description: "Check for API keys in response body",
	// 	Severity:    "CRITICAL",
	// 	Type:        "body",
	// 	Target:      "response",
	// 	Key:         "sk_(live|test)_[a-zA-Z0-9]{24}",
	// 	Operation:   "regex",
	// 	Status:      "enabled",
	// },
	{
		ID:          "secure-cookie",
		Name:        "Secure Cookie",
		Description: "Check if cookies have Secure flag",
		Severity:    "HIGH",
		Type:        "cookie",
		Target:      "response",
		Key:         "Secure",
		Operation:   "exists",
		Status:      "enabled",
	},
	{
		ID:          "httponly-cookie",
		Name:        "HttpOnly Cookie",
		Description: "Check if cookies have HttpOnly flag",
		Severity:    "HIGH",
		Type:        "cookie",
		Target:      "response",
		Key:         "HttpOnly",
		Operation:   "exists",
		Status:      "enabled",
	},
	{
		ID:          "samesite-cookie",
		Name:        "SameSite Cookie",
		Description: "Check if cookies have SameSite attribute",
		Severity:    "MEDIUM",
		Type:        "cookie",
		Target:      "response",
		Key:         "SameSite",
		Operation:   "exists",
		Status:      "enabled",
	},
	{
		ID:          "cors-misconfiguration",
		Name:        "CORS Misconfiguration",
		Description: "Check for overly permissive CORS policy",
		Severity:    "MEDIUM",
		Type:        "header",
		Target:      "response",
		Key:         "Access-Control-Allow-Origin",
		Value:       "*",
		Operation:   "not_equals",
		Status:      "enabled",
	},
	// {
	// 	ID:          "java-stack-trace",
	// 	Name:        "Java Stack Trace",
	// 	Description: "Check for Java stack traces in response",
	// 	Severity:    "MEDIUM",
	// 	Type:        "body",
	// 	Target:      "response",
	// 	Key:         "java\\.lang\\.Exception|at com\\.|at java\\.",
	// 	Operation:   "regex",
	// 	Status:      "enabled",
	// },
	// {
	// 	ID:          "python-stack-trace",
	// 	Name:        "Python Stack Trace",
	// 	Description: "Check for Python stack traces in response",
	// 	Severity:    "MEDIUM",
	// 	Type:        "body",
	// 	Target:      "response",
	// 	Key:         "Traceback \\(most recent call last\\)",
	// 	Operation:   "regex",
	// 	Status:      "enabled",
	// },
	// {
	// 	ID:          "nodejs-error",
	// 	Name:        "Node.js Error",
	// 	Description: "Check for Node.js errors in response",
	// 	Severity:    "MEDIUM",
	// 	Type:        "body",
	// 	Target:      "response",
	// 	Key:         "Error: ENOENT|TypeError:|ReferenceError:",
	// 	Operation:   "regex",
	// 	Status:      "enabled",
	// },
	{
		ID:          "server-version-leak",
		Name:        "Server Version Leak",
		Description: "Check for server version information in headers",
		Severity:    "MEDIUM",
		Type:        "header",
		Target:      "response",
		Key:         "Server",
		Operation:   "not_exists",
		Status:      "enabled",
	},
	{
		ID:          "x-powered-by-leak",
		Name:        "X-Powered-By Leak",
		Description: "Check for X-Powered-By header disclosure",
		Severity:    "MEDIUM",
		Type:        "header",
		Target:      "response",
		Key:         "X-Powered-By",
		Operation:   "not_exists",
		Status:      "enabled",
	},
	// Request-based security checks
	{
		ID:          "authorization-header-present",
		Name:        "Authorization Header Present",
		Description: "Check if Authorization header is present in request",
		Severity:    "HIGH",
		Type:        "header",
		Target:      "request",
		Key:         "Authorization",
		Operation:   "exists",
		Status:      "enabled",
	},
	// {
	// 	ID:          "api-key-in-request-body",
	// 	Name:        "API Key in Request Body",
	// 	Description: "Check for API keys in request body",
	// 	Severity:    "CRITICAL",
	// 	Type:        "body",
	// 	Target:      "request",
	// 	Key:         "api[_-]?key|apikey|access[_-]?token|secret[_-]?key",
	// 	Operation:   "regex",
	// 	Status:      "enabled",
	// },
	// {
	// 	ID:          "password-in-request-body",
	// 	Name:        "Password in Request Body",
	// 	Description: "Check for passwords in request body",
	// 	Severity:    "CRITICAL",
	// 	Type:        "body",
	// 	Target:      "request",
	// 	Key:         "\"password\"\\s*:\\s*\"[^\"]+\"",
	// 	Operation:   "regex",
	// 	Status:      "enabled",
	// },
	// {
	// 	ID:          "sql-injection-in-request",
	// 	Name:        "SQL Injection in Request",
	// 	Description: "Check for potential SQL injection patterns in request body",
	// 	Severity:    "HIGH",
	// 	Type:        "body",
	// 	Target:      "request",
	// 	Key:         "('|(\\-\\-)|;|\\||\\*|(%27)|(%2D%2D)|(%7C)|(%2A))",
	// 	Operation:   "regex",
	// 	Status:      "enabled",
	// },
}

type SecurityChecker struct {
	config    *config.Config
	logger    *zap.Logger
	testsuite *testsuite.TestSuite
	ruleset   string
}

func NewSecurityChecker(cfg *config.Config, logger *zap.Logger) (*SecurityChecker, error) {
	testsuitePath := filepath.Join(cfg.TestSuite.TSPath, cfg.TestSuite.TSFile)
	logger.Info("Parsing TestSuite File", zap.String("path", testsuitePath))

	testsuite, err := testsuite.TSParser(testsuitePath)
	if err != nil {
		logger.Error("Failed to parse TestSuite file", zap.Error(err))
		return nil, fmt.Errorf("failed to parse TestSuite file: %w", err)
	}

	return &SecurityChecker{
		config:    cfg,
		logger:    logger,
		testsuite: &testsuite,
		ruleset:   testsuite.Spec.Security.Ruleset,
	}, nil
}

func (s *SecurityChecker) Start(ctx context.Context) (*SecurityReport, error) {
	if s.ruleset == "" {
		s.ruleset = "basic" // Default to basic ruleset if not specified
	}
	// CLI override
	if ruleSetValue := ctx.Value("rule-set"); ruleSetValue != nil {
		if ruleSetStr, ok := ruleSetValue.(string); ok && ruleSetStr != "basic" {
			s.ruleset = ruleSetStr
		}
	}

	// Create and execute TestSuite to get step data
	tsExecutor, err := testsuite.NewTSExecutor(s.config, s.logger, true)
	if err != nil {
		return nil, fmt.Errorf("failed to create testsuite executor: %w", err)
	}

	tsExecutor.Testsuite = s.testsuite

	// Execute the testsuite
	executionReport, err := tsExecutor.Execute(ctx, nil)
	if err != nil {
		return nil, fmt.Errorf("failed to execute testsuite: %w", err)
	}

	// Convert execution results to Step structures for security analysis
	steps := s.convertExecutionReportToSteps(executionReport, tsExecutor)

	// Run security checks on the steps
	securityReport := s.runSecurityChecks(ctx, steps)

	// Print the security report
	s.printSecurityReport(securityReport)

	return securityReport, nil
}

func (s *SecurityChecker) AddCustomCheck(ctx context.Context) error {
	// Set up signal handling for graceful exit on Ctrl+C
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signalChan)

	// Create a context that can be cancelled by signals
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Handle signals in a goroutine
	go func() {
		select {
		case <-signalChan:
			fmt.Println("\n\n⚠️ Operation cancelled by user.")
			cancel()
		case <-ctx.Done():
			return
		}
	}()

	fmt.Println("\n🔒 Add Custom Security Check")
	fmt.Println("=" + strings.Repeat("=", 50))

	var check SecurityCheck

	// Get check ID with validation loop
	for {
		input, err := readInputWithCancel(ctx, "Enter check ID (unique identifier): ")
		if err != nil {
			return err
		}

		if input != "" {
			check.ID = input
			break
		}
		fmt.Println("❌ Error: Check ID is required. Please try again.")
	}

	// Get check name with validation loop
	for {
		input, err := readInputWithCancel(ctx, "Enter check name: ")
		if err != nil {
			return err
		}

		if input != "" {
			check.Name = input
			break
		}
		fmt.Println("❌ Error: Check name is required. Please try again.")
	}

	// Get check description with validation loop
	for {
		input, err := readInputWithCancel(ctx, "Enter check description: ")
		if err != nil {
			return err
		}

		if input != "" {
			check.Description = input
			break
		}
		fmt.Println("❌ Error: Check description is required. Please try again.")
	}

	// Get severity with validation loop
	for {
		input, err := readInputWithCancel(ctx, "Enter severity (CRITICAL/HIGH/MEDIUM/LOW): ")
		if err != nil {
			return err
		}

		severity := strings.ToUpper(input)
		if severity == "CRITICAL" || severity == "HIGH" || severity == "MEDIUM" || severity == "LOW" {
			check.Severity = severity
			break
		}
		fmt.Println("❌ Error: Invalid severity. Must be one of: CRITICAL, HIGH, MEDIUM, LOW. Please try again.")
	}

	// Get check type with validation loop
	for {
		input, err := readInputWithCancel(ctx, "Enter check type (header/body/cookie/url): ")
		if err != nil {
			return err
		}

		checkType := strings.ToLower(input)
		if checkType == "header" || checkType == "body" || checkType == "cookie" || checkType == "url" {
			check.Type = checkType
			break
		}
		fmt.Println("❌ Error: Invalid type. Must be one of: header, body, cookie, url. Please try again.")
	}

	// Get target with validation loop
	for {
		input, err := readInputWithCancel(ctx, "Enter target (request/response): ")
		if err != nil {
			return err
		}

		target := strings.ToLower(input)
		if target == "request" || target == "response" {
			check.Target = target
			break
		}
		fmt.Println("❌ Error: Invalid target. Must be one of: request, response. Please try again.")
	}

	// Get key with validation loop
	for {
		input, err := readInputWithCancel(ctx, "Enter key (header name, regex pattern, etc.): ")
		if err != nil {
			return err
		}

		if input != "" {
			check.Key = input
			break
		}
		fmt.Println("❌ Error: Key is required. Please try again.")
	}

	// Get expected value (optional)
	input, err := readInputWithCancel(ctx, "Enter expected value (optional, press Enter to skip): ")
	if err != nil {
		return err
	}
	check.Value = input

	// Get operation with validation loop
	validOps := []string{"exists", "equals", "contains", "regex", "not_exists", "not_equals"}
	for {
		input, err := readInputWithCancel(ctx, "Enter operation (exists/equals/contains/regex/not_exists/not_equals): ")
		if err != nil {
			return err
		}

		operation := strings.ToLower(input)
		isValidOp := false
		for _, op := range validOps {
			if operation == op {
				isValidOp = true
				break
			}
		}
		if isValidOp {
			check.Operation = operation
			break
		}
		fmt.Printf("❌ Error: Invalid operation. Must be one of: %s. Please try again.\n", strings.Join(validOps, ", "))
	}

	// Set default status
	check.Status = "enabled"

	if err := s.saveCustomCheck(ctx, check); err != nil {
		return fmt.Errorf("failed to save custom check: %w", err)
	}

	fmt.Printf("\n✅ Custom security check '%s' added successfully!\n", check.Name)
	return nil
}

func (s *SecurityChecker) RemoveCustomCheck(ctx context.Context) error {
	// Set up signal handling for graceful exit on Ctrl+C
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signalChan)

	// Create a context that can be cancelled by signals
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Handle signals in a goroutine
	go func() {
		select {
		case <-signalChan:
			fmt.Println("\n\n⚠️ Operation cancelled by user.")
			cancel()
		case <-ctx.Done():
			return
		}
	}()

	fmt.Println("\n🔒 Remove Custom Security Check")
	fmt.Println("=" + strings.Repeat("=", 50))

	customChecks, err := s.loadCustomChecks(ctx)
	if err != nil {
		return fmt.Errorf("failed to load custom checks: %w", err)
	}

	if len(customChecks) == 0 {
		fmt.Println("No custom security checks found.")
		return nil
	}

	fmt.Println("\nExisting custom checks:")
	for i, check := range customChecks {
		fmt.Printf("%d. [%s] %s - %s\n", i+1, check.ID, check.Name, check.Severity)
	}

	// Check the ID from the CLI before asking
	checkID := ""
	if idValue := ctx.Value("id"); idValue != nil {
		if idStr, ok := idValue.(string); ok {
			checkID = idStr
		}
	}

	// Get check ID with validation loop
	for {
		if checkID == "" {
			input, err := readInputWithCancel(ctx, "\nEnter the ID of the check to remove: ")
			if err != nil {
				return err
			}
			checkID = input
		}

		if checkID == "" {
			fmt.Println("❌ Error: Check ID is required. Please try again.")
			continue
		}

		// Check if the ID exists
		found := false
		for _, check := range customChecks {
			if check.ID == checkID {
				found = true
				break
			}
		}

		if found {
			break
		}

		fmt.Printf("❌ Error: Custom check with ID '%s' not found. Please try again.\n", checkID)
		checkID = "" // Reset to ask again
	}

	// Remove the check
	var updatedChecks []SecurityCheck
	for _, check := range customChecks {
		if check.ID != checkID {
			updatedChecks = append(updatedChecks, check)
		}
	}

	if err := s.saveCustomChecks(ctx, updatedChecks); err != nil {
		return fmt.Errorf("failed to save updated custom checks: %w", err)
	}

	fmt.Printf("\n✅ Custom security check '%s' removed successfully!\n", checkID)
	return nil
}

func (s *SecurityChecker) UpdateCustomCheck(ctx context.Context) error {
	// Set up signal handling for graceful exit on Ctrl+C
	signalChan := make(chan os.Signal, 1)
	signal.Notify(signalChan, os.Interrupt, syscall.SIGTERM)
	defer signal.Stop(signalChan)

	// Create a context that can be cancelled by signals
	ctx, cancel := context.WithCancel(ctx)
	defer cancel()

	// Handle signals in a goroutine
	go func() {
		select {
		case <-signalChan:
			fmt.Println("\n\n⚠️ Operation cancelled by user.")
			cancel()
		case <-ctx.Done():
			return
		}
	}()

	fmt.Println("\n🔒 Update Custom Security Check")
	fmt.Println("=" + strings.Repeat("=", 50))

	customChecks, err := s.loadCustomChecks(ctx)
	if err != nil {
		return fmt.Errorf("failed to load custom checks: %w", err)
	}

	if len(customChecks) == 0 {
		fmt.Println("No custom security checks found.")
		return nil
	}

	fmt.Println("\nExisting custom checks:")
	for i, check := range customChecks {
		fmt.Printf("%d. [%s] %s - %s\n", i+1, check.ID, check.Name, check.Severity)
	}

	// Check the ID from the CLI before asking
	checkID := ""
	if idValue := ctx.Value("id"); idValue != nil {
		if idStr, ok := idValue.(string); ok {
			checkID = idStr
		}
	}

	// Get check ID with validation loop
	var checkIndex = -1
	for {
		if checkID == "" {
			input, err := readInputWithCancel(ctx, "\nEnter the ID of the check to update: ")
			if err != nil {
				return err
			}
			checkID = input
		}

		if checkID == "" {
			fmt.Println("❌ Error: Check ID is required. Please try again.")
			continue
		}

		// Find the check
		for i, check := range customChecks {
			if check.ID == checkID {
				checkIndex = i
				break
			}
		}

		if checkIndex != -1 {
			break
		}

		fmt.Printf("❌ Error: Custom check with ID '%s' not found. Please try again.\n", checkID)
		checkID = "" // Reset to ask again
		checkIndex = -1
	}

	check := &customChecks[checkIndex]
	fmt.Printf("\nUpdating check: %s\n", check.Name)
	fmt.Println("Press Enter to keep current value, or enter new value:")

	// Update name
	input, err := readInputWithCancel(ctx, fmt.Sprintf("Name [%s]: ", check.Name))
	if err != nil {
		return err
	}
	if newName := strings.TrimSpace(input); newName != "" {
		check.Name = newName
	}

	// Update description
	input, err = readInputWithCancel(ctx, fmt.Sprintf("Description [%s]: ", check.Description))
	if err != nil {
		return err
	}
	if newDesc := strings.TrimSpace(input); newDesc != "" {
		check.Description = newDesc
	}

	// Get severity with validation loop
	for {
		input, err := readInputWithCancel(ctx, fmt.Sprintf("Severity [%s] (CRITICAL/HIGH/MEDIUM/LOW): ", check.Severity))
		if err != nil {
			return err
		}
		newSeverity := strings.ToUpper(strings.TrimSpace(input))

		if newSeverity == "" {
			// Keep current value
			break
		}

		if newSeverity == "CRITICAL" || newSeverity == "HIGH" || newSeverity == "MEDIUM" || newSeverity == "LOW" {
			check.Severity = newSeverity
			break
		}

		fmt.Println("❌ Error: Invalid severity. Must be one of: CRITICAL, HIGH, MEDIUM, LOW. Please try again.")
	}

	// Get type with validation loop
	for {
		input, err := readInputWithCancel(ctx, fmt.Sprintf("Type [%s] (header/body/cookie/url): ", check.Type))
		if err != nil {
			return err
		}
		newType := strings.ToLower(strings.TrimSpace(input))

		if newType == "" {
			// Keep current value
			break
		}

		if newType == "header" || newType == "body" || newType == "cookie" || newType == "url" {
			check.Type = newType
			break
		}

		fmt.Println("❌ Error: Invalid type. Must be one of: header, body, cookie, url. Please try again.")
	}

	// Update key
	input, err = readInputWithCancel(ctx, fmt.Sprintf("Key [%s]: ", check.Key))
	if err != nil {
		return err
	}
	if newKey := strings.TrimSpace(input); newKey != "" {
		check.Key = newKey
	}

	// Update value
	input, err = readInputWithCancel(ctx, fmt.Sprintf("Value [%s]: ", check.Value))
	if err != nil {
		return err
	}
	if newValue := strings.TrimSpace(input); newValue != "" {
		check.Value = newValue
	}

	// Get operation with validation loop
	validOps := []string{"exists", "equals", "contains", "regex", "not_exists", "not_equals"}
	for {
		input, err := readInputWithCancel(ctx, fmt.Sprintf("Operation [%s] (exists/equals/contains/regex/not_exists/not_equals): ", check.Operation))
		if err != nil {
			return err
		}
		newOp := strings.ToLower(strings.TrimSpace(input))

		if newOp == "" {
			// Keep current value
			break
		}

		isValidOp := false
		for _, op := range validOps {
			if newOp == op {
				isValidOp = true
				break
			}
		}

		if isValidOp {
			check.Operation = newOp
			break
		}

		fmt.Printf("❌ Error: Invalid operation. Must be one of: %s. Please try again.\n", strings.Join(validOps, ", "))
	}

	// Get status with validation loop
	for {
		input, err := readInputWithCancel(ctx, fmt.Sprintf("Status [%s] (enabled/disabled): ", check.Status))
		if err != nil {
			return err
		}
		newStatus := strings.ToLower(strings.TrimSpace(input))

		if newStatus == "" {
			// Keep current value
			break
		}

		if newStatus == "enabled" || newStatus == "disabled" {
			check.Status = newStatus
			break
		}

		fmt.Println("❌ Error: Invalid status. Must be 'enabled' or 'disabled'. Please try again.")
	}

	if err := s.saveCustomChecks(ctx, customChecks); err != nil {
		return fmt.Errorf("failed to save updated custom checks: %w", err)
	}

	fmt.Printf("\n✅ Custom security check '%s' updated successfully!\n", check.Name)
	return nil
}

func (s *SecurityChecker) ListChecks(ctx context.Context) error {
	if s.ruleset == "" {
		s.ruleset = "basic" // Default to basic ruleset if not specified
	}
	// CLI override
	if ruleSetValue := ctx.Value("rule-set"); ruleSetValue != nil {
		if ruleSetStr, ok := ruleSetValue.(string); ok && ruleSetStr != "basic" {
			s.ruleset = ruleSetStr
		}
	}

	switch s.ruleset {
	case "basic", "built-in":
		fmt.Println("\n🔒 Built-in Security Checks")
		fmt.Println("=" + strings.Repeat("=", 50))

		for _, check := range BuiltInSecurityChecks {
			// Get effective status - check both the Status field and disable list
			effectiveStatus := s.getEffectiveStatus(check)

			statusIcon := "✅ Enabled"
			if effectiveStatus == "disabled" {
				statusIcon = "❌ Disabled"
			}

			fmt.Printf("\n[%s] %s - %s (%s)\n", check.ID, check.Name, check.Severity, statusIcon)
			fmt.Printf("  Type: %s | Operation: %s | Status: %s\n", check.Type, check.Operation, check.Status)
			fmt.Printf("  Description: %s\n", check.Description)
			if check.Key != "" {
				fmt.Printf("  Key: %s\n", check.Key)
			}
			if check.Value != "" {
				fmt.Printf("  Value: %s\n", check.Value)
			}
		}

		fmt.Printf("\nRuleset: %s\n", s.ruleset)
		if len(s.testsuite.Spec.Security.Disable) > 0 {
			fmt.Printf("Disabled checks: %v\n", s.testsuite.Spec.Security.Disable)
		}

	case "custom":
		fmt.Println("\n🔒 Custom Security Checks")
		fmt.Println("=" + strings.Repeat("=", 50))

		customChecks, err := s.loadCustomChecks(ctx)
		if err != nil {
			fmt.Printf("Error loading custom checks: %v\n", err)
			return nil
		}

		if len(customChecks) == 0 {
			fmt.Println("No custom security checks found.")
		} else {
			for _, check := range customChecks {
				statusIcon := "✅ Enabled"
				if check.Status == "disabled" {
					statusIcon = "❌ Disabled"
				}

				fmt.Printf("\n[%s] %s - %s (%s)\n", check.ID, check.Name, check.Severity, statusIcon)
				fmt.Printf("  Type: %s | Operation: %s | Status: %s\n", check.Type, check.Operation, check.Status)
				fmt.Printf("  Description: %s\n", check.Description)
				if check.Key != "" {
					fmt.Printf("  Key: %s\n", check.Key)
				}
				if check.Value != "" {
					fmt.Printf("  Value: %s\n", check.Value)
				}
			}
		}
		fmt.Printf("\nCustom checks file: %s\n", ctx.Value("checks-path"))

	default:
		return fmt.Errorf("invalid rule-set value: %s. Valid values are: basic, custom.", s.ruleset)
	}

	return nil
}

// =================================================================================================

func (s *SecurityChecker) runSecurityChecks(ctx context.Context, steps []Step) *SecurityReport {
	report := &SecurityReport{
		TestSuite: s.testsuite.Name,
		Timestamp: time.Now().Format(time.RFC3339),
		Steps:     make([]StepSecurityResults, 0),
		Summary:   make(map[string]int),
	}

	// Get enabled checks based on ruleset
	enabledChecks := s.getEnabledChecks(ctx)

	for _, step := range steps {
		stepResults := StepSecurityResults{
			StepName:   step.StepName,
			StepMethod: step.StepRequest.Method,
			StepURL:    step.Endpoint,
			Results:    make([]SecurityResult, 0),
		}

		for _, check := range enabledChecks {
			result := s.executeCheck(check, step)
			if result != nil {
				stepResults.Results = append(stepResults.Results, *result)
				report.Summary[result.Severity]++

				switch result.Status {
				case "passed":
					report.Passed++
					stepResults.Passed++
				case "failed":
					report.Failed++
					stepResults.Failed++
				case "warning":
					report.Warnings++
					stepResults.Warnings++
				}
			}
		}

		report.Steps = append(report.Steps, stepResults)
	}

	// Calculate total checks across all steps
	totalChecks := 0
	for _, stepResult := range report.Steps {
		totalChecks += len(stepResult.Results)
	}
	report.TotalChecks = totalChecks

	return report
}

func (s *SecurityChecker) executeCheck(check SecurityCheck, step Step) *SecurityResult {
	result := &SecurityResult{
		CheckID:     check.ID,
		CheckName:   check.Name,
		Severity:    check.Severity,
		Description: check.Description,
		StepName:    step.StepName,
		StepMethod:  step.StepRequest.Method,
		StepURL:     step.Endpoint,
		StatusCode:  step.StepResponse.StatusCode,
		Target:      check.Target,
	}

	target := check.Target
	if target == "" {
		target = "response"
	}

	switch check.Type {
	case "header":
		return s.checkHeader(check, step, result, target)
	case "body":
		return s.checkBody(check, step, result, target)
	case "cookie":
		return s.checkCookie(check, step, result)
	case "url":
		return s.checkURL(check, step, result)
	}

	return nil
}

func (s *SecurityChecker) checkHeader(check SecurityCheck, step Step, result *SecurityResult, target string) *SecurityResult {
	var headerValue string

	if target == "request" {
		headerValue = step.StepRequest.Headers.Get(check.Key)
	} else {
		headerValue = step.StepResponse.Headers.Get(check.Key)
	}

	switch check.Operation {
	case "exists":
		if headerValue == "" {
			result.Status = "failed"
			result.Details = fmt.Sprintf("Missing %s header in %s", check.Key, target)
			result.Recommendation = fmt.Sprintf("Add %s header to %s to improve security", check.Key, target)
		} else {
			result.Status = "passed"
			result.Details = fmt.Sprintf("%s header present in %s: %s", check.Key, target, headerValue)
		}

	case "equals":
		if headerValue == "" {
			result.Status = "failed"
			result.Details = fmt.Sprintf("Missing %s header in %s", check.Key, target)
			result.Recommendation = fmt.Sprintf("Add %s: %s header to %s", check.Key, check.Value, target)
		} else if !strings.EqualFold(headerValue, check.Value) {
			result.Status = "failed"
			result.Details = fmt.Sprintf("%s header in %s has incorrect value: %s (expected: %s)", check.Key, target, headerValue, check.Value)
			result.Recommendation = fmt.Sprintf("Set %s header in %s to %s", check.Key, target, check.Value)
		} else {
			result.Status = "passed"
			result.Details = fmt.Sprintf("%s header in %s correctly set to %s", check.Key, target, check.Value)
		}

	case "contains":
		if headerValue == "" {
			result.Status = "failed"
			result.Details = fmt.Sprintf("Missing %s header in %s", check.Key, target)
			result.Recommendation = fmt.Sprintf("Add %s header containing %s to %s", check.Key, check.Value, target)
		} else if !strings.Contains(strings.ToLower(headerValue), strings.ToLower(check.Value)) {
			result.Status = "failed"
			result.Details = fmt.Sprintf("%s header in %s doesn't contain expected value: %s (looking for: %s)", check.Key, target, headerValue, check.Value)
			result.Recommendation = fmt.Sprintf("Update %s header in %s to include %s", check.Key, target, check.Value)
		} else {
			result.Status = "passed"
			result.Details = fmt.Sprintf("%s header in %s contains expected value", check.Key, target)
		}

	case "not_exists":
		if headerValue != "" {
			result.Status = "failed"
			result.Details = fmt.Sprintf("%s header should not be present in %s but found: %s", check.Key, target, headerValue)
			result.Recommendation = fmt.Sprintf("Remove %s header from %s", check.Key, target)
		} else {
			result.Status = "passed"
			result.Details = fmt.Sprintf("%s header correctly not present in %s", check.Key, target)
		}

	case "not_equals":
		if headerValue == check.Value {
			result.Status = "failed"
			result.Details = fmt.Sprintf("%s header in %s has insecure value: %s", check.Key, target, headerValue)
			result.Recommendation = fmt.Sprintf("Change %s header value in %s from %s to a more secure configuration", check.Key, target, check.Value)
		} else {
			result.Status = "passed"
			result.Details = fmt.Sprintf("%s header in %s has secure value", check.Key, target)
		}
	}

	return result
}

func (s *SecurityChecker) checkBody(check SecurityCheck, step Step, result *SecurityResult, target string) *SecurityResult {
	var body string

	if target == "request" {
		body = step.StepRequest.Body
	} else {
		body = step.StepResponse.Body
	}

	switch check.Operation {
	case "regex":
		regex, err := regexp.Compile(check.Key)
		if err != nil {
			s.logger.Error("Invalid regex pattern", zap.String("pattern", check.Key), zap.Error(err))
			return nil
		}

		// Skip if key is in allowlist
		if s.isInAllowList("keys", check.Name) {
			result.Status = "passed"
			result.Details = "Check skipped - in allowlist"
			return result
		}

		matches := regex.FindAllString(body, -1)
		if len(matches) > 0 {
			result.Status = "failed"
			result.Details = fmt.Sprintf("Found %d potential matches in %s body", len(matches), target)
			result.Recommendation = fmt.Sprintf("Remove sensitive data from %s body", target)
		} else {
			result.Status = "passed"
			result.Details = fmt.Sprintf("No sensitive data patterns found in %s body", target)
		}

	case "contains":
		if strings.Contains(body, check.Key) {
			result.Status = "failed"
			result.Details = fmt.Sprintf("%s body contains: %s", target, check.Key)
			result.Recommendation = fmt.Sprintf("Remove sensitive information from %s body", target)
		} else {
			result.Status = "passed"
			result.Details = fmt.Sprintf("%s body doesn't contain sensitive information", target)
		}

	case "not_contains":
		if !strings.Contains(body, check.Key) {
			result.Status = "passed"
			result.Details = fmt.Sprintf("%s body correctly doesn't contain sensitive information", target)
		} else {
			result.Status = "failed"
			result.Details = fmt.Sprintf("%s body should not contain: %s", target, check.Key)
			result.Recommendation = fmt.Sprintf("Remove sensitive information from %s body", target)
		}
	}

	return result
}

func (s *SecurityChecker) checkCookie(check SecurityCheck, step Step, result *SecurityResult) *SecurityResult {
	// For cookies, we typically only check response cookies (Set-Cookie headers)
	// since request cookies are usually sent via Cookie header which could be checked as headers
	cookies := step.StepResponse.Headers["Set-Cookie"]
	if len(cookies) == 0 {
		result.Status = "passed"
		result.Details = "No cookies set in response"
		return result
	}

	switch check.Operation {
	case "exists":
		found := false
		for _, cookie := range cookies {
			if strings.Contains(cookie, check.Key) {
				found = true
				break
			}
		}

		if !found {
			result.Status = "failed"
			result.Details = fmt.Sprintf("Cookies missing %s attribute", check.Key)
			result.Recommendation = fmt.Sprintf("Add %s attribute to cookies", check.Key)
		} else {
			result.Status = "passed"
			result.Details = fmt.Sprintf("Cookies have %s attribute", check.Key)
		}
	}

	return result
}

func (s *SecurityChecker) checkURL(check SecurityCheck, step Step, result *SecurityResult) *SecurityResult {
	switch check.Operation {
	case "regex":
		regex, err := regexp.Compile(check.Key)
		if err != nil {
			s.logger.Error("Invalid regex pattern", zap.String("pattern", check.Key), zap.Error(err))
			return nil
		}

		if regex.MatchString(step.Endpoint) {
			result.Status = "failed"
			result.Details = fmt.Sprintf("URL matches insecure pattern: %s", check.Key)
			result.Recommendation = "Review URL structure for security issues"
		} else {
			result.Status = "passed"
			result.Details = "URL doesn't match insecure patterns"
		}

	case "contains":
		if strings.Contains(step.Endpoint, check.Key) {
			result.Status = "failed"
			result.Details = fmt.Sprintf("URL contains insecure element: %s", check.Key)
			result.Recommendation = "Remove insecure elements from URL"
		} else {
			result.Status = "passed"
			result.Details = "URL doesn't contain insecure elements"
		}
	}

	return result
}
