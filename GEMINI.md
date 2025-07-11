# GEMINI: Comprehensive Documentation for the Keploy Codebase

---

## Table of Contents
1. [Project Overview and Purpose](#project-overview-and-purpose)
2. [Directory and File Structure](#directory-and-file-structure)
    - [Root Directory](#root-directory)
    - [Key Subdirectories](#key-subdirectories)
3. [Major Use Cases and Workflows](#major-use-cases-and-workflows)
4. [Configuration, Setup, and Developer Notes](#configuration-setup-and-developer-notes)
5. [References and Contribution Guides](#references-and-contribution-guides)

---

## Project Overview and Purpose

**Keploy** is a developer-centric API testing tool that automatically generates tests and mocks from real user traffic, making API testing as fast as unit testing. It records API/database calls and replays them for robust, automated testing. Keploy is language-agnostic, supports eBPF-based instrumentation, and integrates with CI/CD pipelines. It also features an AI-powered Unit Test Generator (ut-gen) that leverages LLMs to generate meaningful unit tests from code semantics.

---

## Directory and File Structure

### Root Directory

- **main.go**: Entry point for the Keploy CLI application. Sets up context, logging, configuration, and command execution.
- **keploy, server, sparshkeploy**: Compiled binaries for different platforms or purposes.
- **keploy.yml**: Main configuration file for Keploy. Controls ports, test settings, proxy, debug, and more.
- **goreleaser.yaml**: Configuration for GoReleaser, used for building and releasing binaries.
- **go.mod, go.sum**: Go module dependencies.
- **Dockerfile**: Container build instructions for Keploy.
- **entrypoint.sh, keploy.sh**: Shell scripts for entry and utility operations.
- **README.md**: Main project documentation, features, installation, and usage.
- **README-UnitGen.md**: Documentation for the Unit Test Generator (ut-gen) feature.
- **READMEes-Es.md, READMEja-JP.md**: Translations of the main README.
- **SECURITY.md**: Security policy and reporting instructions.
- **DEBUG.md**: Debugging guide for developers.
- **.github/**: GitHub-specific files (issue templates, workflows, actions, etc.).
- **.gitignore, .golangci.yml, .pre-commit-config.yaml, .cz.toml, .releaserc.json**: Various configuration files for git, linting, pre-commit hooks, commit conventions, and release automation.
- **coverage.out**: Test coverage output.
- **oss-pledge.json, CITATION.cff**: Open source pledge and citation file.
- **LICENSE**: Project license.

### Key Subdirectories

#### `pkg/` - Core Logic and Interfaces
- **Purpose**: Contains core logic, interfaces, and data models used throughout Keploy.
- **matcher/**: Matching logic for HTTP/gRPC/schema (submodules for protocol-specific matching).
- **models/**: All Go structs for storing captured data (test cases, mocks, etc.).
- **service/**: Business logic for recording, replaying, orchestrating, and tools (record, replay, orchestrator, tools, utgen, etc.).
- **platform/**: Platform-specific integrations (telemetry, auth, coverage, docker, storage, yaml, etc.).
- **core/**: Core system logic (proxy, hooks, app, tester, etc.).
- **util.go, http2.go**: Utilities for HTTP/2 and general helpers.
- **README.md**: Overview of the pkg directory.

#### `utils/` - Utilities
- **Purpose**: General utility functions for context, logging, masking, signals, etc.
- **utils.go**: Main utility functions (file ops, conversions, error handling, etc.).
- **ctx.go**: Context management and signal handling.
- **log/**: Logging utilities (logger, color, time).
- **mask_others.go, mask_windows.go**: OS-specific masking utilities.
- **signal_others.go, signal_windows.go**: OS-specific signal handling.

#### `cli/` - Command Line Interface
- **Purpose**: Defines the root command and subcommands for the CLI.
- **cli.go, root.go**: CLI entry points.
- **provider/**: Provider logic for CLI commands (cmd.go, service.go, etc.).
- **Other files**: Command implementations for record, test, login, mock, utgen, etc.
- **README.md**: Overview of the CLI package.

#### `config/` - Configuration
- **Purpose**: Configuration structures and logic.
- **config.go**: Main configuration struct and helpers.
- **default.go**: Default configuration values.

#### `.github/` - GitHub Integration
- **ISSUE_TEMPLATE/**: Issue templates for bug reports, feature requests, etc.
- **actions/**: Custom GitHub Actions.
- **workflows/**: CI/CD workflows for building, testing, releasing, etc.

---

## Major Use Cases and Workflows

### 1. **API Test Generation from User Traffic**
- Record API and database calls from real user traffic using `keploy record -c "<app command>"`.
- Generate test cases and mocks automatically.
- Replay tests with `keploy test -c "<app command>" --delay 10` (no need for live databases during replay).

### 2. **Unit Test Generation (ut-gen)**
- Use LLMs to generate unit tests from code semantics.
- Supports Go, Node.js, Java, Python, and more.
- See [README-UnitGen.md](README-UnitGen.md) for detailed instructions.

### 3. **CI/CD Integration**
- Run Keploy tests and mocks in CI pipelines (GitHub Actions, Jenkins, etc.).
- Merge Keploy test coverage with standard unit test coverage.

### 4. **Mocking and Stubbing**
- Automatically generate mocks for dependencies (DB, Redis, gRPC, etc.).
- Use mocks for isolated testing and replay.

### 5. **Advanced Features**
- eBPF-based instrumentation for code-less, language-agnostic integration.
- Record/replay complex distributed API flows.
- Multi-purpose mocks for server tests.
- Telemetry and analytics for test runs.

---

## Configuration, Setup, and Developer Notes

### Configuration
- **keploy.yml**: Main config file. Controls ports, test settings, proxy, debug, container names, test selection, mocking, and more.
- **config/config.go**: Defines the configuration struct and logic.
- **goreleaser.yaml**: Build and release configuration for GoReleaser.

### Setup
- Install via script: `curl --silent -O -L https://keploy.io/install.sh && source install.sh`
- See [README.md](README.md) for quick start and language-specific setup.
- For ut-gen, set `API_KEY` for LLM access and follow [README-UnitGen.md](README-UnitGen.md).

### Developer Notes
- See [DEBUG.md](DEBUG.md) for debugging tips (e.g., capturing stack traces with SIGQUIT).
- Logging is handled via `utils/log/logger.go` (writes to `keploy-logs.txt`).
- Contribution and code style: see [CONTRIBUTING.md](CONTRIBUTING.md).
- Code of conduct: see [CODE_OF_CONDUCT.md](CODE_OF_CONDUCT.md).

---

## References and Contribution Guides

- **Main Documentation**: [Keploy Docs](https://keploy.io/docs/)
- **Unit Test Generator**: [README-UnitGen.md](README-UnitGen.md)
- **Debugging Guide**: [DEBUG.md](DEBUG.md)
- **Security Policy**: [SECURITY.md](SECURITY.md)
- **Community**: [Slack](https://join.slack.com/t/keploy/shared_invite/zt-357qqm9b5-PbZRVu3Yt2rJIa6ofrwWNg)
- **FAQs**: [Keploy FAQ](https://keploy.io/docs/keploy-explained/faq/)
- **How Keploy Works**: [How Keploy Works](https://keploy.io/docs/keploy-explained/how-keploy-works/)

---

