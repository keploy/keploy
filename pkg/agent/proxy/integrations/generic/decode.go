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
	"go.keploy.io/server/v3/pkg/provenswitch"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

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
		for {
			// Since protocol packets have to be parsed for checking stream end,
			// clientConnection have deadline for read to determine the end of stream.
			err := clientConn.SetReadDeadline(time.Now().Add(10 * time.Millisecond))
			if err != nil {
				utils.LogError(logger, err, "failed to set the read deadline for the client conn")
				return
			}

			// To read the stream of request packets from the client. The
			// connection's end is its EOF: read so that it is returned at once
			// (ReadBytes retries an empty EOF, 50 ms apart, and under this 10
			// ms deadline that retry comes back as a timeout, which hid the
			// close and left the parser polling it for ever).
			ended := false
			for {
				buffer, err := pUtil.ReadBytesUntilEOF(ctx, logger, clientConn)
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
				}
				if timeout || atEOF {
					logger.Debug("the timeout for the client read in generic or EOF")
					ended = atEOF
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
			// bestMatchedIndx := 0
			// fuzzy match gives the index for the best matched generic mock
			matched, genericResponses, err := fuzzyMatch(ctx, logger, genericRequests, db)
			if err != nil {
				utils.LogError(logger, err, "error while matching generic mocks")
			}
			if !matched && err == nil && provenswitch.On() {
				// The application's background work made again in the
				// test's window, after its marked recording answered
				// there: answered again with it, unchecked, not a miss.
				if again := backgroundAgain(genericRequests, db); again != nil {
					reportBackgroundAgain(ctx, genericRequests, again)
					matched, genericResponses = true, again.Spec.GenericResponses
				}
			}

			if !matched {
				preview := genericRequests[0]
				if len(preview) > 64 {
					preview = preview[:64]
				}
				// Build the universal mismatch report so generic misses show
				// up in the mismatch table / report yaml like HTTP and MySQL
				// misses do, instead of vanishing as a bare error.
				// Under exact serving: one exchange more than the test
				// recorded? Any other miss is compared with the test's
				// answered exchanges too: they are in no read now.
				n := extraCall(genericRequests, db)
				var answered []*models.Mock
				if n == 0 {
					answered = answeredExchanges(db)
				}
				report := buildGenericMismatchReport(ctx, genericRequests, db, answered)
				// The miss was decided where the request first read the
				// round: whether that read found a staging is told from it.
				integrations.StampMark(report, call)
				if n > 0 {
					report.MissKind, report.Recorded = models.MissExtraCall, n
				} else if name, restaged := notOwnedExchange(genericRequests, db, call); name != "" {
					report.ClosestMock, report.NotOwned = name, true
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
				// WARN is emitted once in proxy.sendMockNotFoundError (covers
				// every parser), so this stays Debug to avoid double-logging.
				logger.Debug("no matching generic mock found for outgoing call",
					zap.String("protocol", report.Protocol),
					zap.Int("requestCount", len(genericRequests)),
					zap.Int("firstRequestBytes", len(genericRequests[0])),
					zap.String("closest", report.ClosestMock),
					zap.String("diff", report.Diff),
					zap.String("next_step", report.NextSteps),
					zap.Binary("preview", preview))
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
