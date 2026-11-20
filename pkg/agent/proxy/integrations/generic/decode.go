// Package generic provides functionality for decoding generic dependencies.
package generic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// quietWindow is a request's window: the request is what the parser reads
// within it from its first bytes, with what the socket holds unread once it
// has passed, and a round with no request ends after it.
// The generic protocol has no framing the parser could read a request's end
// from.
const quietWindow = 10 * time.Millisecond

// decodeGeneric answers the client's requests from the mocks, on a goroutine,
// and returns only once that goroutine has returned, ctx's end included: then
// its read or write of clientConn is ended (pUtil.InterruptOnDone), and it is
// waited for (pUtil.JoinParser). It used to return ctx's error at once and
// leave the goroutine reading, matching and logging after it had returned.
func decodeGeneric(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn net.Conn, dstCfg *models.ConditionalDstCfg, mockDb integrations.MockMemDb, _ models.OutgoingOptions) error {
	defer pUtil.InterruptOnDone(ctx, clientConn)()
	// The first round's request begins with reqBuf, the bytes the proxy
	// read. A parser handed none has no request until it reads one: a round
	// that reads nothing is no request.
	var genericRequests [][]byte
	if len(reqBuf) > 0 {
		genericRequests = [][]byte{reqBuf}
	}
	logger.Debug("Into the generic parser in test mode")
	// The requests it has read are held from the read on: it reads on with
	// a deadline before it matches them, blocked reading while it holds them
	// (integrations.HoldRequest, a test's end count). Released once
	// answered; a miss, returned below, is filed before the connection is
	// let go, which drops the mark.
	release, holding := func() {}, false
	if len(reqBuf) > 0 {
		release, holding = integrations.HoldRequest(ctx), true
	}
	// begun: the first round begins with the request's first bytes.
	begun := len(reqBuf) > 0
	errCh := make(chan error, 1)
	go func(errCh chan error, genericRequests [][]byte) {
		defer close(errCh)
		defer integrations.RecoverIntoCtx(ctx, logger, clientConn, nil, errCh)
		for {
			// Since protocol packets have to be parsed for checking stream end,
			// clientConnection have deadline for read to determine the end of stream.
			// The round waits one window from its start for a request.
			start := time.Now()
			deadline := start.Add(quietWindow)
			err := clientConn.SetReadDeadline(deadline)
			if err != nil {
				utils.LogError(logger, err, "failed to set the read deadline for the client conn")
				return
			}
			// first is when the parser read the request's first bytes: the
			// round's start for a round that begins with them (reqBuf), and
			// zero until they come.
			var first time.Time
			if begun {
				first, begun = start, false
			}
			// looked: the parser looked, past the request's window, at what
			// the socket under clientConn holds unread; left is what of
			// that the socket has still to give, in the socket's bytes.
			// drained: the socket has given it all, and the parser reads,
			// without waiting, what a layer above the socket still holds.
			looked, left, drained := false, int64(0), false

			// To read the stream of request packets from the client. The
			// connection's end is its EOF, which ReadBytes returns at the
			// read that gets it.
			ended := false
			for {
				// Past the look, a read is counted at the socket: what it
				// holds unread before the read less what it holds after.
				// Through TLS, or a buffered reader, a read returns bytes in
				// other units than it takes from the socket (a record's
				// plaintext, or what a layer read ahead), so what it returned
				// says nothing of what the socket gave. What arrives as the
				// read takes is not counted as taken, so left can only fall
				// short of what the socket gave: the parser never stops
				// short of what it held at the look.
				var before int64
				if looked {
					before = pUtil.UnreadBytes(clientConn)
				}
				buffer, err := pUtil.ReadBytes(ctx, logger, clientConn)
				netErr, isNet := err.(net.Error)
				timeout := isNet && netErr.Timeout()
				atEOF := errors.Is(err, io.EOF) || (err != nil && err.Error() == "EOF")
				if err != nil && !timeout && !atEOF {
					utils.LogError(logger, err, "failed to read the request message in proxy for generic dependency")
					return
				}
				// What a read returned with its deadline's timeout or its EOF
				// is a request: a request as long as the read buffer (or a
				// multiple) comes back with the next read's timeout, and one
				// a close follows at once (a TLS 1.2 close_notify, or a full
				// buffer's next read) with its EOF. Any other error has
				// returned above.
				if len(buffer) > 0 {
					if !holding {
						release, holding = integrations.HoldRequest(ctx), true
					}
					genericRequests = append(genericRequests, buffer)
					if first.IsZero() {
						first = time.Now()
					}
				}
				if atEOF {
					logger.Debug("the client closed the generic connection")
					ended = true
					break
				}
				// Draining reads have a deadline that has passed: each gives
				// what a layer above the socket holds whole (a TLS record,
				// a buffered reader's bytes), and the first that would read
				// the socket times out at once.
				if drained {
					if timeout {
						break
					}
					continue
				}
				if looked {
					if took := before - pUtil.UnreadBytes(clientConn); took > 0 {
						left -= took
					}
					// The socket has given what it held at the look. The
					// layers above it may hold some of it (a TLS layer reads
					// ahead of its record): read without waiting.
					if left <= 0 {
						drained = true
						if err := clientConn.SetReadDeadline(time.Unix(1, 0)); err != nil {
							utils.LogError(logger, err, "failed to set the read deadline for the client conn")
							return
						}
						continue
					}
				}
				// A deadline that passed before the request's window did
				// (one window from its first bytes, as the parser read
				// them) is not the request's end: the bytes came late in
				// the round, or the parser was held past the deadline
				// while the rest sat in the socket, and a read whose
				// deadline passed returns its timeout without reading what
				// the socket holds. The parser reads on to the window's
				// end.
				if timeout && deadline.Before(first.Add(quietWindow)) {
					deadline = first.Add(quietWindow)
					if err := clientConn.SetReadDeadline(deadline); err != nil {
						utils.LogError(logger, err, "failed to set the read deadline for the client conn")
						return
					}
					continue
				}
				// Past the window, the bytes the socket holds unread came
				// before the parser looked: a parser held past the window's
				// deadline too, between two reads, would have cut the
				// request there. They are the request's: the parser reads
				// on until the socket has given as many bytes as it held at
				// its first look, each read with a deadline one window from
				// when it is set, so a read held past one reads on while the
				// socket still holds some, and one that finds the socket
				// empty at its deadline ends the request. A read times out
				// only once no layer above the socket holds anything whole
				// to give, so what the socket holds is all there is to read.
				// It looks once, so a request whose bytes never pause still
				// ends: once the socket has given what it held then (less
				// what arrived as each read took).
				if timeout && !first.IsZero() {
					if unread := pUtil.UnreadBytes(clientConn); unread > 0 {
						if !looked {
							looked, left = true, unread
						}
						deadline = time.Now().Add(quietWindow)
						if err := clientConn.SetReadDeadline(deadline); err != nil {
							utils.LogError(logger, err, "failed to set the read deadline for the client conn")
							return
						}
						continue
					}
				}
				if timeout {
					logger.Debug("the timeout for the client read in generic")
					break
				}
			}

			if len(genericRequests) == 0 {
				if ended {
					logger.Debug("the application closed the generic connection")
					return
				}
				logger.Debug("the generic request buffer is empty")
				continue
			}

			// The request's first read of the round, before the matcher
			// reads the store: a miss's lookup of an exchange made
			// elsewhere is asked of it (notOwnedExchange).
			call := integrations.MarkCallRound(mockDb)
			// The matcher and the miss read the store held to that read's
			// staging (integrations.HoldToRound): once the replay stages
			// another set, the request finds nothing of it to be answered
			// with, nor compared with, and misses saying so.
			db, _ := integrations.HoldToRound(mockDb, call)
			// An exchange one of the open test's own recordings is, byte for
			// byte (extraCall's reading), answered or not, is read with the
			// set's background hidden once the background is about to answer
			// it: no pool or worker recording answers an exchange one more
			// than the test recorded, which then misses as such, its
			// background copy left unspent, even when two of them are made at
			// once. An exchange the test's own recordings answer never asks.
			held := db
			db = integrations.OwnRepeatRead(db, call, func() bool {
				return findExactMatch(ownExchanges.Of(held, sentKey(genericRequests)), genericRequests, true) != -1
			})
			// bestMatchedIndx := 0
			// fuzzy match gives the index for the best matched generic mock
			matched, genericResponses, err := fuzzyMatch(ctx, logger, genericRequests, db)
			if err != nil {
				utils.LogError(logger, err, "error while matching generic mocks")
			}

			if !matched {
				// An exchange that missed before the background could answer
				// it asks now whether it is one of the open test's own
				// (integrations.OwnRepeat): one is compared and reported with
				// the background hidden.
				integrations.OwnRepeat(db)
				// Build the universal mismatch report so generic misses show
				// up in the mismatch table / report yaml like HTTP and MySQL
				// misses do, instead of vanishing as a bare error.
				// Under exact serving: one exchange more than the test
				// recorded? Any other miss is compared with the test's
				// answered exchanges too: they are in no read now.
				// One whose view moved (n < 0) is told neither: it misses
				// plainly, compared with no answered exchange, and no
				// exchange made elsewhere is named for it.
				n := extraCall(genericRequests, db)
				var answered []*models.Mock
				if n == 0 {
					answered, _ = answeredExchanges(db)
				}
				report := buildGenericMismatchReport(ctx, genericRequests, db, answered)
				// The miss was decided where the request first read the
				// round: whether that read found a staging is told from it.
				integrations.StampMark(report, call)
				if n > 0 {
					report.MissKind, report.Recorded = models.MissExtraCall, n
				} else if name, restaged := notOwnedExchange(genericRequests, db, call); name != "" && n == 0 {
					// The call is name, so no diff: the report's own
					// was against the closest exchange it compared,
					// another one.
					report.ClosestMock, report.NotOwned, report.Diff = name, true, ""
					report.NextSteps = notOwnedExchangeNextSteps(name)
				} else if restaged {
					// The lookup checks the staging last: a matcher held to
					// the request's first read that found it moved found
					// nothing (integrations.HoldToRound).
					report.NextSteps = strings.TrimSpace(report.NextSteps + " " + restagedExchangeNote)
				} else if why := integrations.UnheldWhy(db); why != "" {
					// The store marked the request's round but could not
					// hold it: its matcher read nothing (HoldToRound).
					report.NextSteps = strings.TrimSpace(report.NextSteps + " " + why)
				}
				// Per-call generic detail at Debug; the canonical mock-mismatch
				// WARN is emitted once in proxy.sendMockNotFoundErrorFrom (covers
				// every parser), so this stays Debug to avoid double-logging.
				// It prints only what the report prints, and the report
				// carries none of the live bytes: a live buffer at a place a
				// recording redacted (a token the obfuscator took out) is a
				// secret, at any log level.
				logger.Debug("no matching generic mock found for outgoing call",
					zap.String("protocol", report.Protocol),
					zap.Int("requestCount", len(genericRequests)),
					zap.Int("firstRequestBytes", len(genericRequests[0])),
					zap.String("closest", report.ClosestMock),
					zap.String("diff", report.Diff),
					zap.String("next_step", models.MissLogNextStep(report)))
				errCh <- models.NewMockMismatchError(fmt.Errorf("generic: %w", models.ErrNoMockMatched), report)
				return
			}
			for _, genericResponse := range genericResponses {
				encoded := []byte(genericResponse.Message[0].Data)
				if genericResponse.Message[0].Type != models.String {
					encoded, err = util.DecodeBase64(genericResponse.Message[0].Data)
					if err != nil {
						utils.LogError(logger, err, "failed to decode the base64 response")
						return
					}
				}
				_, err := clientConn.Write(encoded)
				if err != nil {
					if ctx.Err() != nil {
						return
					}
					utils.LogError(logger, err, "failed to write the response message to the client application")
					return
				}
			}

			// Answered: no request is held until the next one is read.
			release()
			holding = false
			// Clear the genericRequests buffer for the next dependency call
			genericRequests = [][]byte{}
			if ended {
				return
			}
			logger.Debug("the genericRequests after the iteration", zap.Int("length", len(genericRequests)))
		}
	}(errCh, genericRequests)

	return pUtil.JoinParser(ctx, errCh)
}
