package telemetry

import "testing"

// A remote in git's SCP-like form on an IPv6 host brackets it, as git writes
// it (git@[fd00::1]:owner/repo.git, or [git@fd00::1]:owner/repo.git), and the
// path follows the "]:". Taken from the first colon, which is the address's,
// the repository came out as ":1]:owner/repo".
func TestNormalizeRepoTakesAnIPv6Host(t *testing.T) {
	for _, c := range []struct{ raw, want string }{
		{"git@[fd00::1]:owner/repo.git", "owner/repo"},
		{"git@[::1]:owner/repo", "owner/repo"},
		{"[git@fd00::1]:owner/repo.git", "owner/repo"},
		{"[fd00::1]:owner/repo", ""},
		{"git@[fd00::1]:/owner/repo.git", "owner/repo"},
		{"git@[fd00::1]", ""},
		{"git@[fd00::1]/owner/repo", ""},
		{"git@[fd00::1]:repo", ""},
		{"ssh://git@[fd00::1]:2222/owner/repo.git", "owner/repo"},
		{"git@github.com:owner/repo.git", "owner/repo"},
		{"git@10.0.0.1:owner/repo.git", "owner/repo"},
		{"https://github.com/owner/repo.git", "owner/repo"},
		{"owner/repo", "owner/repo"},
	} {
		if got := normalizeRepo(c.raw); got != c.want {
			t.Errorf("normalizeRepo(%q) = %q, want %q", c.raw, got, c.want)
		}
	}
}
