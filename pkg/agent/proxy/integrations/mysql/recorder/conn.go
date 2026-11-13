package recorder

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"runtime/debug"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	phase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase"
	connPhase "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire/phase/conn"
	pUtils "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
	"golang.org/x/sync/singleflight"
)

// buildSyntheticPostTLSConfig synthesizes the config-mock request/response
// bundles for a post-TLS connection joined mid-stream (seq==0), where no real
// HandshakeResponse41 / auth exchange was captured (the connection was already
// authenticated before interception). It reconstructs a HandshakeResponse41
// from the stored SSLRequest's fields, plus AuthMoreData(FastAuthSuccess) + OK,
// so the config mock carries an HR41 at requests[1] — which the replayer
// REQUIRES to match the connection at test time (see replayer/conn.go: it
// looks for HandshakeResponse41 at requests[0] or [1] and errors otherwise).
// sslReqPkt must already be decoded. Used by the V2 post-TLS handshake.
func buildSyntheticPostTLSConfig(ctx context.Context, logger *zap.Logger, decodeCtx *wire.DecodeContext, greetingPkt, sslReqPkt *mysql.PacketBundle) ([]mysql.Request, []mysql.Response, string, error) {
	sslReq, ok := sslReqPkt.Message.(*mysql.SSLRequestPacket)
	if !ok {
		return nil, nil, "", fmt.Errorf("stored packet is not SSLRequest, got %T", sslReqPkt.Message)
	}

	// Synthesize a HandshakeResponse41 from the SSLRequest fields.
	// Username/Database are not present in SSLRequest and remain empty; matcher
	// handles empty expected username/database as backward-compatible wildcards.
	syntheticHR41 := &mysql.HandshakeResponse41Packet{
		CapabilityFlags: sslReq.CapabilityFlags,
		MaxPacketSize:   sslReq.MaxPacketSize,
		CharacterSet:    sslReq.CharacterSet,
		Filler:          sslReq.Filler,
		AuthPluginName:  decodeCtx.PluginName,
	}
	hr41Payload, err := connPhase.EncodeHandshakeResponse41(ctx, logger, syntheticHR41)
	if err != nil {
		return nil, nil, "", fmt.Errorf("failed to encode synthetic HandshakeResponse41: %w", err)
	}

	authMorePacket := &mysql.AuthMoreDataPacket{
		StatusTag: mysql.AuthMoreData,
		Data:      "FastAuthSuccess",
	}
	authMorePayload, err := connPhase.EncodeAuthMoreData(ctx, authMorePacket)
	if err != nil {
		return nil, nil, "", fmt.Errorf("failed to encode synthetic AuthMoreData: %w", err)
	}

	okPacket := &mysql.OKPacket{
		Header:      mysql.OK,
		StatusFlags: 2,
	}
	serverCaps := decodeCtx.ServerCaps
	if greeting, ok := greetingPkt.Message.(*mysql.HandshakeV10Packet); ok {
		serverCaps = greeting.CapabilityFlags
	}
	okPayload, err := phase.EncodeOk(ctx, okPacket, serverCaps)
	if err != nil {
		return nil, nil, "", fmt.Errorf("failed to encode synthetic OK packet: %w", err)
	}

	sslReqSeq := byte(1) // Default sequence for SSLRequest in the SSL handshake path.
	if sslReqPkt.Header != nil && sslReqPkt.Header.Header != nil {
		sslReqSeq = sslReqPkt.Header.Header.SequenceID
	}
	hr41Seq := sslReqSeq + 1
	authMoreSeq := hr41Seq + 1
	okSeq := authMoreSeq + 1

	hr41Bundle := mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Header: &mysql.Header{PayloadLength: uint32(len(hr41Payload)), SequenceID: hr41Seq},
			Type:   mysql.HandshakeResponse41,
		},
		Message: syntheticHR41,
	}

	// Synthesize auth responses: AuthMoreData(FastAuthSuccess) + OK.
	authMoreBundle := mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Header: &mysql.Header{PayloadLength: uint32(len(authMorePayload)), SequenceID: authMoreSeq},
			Type:   mysql.AuthStatusToString(mysql.AuthMoreData),
		},
		Message: authMorePacket,
	}
	okBundle := mysql.PacketBundle{
		Header: &mysql.PacketInfo{
			Header: &mysql.Header{PayloadLength: uint32(len(okPayload)), SequenceID: okSeq},
			Type:   mysql.StatusToString(mysql.OK),
		},
		Message: okPacket,
	}

	requests := []mysql.Request{
		{PacketBundle: *sslReqPkt},
		{PacketBundle: hr41Bundle},
	}
	responses := []mysql.Response{
		{PacketBundle: *greetingPkt},
		{PacketBundle: authMoreBundle},
		{PacketBundle: okBundle},
	}
	return requests, responses, mysql.StatusToString(mysql.OK), nil
}

