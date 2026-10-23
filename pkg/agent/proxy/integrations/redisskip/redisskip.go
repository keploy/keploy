// Package redisskip is what a keploy that skips Redis does with Redis:
// it recognises a Redis client by the first frame it sends, so that the
// proxy relays the connection to the real Redis instead of recording or
// mocking it, and it says so once per destination (RedisSkipNotice).
//
// Keploy Enterprise records and mocks Redis with a parser of its own,
// registered as integrations.REDIS, and claims Redis on every platform
// (integrations.ClaimRedis), its parser linked there or not. This package
// registers nothing: the proxy installs its Skip under
// integrations.REDIS_SKIP only in a build that skips Redis
// (integrations.SkipsRedis; pkg/agent/proxy, InitIntegrations).
//
// # Why the match is strict
//
// A connection this package claims is relayed raw and nothing of it is
// captured. A false positive is therefore silent capture loss on a
// connection keploy could have recorded, which is worse than a miss (a
// miss is recorded by the Generic parser, as before). So a connection is
// claimed only when its client's first frame is, byte for byte, the start
// of a RESP command array:
//
//	*<n>\r\n$<len>\r\n<command>\r\n
//
// with <n> and <len> decimal, no sign, no leading zero, <len> equal to the
// length of <command>, and <command> (in any case) one of the Redis command
// names in this package's table. A RESP type byte alone is not enough:
// Enterprise learned that a first-byte check hijacks Mongo, MySQL and gRPC
// streams whose first byte happens to be one. The destination port is not
// consulted, either way: Redis on another port is still Redis, and
// something else on 6379 is not.
//
// What it misses, deliberately: inline commands ("PING\r\n", what a telnet
// session sends; no client library does), a first frame cut before its
// command name's CRLF, and commands outside the table (a module's). Those
// connections go to the Generic parser, as every Redis connection did before
// this package. A cut first frame is not a matter of load: the frame's head
// is a few dozen bytes that every client library writes in one write (a
// command, or a pipeline, is flushed whole), and one write that small reaches
// the proxy's first read whole. Reading on until a frame's head is complete
// was not done because a connection that only looks like the start of one
// (a 0x2a byte, then its client waiting for an answer) would then hang.
// The miss is loud either way: a connection skipped at record and missed at
// replay finds no mock, and one missed at record and skipped at replay
// reaches the real Redis.
package redisskip

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"net"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

const (
	// maxArgs bounds a first command's argument count. Redis itself takes
	// up to INT_MAX; a bound this size keeps a first command with many
	// keys (MSET, DEL, RPUSH) while the number stays a handful of digits.
	maxArgs = 1 << 20
	// maxNameLen bounds a command name's length: the longest in the table
	// is 20 bytes (GEORADIUSBYMEMBER_RO, TDIGEST.TRIMMED_MEAN).
	maxNameLen = 32
)

// Match reports whether buf, the first bytes a client sent on a connection
// (decrypted, on a TLS one), opens with a Redis command: a RESP array header
// whose first element is a bulk string naming a command of the table. See
// the package comment for what it deliberately does not match.
func Match(buf []byte) bool {
	if len(buf) == 0 || buf[0] != '*' {
		return false
	}
	n, i, ok := decimal(buf, 1)
	if !ok || n < 1 || n > maxArgs || !crlfAt(buf, i) {
		return false
	}
	i += 2
	if i >= len(buf) || buf[i] != '$' {
		return false
	}
	l, j, ok := decimal(buf, i+1)
	if !ok || l < 1 || l > maxNameLen || !crlfAt(buf, j) {
		return false
	}
	j += 2
	if len(buf) < j+l+2 || !crlfAt(buf, j+l) {
		return false
	}
	return isCommand(buf[j : j+l])
}

