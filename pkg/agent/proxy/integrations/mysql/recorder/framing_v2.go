package recorder

import (
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"sync"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/fakeconn"
	mysqlUtils "go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/utils"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/mysql/wire"
	"go.keploy.io/server/v3/pkg/agent/proxy/supervisor"
	"go.keploy.io/server/v3/pkg/models/mysql"
	"go.uber.org/zap"
)

// ErrFramingLost is what RecordV2 ends with when it can no longer tell where
// one MySQL packet of the connection ends and the next begins.
//
// The V2 recorder pairs each command with the packets that follow it on the
// server stream, so its mocks are only as right as its framing. Framing is
// lost when bytes are missing from either stream (a capture that dropped some),
// or when a packet arrives that this recorder cannot size the response of. A
// recorder that carries on from there reads packet headers out of the middle
// of row data: it waits for megabytes that are never coming (a header read
// from "000" claims 3,158,064 bytes), or it pairs every later command with
// the previous command's response. Every mock after that point is either
// missing or wrong, and nothing says so.
//
// So it does not carry on from there. When it was a response it lost the
// framing of, with the command read whole, it leaves that exchange out and
// re-aligns at the client's next command (realign): every answer a client that
// waits for each answer before its next command gets starts after that
// command, so the rest of the response is skipped, and only the test cases of
// the exchange it could not frame are left out. A pooled connection carries
// the traffic of every request that borrows it: stopping there left out every
// test case recorded for as long as the connection carried traffic, almost the
// whole recording. Otherwise (a client that pipelines its commands, lost
// framing on the client's stream) it stops: the error retires the parser, and
// the supervisor counts what the connection carries from there as not
// recorded. Fewer mocks, none of them wrong.
//
// The recorder logs it once it knows which of the two follows (reportFault,
// warnFramingLost), so the error wraps supervisor.ErrReported: whoever runs
// the parser does not warn of its retirement again for every connection one
// fault stops.
var ErrFramingLost error = reportedError("mysql: the connection's packet framing is lost")

// reportedError is a sentinel error the recorder logs where it happens, which
// says so to the supervisor (supervisor.ErrReported).
type reportedError string

func (e reportedError) Error() string { return string(e) }
func (reportedError) Unwrap() error   { return supervisor.ErrReported }

// framingLost wraps why framing was lost.
func framingLost(format string, args ...any) error {
	return fmt.Errorf("%w: %s", ErrFramingLost, fmt.Sprintf(format, args...))
}

// responseFault is lost framing found while reading a response, with what was
// found (msg). It is logged where the command loop decides what follows from
// it (reportFault): the exchange left out and the recording going on from the
// next command, or the connection no longer recorded.
type responseFault struct {
	msg string
	err error
}

func (f *responseFault) Error() string { return f.err.Error() }
func (f *responseFault) Unwrap() error { return f.err }

// found is the lost framing err a response showed, msg saying what was found.
func found(msg string, err error) error {
	return &responseFault{msg: msg, err: err}
}

// reportFault logs lost framing a response showed (found), at WARN through its
// limiter, with what follows from it: realigned, the exchange is left out and
// the connection's recording goes on; otherwise the connection is no longer
// recorded. Lost framing not found in a response was logged where it was.
func reportFault(logger *zap.Logger, sess *supervisor.Session, err error, realigned bool) {
	var f *responseFault
	if !errors.As(err, &f) {
		return
	}
	if !realigned {
		warnFramingLost(logger, sess, f.msg+"; the connection is no longer recorded", err)
		return
	}
	warnLimited(logger, sess, f.msg+"; the exchange is left out, and the connection's recording goes on from its next command", err,
		"user traffic is unaffected. The test cases recorded while this exchange ran are left out of the recording rather than saved without its mock; the connection's later queries are recorded")
}

// answered reports whether the server answers a command (the command's first
// byte): all but COM_QUIT, COM_STMT_CLOSE and COM_STMT_SEND_LONG_DATA
// (wire.IsNoResponseCommand).
func answered(command byte) bool {
	return command != mysql.COM_QUIT && command != mysql.COM_STMT_CLOSE && command != mysql.COM_STMT_SEND_LONG_DATA
}