// fetchGreetingDialTimeout bounds each dial fetchServerGreeting makes.
const fetchGreetingDialTimeout = 3 * time.Second

// fetchServerGreeting connects to the MySQL server directly and reads the
// initial HandshakeV10 greeting packet. This is used as a fallback when the
// pre-TLS handshake was not captured by the proxy (e.g. the connection was
// established before interception started).
func fetchServerGreeting(ctx context.Context, logger *zap.Logger, opts models.OutgoingOptions) ([]byte, error) {
	addr := ""
	if opts.DstCfg != nil {
		addr = opts.DstCfg.Addr
	}
	if addr == "" {
		return nil, fmt.Errorf("no destination address available to fetch server greeting")
	}
	// Never dial an address the capture layer fabricated. When the proxyless
	// SSL-uprobe path cannot resolve a decrypted stream's real destination it
	// substitutes a loopback stand-in (and content matching later forces the
	// well-known port), yielding e.g. "127.0.0.1:3306" — an address this
	// connection never talked to. Dialing it either fails instantly
	// (ECONNREFUSED, aborting the capture) or, worse, fetches a greeting from
	// an unrelated local server. Fail here so callers fall through to their
	// stash/cache fallbacks or abort cleanly.
	if opts.DstCfg != nil && opts.DstCfg.AddrFabricated {
		return nil, fmt.Errorf("destination address %s is a capture-layer stand-in (real destination unresolved), refusing to dial it to fetch the server greeting", addr)
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}

	dialer := &net.Dialer{Timeout: fetchGreetingDialTimeout}
	dial := func(ctx context.Context, a string) (net.Conn, error) {
		return dialer.DialContext(ctx, "tcp", a)
	}
	// A connection made from another network namespace to a namespace-local
	// address (a pod's 127.0.0.1) is a different server from keploy's own
	// namespace. The capture layer then supplies a dialer that connects from
	// the connection's namespace; without one the address is dialled here as
	// before.
	if nsDial := models.DstDialerFrom(ctx); nsDial != nil {
		dial = func(ctx context.Context, a string) (net.Conn, error) {
			dctx, cancel := context.WithTimeout(ctx, fetchGreetingDialTimeout)
			defer cancel()
			return nsDial(dctx, a)
		}
	}
	conn, err := pUtils.DialDestinationWith(ctx, logger, pUtils.DialTarget{Addr: addr}, dial)
	if err != nil {
		return nil, fmt.Errorf("dial %s: %w", addr, err)
	}
	defer conn.Close()
	cancelRead := make(chan struct{})
	go func() {
		select {
		case <-ctx.Done():
			_ = conn.SetReadDeadline(time.Now())
		case <-cancelRead:
		}
	}()
	defer close(cancelRead)

	// MySQL server sends greeting immediately upon connection.
	readDeadline := time.Now().Add(3 * time.Second)
	if d, ok := ctx.Deadline(); ok && d.Before(readDeadline) {
		readDeadline = d
	}
	if err := conn.SetReadDeadline(readDeadline); err != nil {
		return nil, fmt.Errorf("set read deadline: %w", err)
	}
	// Read the 4-byte MySQL packet header first.
	header := make([]byte, 4)
	if _, err := io.ReadFull(conn, header); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("read greeting header: %w", err)
	}
	payloadLen := int(header[0]) | int(header[1])<<8 | int(header[2])<<16
	payload := make([]byte, payloadLen)
	if _, err := io.ReadFull(conn, payload); err != nil {
		if ctx.Err() != nil {
			return nil, ctx.Err()
		}
		return nil, fmt.Errorf("read greeting payload: %w", err)
	}

	buf := make([]byte, 4+payloadLen)
	copy(buf, header)
	copy(buf[4:], payload)
	return buf, nil
}

