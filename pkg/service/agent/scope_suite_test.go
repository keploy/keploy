package agent

import "testing"

func TestSuiteScopeOf(t *testing.T) {
	cases := map[string]string{
		"projects/atg-tests/pages/generate/components/keploy-agent-drawer.spec.ts > KeployAgentDrawer - Fresh Agent Installation > should show tab selection when nothing installed": "__suite__:projects/atg-tests/pages/generate/components/keploy-agent-drawer.spec.ts",
		"projects/atg-tests/pages/ci-cd/page.spec.ts > CI/CD Page > Empty State > x":                                                                                                 "__suite__:projects/atg-tests/pages/ci-cd/page.spec.ts",
	}
	for in, want := range cases {
		if got := suiteScopeOf(in); got != want {
			t.Errorf("suiteScopeOf(%.40q)\n got %q\nwant %q", in, got, want)
		}
	}
}