// nextCommandAfterFault reads the client's next command after a response left
// out (realign), and returns it as the command loop reads one: its packet, the
// sequence id its answer starts at, and whether it continued over several
// packets. It passes over the client packets still part of the exchange left
// out: a command starts a new sequence, at 0, and what a client sends inside
// an exchange (a LOCAL INFILE upload, an auth switch response) is numbered on
// from it. An upload of more than 254 packets numbers on past 255 to 0: a
// packet at 0 right after one at 255 that carried data is more of it. (The
// upload ends with an empty packet; a command after that starts at 0 again.)
// Each packet passed over is input consumed: its bytes armed the supervisor's
// pending work, which a connection that then sits idle must not leave armed
// (clearPending).
//
// A client that waits for each answer before its next command sends that
// command on its own, so it starts a chunk of the capture. One that does not
// (in the middle of a chunk) is not where the client's stream can be taken up
// again: lost framing.
func nextCommandAfterFault(ctx context.Context, logger *zap.Logger, sess *supervisor.Session) (cmdBuf []byte, respFirst byte, joined bool, err error) {
	// prevSeq and prevData: the last packet passed over ended at sequence id
	// prevSeq, and carried data (one has been passed over: passed).
	var prevSeq byte
	prevData, passed := false, false
	for {
		atChunk := sess.ClientStream.AtChunkBoundary()
		var cs commandSeq
		inExchange := false
		var lastSeq byte
		buf, err := mysqlUtils.ReadPacketBufferChecked(ctx, logger, sess.ClientStream, func(header []byte) error {
			if !cs.started && !inExchange && (header[3] != 0 || passed && prevSeq == 255 && prevData) {
				inExchange = true
			}
			lastSeq = header[3]
			if inExchange {
				return nil
			}
			return cs.take(header)
		})
		if err != nil {
			return nil, 0, false, err
		}
		if inExchange {
			prevSeq, prevData, passed = lastSeq, len(buf) > 4, true
			clearPending(sess)
			continue
		}
		if !atChunk {
			return nil, 0, false, framingLost("the command after a response left out does not start a chunk of the capture: the client did not send it on its own")
		}
		return buf, cs.next, cs.joined, nil
	}
}

// packetSeq checks the sequence id of every packet of one exchange.
//
// A command is packet 0 of a new sequence, and every packet of its response
// is numbered on from it (1, 2, ... wrapping at 256). Clients reject a packet
// out of sequence ("packets out of order"), so a server never sends one; a
// recorder that reads one has not read the packet the server sent. Checking
// it costs a byte compare per packet and catches a misframed header with
// probability 255/256: a header read out of row data, of a column definition
// or of the next response.
type packetSeq struct {
	next byte
	// what names the exchange in the error.
	what string
	// oversized is set once a packet of the response continued over several
	// (a payload of 16 MiB or more): see oversizedNotRecorded.
	oversized bool
}

// responseSeq is the sequence of the response to a command whose last packet
// had sequence id first-1: 1 after a command of one packet, more after a
// command of 16 MiB or more, which continues over several.
func responseSeq(command string, first byte) packetSeq {
	return packetSeq{next: first, what: command}
}

// take checks buf's sequence id (buf is a whole packet, header included) and
// advances the sequence.
func (s *packetSeq) take(buf []byte, part string) error {
	if len(buf) < 4 {
		return framingLost("the %s of the %s response is %d bytes, shorter than a packet header", part, s.what, len(buf))
	}
	if got := buf[3]; got != s.next {
		// The header's bytes say what the reader was on: a row's bytes, the
		// payload of an OK, a header read one byte early.
		return framingLost("the %s of the %s response has sequence id %d where %d comes next (header % x)", part, s.what, got, s.next, buf[:4])
	}
	s.next++
	if continues(buf) {
		s.oversized = true
	}
	return nil
}

// continues reports whether a packet's header (buf[:4]) says its payload
// continues in the next packet: a payload of MaxPacketPayload bytes does.
func continues(buf []byte) bool {
	return mysqlUtils.GetPayloadLength(buf[:3]) == mysqlUtils.MaxPacketPayload
}

// oversizedNotRecorded is why an exchange with a packet of 16 MiB or more is
// left out (errNotRecorded) once it is framed. The recorder joins such a
// packet, so the connection's framing goes on past it, but the replayer can
// serve neither: it reads a command one packet at a time, and its encoders
// write a row's length in the 3 bytes of one header. A mock with one would
// fail its test case on replay.
func oversizedNotRecorded(command, what string) error {
	return notRecorded("the %s exchange has a %s of 16 MiB or more, which the replayer cannot serve", command, what)
}

// warnLimiter lets one warning of a kind through per interval, process-wide,
// and counts the ones it holds back so the next one says how many it stands
// for. Each of these warnings retires a connection's recording or leaves an
// exchange out, so none of them may be silent; a node recording hundreds of
// connections through one fault must not log hundreds of lines either. The
// errors that retire a connection wrap supervisor.ErrReported, so the
// dispatcher does not warn of each retirement again.
type warnLimiter struct {
	every time.Duration

	mu   sync.Mutex
	last time.Time
	held uint64
}

// allow reports whether a warning may be logged at now, and how many were
// held back since the last one that was.
func (l *warnLimiter) allow(now time.Time) (bool, uint64) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if !l.last.IsZero() && now.Sub(l.last) < l.every {
		l.held++
		return false, 0
	}
	held := l.held
	l.last, l.held = now, 0
	return true, held
}

