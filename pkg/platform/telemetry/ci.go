package telemetry

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"
)

// ciProviders maps environment variables to CI provider names.
// More specific checks come first.
var ciProviders = []struct {
	envVar   string
	provider string
}{
	{"GITHUB_ACTIONS", "github-actions"},
	{"GITLAB_CI", "gitlab-ci"},
	{"JENKINS_URL", "jenkins"},
	{"CIRCLECI", "circleci"},
	{"TRAVIS", "travis-ci"},
	{"BUILDKITE", "buildkite"},
	{"TF_BUILD", "azure-pipelines"},
	{"BITBUCKET_PIPELINE_UUID", "bitbucket"},
	{"CODEBUILD_BUILD_ID", "aws-codebuild"},
	{"TEAMCITY_VERSION", "teamcity"},
	{"DRONE", "drone"},
	{"HARNESS_BUILD_ID", "harness"},
	{"WOODPECKER", "woodpecker"},
	{"SEMAPHORE", "semaphore"},
}

// CI-specific env vars that hold the repository slug (owner/repo).
var ciRepoVars = []string{
	"GITHUB_REPOSITORY",        // github-actions: "owner/repo"
	"CI_PROJECT_PATH",          // gitlab-ci:      "group/project"
	"TRAVIS_REPO_SLUG",         // travis-ci:      "owner/repo"
	"CIRCLE_PROJECT_REPONAME",  // circleci:       "repo" (no owner)
	"BUILDKITE_REPO",           // buildkite:      git URL
	"BITBUCKET_REPO_FULL_NAME", // bitbucket:      "owner/repo"
	"BUILD_REPOSITORY_NAME",    // azure-pipelines
	"DRONE_REPO",               // drone:          "owner/repo"
	"CI_REPO",                  // woodpecker:     "owner/repo"
	"SEMAPHORE_GIT_REPO_SLUG",  // semaphore:      "owner/repo"
	"HARNESS_REPO",             // harness
}

// detectCI returns whether the process is running inside a CI environment
// and the name of the CI provider (empty string if unknown or not CI).
func detectCI() (bool, string) {
	for _, p := range ciProviders {
		if os.Getenv(p.envVar) != "" {
			return true, p.provider
		}
	}
	if os.Getenv("CI") != "" {
		return true, "unknown"
	}
	return false, ""
}

// gitRepoOnce caches the result of the (potentially slow) git command.
var gitRepoOnce struct {
	sync.Once
	repo string
}

// detectGitRepo returns the normalized git repository slug (e.g. "keploy/keploy").
// It first checks CI-specific env vars, then falls back to parsing the local
// .git remote origin URL. Returns "" if nothing is found.
// This function never panics or blocks for more than 2 seconds.
func detectGitRepo() string {
	// 1. Try CI env vars first (fast, always available in CI)
	for _, v := range ciRepoVars {
		if val := os.Getenv(v); val != "" {
			return normalizeRepo(val)
		}
	}

	// 2. Fall back to local .git (cached; runs git only once per process)
	gitRepoOnce.Do(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, "git", "config", "--get", "remote.origin.url")
		cmd.Stderr = nil // suppress any stderr output
		out, err := cmd.Output()
		if err == nil {
			gitRepoOnce.repo = normalizeRepo(strings.TrimSpace(string(out)))
		}
	})
	return gitRepoOnce.repo
}

// normalizeRepo extracts "owner/repo" from various formats:
//   - "git@github.com:owner/repo.git"
//   - "https://github.com/owner/repo.git"
//   - "owner/repo"
func normalizeRepo(raw string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return ""
	}
	// Strip .git suffix
	raw = strings.TrimSuffix(raw, ".git")

	// SSH format: git@host:owner/repo
	if idx := strings.Index(raw, ":"); idx > 0 && strings.Contains(raw[:idx], "@") {
		return raw[idx+1:]
	}
	// HTTPS format: https://host/owner/repo
	if strings.HasPrefix(raw, "http://") || strings.HasPrefix(raw, "https://") {
		parts := strings.SplitN(raw, "/", 4) // ["https:", "", "host", "owner/repo"]
		if len(parts) >= 4 {
			return parts[3]
		}
	}
	// Already "owner/repo" or just a name
	return raw
}
