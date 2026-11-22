package models

import (
	"fmt"
	"slices"
	"strings"
)

// AllowEgressDNS is the test.allowEgress entry (`--allow-egress dns`) that
// lets a replay's DNS mock misses be forwarded to every resolver in the
// agent's resolv.conf, the host's and public ones included. Without it a
// miss is forwarded only to a resolver inside the replay's own network:
// docker's embedded resolver, the container network's own resolver, or the
// cluster DNS of an in-cluster agent. Native replays have no such resolver,
// so there a miss is answered by keploy alone.
const AllowEgressDNS = "dns"

// allowEgressEntries are the test.allowEgress entries keploy acts on. An entry
// outside this set is refused rather than ignored: a list that names a
// destination keploy then quietly does not open would read as an opt-in that
// works.
var allowEgressEntries = []string{AllowEgressDNS}

// NormalizeAllowEgress returns the test.allowEgress list in keploy's own
// spelling of each entry (trimmed, lower case, each once), or an error naming
// the first entry keploy does not act on and the ones it does. Every place an
// entry crosses a process boundary (keploy test's flag, the agent's flag, the
// docker alias) carries the normalized list, so a value never reaches an argv
// keploy did not spell itself.
func NormalizeAllowEgress(entries []string) ([]string, error) {
	var out []string
	for _, e := range entries {
		n := strings.ToLower(strings.TrimSpace(e))
		if !slices.Contains(allowEgressEntries, n) {
			return nil, fmt.Errorf("test.allowEgress entry %q is not one keploy can open; the supported entries are: %s", e, strings.Join(allowEgressEntries, ", "))
		}
		if !slices.Contains(out, n) {
			out = append(out, n)
		}
	}
	return out, nil
}

// AllowsEgress reports whether a normalized test.allowEgress list opens entry.
func AllowsEgress(entries []string, entry string) bool {
	return slices.Contains(entries, entry)
}
