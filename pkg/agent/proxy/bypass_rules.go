package proxy

import (
	"fmt"
	"regexp"
	"sync"

	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// bypassByServerName is the bypassRule, if any, that passes a TLS connection
// through whole: a rule with a host and no path, whose host pattern matches the
// server name the client asked for (its SNI, else its CONNECT target), on the
// connection's port or on any port (0). A rule with a path is left to the HTTP
// parser, which sees each call's path. The kernel passes through only the
// port-only rules' ports (config.GetByPassPorts), so a rule with a host is
// applied here and in the HTTP parser, and never to the port's other hosts.
func bypassByServerName(logger *zap.Logger, rules []models.BypassRule, serverName string, port uint) (models.BypassRule, bool) {
	if serverName == "" {
		return models.BypassRule{}, false
	}
	for _, rule := range rules {
		if rule.Host == "" || rule.Path != "" || (rule.Port != 0 && rule.Port != port) {
			continue
		}
		re, err := regexp.Compile(rule.Host)
		if err != nil {
			utils.LogError(logger, err, "failed to compile the host regex of a bypassRule", zap.String("host", rule.Host))
			continue
		}
		if re.MatchString(serverName) {
			return rule, true
		}
	}
	return models.BypassRule{}, false
}

// bypassRulesUnseen is the rules for this port (the rules that name it, not
// those for any port) that a connection to it cannot apply: one with a host,
// when the connection shows keploy no host, and one with a path, when it shows
// no path. Keploy sees a host in an HTTP call (its Host) and in a TLS
// connection (the server name), and a path only in an HTTP call.
func bypassRulesUnseen(rules []models.BypassRule, port uint, hostSeen, pathSeen bool) []models.BypassRule {
	var out []models.BypassRule
	for _, rule := range rules {
		if rule.Port != port {
			continue
		}
		if (rule.Host != "" && !hostSeen) || (rule.Path != "" && !pathSeen) {
			out = append(out, rule)
		}
	}
	return out
}

// bypassRulesWarned holds each bypassRule warnBypassRulesUnseen has warned of.
var bypassRulesWarned sync.Map

// warnBypassRulesUnseen warns, once for each rule, of the bypassRules for port
// that a connection to dst cannot apply (bypassRulesUnseen). The connection's
// calls are recorded or mocked like any other: the rule names some of the
// port's calls, and passing the whole port through would pass calls it does not
// name. The warning names the rule and the port-only rule that passes the whole
// port through. keploy refuses such a rule at start where the port's protocol
// is known then (config.CheckBypassRules, mysqlPorts).
func warnBypassRulesUnseen(logger *zap.Logger, rules []models.BypassRule, port uint, dst string, hostSeen, pathSeen bool) {
	for _, rule := range bypassRulesUnseen(rules, port, hostSeen, pathSeen) {
		key := fmt.Sprintf("%q|%q|%d", rule.Host, rule.Path, rule.Port)
		if _, warned := bypassRulesWarned.LoadOrStore(key, true); warned {
			continue
		}
		logger.Warn(fmt.Sprintf("the bypassRule {host: %q, path: %q, port: %d} cannot pass the calls to %s through: the connection shows keploy no host or path to match, so its calls are recorded or mocked. To pass every call to port %d through, use a rule with the port alone ({port: %d})",
			rule.Host, rule.Path, rule.Port, dst, rule.Port, rule.Port))
	}
}