// reset forgets the last warning and what was held back (tests).
func (l *warnLimiter) reset() {
	l.mu.Lock()
	l.last, l.held = time.Time{}, 0
	l.mu.Unlock()
}

// framingWarnEvery is how often each of these warnings may be logged.
const framingWarnEvery = 10 * time.Second

// warnLimiters holds one warnLimiter per message, so a warning that is held
// back is counted under its own message, never under another cause's.
var warnLimiters sync.Map // message -> *warnLimiter

func limiterFor(msg string) *warnLimiter {
	if l, ok := warnLimiters.Load(msg); ok {
		return l.(*warnLimiter)
	}
	l, _ := warnLimiters.LoadOrStore(msg, &warnLimiter{every: framingWarnEvery})
	return l.(*warnLimiter)
}

// resetWarnLimiters forgets every warning logged so far (tests).
func resetWarnLimiters() {
	warnLimiters.Range(func(_, l any) bool {
		l.(*warnLimiter).reset()
		return true
	})
}

// warnFramingLost logs that a connection's recording stopped at err, at most
// once per framingWarnEvery for msg, at WARN: what the connection carries from
// here is not recorded, and an operator has to be able to find out why.
func warnFramingLost(logger *zap.Logger, sess *supervisor.Session, msg string, err error) {
	warnLimited(logger, sess, msg, err, "user traffic is unaffected: the relay keeps forwarding raw bytes. This connection's later queries are not recorded rather than paired with the wrong responses, and every test case recorded while it carries traffic is left out of the recording rather than saved without its mocks. If this repeats, set KEPLOY_DISABLE_PARSING=1 to disable record parsing entirely (raw passthrough)")
}

// warnLimited logs msg at WARN through its limiter, with err, the connection,
// how many of the same warning were held back since the last one logged, and
// what follows from it.
func warnLimited(logger *zap.Logger, sess *supervisor.Session, msg string, err error, nextStep string) {
	if logger == nil {
		return
	}
	ok, held := limiterFor(msg).allow(time.Now())
	if !ok {
		return
	}
	fields := []zap.Field{zap.Error(err), zap.String("connID", sess.ClientConnID)}
	if sess.Opts.DstCfg != nil && sess.Opts.DstCfg.Addr != "" {
		fields = append(fields, zap.String("dest", sess.Opts.DstCfg.Addr))
	}
	if held > 0 {
		fields = append(fields, zap.Uint64("sameWarningsHeldBack", held))
	}
	fields = append(fields, zap.String("next_step", nextStep))
	logger.Warn(msg, fields...)
}

// errNotRecorded is what collecting a response ends with when the recorder
// framed the whole response, so the connection's recording can go on, but
// cannot record the exchange as a mock that replays what the server answered.
// The caller leaves the exchange out (leaveOut).
var errNotRecorded = errors.New("mysql: the exchange is framed but cannot be recorded")

func notRecorded(format string, args ...any) error {
	return fmt.Errorf("%w: %s", errNotRecorded, fmt.Sprintf(format, args...))
}

// leaveOut records that the exchange [reqTs, resTs] has no mock: the test
// cases recorded over it are left out of the recording, and counted there
// (Session.RecordOrphanWindow), rather than saved without the mock their
// replay needs.
func leaveOut(logger *zap.Logger, sess *supervisor.Session, reqTs, resTs time.Time, err error) {
	sess.RecordOrphanWindow(reqTs, resTs)
	clearPending(sess)
	warnLimited(logger, sess, "V2: a mysql exchange is not recorded; the test cases recorded over it are left out", err,
		"the connection's other queries are still recorded; the test cases recorded while this exchange ran are left out of the recording, not saved without its mock")
}

// clearPending tells the supervisor the input read so far is consumed, as an
// emitted mock does (Session.EmitMock): its client bytes armed pending work,
// and work left pending through the next idle spell is taken for a hung
// parser, which retires a connection whose recording was going on.
func clearPending(sess *supervisor.Session) {
	if sess.OnPendingCleared != nil {
		sess.OnPendingCleared()
	}
}

// leaveOutInFlight leaves out the exchange a recording stops in, from its
// command to now, and returns err. The supervisor counts what the connection
// carries from the parser's retirement, which follows this return; the
// exchange it stopped in began before that, and its test case, saved without
// the mock, would replay without it. A stream that ended (EOF, closed) or a
// recording that ended (ctx) stops in no exchange of its own.
// io.ErrUnexpectedEOF is not a stream end here: the stream readers end with
// io.EOF, and io.ErrUnexpectedEOF is a decoder's "input cut short", which
// stops the recording inside its exchange.
//
// The window ends at the stop (stoppedAt), not at the last chunk the parser
// read: a parser behind its connection (CPU starvation) has bytes buffered
// that it has not read yet, which arrived between the two, and a test case
// recorded over them would be in neither this window nor the supervisor's.
// Chunk stamps are wall-clock arrival times, so the two compare.
func leaveOutInFlight(sess *supervisor.Session, reqTs time.Time, err error) error {
	if err == nil || streamEnded(err) {
		return err
	}
	sess.RecordOrphanWindow(reqTs, stoppedAt(reqTs))
	return err
}