// greetingFetches coalesces concurrent direct greeting fetches for the same
// store and destination key (see fetchServerGreetingShared).
var greetingFetches singleflight.Group

// greetingFetchFailureBackoff is how long a failed direct fetch is answered
// from memory instead of dialled again. A variable so tests can shorten it.
var greetingFetchFailureBackoff = 15 * time.Second

// greetingFetchFailures remembers the most recent failed fetch per flight key
// for greetingFetchFailureBackoff. Values are greetingFetchFailure.
var greetingFetchFailures sync.Map

type greetingFetchFailure struct {
	err error
	at  time.Time
}

// greetingFetchResult is what one flight hands every caller sharing it.
type greetingFetchResult struct {
	buf        []byte
	fromCache  bool // served by an entry remembered before this flight started
	remembered bool // this flight's fetch was remembered for later connections
}

// fetchServerGreetingShared is how the post-TLS paths fall back to dialling the
// server for its greeting. Use it instead of calling fetchServerGreeting
// directly: it dials at most ONCE per server (per greetingMemoKey), not once
// per connection, per app or per recording session.
//
// Every dial reads the greeting and hangs up before authenticating, and MySQL
// counts that as an aborted handshake from the dialling host. Enough of them
// (max_connect_errors, default 100) blocks the host. When the dial comes from a
// node's network (a DaemonSet agent), pods reaching the server through SNAT
// share that host, so the whole node loses its database. Before the capture
// layer reported real destinations, these dials hit the pod's own IP and failed
// instantly. Now they reach the server, and a pool of pre-recording connections
// with nothing captured dialled once per connection at every recording start,
// each costing up to 3s dial + 2s loopback-fallback dial + 3s read when a
// firewall drops it.
//
// So:
//   - A greeting is remembered PER SERVER (models.HandshakeServerKey): a
//     server's greeting belongs to the server, not to the app or the recording
//     session that happened to need it. The key carries no app/session scope
//     for a routable address, so the next session, or another app on the same
//     node, reuses it. A namespace-local address (127.0.0.1, ::1, link-local)
//     names a different server in every pod, so its key is qualified by the
//     connection's network namespace (OutgoingOptions.NetNS). The entry holds
//     the greeting bytes only: no SSLRequest (none was captured), no timing.
//     It lives as long as a live greeting does (lastGreetingTTL), a greeting a
//     raw leg captures replaces it (rememberServerGreeting), and a fetch never
//     replaces one a raw leg recorded meanwhile.
//   - Concurrent callers for the same store and key share ONE in-flight dial.
//     The dial is detached from the calling connection's cancellation, so one
//     stream's teardown does not fail the others waiting on it. It stays bounded
//     by fetchServerGreeting's own dial and read limits. Each caller stops
//     waiting when its own ctx ends, and a caller whose ctx has already ended
//     never STARTS a dial: recording stop is not a reason to reach the server.
//     It still takes a greeting remembered for the server, which reaches none.
//   - A failed fetch caches nothing as a success. Callers already waiting share
//     its error, and callers arriving within greetingFetchFailureBackoff get
//     that error without dialling again. After that the next caller dials. A
//     reply that is not a HandshakeV10 greeting (for example the ERR packet
//     MySQL sends a blocked host) counts as a failure. A dialer that could not
//     reach its own connection's namespace (models.ErrDstDialerUnavailable)
//     does not: that is about one caller, and the others still dial.
//   - Nothing is remembered without an address: never under a port, and never
//     for a fabricated destination, which fetchServerGreeting refuses to dial
//     in the first place.
//
// The flight runs on its own goroutine, where singleflight re-panics any panic
// out of reach of every recover, so it recovers its own and fails instead.
// It also does not log; its callers do, with their own loggers.
func fetchServerGreetingShared(ctx context.Context, logger *zap.Logger, hsStore *models.TLSHandshakeStore, opts models.OutgoingOptions) ([]byte, error) {
	key := greetingMemoKey(opts)
	if hsStore == nil || key == "" {
		// Nothing to remember under and nothing to coalesce on: no store, no
		// address, or a stand-in address (which fetchServerGreeting refuses).
		return fetchServerGreeting(ctx, logger, opts)
	}
	// Same order as inside the flight: a remembered greeting (for instance one a
	// raw leg recorded since the last failure) beats a remembered failure. It
	// is read before the caller's end is: it reaches no server, so a caller at
	// the recording's stop, whose connection's capture is still being read,
	// takes it too.
	if g, ok := hsStore.ServerGreeting(key); ok {
		logger.Debug("post-TLS MySQL: using the greeting remembered for this server; not dialling",
			zap.String("serverKey", key))
		return g, nil
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	// The store pointer is part of the key: the server key alone is not unique
	// across stores (each has its own cache), and a flight must remember into
	// the store its callers read.
	flight := fmt.Sprintf("%p|%s", hsStore, key)
	if err := recentGreetingFetchFailure(flight); err != nil {
		logger.Debug("post-TLS MySQL: not dialling the server for its greeting again; a fetch failed moments ago",
			zap.String("serverKey", key), zap.Error(err))
		return nil, err
	}
	dialLogger := logger
	// A flight dials with its LEADER's dialer (its ctx). When that dialer could
	// not reach its own connection's namespace (models.ErrDstDialerUnavailable),
	// the failure is the leader's alone: it is not remembered for the server,
	// and a caller that merely shared the flight tries once more, with a flight
	// of its own if no one else has started one.
	for attempt := 0; ; attempt++ {
		led := false // set only if this caller's function runs the flight
		ch := greetingFetches.DoChan(flight, func() (res interface{}, err error) {
			led = true
			dialled := false
			defer func() {
				if r := recover(); r != nil {
					res, err = nil, fmt.Errorf("greeting fetch from %s panicked: %v\n%s", opts.DstCfg.Addr, r, debug.Stack())
					dialled = true
				}
				if !dialled {
					return
				}
				switch {
				case err == nil:
					greetingFetchFailures.Delete(flight)
				case errors.Is(err, models.ErrDstDialerUnavailable):
					// This caller's namespace, not the server: remember nothing.
				default:
					greetingFetchFailures.Store(flight, greetingFetchFailure{err: err, at: time.Now()})
					pruneGreetingFetchFailures()
				}
			}()
			// A flight that finished just before this one started may already have
			// remembered the greeting (so may a raw leg), or may have failed: a
			// caller can pass the check above just as that flight ends. Either way,
			// no dial.
			if g, ok := hsStore.ServerGreeting(key); ok {
				return greetingFetchResult{buf: g, fromCache: true}, nil
			}
			if err := recentGreetingFetchFailure(flight); err != nil {
				return nil, err
			}
			dialled = true
			buf, err := fetchServerGreeting(context.WithoutCancel(ctx), dialLogger, opts)
			if err != nil {
				return nil, err
			}
			if err := validateGreeting(buf); err != nil {
				return nil, fmt.Errorf("server at %s: %w", opts.DstCfg.Addr, err)
			}
			remembered := hsStore.RememberServerGreetingIfAbsent(key, buf)
			return greetingFetchResult{buf: buf, remembered: remembered}, nil
		})
		select {
		case r := <-ch:
			if r.Err != nil {
				if !led && attempt == 0 && errors.Is(r.Err, models.ErrDstDialerUnavailable) {
					logger.Debug("post-TLS MySQL: the shared greeting fetch could not dial from its own connection's namespace; trying with this connection's",
						zap.String("serverKey", key), zap.Error(r.Err))
					continue
				}
				return nil, r.Err
			}
			res, _ := r.Val.(greetingFetchResult)
			switch {
			case res.fromCache:
				logger.Debug("post-TLS MySQL: greeting was remembered by an earlier connection; not dialling",
					zap.String("serverKey", key))
			case r.Shared:
				logger.Debug("post-TLS MySQL: shared another connection's greeting fetch instead of dialling",
					zap.String("serverKey", key))
			case res.remembered:
				logger.Debug("post-TLS MySQL: remembered a directly fetched greeting for later connections to this server",
					zap.String("serverKey", key))
			}
			return res.buf, nil
		case <-ctx.Done():
			return nil, ctx.Err()
		}
	}
}

// lastGreetingKey is the key of the app-scoped last-greeting cache
// (models.HandshakeLastKey) for a connection, or "" for none.
//
// That key combines the app/session scope with the destination address, and
// the scope is shared by every replica of a deployment. For a namespace-local
// address the capture layer observed (not a stand-in), each replica reaches its
// OWN server there (a sidecar on 127.0.0.1), so the key also carries the
// connection's network namespace, and without one there is no key: nothing is
// shared, as for the per-server memo (models.HandshakeServerKey). A stand-in
// address (AddrFabricated) names no server at all; its per-scope bucket is
// documented on models.HandshakeLastKey.
func lastGreetingKey(scope, netNS string, dst *models.ConditionalDstCfg) string {
	if dst != nil && !dst.AddrFabricated {
		if host, _, err := net.SplitHostPort(dst.Addr); err == nil && models.AddrIsNetnsLocal(host) {
			if netNS == "" {
				return ""
			}
			scope += "|netns=" + netNS
		}
	}
	return models.HandshakeLastKey(scope, dst)
}

// greetingMemoKey is the key a connection's server greeting is remembered
// under: the server behind its destination (models.HandshakeServerKey). "" when
// the destination names no server.
func greetingMemoKey(opts models.OutgoingOptions) string {
	return models.HandshakeServerKey(opts.NetNS, opts.DstCfg)
}

// rememberServerGreeting records a greeting a raw leg CAPTURED from the server
// on a live connection, so a later connection to the same server that needs
// one (a pooled connection whose own greeting predates the recording) is
// served without dialling, and a greeting fetched earlier is refreshed.
//
// identity is greetingServerIdentity of the caller's decoding of greeting, and
// the proof that it IS one: an ERR packet, or a mid-stream packet mistaken for a
// greeting, never decodes as a HandshakeV10 (identity is then ""), and must
// never be handed to another connection. Taking the caller's decoding spares
// every recorded handshake a second decode and a second fingerprint.
func rememberServerGreeting(ctx context.Context, opts models.OutgoingOptions, greeting []byte, identity string) {
	if identity == "" || len(greeting) < 5 || greeting[4] != mysql.HandshakeV10 {
		return
	}
	hsStore, _ := ctx.Value(models.TLSHandshakeStoreKey).(*models.TLSHandshakeStore)
	if hsStore == nil {
		return
	}
	if key := greetingMemoKey(opts); key != "" {
		hsStore.RememberServerGreeting(key, greeting, identity)
	}
}

// recentGreetingFetchFailure returns the failure recorded for flight within
// greetingFetchFailureBackoff, or nil.
func recentGreetingFetchFailure(flight string) error {
	v, ok := greetingFetchFailures.Load(flight)
	if !ok {
		return nil
	}
	f, ok := v.(greetingFetchFailure)
	if !ok {
		return nil
	}
	ago := time.Since(f.at)
	if ago >= greetingFetchFailureBackoff {
		return nil
	}
	return fmt.Errorf("greeting fetch failed %s ago, not retrying for %s: %w",
		ago.Truncate(time.Millisecond), greetingFetchFailureBackoff, f.err)
}

// pruneGreetingFetchFailures drops failure records past their backoff so the
// map holds only live ones: one per destination that failed in the last
// greetingFetchFailureBackoff.
func pruneGreetingFetchFailures() {
	greetingFetchFailures.Range(func(k, v interface{}) bool {
		if f, ok := v.(greetingFetchFailure); !ok || time.Since(f.at) >= greetingFetchFailureBackoff {
			greetingFetchFailures.Delete(k)
		}
		return true
	})
}

// validateGreeting accepts only a HandshakeV10 greeting packet: protocol
// version 10 and a payload that decodes. Anything else, such as the ERR packet
// MySQL sends a blocked host or a truncated reply, must not be cached or reused.
func validateGreeting(buf []byte) error {
	if len(buf) < 5 {
		return fmt.Errorf("greeting reply too short (%d bytes)", len(buf))
	}
	if buf[4] != mysql.HandshakeV10 {
		return fmt.Errorf("reply is not a HandshakeV10 greeting (first payload byte 0x%02x)", buf[4])
	}
	if _, err := connPhase.DecodeHandshakeV10(context.Background(), zap.NewNop(), buf[4:]); err != nil {
		return fmt.Errorf("undecodable HandshakeV10 greeting: %w", err)
	}
	return nil
}
