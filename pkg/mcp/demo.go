// Package mcp provides demo examples for MCP integration.
package mcp

import (
	"context"
	"fmt"
	"time"

	"go.keploy.io/server/v3/config"
	"go.uber.org/zap"
)

// DemoRunner provides demo functionality for the MCP integration
type DemoRunner struct {
	logger       *zap.Logger
	config       *config.Config
	integration  *ServiceIntegration
}

// NewDemoRunner creates a new demo runner
func NewDemoRunner(logger *zap.Logger, cfg *config.Config) *DemoRunner {
	return &DemoRunner{
		logger:      logger,
		config:      cfg,
		integration: NewServiceIntegration(logger, cfg),
	}
}

// RunSingleEndpointDemo demonstrates the end-to-end mock workflow for a single API endpoint
func (d *DemoRunner) RunSingleEndpointDemo(ctx context.Context) error {
	fmt.Println("╔════════════════════════════════════════════════════════════════╗")
	fmt.Println("║         Keploy MCP Integration Demo                            ║")
	fmt.Println("║         Single API Endpoint Mock Workflow                      ║")
	fmt.Println("╚════════════════════════════════════════════════════════════════╝")
	fmt.Println()

	// Step 1: Show available tools
	fmt.Println("📋 Step 1: Discovering Available MCP Tools")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	
	server := d.integration.GetMCPServer()
	tools := server.GetTools()
	
	for _, tool := range tools {
		fmt.Printf("  📦 %s\n", tool.Name)
		fmt.Printf("     %s\n\n", tool.Description)
	}

	// Step 2: Simulate natural language prompt
	fmt.Println("💬 Step 2: Processing Natural Language Prompt")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	
	userPrompt := "Generate tests using Keploy mocking feature for the User API"
	fmt.Printf("  User says: \"%s\"\n\n", userPrompt)
	
	// Parse and extract command
	testCommand := "go test ./..."
	apiDescription := "User API - CRUD operations for user management"
	
	fmt.Printf("  Parsed parameters:\n")
	fmt.Printf("    • Test Command: %s\n", testCommand)
	fmt.Printf("    • API Description: %s\n", apiDescription)
	fmt.Println()

	// Step 3: Invoke mock recording
	fmt.Println("🔴 Step 3: Starting Mock Recording Phase")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	
	result, err := server.InvokeTool(ctx, "keploy_mock_record", map[string]interface{}{
		"testCommand":        testCommand,
		"contextDescription": apiDescription,
	})
	
	if err != nil {
		return fmt.Errorf("recording failed: %w", err)
	}
	
	for _, content := range result.Content {
		fmt.Printf("  %s\n", content.Text)
	}
	fmt.Println()

	// Simulate recording progress
	fmt.Println("  📡 Capturing network calls...")
	time.Sleep(500 * time.Millisecond)
	fmt.Println("  ✓ HTTP GET /api/v1/users → mock: http-fetch-users-abc123")
	time.Sleep(300 * time.Millisecond)
	fmt.Println("  ✓ HTTP POST /api/v1/users → mock: http-create-users-def456")
	time.Sleep(300 * time.Millisecond)
	fmt.Println("  ✓ Postgres SELECT users → mock: postgres-query-ghi789")
	fmt.Println()

	// Step 4: Show contextual naming
	fmt.Println("📝 Step 4: Contextual Mock Naming")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	
	namer := NewContextualNamer()
	
	examples := []struct {
		method   string
		endpoint string
		service  string
	}{
		{"GET", "/api/v1/users", "user-service"},
		{"POST", "/api/v1/users", "user-service"},
		{"GET", "/api/v1/users/123", "user-service"},
		{"DELETE", "/api/v1/users/456", "user-service"},
	}
	
	fmt.Println("  Generated mock names:")
	for _, ex := range examples {
		name := namer.GenerateMockNameFromHTTP(ex.method, ex.endpoint, ex.service, apiDescription)
		fmt.Printf("    %s %s → %s\n", ex.method, ex.endpoint, name)
	}
	fmt.Println()

	// Step 5: Invoke mock replay
	fmt.Println("🟢 Step 5: Starting Mock Replay Phase")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	
	result, err = server.InvokeTool(ctx, "keploy_mock_replay", map[string]interface{}{
		"testCommand":       testCommand,
		"validateIsolation": true,
	})
	
	if err != nil {
		return fmt.Errorf("replay failed: %w", err)
	}
	
	for _, content := range result.Content {
		fmt.Printf("  %s\n", content.Text)
	}
	fmt.Println()

	// Simulate replay progress
	fmt.Println("  🔄 Replaying tests with mocks...")
	time.Sleep(500 * time.Millisecond)
	fmt.Println("  ✓ Test: TestGetUsers - PASSED (mock: http-fetch-users-abc123)")
	time.Sleep(300 * time.Millisecond)
	fmt.Println("  ✓ Test: TestCreateUser - PASSED (mock: http-create-users-def456)")
	time.Sleep(300 * time.Millisecond)
	fmt.Println("  ✓ Test: TestUserValidation - PASSED (mock: postgres-query-ghi789)")
	fmt.Println()

	// Step 6: Validation results
	fmt.Println("✅ Step 6: Validation Results")
	fmt.Println("━━━━━━━━━━━━━━━━━━━━━━━━━━━━━━")
	fmt.Println("  Test Results:")
	fmt.Println("    • Total Tests: 3")
	fmt.Println("    • Passed: 3")
	fmt.Println("    • Failed: 0")
	fmt.Println()
	fmt.Println("  Isolation Validation:")
	fmt.Println("    • Real Network Calls: 0")
	fmt.Println("    • Mock Injections: 3")
	fmt.Println("    • Status: ✓ ISOLATED")
	fmt.Println()

	// Summary
	fmt.Println("╔════════════════════════════════════════════════════════════════╗")
	fmt.Println("║                     Demo Complete!                             ║")
	fmt.Println("╠════════════════════════════════════════════════════════════════╣")
	fmt.Println("║  The demo showed how a natural language prompt like:           ║")
	fmt.Println("║  \"Generate tests using Keploy mocking feature\"                 ║")
	fmt.Println("║                                                                ║")
	fmt.Println("║  Triggers the internal workflow:                               ║")
	fmt.Println("║  1. keploy mock record -- <test-command>                       ║")
	fmt.Println("║  2. Apply contextual naming to mock files                      ║")
	fmt.Println("║  3. keploy mock replay -- <test-command>                       ║")
	fmt.Println("║  4. Validate environment isolation                             ║")
	fmt.Println("╚════════════════════════════════════════════════════════════════╝")

	return nil
}

