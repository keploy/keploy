package mock

import (
	"strings"
	"testing"

	"go.keploy.io/server/v3/pkg/models"
)

func TestRewritePreTestToolchainMisses(t *testing.T) {
	dnsDefault := "DNS mocks may be missing. Re-record to capture DNS queries."
	cases := []struct {
		name     string
		miss     models.UnmatchedCall
		preTest  bool
		goproxy  string
		wantSub  string
		neverSub string
	}{
		{
			name: "maven DNS miss before any test gets mvn hint, never re-record DNS",
			miss: models.UnmatchedCall{
				Protocol:      "DNS",
				ActualSummary: "A repo.maven.apache.org.",
				NextSteps:     dnsDefault,
			},
			preTest:  true,
			wantSub:  "mvn dependency:go-offline",
			neverSub: "Re-record to capture DNS queries",
		},
		{
			name: "pip DNS miss before any test gets pip hint",
			miss: models.UnmatchedCall{
				Protocol:      "DNS",
				ActualSummary: "A pypi.org.",
				NextSteps:     dnsDefault,
			},
			preTest:  true,
			wantSub:  "pip install",
			neverSub: "Re-record to capture DNS queries",
		},
		{
			name: "nuget DNS miss before any test gets dotnet restore hint",
			miss: models.UnmatchedCall{
				Protocol:      "DNS",
				ActualSummary: "A api.nuget.org.",
				NextSteps:     dnsDefault,
			},
			preTest:  true,
			wantSub:  "dotnet restore",
			neverSub: "Re-record to capture DNS queries",
		},
		{
			name: "gradle DNS miss before any test gets gradlew hint",
			miss: models.UnmatchedCall{
				Protocol:      "DNS",
				ActualSummary: "A services.gradle.org.",
				NextSteps:     dnsDefault,
			},
			preTest:  true,
			wantSub:  "./gradlew",
			neverSub: "Re-record to capture DNS queries",
		},
		{
			name: "npm DNS miss before any test gets npm ci hint",
			miss: models.UnmatchedCall{
				Protocol:      "DNS",
				ActualSummary: "A registry.npmjs.org.",
				NextSteps:     dnsDefault,
			},
			preTest:  true,
			wantSub:  "npm ci",
			neverSub: "Re-record to capture DNS queries",
		},
		{
			name: "custom GOPROXY host gets go mod download hint",
			miss: models.UnmatchedCall{
				Protocol:      "DNS",
				ActualSummary: "A goproxy.internal.corp.",
				NextSteps:     dnsDefault,
			},
			preTest:  true,
			goproxy:  "https://goproxy.internal.corp,direct",
			wantSub:  "go mod download",
			neverSub: "Re-record to capture DNS queries",
		},
		{
			name: "private registry DNS miss before any test suppresses re-record DNS hint",
			miss: models.UnmatchedCall{
				Protocol:      "DNS",
				ActualSummary: "A artifactory.internal.example.",
				NextSteps:     dnsDefault,
			},
			preTest:  true,
			wantSub:  "Pre-fetch dependencies outside replay",
			neverSub: "Re-record to capture DNS queries",
		},
		{
			name: "DNS miss during an actual test keeps original hint untouched",
			miss: models.UnmatchedCall{
				Protocol:      "DNS",
				ActualSummary: "A repo.maven.apache.org.",
				NextSteps:     dnsDefault,
			},
			preTest: false,
			wantSub: "Re-record to capture DNS queries",
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := rewritePreTestToolchainMisses([]models.UnmatchedCall{tc.miss}, tc.preTest, tc.goproxy)
			if len(got) != 1 {
				t.Fatalf("len = %d, want 1", len(got))
			}
			if !strings.Contains(got[0].NextSteps, tc.wantSub) {
				t.Fatalf("NextSteps = %q, want substring %q", got[0].NextSteps, tc.wantSub)
			}
			if tc.neverSub != "" && strings.Contains(got[0].NextSteps, tc.neverSub) {
				t.Fatalf("NextSteps = %q, must not contain %q", got[0].NextSteps, tc.neverSub)
			}
			if strings.Contains(got[0].NextSteps, "`") {
				t.Fatalf("NextSteps = %q, must not contain backticks (withheld by log sanitizer)", got[0].NextSteps)
			}
			if strings.Count(got[0].NextSteps, "'")%2 != 0 {
				t.Fatalf("NextSteps = %q, single quotes must pair cleanly", got[0].NextSteps)
			}
		})
	}
}