// stoppedAt is when a recording that stops now stops, for a window that
// starts at start: now, or start when the clock reads earlier than a chunk's
// stamp.
func stoppedAt(start time.Time) time.Time {
	return later(start, time.Now())
}

// streamEnded reports whether err is the end of a stream or of the recording,
// not a fault of the connection's bytes.
func streamEnded(err error) bool {
	return errors.Is(err, io.EOF) || errors.Is(err, fakeconn.ErrClosed) ||
		errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// firstResponsePacket is the first packet of a response: the one read to
// settle a held PREPARE (carry, see heldPrepare.settle), checked against the
// response's sequence, or the next one off the stream. The server's own ERR (see
// mayBeServersOwnErr) ends the recording as the end of the stream does
// (errServerDropped): it answers no command, and the server closes after it.
func firstResponsePacket(ctx context.Context, logger *zap.Logger, sess *supervisor.Session, seq *packetSeq, carry *[]byte) ([]byte, error) {
	if buf := *carry; buf != nil {
		*carry = nil
		if err := seq.take(buf, "first packet"); err != nil {
			return nil, found("V2: mysql response packet out of sequence", err)
		}
		return buf, nil
	}
	ownErr := false
	buf, err := mysqlUtils.ReadPacketBufferChecked(ctx, logger, sess.DestStream, func(header []byte) error {
		if seq.next != 0 && mayBeServersOwnErr(header) {
			ownErr = true
			return nil
		}
		return seq.take(header, "first packet")
	})
	if err != nil {
		if errors.Is(err, ErrFramingLost) {
			err = found("V2: mysql response packet out of sequence", err)
		}
		return nil, err
	}
	if ownErr {
		if !isServersOwnErr(buf) {
			return nil, framingLostAt(logger, sess, "the first packet of the %s response has sequence id 0 and is not an ERR", seq.what)
		}
		logger.Debug("V2: the server dropped the connection with an error of its own; its recording ends here",
			zap.String("connID", sess.ClientConnID), zap.ByteString("err", buf[4:]))
		return nil, errServerDropped
	}
	return buf, nil
}

// errServerDropped is the end of a connection the server dropped with an ERR
// of its own (isServersOwnErr). It wraps io.EOF: it ends the recording as the
// end of the stream does, with no exchange in flight (the command it reached
// is answered by nothing), and no framing lost.
var errServerDropped = fmt.Errorf("mysql: the server dropped the connection with an error of its own: %w", io.EOF)

// mayBeServersOwnErr reports whether a packet header can be the server's own
// ERR: one a server sends unasked as it drops a connection (MySQL 8's 4031
// at wait_timeout, an ERR at shutdown), at sequence id 0, since it answers no
// command. It is no larger than an ERR. Any response's packet has another
// sequence id: an answer starts at 1 (or later, after a command of 16 MiB or
// more).
func mayBeServersOwnErr(header []byte) bool {
	return header[3] == 0 && mysqlUtils.GetPayloadLength(header[:3]) <= maxErrPayload
}

// isServersOwnErr reports whether a whole packet is the server's own ERR: at
// sequence id 0, in the shape every ERR of the 4.1 protocol has (0xFF, a
// 2-byte code, '#' and a SQL state of five digits or capital letters, then the
// message). The sequence id and the 0xFF are a byte each, and row bytes a hole
// put the reader on can carry both; the shape they cannot carry by chance.
func isServersOwnErr(buf []byte) bool {
	if len(buf) < 4+errSQLStateEnd || buf[3] != 0 || buf[4] != mysql.ERR || buf[4+3] != '#' {
		return false
	}
	for _, c := range buf[4+4 : 4+errSQLStateEnd] {
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') {
			return false
		}
	}
	return true
}

// errSQLStateEnd is where an ERR payload's SQL state ends: after 0xFF (1), the
// code (2), '#' (1) and the state itself (5).
const errSQLStateEnd = 9

// rowIsAnError reports whether a packet where a row may come is an ERR. No row
// starts with 0xFF (a text row's first column is a length-encoded string, which
// 0xFF does not start; a binary row starts with 0x00), so an error ends the
// response there: a query killed, or out of time, partway through its rows.
func rowIsAnError(buf []byte) bool {
	return len(buf) > 4 && buf[4] == mysql.ERR
}

// serverMoreResultsExists is SERVER_MORE_RESULTS_EXISTS in a packet's status
// flags: another result of the same command follows (a CALL, a multi-statement
// query).
const serverMoreResultsExists = 0x0008