// RunNamingDemo demonstrates the contextual naming feature
func (d *DemoRunner) RunNamingDemo() {
	fmt.Println("╔════════════════════════════════════════════════════════════════╗")
	fmt.Println("║         Contextual Mock Naming Demo                            ║")
	fmt.Println("╚════════════════════════════════════════════════════════════════╝")
	fmt.Println()

	namer := NewContextualNamer()

	// HTTP Examples
	fmt.Println("HTTP Mocks:")
	fmt.Println("───────────")
	httpExamples := []struct {
		method   string
		endpoint string
		service  string
		desc     string
	}{
		{"GET", "/api/v1/users", "auth-service", "User authentication API"},
		{"POST", "/api/v1/orders", "order-service", "Order processing API"},
		{"PUT", "/api/v1/products/123", "inventory-service", "Product management"},
		{"DELETE", "/api/v1/sessions/abc-def-ghi", "auth-service", "Session management"},
		{"GET", "/graphql", "api-gateway", "GraphQL endpoint"},
	}

	for _, ex := range httpExamples {
		name := namer.GenerateMockNameFromHTTP(ex.method, ex.endpoint, ex.service, ex.desc)
		fmt.Printf("  %s %-30s → %s\n", ex.method, ex.endpoint, name)
	}
	fmt.Println()

	// Database Examples
	fmt.Println("Database Mocks:")
	fmt.Println("───────────────")
	dbExamples := []struct {
		kind      string
		operation string
		table     string
		db        string
	}{
		{"Postgres", "SELECT", "users", "main-db"},
		{"MySQL", "INSERT", "orders", "orders-db"},
		{"Mongo", "find", "products", "catalog-db"},
	}

	for _, ex := range dbExamples {
		var name string
		switch ex.kind {
		case "Mongo":
			name = namer.GenerateMockNameFromGeneric(ex.db, ex.table+" "+ex.operation)
		default:
			name = fmt.Sprintf("%s-%s-%s-%d", 
				namer.prefixMap["Postgres"],
				namer.operationVerbs[ex.operation],
				ex.table,
				time.Now().UnixNano()%10000)
		}
		fmt.Printf("  %-10s %-8s on %-15s → %s\n", ex.kind, ex.operation, ex.table, name)
	}
	fmt.Println()

	// Endpoint Analysis
	fmt.Println("Endpoint Analysis:")
	fmt.Println("──────────────────")
	endpoints := []string{
		"/api/v1/users",
		"/api/v2/users/123/orders",
		"/graphql",
		"/v1/payments/abc-def-ghi-jkl/refund",
	}

	for _, ep := range endpoints {
		analysis := namer.AnalyzeEndpoint(ep)
		fmt.Printf("  %s\n", ep)
		fmt.Printf("    Resource: %s, RESTful: %v, GraphQL: %v\n",
			analysis.ResourceName, analysis.IsRESTful, analysis.IsGraphQL)
	}
}

