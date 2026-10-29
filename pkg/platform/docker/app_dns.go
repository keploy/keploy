package docker

import (
	"strings"

	"go.keploy.io/server/v3/pkg/models"
)

// agentDNSFlags renders the app's own resolver settings (models.AppDNS) as
// the agent container's `docker run` flags. The app joins the agent
// container's network namespace (`--network=container:<agent>`), where Docker
// refuses DNS servers of the app's own ("conflicting options: dns and the
// network mode") and ignores its search list and options: the app reads the
// /etc/resolv.conf it shares with the agent container. So the agent container
// is started with them, and the app resolves as it does without keploy, and
// keploy's own forwarding on a miss reads the same resolv.conf. Each value is
// one word of the alias (aliasWord). A compose service's dns, dns_search and
// dns_opt are moved onto the keploy-agent service instead
// (modifyAppServiceForKeploy).
func agentDNSFlags(dns models.AppDNS) (string, error) {
	var b strings.Builder
	for _, f := range []struct {
		flag   string
		values []string
	}{
		{"--dns", dns.Servers},
		{"--dns-search", dns.Search},
		{"--dns-option", dns.Options},
	} {
		for _, v := range f.values {
			w, err := aliasWord("the app's "+f.flag, v)
			if err != nil {
				return "", err
			}
			b.WriteString(" " + f.flag + " " + w)
		}
	}
	return b.String(), nil
}