// serverStatusCursorExists is SERVER_STATUS_CURSOR_EXISTS in a packet's status
// flags: the rows of the result are held in a cursor on the server, for the
// client to ask for with COM_STMT_FETCH.
const serverStatusCursorExists = 0x0040

// opensCursor reports whether the EOF after a COM_STMT_EXECUTE's column
// definitions says the server opened a cursor (the execute asked for one,
// CURSOR_TYPE_READ_ONLY): the response ends there, with no rows. Under
// CLIENT_DEPRECATE_EOF the same status comes in the OK that replaces the
// EOF, which ends the response as a result of no rows does.
func opensCursor(eof []byte) bool {
	return mysqlUtils.IsEOFPacket(eof) && len(eof) >= 4+5 &&
		binary.LittleEndian.Uint16(eof[7:9])&serverStatusCursorExists != 0
}

// comStmtFetch is COM_STMT_FETCH, which asks a cursor for rows. The decoder
// has none for it.
const comStmtFetch = 0x1c

// frameFetchReply reads the reply to a COM_STMT_FETCH to its end, checking
// each packet's sequence id: binary rows of the cursor, each starting 0x00,
// then the EOF that ends them (the OK that replaces it, under
// CLIENT_DEPRECATE_EOF), or an ERR in place of a row. No binary row starts
// with 0xFE or 0xFF, so the reply frames itself.
//
// On a framing that is only assumed (a connection joined mid-stream), the
// packet that ends the rows settles it: a 5-byte EOF means the client never
// negotiated CLIENT_DEPRECATE_EOF, and the OK that replaces it means it did.
func frameFetchReply(ctx context.Context, logger *zap.Logger, sess *supervisor.Session, decodeCtx *wire.DecodeContext, seq *packetSeq, carry *[]byte) error {
	buf, err := firstResponsePacket(ctx, logger, sess, seq, carry)
	for {
		if err != nil {
			return err
		}
		if len(buf) < 5 {
			return framingLostAt(logger, sess, "an empty packet in the %s response", seq.what)
		}
		switch {
		case buf[4] == mysql.ERR:
			return nil
		case buf[4] == 0x00: // a row
		case fetchEnd(decodeCtx, buf):
			return nil
		default:
			return framingLostAt(logger, sess, "a %#x packet in the %s response, neither a row, its end nor an ERR", buf[4], seq.what)
		}
		buf, err = readResponsePacket(ctx, logger, sess, seq, "row")
	}
}

// fetchEnd reports whether buf ends a COM_STMT_FETCH's rows on this
// connection's framing, and settles an assumed framing on it.
func fetchEnd(decodeCtx *wire.DecodeContext, buf []byte) bool {
	eof, okEOF := mysqlUtils.IsEOFPacket(buf), mysqlUtils.IsOKReplacingEOF(buf)
	if decodeCtx.FramingAssumed && (eof || okEOF) {
		decodeCtx.FramingAssumed = false
		if eof {
			decodeCtx.ClientCaps &^= wire.CLIENT_DEPRECATE_EOF
			decodeCtx.ClientCapabilities &^= wire.CLIENT_DEPRECATE_EOF
		}
		return true
	}
	if decodeCtx.DeprecateEOF() {
		return okEOF
	}
	return eof
}

// terminatorMoreResults reports whether a result set's terminator says another
// result follows. A legacy EOF (0xFE, warnings, status) and the OK that
// replaces it under CLIENT_DEPRECATE_EOF (0xFE, 0x00 affected rows, 0x00 last
// insert id, status) both carry the status flags at payload bytes 3-4.
func terminatorMoreResults(buf []byte) bool {
	if len(buf) < 4+5 {
		return false
	}
	return binary.LittleEndian.Uint16(buf[7:9])&serverMoreResultsExists != 0
}

// okMoreResults reports whether an OK packet (header included) says another
// result follows. One too short to carry status flags (a client that did not
// negotiate CLIENT_PROTOCOL_41 or CLIENT_TRANSACTIONS) says nothing follows.
func okMoreResults(buf []byte) bool {
	if len(buf) < 5 {
		return false
	}
	p := buf[5:]
	for i := 0; i < 2; i++ { // affected rows, last insert id
		_, _, n := mysqlUtils.ReadLengthEncodedInteger(p)
		if n == 0 || n > len(p) {
			return false
		}
		p = p[n:]
	}
	return len(p) >= 2 && binary.LittleEndian.Uint16(p)&serverMoreResultsExists != 0
}