// decimal reads the decimal number at buf[i:]: at least one digit, no sign,
// no leading zero, at most 7 digits (more than maxArgs needs). It returns
// the number, the index just past it, and whether there was one.
func decimal(buf []byte, i int) (int, int, bool) {
	const maxDigits = 7
	start := i
	n := 0
	for i < len(buf) && buf[i] >= '0' && buf[i] <= '9' {
		if i-start == maxDigits {
			return 0, 0, false
		}
		n = n*10 + int(buf[i]-'0')
		i++
	}
	if i == start || (buf[start] == '0' && i-start > 1) {
		return 0, 0, false
	}
	return n, i, true
}

func crlfAt(buf []byte, i int) bool {
	return i+1 < len(buf) && buf[i] == '\r' && buf[i+1] == '\n'
}

// isCommand reports whether name, in any case, is a command of the table.
func isCommand(name []byte) bool {
	var up [maxNameLen]byte
	for k, c := range name {
		if c >= 'a' && c <= 'z' {
			c -= 'a' - 'A'
		}
		up[k] = c
	}
	_, ok := commands[string(up[:len(name)])]
	return ok
}

// IsRecordedRedis reports whether m is a Generic mock whose first request
// opens with a Redis command (Match), its data as the Generic parser stores
// it (the bytes when they are ASCII, their base64 when not): an exchange of
// Redis traffic as a keploy without a Redis parser and without this skip
// recorded it, one mock per exchange. Recorded judges a connection by its
// first one.
func IsRecordedRedis(m *models.Mock) bool {
	if m == nil || m.Kind != models.GENERIC || len(m.Spec.GenericRequests) == 0 {
		return false
	}
	req := m.Spec.GenericRequests[0]
	if req.Origin == models.FromServer || len(req.Message) == 0 {
		return false
	}
	// The Generic parser matches by data alone, raw when the live bytes are
	// ASCII, base64 when not, and so is this read: raw first (base64 has no
	// '*', so base64 data never matches raw), then decoded, whatever the
	// payload's type says.
	// Match reads no more than a frame's head, so only the head is read:
	// headLen bytes raw, and the base64 that encodes at least as many.
	data := req.Message[0].Data
	if Match([]byte(data[:min(len(data), headLen)])) {
		return true
	}
	b, err := base64.StdEncoding.DecodeString(data[:min(len(data), base64.StdEncoding.EncodedLen(headLen))])
	return err == nil && Match(b)
}

// headLen bounds what Match reads of a frame: '*', maxArgs' digits, CRLF,
// '$', a name length's digits, CRLF, the longest name and its CRLF, with
// room to spare.
const headLen = 64

// LeftOutNotice is what the agent of a build that skips Redis says, at Warn
// once per process (the first stored test set that has any), with how many,
// of a set's Redis mocks it leaves out of the replay (LeftOut).
const LeftOutNotice = "keploy leaves this test set's Redis mocks out of its replay: " +
	"this keploy relays Redis to the real server instead, so the test set needs its Redis running while it is replayed. " +
	"To record and mock Redis, use Keploy Enterprise: " + integrations.RedisEnterpriseURL

// Recorded reads a test set's mocks, one at a time, for those this build
// leaves out of a replay (LeftOut): the mocks of Redis traffic, in a build
// that skips Redis (integrations.SkipsRedis), whose proxy relays Redis to the
// real server. The agent reads the set so, leaves those mocks out of the pool
// it serves, and names them in its answer to the store
// (models.StoreMocksResp): keploy test holds no test to consuming one, the
// call each recorded being made for real, by what the agent said, never by a
// reading of its own. The two ends need not be one build: a keploy test on
// macOS or Windows drives an agent in a Linux container, and an agent image
// may be of another release than its keploy test.
//
// A Generic mock is judged by its connection (Metadata["connID"]), as the
// proxy judges a connection: by its first frame. A connection whose first
// exchange opens with a Redis command (IsRecordedRedis) is skipped at replay,
// and every mock of it is left out; one whose first exchange does not (a
// command outside the table, an inline one) is served by the Generic parser
// in every exchange, the Redis commands after the first included, and none
// of its mocks is left out. A Generic mock that names no connection is
// judged alone, and a mock of Keploy Enterprise's Redis parser (models.REDIS)
// is left out.
//
// NewRecorded returns nil in a build that does not skip Redis: a nil
// Recorded leaves nothing out.
type Recorded struct {
	// first is each connection's first exchange.
	first map[string]exchange
	// conn is the connection of each Generic mock that names one.
	conn map[string]string
	// alone is the mocks left out on their own.
	alone map[string]bool
}

