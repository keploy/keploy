package proxy

import (
	"context"
	"math"
	"net"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/redisskip"
	"go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// Redis in a keploy that skips Redis (integrations.SkipsRedis: this build,
// unless Keploy Enterprise links its parser or claims Redis): a
// connection whose client opens with a Redis command (redisskip.Match) is
// relayed to the real Redis, in record and in test mode, and nothing of it is
// recorded or mocked. Redis mocks a test set holds (an earlier keploy's,
// through the Generic parser, or Keploy Enterprise's) are therefore never
// served either: the agent leaves them out of the set's pool, and keploy test
// holds no test to them (redisskip.Recorded), so a test set recorded with
// them needs its Redis running at replay, as it does at record. The user is told
// once per destination (integrations.RedisSkipNotice), with where to get
// Redis recorded and mocked.

// redisSkipSaid is what the Redis skip says once per destination: the
// notice, and that a replay could not reach a Redis.
type redisSkipSaid struct {
	// skipped says the RedisSkipNotice: a client's pool opens many
	// connections to one Redis, and each would otherwise repeat it.
	skipped redisskip.Notices
	// unreachable says, once, that a replay could not reach a Redis: with
	// Redis no longer mocked, a test set whose Redis is down at replay fails
	// on every Redis call, and a pool retries, so the reason is said once,
	// loud, and every later failure at Debug.
	unreachable redisskip.Notices
}

// processRedisSkipSaid is every proxy's redisSaid (New): the notice is said
// once per destination for the whole process, whichever Proxy serves the
// connection, and keploy's agent is one process per keploy record or keploy
// test.
var processRedisSkipSaid = &redisSkipSaid{}

// said is p's redisSaid, the process's for a Proxy not made by New.
func (p *Proxy) said() *redisSkipSaid {
	if p.redisSaid != nil {
		return p.redisSaid
	}
	return processRedisSkipSaid
}

// redisSkipPriority places the skip after every registered parser, whatever
// its priority: a parser that claims the connection's first bytes keeps it,
// and the skip takes only what would otherwise have gone to the Generic
// parser.
const redisSkipPriority = math.MinInt

// installRedisSkip installs the Redis skip among p's parsers in a build that
// skips Redis (integrations.SkipsRedis): one that links no Redis parser
// (nothing registered as integrations.REDIS) and that no package claimed
// Redis for (integrations.ClaimRedis, which Keploy Enterprise does on every
// platform, its Redis parser linked there or not). It is called from
// InitIntegrations, once every package's init has run, so whether the skip
// is installed depends only on what the build links, never on the order in
// which packages registered their parsers or claimed Redis. The skip is p's
// own: it is not put in integrations.Registered.
func (p *Proxy) installRedisSkip() {
	if !integrations.SkipsRedis() {
		return
	}
	p.redisSkipOn.Store(true)
	if _, ok := p.Integrations[integrations.REDIS_SKIP]; ok {
		return
	}
	p.Integrations[integrations.REDIS_SKIP] = redisskip.New(p.logger)
	p.integrationsPriority = append(p.integrationsPriority, ParserPriority{Priority: redisSkipPriority, ParserType: integrations.REDIS_SKIP})
}

// redisUnreachable is a replay's failure to reach a Redis the skip relays
// to, which serveRedisSkip has said already (at Error the first time for its
// destination, at Debug after): the accept loop logs it at Debug, so a pool
// that reconnects to a Redis that is down does not log an error each time.
type redisUnreachable struct{ err error }

func (e redisUnreachable) Error() string { return e.err.Error() }

func (e redisUnreachable) Unwrap() error { return e.err }

// serveRedisSkip relays a connection the Redis skip matched to the real
// Redis, raw, and records nothing of it. *dst is the connection to the
// destination when one was dialled before dispatch (a record); otherwise (a
// replay, which dials nothing before dispatch) dial reaches the real Redis
// and *dst is set to what it returns, so the caller's deferred close closes
// it; dial also returns the address it dialled (liveAddr's), which the error
// names. The notice is said before the dial, so a replay whose Redis is down
// still tells the user why keploy needs it up.
//
// util.RelayRawPassthrough, not globalPassThrough: it forwards a client's
// half-close (FIN) on a clean EOF and lets the surviving direction finish,
// which a pass-through has to (see relayDeclinedConn).
//
// A replay's dial goes to the destination itself: a connection that reached
// its Redis through an HTTP CONNECT proxy at record is dialled direct at
// replay, where keploy answered the CONNECT itself (connect.go).
func (p *Proxy) serveRedisSkip(ctx context.Context, logger *zap.Logger, dstAddr string, src net.Conn, dst *net.Conn, dial func() (net.Conn, string, error)) error {
	said := p.said()
	said.skipped.Log(p.logger, dstAddr)
	if *dst == nil {
		c, dialled, err := dial()
		if err != nil {
			// The connection or the agent ending (its context) is no
			// failure to reach the Redis, and must not use up the one
			// error said for it. The dial touches nothing else of theirs,
			// so anything else that fails it is the Redis's, or the way
			// to it: a reset is the destination's.
			if ctx.Err() != nil {
				logger.Debug("the Redis connection ended before keploy reached its server", zap.String("server address", dstAddr), zap.Error(err))
				return err
			}
			if !said.unreachable.First(dstAddr) {
				logger.Debug("could not connect to the Redis this connection is for", zap.String("server address", dstAddr), zap.String("dialled", dialled), zap.Error(err))
				return redisUnreachable{err}
			}
			utils.LogError(p.logger, err, "keploy could not connect to a Redis the application calls",
				zap.String("destination", dstAddr),
				zap.String("dialled", dialled),
				zap.String("next_step", "this keploy neither records nor mocks Redis, and serves no Redis mock a test set holds: "+
					"the application's Redis must be running, reachable from keploy and, over TLS, accept keploy's connection during keploy test, as during keploy record. "+
					"To record and mock Redis, use Keploy Enterprise: "+integrations.RedisEnterpriseURL))
			return redisUnreachable{err}
		}
		*dst = c
		// connCloser, started before dispatch, took no destination to
		// close on the connection's cancellation: this one is closed then
		// too, so a relay blocked reading it ends with the connection.
		stop := context.AfterFunc(ctx, func() { _ = c.Close() })
		defer stop()
	}
	logger.Debug("relaying a Redis connection to the real server; nothing of it is recorded or mocked",
		zap.String("parser", string(integrations.REDIS_SKIP)))
	util.RelayRawPassthrough(src, *dst)
	return nil
}
