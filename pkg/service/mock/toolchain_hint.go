package mock

import (
	"net/url"
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

type toolchainRegistry struct {
	hosts []string
	hint  string
}

var toolchainRegistries = []toolchainRegistry{
	{
		hosts: []string{"proxy.golang.org", "sum.golang.org", "goproxy.io", "goproxy.cn", "proxy.golang.com.cn", "go.dev", "golang.org"},
		hint:  "A Go module or toolchain lookup failed before any test began under replay isolation. Run 'go mod download' before running 'keploy mock replay', then replay again.",
	},
	{
		hosts: []string{"repo.maven.apache.org", "repo1.maven.org"},
		hint:  "A Maven dependency lookup failed before any test began under replay isolation. Run 'mvn -DskipTests=true test' (or 'mvn dependency:go-offline') before running 'keploy mock replay', then replay again.",
	},
	{
		hosts: []string{"services.gradle.org", "plugins.gradle.org", "downloads.gradle.org"},
		hint:  "A Gradle dependency lookup failed before any test began under replay isolation. Run './gradlew testClasses' (or './gradlew dependencies') before running 'keploy mock replay', then replay again.",
	},
	{
		hosts: []string{"registry.npmjs.org", "registry.yarnpkg.com", "registry.npmmirror.com", "npm.pkg.github.com", "nodejs.org"},
		hint:  "An npm/Node package lookup failed before any test began under replay isolation. Install dependencies first ('npm ci', 'pnpm install --frozen-lockfile', or 'yarn install --immutable') before running 'keploy mock replay', then replay again.",
	},
	{
		hosts: []string{"pypi.org", "files.pythonhosted.org"},
		hint:  "A Python/pip package lookup failed before any test began under replay isolation. Install dependencies first ('pip install -r requirements.txt' or 'uv sync') before running 'keploy mock replay', then replay again.",
	},
	{
		hosts: []string{"api.nuget.org", "nuget.org"},
		hint:  "A NuGet/.NET package lookup failed before any test began under replay isolation. Run 'dotnet restore' (or 'dotnet build') before running 'keploy mock replay', then replay again.",
	},
	{
		hosts: []string{"playwright.azureedge.net", "playwright-akamai.azureedge.net", "playwright-verizon.azureedge.net", "playwright.download.prss.microsoft.com", "cdn.playwright.dev"},
		hint:  "A Playwright browser download failed before any test began under replay isolation. Run 'npx playwright install' before running 'keploy mock replay', then replay again.",
	},
	{
		hosts: []string{"repo.yarnpkg.com"},
		hint:  "A Corepack package-manager lookup failed before any test began under replay isolation. Run 'corepack install' before running 'keploy mock replay', then replay again.",
	},
}

const genericPreTestDNSHint = "A DNS lookup failed before any test began (likely a build or dependency fetch under replay isolation; re-recording will not fix a build-time fetch). Pre-fetch dependencies outside replay before running 'keploy mock replay', then replay again."

// rewritePreTestToolchainMisses replaces misleading "Re-record to capture DNS
// queries" or HTTP "re-record the mock set" advice when the test command failed
// before any test began and missed a build/registry lookup.
func rewritePreTestToolchainMisses(misses []models.UnmatchedCall, preTestFailed bool, goproxy string) []models.UnmatchedCall {
	if !preTestFailed || len(misses) == 0 {
		return misses
	}
	var customGoProxies []string
	for _, p := range strings.FieldsFunc(goproxy, func(r rune) bool { return r == ',' || r == '|' }) {
		if u, err := url.Parse(strings.TrimSpace(p)); err == nil && u.Hostname() != "" {
			customGoProxies = append(customGoProxies, strings.ToLower(u.Hostname()))
		}
	}
	out := make([]models.UnmatchedCall, len(misses))
	for i, miss := range misses {
		hint := preTestMissHint(miss, customGoProxies)
		if hint != "" {
			miss.NextSteps = hint
			if miss.Whole != nil {
				w := *miss.Whole
				w.NextStep = hint
				miss.Whole = &w
			}
		}
		out[i] = miss
	}
	return out
}

func preTestMissHint(miss models.UnmatchedCall, customGoProxies []string) string {
	var parts []string
	parts = append(parts, miss.ActualSummary, miss.Destination, miss.Operation)
	if miss.Whole != nil {
		parts = append(parts, miss.Whole.Call)
	}
	text := strings.ToLower(strings.Join(parts, " "))
	for _, host := range customGoProxies {
		if containsHostToken(text, host) {
			return toolchainRegistries[0].hint
		}
	}
	for _, reg := range toolchainRegistries {
		for _, host := range reg.hosts {
			if containsHostToken(text, host) {
				return reg.hint
			}
		}
	}
	if strings.EqualFold(miss.Protocol, "DNS") {
		return genericPreTestDNSHint
	}
	return ""
}

func containsHostToken(text, host string) bool {
	if host == "" {
		return false
	}
	for i := 0; ; {
		j := strings.Index(text[i:], host)
		if j < 0 {
			return false
		}
		at, end := i+j, i+j+len(host)
		before := at == 0 || !isHostChar(text[at-1]) || text[at-1] == '.'
		// DNS FQDNs end with a trailing dot (e.g. "A pypi.org.").
		after := end == len(text) || !isHostChar(text[end]) || (text[end] == '.' && (end+1 == len(text) || !isHostChar(text[end+1])))
		if before && after {
			return true
		}
		i = at + 1
	}
}

func isHostChar(c byte) bool {
	return c == '.' || c == '-' || (c >= 'a' && c <= 'z') || (c >= '0' && c <= '9')
}