// singlePacketReply holds the commands this recorder has no decoder for whose
// reply is one packet, and what that packet may start with: the recorder can
// frame the reply and go on, leaving the exchange out. Any other undecoded
// command's reply cannot be framed, and ends the recording.
var singlePacketReply = map[byte]func(first byte) bool{
	0x05: okOrErr,  // COM_CREATE_DB
	0x06: okOrErr,  // COM_DROP_DB
	0x07: okOrErr,  // COM_REFRESH
	0x09: anyFirst, // COM_STATISTICS, a string (pkg/models/mysql numbers it 0x08)
	0x0c: okOrErr,  // COM_PROCESS_KILL
	0x1b: eofOrErr, // COM_SET_OPTION
}

func okOrErr(first byte) bool  { return first == mysql.OK || first == mysql.ERR }
func eofOrErr(first byte) bool { return first == mysql.EOF || first == mysql.ERR }
func anyFirst(byte) bool       { return true }

// packetChain checks the headers of one client packet: a payload of 16 MiB or
// more continues over packets numbered on from its first. next is then the
// sequence id after its last packet, the one a response to it starts at, and
// joined says it continued.
type packetChain struct {
	next    byte
	started bool
	joined  bool
}

func (c *packetChain) take(header []byte) error {
	if c.started {
		if header[3] != c.next {
			return framingLost("a continuation of a client packet has sequence id %d where %d comes next", header[3], c.next)
		}
		c.joined = true
	}
	c.started, c.next = true, header[3]+1
	return nil
}

// commandSeq checks the headers of one command: its first packet starts a
// new sequence (commandHeader), and a command of 16 MiB or more continues
// over packets numbered on from it (packetChain).
type commandSeq struct {
	packetChain
}

func (c *commandSeq) take(header []byte) error {
	if !c.started {
		if err := commandHeader(header); err != nil {
			return err
		}
	}
	return c.packetChain.take(header)
}

// commandHeader checks that a client packet starts a command: a command is
// packet 0 of a new sequence. Any other sequence id is a header read from
// where no packet starts (client bytes the capture lost), or a client packet
// the command loop does not expect (a LOCAL INFILE upload, the auth exchange
// inside COM_CHANGE_USER).
func commandHeader(header []byte) error {
	if len(header) < 4 {
		return framingLost("a client packet of %d bytes, shorter than a packet header", len(header))
	}
	if header[3] != 0 {
		return framingLost("a client packet with sequence id %d where a command (sequence id 0) must start", header[3])
	}
	return nil
}

// later is the later of two times.
func later(a, b time.Time) time.Time {
	if b.After(a) {
		return b
	}
	return a
}

// maxErrPayload bounds the payload of an ERR packet: a code, a SQL state and
// a message of at most 512 bytes (MYSQL_ERRMSG_SIZE), with room to spare.
const maxErrPayload = 4096

// ownClientCaps is the capability flags of a connection joined mid-stream when
// the SSLRequest stored for it is provably its own (greetingOwn: pushed by its
// own raw leg). A client sends the same flags in its HandshakeResponse41. An
// SSLRequest borrowed from the cache (greetingCached) is another connection's,
// whose client may frame otherwise (a second driver in the same app): framed
// by it, a connection the recorder could settle would stop instead, so for it
// the framing is assumed and settled (DecodeContext.FramingAssumed).
func ownClientCaps(source preTLSGreetingSource, sslReq *mysql.PacketBundle) (uint32, bool) {
	if source != greetingOwn || sslReq == nil {
		return 0, false
	}
	req, ok := sslReq.Message.(*mysql.SSLRequestPacket)
	if !ok {
		return 0, false
	}
	return req.CapabilityFlags, true
}

// leavesFramingOpen reports whether a PREPARE's response, read on a framing
// that is only assumed, leaves it open: with one run of definitions, the
// packet after the run is its EOF (the client never negotiated
// CLIENT_DEPRECATE_EOF) or the answer to the client's next command (it did).
// See heldPrepare.
func leavesFramingOpen(decodeCtx *wire.DecodeContext, sp *mysql.StmtPrepareOkPacket) bool {
	return decodeCtx.FramingAssumed && decodeCtx.DeprecateEOF() && (sp.NumParams > 0) != (sp.NumColumns > 0)
}

// maxEOFPayload is the payload of a legacy EOF packet: 0xFE, warnings, status.
const maxEOFPayload = 5

// answerNeverAnEOF holds the commands whose answer never starts with a packet
// shaped like an EOF (0xFE, a payload of 5 bytes or 1): it is an OK, an ERR, a
// result's column count (one that starts 0xFE is 9 bytes), a PREPARE's OK, or
// a LOCAL INFILE request (0xFB). Any other command's answer may: COM_SET_OPTION
// and COM_DEBUG are answered with an EOF, a cursor's COM_STMT_FETCH with one
// once its rows ran out, COM_FIELD_LIST with one alone when no column
// matches, and COM_CHANGE_USER with an auth switch that, in its old form, is
// one 0xFE byte.
var answerNeverAnEOF = map[byte]bool{
	mysql.COM_QUERY:            true,
	mysql.COM_STMT_EXECUTE:     true,
	mysql.COM_STMT_PREPARE:     true,
	mysql.COM_PING:             true,
	mysql.COM_INIT_DB:          true,
	mysql.COM_STMT_RESET:       true,
	mysql.COM_RESET_CONNECTION: true,
	0x05:                       true, // COM_CREATE_DB: OK or ERR
	0x06:                       true, // COM_DROP_DB: OK or ERR
	0x07:                       true, // COM_REFRESH: OK or ERR
	0x0c:                       true, // COM_PROCESS_KILL: OK or ERR
}

