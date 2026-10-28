package proxy

import (
	"context"
	"errors"
	"fmt"
	"net"
	"regexp"
	"sync"
	"time"

	pTls "go.keploy.io/server/v3/pkg/agent/proxy/tls"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
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
		re, err := compileRuleHost(rule.Host)
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

// ruleHostRegexps holds each bypassRule host pattern compiled, with its error:
// a session's rules are fixed, so each is compiled once, not on every
// connection.
var ruleHostRegexps sync.Map // pattern -> ruleHostRegexp

type ruleHostRegexp struct {
	re  *regexp.Regexp
	err error
}

// compileRuleHost is regexp.Compile(pattern), compiled once per pattern.
func compileRuleHost(pattern string) (*regexp.Regexp, error) {
	if v, ok := ruleHostRegexps.Load(pattern); ok {
		c := v.(ruleHostRegexp)
		return c.re, c.err
	}
	re, err := regexp.Compile(pattern)
	ruleHostRegexps.Store(pattern, ruleHostRegexp{re: re, err: err})
	return re, err
}

// serverNameOf is pTls.ServerNameOf: a variable so a test can stand in a
// ClientHello keploy could not read whole before its TLS.
var serverNameOf = pTls.ServerNameOf

// hostRulesAt reports whether a bypassRule with a host and no path applies to
// a connection to port (it names the port, or any port): one that
// bypassByServerName may find.
func hostRulesAt(rules []models.BypassRule, port uint) bool {
	for _, rule := range rules {
		if rule.Host != "" && rule.Path == "" && (rule.Port == 0 || rule.Port == port) {
			return true
		}
	}
	return false
}

// callRulesAt reports whether a bypassRule with a host or a path applies to a
// connection to port (it names the port, or any port): one by which the HTTP
// parser may pass some of the connection's calls through
// (utils.IsPassThrough) and record or mock the others.
func callRulesAt(rules []models.BypassRule, port uint) bool {
	for _, rule := range rules {
		if (rule.Host != "" || rule.Path != "") && (rule.Port == 0 || rule.Port == port) {
			return true
		}
	}
	return false
}

// bypassRulesUndecided is the rules for this port (the rules that name it, not
// those for any port) that a connection to it cannot decide, and so passes
// through: one with a host, when the connection shows keploy no host, and one
// with a path, when it shows no path. Keploy sees a host in an HTTP call (its
// Host) and in a TLS connection (the server name, host), and a path only in an
// HTTP call, whose parser decides each call by both (http). A rule whose host
// the connection shows and does not match, or does not compile, names none of
// its calls, so it is decided: not this connection.
//
// Before, the kernel passed every bypassRule's port through whole, whatever
// its host or path. A connection that shows no host or path for such a rule is
// still passed through, so a test set recorded then still replays, and keploy
// says so once for each rule (warnBypassRulesUndecided). A rule a connection
// can decide is applied by its host (and path) alone.
func bypassRulesUndecided(logger *zap.Logger, rules []models.BypassRule, port uint, host string, http bool) []models.BypassRule {
	if http {
		return nil
	}
	var out []models.BypassRule
	for _, rule := range rules {
		if rule.Port == 0 || rule.Port != port || (rule.Host == "" && rule.Path == "") {
			continue
		}
		if rule.Host != "" {
			re, err := compileRuleHost(rule.Host)
			if err != nil {
				utils.LogError(logger, err, "failed to compile the host regex of a bypassRule", zap.String("host", rule.Host))
				continue
			}
			if host != "" && (!re.MatchString(host) || rule.Path == "") {
				// The host decides it: another server's, or this one's
				// whole (bypassByServerName).
				continue
			}
		}
		out = append(out, rule)
	}
	return out
}

// bypassRulesWarned holds each bypassRule warnBypassRulesUndecided has warned
// of.
var bypassRulesWarned sync.Map

// warnBypassRulesUndecided warns, once for each rule, that a bypassRule names
// a host or a path a connection to its port does not show keploy (one to dst,
// whose host, if it showed one, is host), and so passes each such connection
// through: with no host shown, whichever host it is for. The warning names the
// rule and the port-only rule that says so. keploy refuses such a rule at
// start where the port's protocol is known then (config.CheckBypassRules,
// mysqlPorts).
func warnBypassRulesUndecided(logger *zap.Logger, rules []models.BypassRule, dst, host string) {
	for _, rule := range rules {
		key := fmt.Sprintf("%q|%q|%d", rule.Host, rule.Path, rule.Port)
		if _, warned := bypassRulesWarned.LoadOrStore(key, true); warned {
			continue
		}
		ruleText := fmt.Sprintf("{host: %q, path: %q, port: %d}", rule.Host, rule.Path, rule.Port)
		fix := fmt.Sprintf("To pass all of port %d through on purpose, write the rule with the port alone ({port: %d}); to record and mock these calls, remove the rule", rule.Port, rule.Port)
		if host == "" {
			logger.Warn(fmt.Sprintf("the bypassRule %s names a host or a path, and a connection to port %d shows keploy neither (this one, to %s, is not an HTTP/1 call), so keploy passes every such connection to port %d through, unrecorded and unmocked, whichever host it is for. %s",
				ruleText, rule.Port, dst, rule.Port, fix))
			continue
		}
		if rule.Host == "" {
			// A rule with a path alone names every host on its port: the
			// next connection's host is another's.
			logger.Warn(fmt.Sprintf("the bypassRule %s names a path, and a connection to port %d shows keploy none (this one, to %s at %s, is not an HTTP/1 call), so keploy passes every such connection to port %d through whole, unrecorded and unmocked, whichever host it is for. %s",
				ruleText, rule.Port, host, dst, rule.Port, fix))
			continue
		}
		logger.Warn(fmt.Sprintf("the bypassRule %s names a path, and a connection to %s on port %d shows keploy none (this one, to %s, is not an HTTP/1 call), so keploy passes every such connection to %s through whole, unrecorded and unmocked. %s",
			ruleText, host, rule.Port, dst, host, fix))
	}
}

// passBypassRulesUndecided passes the connection through for the bypassRules
// it cannot decide (bypassRulesUndecided), warning of each once: to *dst, or,
// where keploy has not dialled the server (keploy test), to the server dial
// reaches, kept in *dst so the connection's end closes it. host is the host
// the connection showed, "" for none.
func (p *Proxy) passBypassRulesUndecided(ctx context.Context, logger *zap.Logger, src net.Conn, dst *net.Conn, dial func() (net.Conn, error), undecided []models.BypassRule, dstAddr, host string) error {
	warnBypassRulesUndecided(logger, undecided, dstAddr, host)
	if *dst == nil {
		conn, err := dial()
		if err != nil {
			if refusedLocally(err) {
				logger.Debug("nothing is listening at the destination a bypassRule passes the connection through to; the caller sees the connection refused", zap.String("server address", dstAddr))
				return err
			}
			utils.LogError(logger, err, "failed to dial the server a bypassRule passes the connection through to", zap.String("server address", dstAddr), zap.String("next_step", util.NextStepDialDestination))
			return err
		}
		*dst = conn
	}
	return p.globalPassThrough(ctx, src, *dst)
}

// replayLook is what lookUndecidedReplay learned of a keploy test connection.
// Src replaces the client's connection (it replays what the look read of it),
// and Predialed, when not nil, is the server the look reached, for the
// connection's first dial of it. Dst, when not nil, is that server, which
// spoke first: the connection is passed through to it for Undecided.
type replayLook struct {
	Src       net.Conn
	Predialed *util.Predialed
	Dst       net.Conn
	Undecided []models.BypassRule
}

// lookUndecidedReplay looks, in keploy test, at a connection to a port that a
// bypassRule with a host or a path names (bypassRulesUndecided) before it is
// dispatched on its client's bytes. A protocol whose server speaks first (SMTP,
// MySQL on a port keploy does not take for MySQL) shows keploy no host, so
// keploy record passed it through, as the kernel passed the rule's whole port
// before; read for its client's bytes, it would wait for a greeting that keploy,
// with no mock for it, never sends.
//
// So once the test set's mocks are stored, a connection whose client has not
// spoken, to a port at which the recording holds no mock
// (mysqlPortRegistry.recordedAt; a mock that names no port counts only where
// it names no host either, or a host the rules may name: rulesMayName), has
// its server reached and both ends read,
// and whichever speaks first decides, as in a record (probeMysqlRecord):
//
//   - the server, or its closing without a word: passed through (Dst), as
//     keploy record passed it;
//   - the client: dispatched on its bytes, which decide as they show (an HTTP
//     call by its Host and path, a TLS one by its server name), the server
//     reached handed on (Predialed): a connection those bytes leave undecided
//     is passed through on it, so its server sees one connection, and one
//     they decide lets it go then (handleConnection), since what passes a
//     call through dials its own (util.PassThrough). So is a client whose
//     server's first bytes are what the server of a protocol whose client
//     speaks without waiting sends at accept (clientFirstPreamble): it is
//     waited for.
//
// A connection whose client speaks before the mocks are stored, or to a port at
// which the recording holds a mock, is dispatched on its bytes, and its server
// is not reached for the look: a call keploy mocks never reaches its server.
// No clock decides anything. An error is the server not reached, or a read of
// the look's that did not complete; Src is still the client's connection.
func (p *Proxy) lookUndecidedReplay(ctx context.Context, logger *zap.Logger, srcConn net.Conn, dstAddr string, pre *util.Predialed, port uint, rules []models.BypassRule) (replayLook, error) {
	look := replayLook{Src: srcConn}
	// A rule whose host does not compile is said so where the connection is
	// decided (bypassRulesUndecided), not here as well.
	undecided := bypassRulesUndecided(zap.NewNop(), rules, port, "", false)
	// A port known to speak MySQL, as learned or as the recording's MySQL mocks
	// name it, is passed through by the MySQL probe's verdict, unread.
	if len(undecided) == 0 || p.mysqlPorts == nil || p.mysqlPorts.Has(uint32(port)) || !wakeable(srcConn) {
		return look, nil
	}
	client := startFirstRead(srcConn, logger)
	select {
	case <-client.done:
	case <-p.mysqlPorts.derivedSignal():
	case <-ctx.Done():
	}
	var up *serverLook
	if !ended(client.done) && ctx.Err() == nil && !p.mysqlPorts.recordedAt(uint32(port), func(host string) bool { return rulesMayName(undecided, host) }) {
		up = startServerLook(ctx, logger, dstAddr, pre)
		select {
		case <-client.done:
		case <-up.done:
			if up.err == nil && up.dialErr == nil && clientFirstPreamble(up.first) {
				// Its client speaks anyway.
				if testHookLookWaitsForClient != nil {
					testHookLookWaitsForClient()
				}
				select {
				case <-client.done:
				case <-ctx.Done():
				}
			}
		case <-ctx.Done():
		}
	}
	read, readErr := client.stop()
	look.Src = replayConn(srcConn, read, logger)
	if up == nil {
		if errors.Is(readErr, errLookIncomplete) {
			return look, readErr
		}
		return look, nil
	}
	// A client that has gone, or a run that has ended, needs no server: a
	// dial still in flight is abandoned rather than waited for. One that
	// spoke may be passed through yet, by what it showed.
	clientSpoke := len(read) > 0
	clientGone := !clientSpoke && readErr != nil && !isTimeout(readErr)
	conn, first, upErr, dialErr := up.stop(clientGone || ctx.Err() != nil)
	switch {
	case errors.Is(readErr, errLookIncomplete) || errors.Is(upErr, errLookIncomplete) || errors.Is(dialErr, errLookIncomplete):
		// A read panicked (recovered, and logged): what it read is lost.
		if conn != nil {
			_ = conn.Close()
		}
		return look, errLookIncomplete
	case clientSpoke || clientGone || ctx.Err() != nil:
		look.Predialed = handBackLooked(pre, conn, first, dstAddr, logger)
		return look, nil
	case dialErr != nil:
		// Not reached: whichever end would have spoken, the connection
		// cannot be served. It has no mock at this port, and passed
		// through, it has no server.
		return look, dialErr
	}
	look.Dst = replayConn(conn, first, logger)
	look.Undecided = undecided
	return look, nil
}

// rulesMayName reports whether a connection to host may be one that rules, the
// rules a look is for (bypassRulesUndecided), name: one of them names no host
// (a path-only rule names every host on its port), or its host pattern
// matches host. A recording's port-less mock of another host is not at the
// rules' port (mysqlPortRegistry.recordedAt).
func rulesMayName(rules []models.BypassRule, host string) bool {
	for _, rule := range rules {
		if rule.Host == "" {
			return true
		}
		if re, err := compileRuleHost(rule.Host); err == nil && re.MatchString(host) {
			return true
		}
	}
	return false
}

// testHookLookWaitsForClient, set only by a test, is called when
// lookUndecidedReplay, having read what a client-first protocol's server sends
// at accept (clientFirstPreamble), waits for the client: the point after which
// the client's bytes cannot decide the look before the server's did.
var testHookLookWaitsForClient func()

// ended reports whether ch is closed.
func ended(ch <-chan struct{}) bool {
	select {
	case <-ch:
		return true
	default:
		return false
	}
}

// handBackLooked gives conn, the server lookUndecidedReplay reached, to the
// connection's first dial of it, with what the look read of it: back to pre
// when it was pre's (and pre is returned), else as a Predialed of its own
// (util.Adopt). Either way the connection lets it go when nothing dials it
// (handleConnection). nil when the look reached none, or pre could not take
// it back (it is closed then).
func handBackLooked(pre *util.Predialed, conn net.Conn, read []byte, dstAddr string, logger *zap.Logger) *util.Predialed {
	if conn == nil {
		return nil
	}
	var replay net.Conn
	if len(read) > 0 {
		replay = replayConn(conn, read, logger)
	}
	if pre.Is(conn) {
		if !pre.Return(conn, replay) {
			_ = conn.Close()
			return nil
		}
		return pre
	}
	if replay == nil {
		replay = conn
	}
	return util.Adopt(dstAddr, replay)
}

// serverLook reaches a keploy test connection's server and reads what it sends
// first, on a goroutine of its own, with no deadline: until it sends, closes,
// or stop wakes it.
type serverLook struct {
	done   chan struct{}
	cancel context.CancelFunc // abandons a dial still in flight

	mu      sync.Mutex
	conn    net.Conn
	stopped bool

	first   []byte
	err     error // what ended the read
	dialErr error // the server could not be reached
}

// startServerLook reaches dstAddr through pre, the connection dialled while the
// application's handshake was held, or by dialling it (dialProbeUpstream).
func startServerLook(ctx context.Context, logger *zap.Logger, dstAddr string, pre *util.Predialed) *serverLook {
	dialCtx, cancel := context.WithCancel(ctx)
	l := &serverLook{done: make(chan struct{}), cancel: cancel, dialErr: errLookIncomplete}
	go func() {
		defer close(l.done)
		defer util.RecoverWithoutClose(logger)
		conn := pre.Look(dstAddr)
		if conn == nil {
			c, err := dialProbeUpstream(dialCtx, logger, dstAddr)
			if err != nil {
				l.dialErr = err
				return
			}
			conn = c
		}
		l.mu.Lock()
		l.conn, l.dialErr, l.err = conn, nil, errLookIncomplete
		stopped := l.stopped
		l.mu.Unlock()
		if stopped {
			l.err = nil
			return
		}
		buf := make([]byte, 4096)
		n, err := conn.Read(buf)
		l.first, l.err = buf[:n], err
	}()
	return l
}

// stop ends the look if it is still reading, and returns the server, what was
// read from it, what ended the read (nil, or a timeout, for none), and why the
// server could not be reached. A dial in progress is let finish, since the
// connection may be the application's, unless abandon says nobody will use it.
func (l *serverLook) stop(abandon bool) (conn net.Conn, first []byte, err, dialErr error) {
	l.mu.Lock()
	l.stopped = true
	if l.conn != nil {
		_ = l.conn.SetReadDeadline(time.Unix(1, 0))
	}
	l.mu.Unlock()
	if abandon {
		l.cancel()
	}
	<-l.done
	l.cancel()
	if l.conn != nil {
		_ = l.conn.SetReadDeadline(time.Time{})
	}
	if isTimeout(l.err) {
		l.err = nil
	}
	return l.conn, l.first, l.err, l.dialErr
}
