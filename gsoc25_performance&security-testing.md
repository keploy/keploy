# Keploy GSoC'25_Performance&Security Features Documentation

This document provides a comprehensive overview of the three advanced features implemented in Keploy: **TestSuite Execution**, **Load Testing**, and **Security Checking**.

## Table of Contents
1. [Overview](#overview)
2. [TestSuite Feature](#testsuite-feature)
3. [Load Testing Feature](#load-testing-feature)
4. [Security Checking Feature](#security-checking-feature)
5. [Integration and Architecture](#integration-and-architecture)
6. [Usage Examples](#usage-examples)
7. [Configuration](#configuration)

---

## Overview

These three features work together to provide a comprehensive testing solution:

- **TestSuite**: Core execution engine for running API test suites defined in YAML
- **Load Testing**: Performance testing capability that executes test suites under load
- **Security Checking**: Security vulnerability scanning for API endpoints

All features are integrated into the Keploy CLI and share common configuration patterns and data structures.

---

## TestSuite Feature

### Purpose
The TestSuite feature provides a declarative way to define and execute API test sequences using YAML configuration files. It serves as the foundation for both load testing and security checking.

### Architecture

#### Core Components

1. **TSExecutor** (`pkg/service/testsuite/testsuite.go`)
   - Main execution engine for test suites
   - Handles HTTP requests, response validation, and variable extraction
   - Supports rate limiting for controlled execution

2. **TestSuite Data Structures**
   ```go
   type TestSuite struct {
       Version string        `yaml:"version"`
       Kind    string        `yaml:"kind"`
       Name    string        `yaml:"name"`
       Spec    TestSuiteSpec `yaml:"spec"`
   }
   
   type TestSuiteSpec struct {
       Metadata TestSuiteMetadata `yaml:"metadata"`
       Security Security          `yaml:"security,omitempty"`
       Load     LoadOptions       `yaml:"load,omitempty"`
       Steps    []TestStep        `yaml:"steps"`
   }
   ```

3. **TestStep Execution**
   - Each step represents an HTTP request with assertions
   - Supports variable extraction and interpolation
   - Provides detailed execution results

#### Key Features

- **Variable Extraction**: Extract values from responses using JSON path notation
- **Variable Interpolation**: Use extracted variables in subsequent requests
- **Assertions**: Validate response status codes, headers, and body content
- **Rate Limiting**: Control request rate for load testing scenarios
- **Detailed Reporting**: Comprehensive execution reports with timing and error information

### Implementation Details

#### CLI Integration (`cli/testsuite.go`)
```go
func TestSuite(ctx context.Context, logger *zap.Logger, _ *config.Config, 
               serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
    var cmd = &cobra.Command{
        Use:     "testsuite",
        Short:   "execute a testsuite against a given url (--base-url)",
        Example: `keploy testsuite --base-url "http://localhost:8080/path/to/user/app"`,
        RunE: func(cmd *cobra.Command, args []string) error {
            // Service factory pattern for dependency injection
            svc, err := serviceFactory.GetService(ctx, cmd.Name())
            if err != nil {
                utils.LogError(logger, err, "failed to get service")
                return nil
            }
            
            // Execute the test suite
            _, err = tsSvc.Execute(ctx, nil)
            return err
        },
    }
}
```

#### Service Factory Integration (`cli/provider/service.go`)
The service factory provides dependency injection and service instantiation:
```go
func (n *ServiceProvider) GetService(ctx context.Context, cmd string) (interface{}, error) {
    switch cmd {
    case "testsuite":
        return testsuite.NewTSExecutor(n.cfg, n.logger, false)
    // ... other services
    }
}
```

### Usage
```bash
# Execute a test suite
keploy testsuite --base-url "http://localhost:8080" --ts-path ./keploy/testsuite --ts-file suite-0.yaml

# With custom configuration
keploy testsuite --base-url "http://localhost:8080" --config-path ./config
```

---

## Load Testing Feature

### Purpose
The Load Testing feature enables performance testing by executing test suites under various load profiles. It provides comprehensive metrics collection and threshold-based pass/fail criteria.

### Architecture

#### Core Components

1. **LoadTester** (`pkg/service/load/load.go`)
   - Main coordinator for load test execution
   - Manages test suite parsing and load configuration
   - Generates comprehensive load test reports

2. **Scheduler** (`pkg/service/load/scheduler.go`)
   - Orchestrates virtual user (VU) execution
   - Supports multiple load profiles (constant, ramping)
   - Manages timing and synchronization

3. **VUWorker** (`pkg/service/load/vu_worker.go`)
   - Individual virtual user implementation
   - Executes test suite repeatedly under load
   - Collects per-VU metrics and timing data

4. **MetricsCollector** (`pkg/service/load/metrics_collector.go`)
   - Aggregates metrics from all virtual users
   - Provides centralized data collection point
   - Calculates aggregate statistics

5. **ThresholdEvaluator** (`pkg/service/load/threshold_evaluator.go`)
   - Evaluates performance thresholds
   - Determines pass/fail status for load tests
   - Supports various metric types and conditions

#### Load Profiles

1. **Constant VUs Profile**
   - Maintains constant number of virtual users
   - Consistent load throughout test duration
   ```yaml
   load:
     profile: constant_vus
     vus: 10
     duration: 5m
     rps: 50
   ```

2. **Ramping VUs Profile**
   - Gradually increases/decreases virtual users
   - Supports multiple stages with different targets
   ```yaml
   load:
     profile: ramping_vus
     vus: 30
     duration: 3m
     stages:
       - duration: 30s
         target: 10
       - duration: 2m
         target: 30
   ```

#### Metrics and Thresholds

The system collects comprehensive metrics:
- **Request metrics**: Count, failure rate, response times
- **Performance metrics**: P95 latency, throughput
- **Data transfer**: Bytes sent/received

Thresholds provide pass/fail criteria:
```yaml
thresholds:
  - metric: http_req_duration_p95
    condition: "< 500ms"
    severity: high
    comment: "Ensure 95% of requests are below 500ms latency"
  - metric: http_req_failed_rate
    condition: "<= 1%"
    severity: critical
    comment: "Error rate must stay under 1%"
```

### Implementation Details

#### CLI Integration (`cli/load.go`)
```go
func Load(ctx context.Context, logger *zap.Logger, _ *config.Config, 
          serviceFactory ServiceFactory, cmdConfigurator CmdConfigurator) *cobra.Command {
    var cmd = &cobra.Command{
        Use:     "load",
        Short:   "load test a given testsuite.",
        Example: `keploy load -f test_suite.yaml --out json > report.json`,
        RunE: func(cmd *cobra.Command, args []string) error {
            // CLI parameter extraction
            vus, _ := cmd.Flags().GetInt("vus")
            duration, _ := cmd.Flags().GetString("duration")
            rps, _ := cmd.Flags().GetInt("rps")
            
            // Context with CLI overrides
            ctx := context.WithValue(ctx, "vus", vus)
            ctx = context.WithValue(ctx, "duration", duration)
            ctx = context.WithValue(ctx, "rps", rps)
            
            return ltSvc.Start(ctx)
        },
    }
}
```

#### Integrated Dashboard
- **Built-in Web Server**: Embedded dashboard server integrated directly into Keploy binary
- **Auto-Browser Launch**: Automatically opens browser to dashboard URL when load test starts
- **DashboardExposer** (`pkg/service/load/dashboard_exposer.go`): 
  - Serves embedded React dashboard from Go binary
  - Handles cross-platform browser launching (Windows, Linux, macOS, WSL)
  - Provides real-time metrics API endpoints
- **Exporter** (`pkg/service/load/exporter.go`): Real-time metrics streaming to dashboard
- **Live Monitoring**: Real-time updates of VU count, RPS, response times, and threshold status
- **Zero Configuration**: No external dependencies or separate dashboard setup required

#### Dashboard Architecture

The dashboard system is fully integrated into the Keploy binary using Go's embed feature:

```go
//go:embed out/*
var content embed.FS

func (de *DashboardExposer) fileSystem() http.FileSystem {
    fsys, err := fs.Sub(content, "out")
    return http.FS(fsys)
}
```

**Key Features:**
- **Embedded Static Files**: Frontend assets bundled into Go binary at compile time
- **Smart Browser Detection**: Cross-platform browser launching with WSL support

### Advanced Features

1. **Rate Limiting**: Precise RPS control using token bucket algorithm
2. **Integrated Dashboard**: Built-in web dashboard that automatically launches in browser
   - **Embedded Server**: Dashboard server runs within Keploy binary (no external dependencies)
   - **Auto-Launch**: Browser automatically opens to `http://localhost:3000` when load test starts
   - **Cross-Platform**: Supports Windows, Linux, macOS, and WSL browser launching
   - **Real-time Monitoring**: Live metrics updates including VU count, RPS, response times
   - **Threshold Visualization**: Real-time pass/fail status of performance thresholds
3. **Report Generation**: JSON and CLI-formatted comprehensive reports
4. **CLI Overrides**: Command-line parameters override YAML configuration
5. **Threshold Evaluation**: Automated pass/fail determination with live status updates

### Usage
```bash
# Basic load test (automatically opens dashboard in browser)
keploy load -f suite-0.yaml --base-url "http://localhost:8080"

# With CLI overrides (dashboard launches at http://localhost:3000)
keploy load -f suite-0.yaml --base-url "http://localhost:8080" --vus 20 --duration 10m --rps 100

# Generate JSON report while monitoring via dashboard
keploy load -f suite-0.yaml --out json > load_report.json
```

---

## Security Checking Feature

### Purpose
The Security Checking feature provides automated security vulnerability scanning for API endpoints. It executes both built-in and custom security checks against API responses and requests.

### Architecture

#### Core Components

1. **SecurityChecker** (`pkg/service/secure/secure.go`)
   - Main security scanning engine
   - Manages built-in and custom security checks
   - Executes checks against test suite results

2. **Security Checks System**
   - Built-in checks for common vulnerabilities
   - Custom check support via YAML configuration
   - Severity-based filtering and reporting

3. **AllowList System**
   - Filter false positives
   - Customizable per header, key, or pattern
   - Integration with test suite configuration

#### Security Check Types

The system includes various built-in security checks:

1. **Header Security Checks**
   - Missing security headers (HSTS, CSP, X-Frame-Options)
   - Insecure header values
   - Information disclosure in headers

2. **Response Body Checks**
   - Sensitive data exposure
   - Error message leakage
   - Debug information disclosure

3. **Cookie Security**
   - Missing secure flags
   - HttpOnly flag validation
   - SameSite attribute checking

4. **General Security**
   - HTTP vs HTTPS enforcement
   - Server information disclosure
   - Version information exposure

#### Custom Security Checks

Users can define custom security checks:
```yaml
# custom-checks.yaml
- id: custom_header_check
  name: Custom Header Validation
  description: Check for custom security header
  severity: MEDIUM
  type: header
  target: response
  key: X-Custom-Security
  operation: exists
  status: enabled
```

### Implementation Details

#### CLI Integration (`cli/secure.go`)
The security feature provides multiple subcommands:
- `keploy secure` - Run security checks
- `keploy secure add` - Add custom security check
- `keploy secure remove` - Remove custom security check
- `keploy secure update` - Update custom security check
- `keploy secure list` - List available security checks

#### Security Check Execution
```go
type SecurityCheck struct {
    ID          string `json:"id"`
    Name        string `json:"name"`
    Description string `json:"description"`
    Severity    string `json:"severity"`        // "CRITICAL", "HIGH", "MEDIUM", "LOW"
    Type        string `json:"type"`            // "header", "body", "cookie", "url"
    Target      string `json:"target"`          // "request", "response"
    Key         string `json:"key"`             // Header name, JSON path, etc.
    Value       string `json:"value,omitempty"` // Expected value or pattern
    Operation   string `json:"operation"`       // "exists", "equals", "contains", "regex"
    Status      string `json:"status"`          // "enabled", "disabled"
}
```

#### Integration with TestSuite
Security checks are executed against test suite results:
1. Test suite executes API calls
2. Security checker analyzes requests/responses
3. Checks are applied based on configuration
4. Results are filtered by severity and allowlists
5. Comprehensive security report is generated

### Usage
```bash
# Run security checks
keploy secure --base-url "http://localhost:8080"

# List available checks
keploy secure list --rule-set basic

# Add custom check
keploy secure add

# Run with custom severity threshold
keploy secure --severity-threshold HIGH
```

---

## Integration and Architecture

### Service Factory Pattern

All three features are integrated through a unified service factory pattern:

```go
// cli/provider/service.go
func (n *ServiceProvider) GetService(ctx context.Context, cmd string) (interface{}, error) {
    switch cmd {
    case "secure":
        return secure.NewSecurityChecker(n.cfg, n.logger)
    case "load":
        return load.NewLoadTester(n.cfg, n.logger)
    case "testsuite":
        return testsuite.NewTSExecutor(n.cfg, n.logger, false)
    // ... other services
    }
}
```

### Shared Configuration

All features share common configuration patterns:

```yaml
# keploy.yml
testSuite:
  tsPath: keploy/testsuite
  tsFile: suite-0.yaml
  baseUrl: ""

load:
  vus: 0
  duration: ""
  rps: 0
  output: ""

# Suite configuration (suite-0.yaml)
spec:
  security:
    ruleset: basic
    severity_threshold: MEDIUM
    disable: []
    allowlist:
      headers: ["Server"]
      keys: ["data.debug"]
  
  load:
    profile: constant_vus
    vus: 10
    duration: 5m
    thresholds:
      - metric: http_req_duration_p95
        condition: "< 500ms"
        severity: high
```

### Cross-Feature Integration

1. **Load Testing + Security**: Load tests can include security checking
2. **TestSuite Foundation**: Both load and security features build on testsuite execution
3. **Shared Reporting**: Common report formats and output options
4. **Configuration Inheritance**: Features inherit and extend base configurations

---

## Usage Examples

### Complete Test Suite Example

```yaml
# suite-0.yaml
version: api.keploy.io/v2beta1
kind: TestSuite
name: Todo_CRUD_Operations
spec:
  metadata:
    description: Test CRUD operations for Todo API
  
  security:
    ruleset: basic
    severity_threshold: MEDIUM
    allowlist:
      headers: ["Server"]
      keys: ["data.debug"]
  
  load:
    profile: ramping_vus
    vus: 30
    duration: 3m
    stages:
      - duration: 30s
        target: 10
      - duration: 2m
        target: 30
    thresholds:
      - metric: http_req_duration_p95
        condition: "< 500ms"
        severity: high
  
  steps:
    - name: Create_todo
      method: POST
      url: /todos
      headers:
        Content-Type: application/json
      body: |
        {
          "title": "Test Todo",
          "completed": false
        }
      extract:
        todo_id: id
      assert:
        - type: status_code
          expected_string: "201"
    
    - name: Get_todo
      method: GET
      url: /todos/{{todo_id}}
      assert:
        - type: status_code
          expected_string: "200"
```

### Command Usage Examples

```bash
# 1. Test functionality
keploy testsuite --base-url "http://localhost:8080"

# 2. Perform load testing (dashboard launches for real-time monitoring)
keploy load --base-url "http://localhost:8080"

# 3. Security scanning
keploy secure --base-url "http://localhost:8080"
```

---

## Configuration

### Global Configuration (`keploy.yml`)

```yaml
testSuite:
  tsPath: keploy/testsuite      # Path to test suite directory
  tsFile: suite-0.yaml          # Test suite file name
  baseUrl: ""                   # Base URL for API endpoints
```

### Feature-Specific Configuration

Each feature supports extensive configuration through both YAML files and CLI flags, providing flexibility for different testing scenarios and environments.

---

## Conclusion

These three features provide a comprehensive testing solution that covers functional testing, performance testing, and security scanning. The modular architecture and shared configuration patterns make them easy to use individually or in combination, while the integration with Keploy's existing ecosystem ensures seamless workflow integration.

The features are designed with extensibility in mind, supporting custom security checks, flexible load profiles, and comprehensive reporting options to meet diverse testing requirements.