// heldPrepare is a PREPARE read whole on a framing that is only assumed (a
// connection joined mid-stream), whose response has one run of definitions
// (leavesFramingOpen). Without CLIENT_DEPRECATE_EOF an EOF ends the run, and
// under it nothing does, so the server's next packet is the PREPARE's EOF or
// the answer to the client's next command. The two differ in sequence id: the
// EOF continues the PREPARE's sequence (eofSeq), and an answer starts its own.
//
// That packet comes only once the client sends a command with a response, so
// the recorder does not wait for it: a client that sends COM_STMT_CLOSE or
// COM_STMT_SEND_LONG_DATA next, or nothing for a while, would be left unread,
// its bytes pending work, and the connection retired as hung once it sat idle.
// It holds the PREPARE's mock instead and goes on reading the client. The
// first packet of the next response settles the framing (settle), or, when
// the client's stream ends first, what the server sent after the definitions
// does (end). The mocks of the commands without a response sent meanwhile are
// recorded behind the PREPARE's (queued), so mocks are emitted in the order
// their commands were sent. A recording that stops while a PREPARE is held
// leaves it out (leaveOut): its test case is not saved without its mock.
type heldPrepare struct {
	what string
	sp   *mysql.StmtPrepareOkPacket
	// eofSeq is the sequence id of the EOF after the definitions, if one came.
	eofSeq       byte
	reqTs, resTs time.Time
	// record emits the PREPARE's mock, its response ending at resTs.
	record func(resTs time.Time)
	// queued emits, in order, the mocks of the commands without a response
	// the client sent after the PREPARE.
	queued []func()
}

// queue holds the mock emit records behind the PREPARE's. The command's bytes
// are consumed, so they are no longer pending work.
func (h *heldPrepare) queue(sess *supervisor.Session, emit func()) {
	h.queued = append(h.queued, emit)
	clearPending(sess)
}

// release settles the framing, records the PREPARE with eof after its
// definitions when one came (the client framed with EOFs from then on), and
// then the mocks queued behind it.
func (h *heldPrepare) release(decodeCtx *wire.DecodeContext, eof []byte, at time.Time) {
	decodeCtx.FramingAssumed = false
	resTs := h.resTs
	if eof != nil {
		decodeCtx.ClientCaps &^= wire.CLIENT_DEPRECATE_EOF
		decodeCtx.ClientCapabilities &^= wire.CLIENT_DEPRECATE_EOF
		if h.sp.NumParams > 0 {
			h.sp.EOFAfterParamDefs = eof
		} else {
			h.sp.EOFAfterColumnDefs = eof
		}
		resTs = later(resTs, at)
	}
	h.record(resTs)
	h.flush()
}

// leaveOut leaves the PREPARE's exchange out, and records the mocks queued
// behind it, which do not depend on its framing. why is logged at WARN, but
// for lost framing, which is logged where what follows from it is decided
// (reportFault).
func (h *heldPrepare) leaveOut(logger *zap.Logger, sess *supervisor.Session, why error) {
	if errors.Is(why, ErrFramingLost) {
		sess.RecordOrphanWindow(h.reqTs, h.resTs)
		clearPending(sess)
	} else {
		leaveOut(logger, sess, h.reqTs, h.resTs, why)
	}
	h.flush()
}

func (h *heldPrepare) flush() {
	for _, emit := range h.queued {
		emit()
	}
	h.queued = nil
}