type exchange struct {
	at    time.Time
	name  string
	redis bool
}

// NewRecorded is a Recorded for this build: nil when it does not skip Redis
// (integrations.SkipsRedis).
func NewRecorded() *Recorded {
	if !integrations.SkipsRedis() {
		return nil
	}
	return &Recorded{first: map[string]exchange{}, conn: map[string]string{}, alone: map[string]bool{}}
}

// Add reads m.
func (r *Recorded) Add(m *models.Mock) {
	if r == nil || m == nil {
		return
	}
	switch m.Kind {
	case models.REDIS:
		r.alone[m.Name] = true
		return
	case models.GENERIC:
	default:
		return
	}
	id := m.Spec.Metadata["connID"]
	if id == "" {
		if IsRecordedRedis(m) {
			r.alone[m.Name] = true
		}
		return
	}
	r.conn[m.Name] = id
	at := m.Spec.ReqTimestampMock
	if f, seen := r.first[id]; !seen || at.Before(f.at) || (at.Equal(f.at) && nameBefore(m.Name, f.name)) {
		r.first[id] = exchange{at: at, name: m.Name, redis: IsRecordedRedis(m)}
	}
}

// LeftOut is the names of the mocks read that this build leaves out of a
// replay; nil when none is.
func (r *Recorded) LeftOut() map[string]bool {
	if r == nil {
		return nil
	}
	out := map[string]bool{}
	for n := range r.alone {
		out[n] = true
	}
	for n, id := range r.conn {
		if r.first[id].redis {
			out[n] = true
		}
	}
	if len(out) == 0 {
		return nil
	}
	return out
}

// LeftOutOf is the names of the mocks of lists, a test set's, that this
// build leaves out of a replay (Recorded).
func LeftOutOf(lists ...[]*models.Mock) map[string]bool {
	r := NewRecorded()
	for _, l := range lists {
		for _, m := range l {
			r.Add(m)
		}
	}
	return r.LeftOut()
}

// SortNames sorts mock names as recorded: mock-2 before mock-10.
func SortNames(names []string) {
	sort.Slice(names, func(i, j int) bool { return nameBefore(names[i], names[j]) })
}

// nameBefore orders mock names as recorded: mock-2 before mock-10.
func nameBefore(a, b string) bool {
	pa, na, oka := splitName(a)
	pb, nb, okb := splitName(b)
	if oka && okb && pa == pb {
		return na < nb
	}
	return a < b
}

func splitName(s string) (string, int, bool) {
	i := strings.LastIndexByte(s, '-')
	if i < 0 {
		return "", 0, false
	}
	n, err := strconv.Atoi(s[i+1:])
	if err != nil {
		return "", 0, false
	}
	return s[:i], n, true
}

// Skip is the integration the proxy installs as integrations.REDIS_SKIP. Its
// MatchType is Match; the proxy serves what it matches itself (relays it to
// the real Redis), so its RecordOutgoing and MockOutgoing are never the way
// a matched connection is served.
type Skip struct{}

// New returns the Skip, in the shape of an integrations.Initializer.
func New(*zap.Logger) integrations.Integrations { return Skip{} }

// MatchType reports whether the connection's first bytes open with a Redis
// command (Match).
func (Skip) MatchType(_ context.Context, buf []byte) bool { return Match(buf) }

