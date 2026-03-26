# Contributing to Keploy

Thank you for your interest in contributing to Keploy! 🎉 We welcome contributions from everyone.

## 🚀 Quick Start

### 1. Fork the Repository

Click the "Fork" button on [github.com/keploy/keploy](https://github.com/keploy/keploy)

### 2. Clone Your Fork

```bash
git clone https://github.com/YOUR_USERNAME/keploy.git
cd keploy
```

### 3. Add Upstream Remote

```bash
git remote add upstream https://github.com/keploy/keploy.git
```

### 4. Create a Branch

```bash
git checkout -b feature/your-feature-name
```

## 🛠️ Development Setup

### Prerequisites

- [Go](https://golang.org/dl/) (version 1.21 or higher)
- [Git](https://git-scm.com/)
- [Docker](https://www.docker.com/) (optional, for testing)

### Build the Project

```bash
go build -o keploy .
```

### Run Tests

```bash
go test ./...
```

## 📝 Making Changes

1. **Write clean code** - Follow Go best practices
2. **Add tests** - Include tests for new features
3. **Update documentation** - Keep docs in sync with changes
4. **Commit with clear messages** - Use conventional commits

### Commit Message Format

```
type: short description

Examples:
- feat: add new recording feature
- fix: resolve connection timeout issue
- docs: update installation guide
- test: add unit tests for parser
```

## 🔄 Submitting a Pull Request

1. **Push your branch**
   ```bash
   git push origin feature/your-feature-name
   ```

2. **Open a Pull Request** on GitHub

3. **Fill out the PR template** with:
   - What changes you made
   - Why you made them
   - How to test them

4. **Wait for review** - Maintainers will review your PR

## 🐛 Reporting Bugs

Found a bug? [Open an issue](https://github.com/keploy/keploy/issues/new) with:

- Clear title and description
- Steps to reproduce
- Expected vs actual behavior
- Environment details (OS, Go version, etc.)

## 💡 Suggesting Features

Have an idea? [Open a feature request](https://github.com/keploy/keploy/issues/new) with:

- Problem you're trying to solve
- Proposed solution
- Alternatives considered

## 🏷️ Good First Issues

New to open source? Look for issues labeled:

- [`good first issue`](https://github.com/keploy/keploy/labels/good%20first%20issue)
- [`help wanted`](https://github.com/keploy/keploy/labels/help%20wanted)

## 📜 Code of Conduct

Please read our [Code of Conduct](CODE_OF_CONDUCT.md) before contributing.

## 💬 Get Help

- 💬 [Slack Community](https://join.slack.com/t/keploy/shared_invite/zt-357qqm9b5-PbZRVu3Yt2rJIa6ofrwWNg)
- 📘 [Documentation](https://keploy.io/docs/)

---

Thank you for contributing to Keploy! 🐰