// settle reads the first packet of the response to cmd, whose sequence starts
// at respFirst, and settles the framing with it:
//   - an EOF that continues the PREPARE's sequence is the PREPARE's: the
//     client framed with EOFs. It is recorded with the PREPARE, and the
//     response is read from the packet after it (nil, nil). When the EOF's
//     sequence id is the one the answer starts at too (a run of 255
//     definitions, mod 256), it is the PREPARE's only when cmd's answer never
//     starts with an EOF (answerNeverAnEOF); otherwise either is possible, and
//     the recording stops.
//   - a packet that starts the answer: the client negotiated
//     CLIENT_DEPRECATE_EOF. It is returned as the response's first packet.
//   - the server's own ERR, at sequence id 0, as it drops the connection: no
//     EOF came, so the client negotiated CLIENT_DEPRECATE_EOF. It answers no
//     command (errServerDropped).
//
// A header with any other sequence id, or one at the EOF's that claims more
// than an EOF holds, is not where a packet starts, and is refused before the
// payload it claims is waited for. The PREPARE is recorded or left out by the
// time settle returns.
func (h *heldPrepare) settle(ctx context.Context, logger *zap.Logger, sess *supervisor.Session, decodeCtx *wire.DecodeContext, cmd, respFirst byte) ([]byte, error) {
	buf, err := mysqlUtils.ReadPacketBufferChecked(ctx, logger, sess.DestStream, func(header []byte) error {
		switch {
		case header[3] == respFirst && continues(header):
			// No response starts with a packet of 16 MiB or more (an OK, an
			// ERR, a column count, a PREPARE's OK, the server's own ERR).
			return framingLost("the first packet of the response after the %s response claims 16 MiB or more", h.what)
		case header[3] == respFirst:
		case header[3] == h.eofSeq && mysqlUtils.GetPayloadLength(header[:3]) <= maxEOFPayload:
		case respFirst != 0 && mayBeServersOwnErr(header):
		default:
			return framingLost("the packet after the definitions of the %s response has sequence id %d, neither %d (its EOF), %d (the next answer) nor 0 (an ERR of the server's own)", h.what, header[3], h.eofSeq, respFirst)
		}
		return nil
	})
	if err != nil {
		if errors.Is(err, ErrFramingLost) {
			err = found("V2: mysql response packet out of sequence", err)
			h.leaveOut(logger, sess, err)
			return nil, err
		}
		h.leaveOut(logger, sess, notRecorded("the recording stopped before a packet settled the %s response's framing: %v", h.what, err))
		return nil, err
	}
	switch {
	case buf[3] == h.eofSeq && mysqlUtils.IsEOFPacket(buf):
		if h.eofSeq == respFirst && !answerNeverAnEOF[cmd] {
			err := framingLostAt(logger, sess, "the packet after the definitions of the %s response is an EOF that may be its own or the answer to the next command", h.what)
			h.leaveOut(logger, sess, err)
			return nil, err
		}
		h.release(decodeCtx, buf, sess.DestStream.LastReadTime())
		return nil, nil
	case buf[3] == respFirst:
		h.release(decodeCtx, nil, time.Time{})
		return buf, nil
	case buf[3] == 0 && isServersOwnErr(buf):
		h.release(decodeCtx, nil, time.Time{})
		logger.Debug("V2: the server dropped the connection with an error of its own; its recording ends here",
			zap.String("connID", sess.ClientConnID), zap.ByteString("err", buf[4:]))
		return nil, errServerDropped
	default:
		err := framingLostAt(logger, sess, "the packet after the definitions of the %s response has sequence id %d and is neither its EOF, the next answer nor an ERR of the server's own", h.what, buf[3])
		h.leaveOut(logger, sess, err)
		return nil, err
	}
}

// end settles the framing once the client's stream ended with the PREPARE
// still held: no command with a response followed it, so all the server sent
// after the definitions is on its stream. An EOF that continues the
// PREPARE's sequence is its EOF; the server's own ERR means none came. A
// stream that ends with nothing more leaves the PREPARE out: no EOF arrived,
// but a capture that lost one cannot be told from a client that was never
// sent one. Any other packet is lost framing, which it returns.
func (h *heldPrepare) end(ctx context.Context, logger *zap.Logger, sess *supervisor.Session, decodeCtx *wire.DecodeContext) error {
	buf, err := mysqlUtils.ReadPacketBufferChecked(ctx, logger, sess.DestStream, func(header []byte) error {
		if header[3] == h.eofSeq && mysqlUtils.GetPayloadLength(header[:3]) <= maxEOFPayload || mayBeServersOwnErr(header) {
			return nil
		}
		return framingLost("the packet after the definitions of the %s response has sequence id %d, neither %d (its EOF) nor 0 (an ERR of the server's own)", h.what, header[3], h.eofSeq)
	})
	switch {
	case errors.Is(err, ErrFramingLost):
		err = found("V2: mysql response packet out of sequence", err)
		h.leaveOut(logger, sess, err)
		return err
	case err != nil:
		h.leaveOut(logger, sess, notRecorded("the connection ended before a packet settled the %s response's framing: %v", h.what, err))
		return nil
	case buf[3] == h.eofSeq && mysqlUtils.IsEOFPacket(buf):
		h.release(decodeCtx, buf, sess.DestStream.LastReadTime())
		return nil
	case buf[3] == 0 && isServersOwnErr(buf):
		h.release(decodeCtx, nil, time.Time{})
		return nil
	default:
		err := framingLostAt(logger, sess, "the packet after the definitions of the %s response has sequence id %d and is neither its EOF nor an ERR of the server's own", h.what, buf[3])
		h.leaveOut(logger, sess, err)
		return err
	}
}