// RecordOutgoing declines: a dispatcher that reaches it relays the
// connection raw, which is what a skipped Redis connection is owed.
func (Skip) RecordOutgoing(context.Context, *integrations.RecordSession) error {
	return fmt.Errorf("redis is recorded only by Keploy Enterprise: %w", integrations.ErrParserDeclined)
}

// MockOutgoing has nothing to serve a Redis connection from: this build
// keeps no Redis mocks, and the proxy relays a skipped connection to the
// real Redis before any parser would be asked.
func (Skip) MockOutgoing(context.Context, net.Conn, *models.ConditionalDstCfg, integrations.MockMemDb, models.OutgoingOptions) error {
	return errors.New("redis is mocked only by Keploy Enterprise; this keploy relays Redis to the real server instead")
}

// Notices says the RedisSkipNotice once per destination: a client's pool
// opens many connections to one Redis, and each would otherwise repeat it.
// The zero value is ready to use.
type Notices struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

// Log logs the RedisSkipNotice at Warn, with the destination, the first
// time it is called for dst, and reports whether it did.
func (n *Notices) Log(logger *zap.Logger, dst string) bool {
	if !n.First(dst) {
		return false
	}
	logger.Warn(integrations.RedisSkipNotice, zap.String("destination", dst))
	return true
}

// First reports whether this is the first call for dst: true once per
// destination, however many callers ask at once.
func (n *Notices) First(dst string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if _, ok := n.seen[dst]; ok {
		return false
	}
	if n.seen == nil {
		n.seen = make(map[string]struct{})
	}
	n.seen[dst] = struct{}{}
	return true
}

// commands are the Redis command names, upper case: Redis 7.4's and 8's
// core commands, and the commands of the data types Redis 8 ships (JSON,
// search and query, time series, the probabilistic ones). A name outside
// it makes Match miss, so it is the strict half of the match: no other
// protocol's first frame is a RESP array naming one of these.
var commands = func() map[string]struct{} {
	m := make(map[string]struct{}, len(commandNames))
	for _, c := range commandNames {
		if len(c) > maxNameLen {
			panic("redisskip: command name longer than maxNameLen: " + c)
		}
		m[c] = struct{}{}
	}
	return m
}()