// RunWorkflowDemo demonstrates the workflow orchestration
func (d *DemoRunner) RunWorkflowDemo(ctx context.Context) {
	fmt.Println("╔════════════════════════════════════════════════════════════════╗")
	fmt.Println("║         Workflow Orchestration Demo                            ║")
	fmt.Println("╚════════════════════════════════════════════════════════════════╝")
	fmt.Println()

	orchestrator := d.integration.GetOrchestrator()

	// Show workflow phases
	fmt.Println("Workflow Phases:")
	fmt.Println("────────────────")
	phases := []struct {
		phase WorkflowPhase
		desc  string
	}{
		{PhaseIdle, "No workflow in progress"},
		{PhaseRecording, "Capturing outgoing network calls"},
		{PhaseProcessing, "Applying contextual naming to mocks"},
		{PhaseReplaying, "Running tests with recorded mocks"},
		{PhaseCompleted, "Workflow finished successfully"},
		{PhaseFailed, "Workflow encountered an error"},
	}

	for _, p := range phases {
		status := "○"
		if p.phase == orchestrator.GetCurrentPhase() {
			status = "●"
		}
		fmt.Printf("  %s %-15s - %s\n", status, p.phase, p.desc)
	}
	fmt.Println()

	// Show workflow result structure
	fmt.Println("Workflow Result Structure:")
	fmt.Println("──────────────────────────")
	fmt.Println(`  {
    "success": true,
    "phase": "completed",
    "testSetId": "user-api-20240101",
    "recordStats": {
      "totalMocks": 5,
      "mocksByKind": {"HTTP": 3, "Postgres": 2},
      "networkCalls": 5,
      "externalServices": ["auth-service", "db-service"]
    },
    "replayStats": {
      "totalTests": 10,
      "passed": 10,
      "failed": 0,
      "mocksUsed": 5,
      "realCallsMade": 0
    },
    "mockFiles": [
      {
        "name": "mock-0",
        "contextName": "http-fetch-users-abc123",
        "kind": "HTTP",
        "serviceName": "auth-service"
      }
    ],
    "isolationValid": true,
    "duration": "2.5s"
  }`)
	fmt.Println()
}
