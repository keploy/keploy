// Package generic provides functionality for decoding generic dependencies.
package generic

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync/atomic"
	"time"

	"go.keploy.io/server/v3/pkg/agent/proxy/integrations"
	"go.keploy.io/server/v3/pkg/agent/proxy/integrations/util"
	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/pkg/models"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// quietWindow is how long a round of the parser reads its connection: the
// request is what came in it. The generic protocol has no framing the
// parser could read a request's end from.
const quietWindow = 10 * time.Millisecond

// restWait is how long, in nanoseconds, the parser waits for the rest of a
// request whose bytes begin a recorded request (restToCome). Atomic: a row
// sets it while another row's parser may still read it.
var restWait atomic.Int64

func init() { restWait.Store(int64(time.Second)) }

// decodeGeneric answers the client's requests from the mocks, on a goroutine,
// and returns only once that goroutine has returned, ctx's end included: then
// its read or write of clientConn is ended (pUtil.InterruptOnDone), and it is
// waited for (pUtil.JoinParser). It used to return ctx's error at once and
// leave the goroutine reading, matching and logging after it had returned.
func decodeGeneric(ctx context.Context, logger *zap.Logger, reqBuf []byte, clientConn net.Conn, dstCfg *models.ConditionalDstCfg, mockDb integrations.MockMemDb, _ models.OutgoingOptions) error {
	defer pUtil.InterruptOnDone(ctx, clientConn)()
	genericRequests := [][]byte{reqBuf}
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
	errCh := make(chan error, 1)
	go func(errCh chan error, genericRequests [][]byte) {
		defer close(errCh)
		defer integrations.RecoverIntoCtx(ctx, logger, clientConn, nil, errCh)
		// readPieces reads the request's pieces until a read returns the
		// deadline's timeout or the connection's EOF, which ReadBytes returns
		// at the read that gets it. It returns whether it was the EOF, how
		// many bytes it read, and false for any other error (logged). With
		// rearm, a read that returns bytes gives the next one quietWindow
		// from then.
		readPieces := func(rearm bool) (ended bool, got int, ok bool) {
			for {
				buffer, err := pUtil.ReadBytes(ctx, logger, clientConn)
				netErr, isNet := err.(net.Error)
				timeout := isNet && netErr.Timeout()
				atEOF := errors.Is(err, io.EOF) || (err != nil && err.Error() == "EOF")
				if err != nil && !timeout && !atEOF {
					utils.LogError(logger, err, "failed to read the request message in proxy for generic dependency")
					return false, got, false
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
					got += len(buffer)
				}
				if timeout || atEOF {
					logger.Debug("the timeout for the client read in generic or EOF")
					return atEOF, got, true
				}
				if rearm {
					if err := clientConn.SetReadDeadline(time.Now().Add(quietWindow)); err != nil {
						utils.LogError(logger, err, "failed to set the read deadline for the client conn")
						return false, got, false
					}
				}
			}
		}
		for {
			// Since protocol packets have to be parsed for checking stream end,
			// clientConnection have deadline for read to determine the end of stream.
			err := clientConn.SetReadDeadline(time.Now().Add(quietWindow))
			if err != nil {
				utils.LogError(logger, err, "failed to set the read deadline for the client conn")
				return
			}

			// To read the stream of request packets from the client: the
			// round's window is from its start, so bytes that never pause
			// still end a request.
			ended, _, ok := readPieces(false)
			if !ok {
				return
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
			// A request whose bytes begin a recorded request, and are none,
			// is not whole: its window ended between two of its reads (it
			// came late in the round, or the parser was held past the
			// deadline while the rest sat in the socket). Its rest is read
			// before it is matched: what the socket holds comes back at
			// once, what is still on its way within restWait. Matched in
			// part, it missed, or took a recording's answer and left its
			// rest to miss as a request of its own. A request whose rest
			// never comes is matched as it is, after restWait.
			for !ended && restToCome(db, genericRequests) {
				if err := clientConn.SetReadDeadline(time.Now().Add(time.Duration(restWait.Load()))); err != nil {
					utils.LogError(logger, err, "failed to set the read deadline for the client conn")
					return
				}
				var got int
				if ended, got, ok = readPieces(true); !ok {
					return
				}
				if got == 0 {
					break
				}
			}
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