var commandNames = []string{
	// Connection, server and cluster.
	"ACL", "ASKING", "AUTH", "BGREWRITEAOF", "BGSAVE", "CLIENT", "CLUSTER",
	"COMMAND", "CONFIG", "DBSIZE", "DEBUG", "ECHO", "FAILOVER", "FLUSHALL",
	"FLUSHDB", "HELLO", "INFO", "LASTSAVE", "LATENCY", "LOLWUT", "MEMORY",
	"MODULE", "MONITOR", "PING", "PSYNC", "QUIT", "READONLY", "READWRITE",
	"REPLCONF", "REPLICAOF", "RESET", "ROLE", "SAVE", "SELECT", "SENTINEL",
	"SHUTDOWN", "SLAVEOF", "SLOWLOG", "SWAPDB", "SYNC", "TIME", "WAIT",
	"WAITAOF",
	// Keys.
	"COPY", "DEL", "DELEX", "DIGEST", "DUMP", "EXISTS", "EXPIRE", "EXPIREAT",
	"EXPIRETIME", "KEYS", "MIGRATE", "MOVE", "OBJECT", "PERSIST", "PEXPIRE",
	"PEXPIREAT", "PEXPIRETIME", "PTTL", "RANDOMKEY", "RENAME", "RENAMENX",
	"RESTORE", "RESTORE-ASKING", "SCAN", "SORT", "SORT_RO", "TOUCH", "TTL",
	"TYPE", "UNLINK",
	// Strings and bitmaps.
	"APPEND", "BITCOUNT", "BITFIELD", "BITFIELD_RO", "BITOP", "BITPOS",
	"DECR", "DECRBY", "GET", "GETBIT", "GETDEL", "GETEX", "GETRANGE",
	"GETSET", "INCR", "INCRBY", "INCRBYFLOAT", "LCS", "MGET", "MSET",
	"MSETEX", "MSETNX", "PSETEX", "SET", "SETBIT", "SETEX", "SETNX",
	"SETRANGE", "STRLEN", "SUBSTR",
	// Hashes.
	"HDEL", "HEXISTS", "HEXPIRE", "HEXPIREAT", "HEXPIRETIME", "HGET",
	"HGETALL", "HGETDEL", "HGETEX", "HINCRBY", "HINCRBYFLOAT", "HKEYS", "HLEN",
	"HMGET", "HMSET", "HPERSIST", "HPEXPIRE", "HPEXPIREAT", "HPEXPIRETIME",
	"HPTTL", "HRANDFIELD", "HSCAN", "HSET", "HSETEX", "HSETNX", "HSTRLEN",
	"HTTL", "HVALS",
	// Lists.
	"BLMOVE", "BLMPOP", "BLPOP", "BRPOP", "BRPOPLPUSH", "LINDEX", "LINSERT",
	"LLEN", "LMOVE", "LMPOP", "LPOP", "LPOS", "LPUSH", "LPUSHX", "LRANGE",
	"LREM", "LSET", "LTRIM", "RPOP", "RPOPLPUSH", "RPUSH", "RPUSHX",
	// Sets.
	"SADD", "SCARD", "SDIFF", "SDIFFSTORE", "SINTER", "SINTERCARD",
	"SINTERSTORE", "SISMEMBER", "SMEMBERS", "SMISMEMBER", "SMOVE", "SPOP",
	"SRANDMEMBER", "SREM", "SSCAN", "SUNION", "SUNIONSTORE",
	// Sorted sets.
	"BZMPOP", "BZPOPMAX", "BZPOPMIN", "ZADD", "ZCARD", "ZCOUNT", "ZDIFF",
	"ZDIFFSTORE", "ZINCRBY", "ZINTER", "ZINTERCARD", "ZINTERSTORE",
	"ZLEXCOUNT", "ZMPOP", "ZMSCORE", "ZPOPMAX", "ZPOPMIN", "ZRANDMEMBER",
	"ZRANGE", "ZRANGEBYLEX", "ZRANGEBYSCORE", "ZRANGESTORE", "ZRANK", "ZREM",
	"ZREMRANGEBYLEX", "ZREMRANGEBYRANK", "ZREMRANGEBYSCORE", "ZREVRANGE",
	"ZREVRANGEBYLEX", "ZREVRANGEBYSCORE", "ZREVRANK", "ZSCAN", "ZSCORE",
	"ZUNION", "ZUNIONSTORE",
	// Streams.
	"XACK", "XACKDEL", "XADD", "XAUTOCLAIM", "XCLAIM", "XDEL", "XDELEX",
	"XGROUP", "XINFO", "XLEN", "XPENDING", "XRANGE", "XREAD", "XREADGROUP",
	"XREVRANGE", "XSETID", "XTRIM",
	// Geo and HyperLogLog.
	"GEOADD", "GEODIST", "GEOHASH", "GEOPOS", "GEORADIUS", "GEORADIUS_RO",
	"GEORADIUSBYMEMBER", "GEORADIUSBYMEMBER_RO", "GEOSEARCH",
	"GEOSEARCHSTORE", "PFADD", "PFCOUNT", "PFDEBUG", "PFMERGE", "PFSELFTEST",
	// Pub/sub, transactions, scripting and functions.
	"PSUBSCRIBE", "PUBLISH", "PUBSUB", "PUNSUBSCRIBE", "SPUBLISH",
	"SSUBSCRIBE", "SUBSCRIBE", "SUNSUBSCRIBE", "UNSUBSCRIBE",
	"DISCARD", "EXEC", "MULTI", "UNWATCH", "WATCH",
	"EVAL", "EVAL_RO", "EVALSHA", "EVALSHA_RO", "FCALL", "FCALL_RO",
	"FUNCTION", "SCRIPT",
	// Vector sets (Redis 8).
	"VADD", "VCARD", "VDIM", "VEMB", "VGETATTR", "VINFO", "VISMEMBER",
	"VLINKS", "VRANDMEMBER", "VREM", "VSETATTR", "VSIM",
	// JSON (Redis 8).
	"JSON.ARRAPPEND", "JSON.ARRINDEX", "JSON.ARRINSERT", "JSON.ARRLEN",
	"JSON.ARRPOP", "JSON.ARRTRIM", "JSON.CLEAR", "JSON.DEBUG", "JSON.DEL",
	"JSON.FORGET", "JSON.GET", "JSON.MERGE", "JSON.MGET", "JSON.MSET",
	"JSON.NUMINCRBY", "JSON.NUMMULTBY", "JSON.OBJKEYS", "JSON.OBJLEN",
	"JSON.RESP", "JSON.SET", "JSON.STRAPPEND", "JSON.STRLEN", "JSON.TOGGLE",
	"JSON.TYPE",
	// Search and query (Redis 8).
	"FT._LIST", "FT.AGGREGATE", "FT.ALIASADD", "FT.ALIASDEL",
	"FT.ALIASUPDATE", "FT.ALTER", "FT.CONFIG", "FT.CREATE", "FT.CURSOR",
	"FT.DICTADD", "FT.DICTDEL", "FT.DICTDUMP", "FT.DROPINDEX", "FT.EXPLAIN",
	"FT.EXPLAINCLI", "FT.HYBRID", "FT.INFO", "FT.PROFILE", "FT.SEARCH",
	"FT.SPELLCHECK", "FT.SUGADD", "FT.SUGDEL", "FT.SUGGET", "FT.SUGLEN",
	"FT.SYNDUMP", "FT.SYNUPDATE", "FT.TAGVALS",
	// Time series (Redis 8).
	"TS.ADD", "TS.ALTER", "TS.CREATE", "TS.CREATERULE", "TS.DECRBY",
	"TS.DEL", "TS.DELETERULE", "TS.GET", "TS.INCRBY", "TS.INFO", "TS.MADD",
	"TS.MGET", "TS.MRANGE", "TS.MREVRANGE", "TS.QUERYINDEX", "TS.RANGE",
	"TS.REVRANGE",
	// Probabilistic (Redis 8).
	"BF.ADD", "BF.CARD", "BF.EXISTS", "BF.INFO", "BF.INSERT", "BF.LOADCHUNK",
	"BF.MADD", "BF.MEXISTS", "BF.RESERVE", "BF.SCANDUMP",
	"CF.ADD", "CF.ADDNX", "CF.COUNT", "CF.DEL", "CF.EXISTS", "CF.INFO",
	"CF.INSERT", "CF.INSERTNX", "CF.LOADCHUNK", "CF.MEXISTS", "CF.RESERVE",
	"CF.SCANDUMP",
	"CMS.INCRBY", "CMS.INFO", "CMS.INITBYDIM", "CMS.INITBYPROB", "CMS.MERGE",
	"CMS.QUERY",
	"TOPK.ADD", "TOPK.COUNT", "TOPK.INCRBY", "TOPK.INFO", "TOPK.LIST",
	"TOPK.QUERY", "TOPK.RESERVE",
	"TDIGEST.ADD", "TDIGEST.BYRANK", "TDIGEST.BYREVRANK", "TDIGEST.CDF",
	"TDIGEST.CREATE", "TDIGEST.INFO", "TDIGEST.MAX", "TDIGEST.MERGE",
	"TDIGEST.MIN", "TDIGEST.QUANTILE", "TDIGEST.RANK", "TDIGEST.RESET",
	"TDIGEST.REVRANK", "TDIGEST.TRIMMED_MEAN",
}
