// Package http provides functionality for handling HTTP outgoing calls.
package http

import (
	"context"
	"fmt"
	"io"
	"net"
	"strconv"
	"strings"
	"time"

	pUtil "go.keploy.io/server/v3/pkg/agent/proxy/util"
	"go.keploy.io/server/v3/utils"
	"go.uber.org/zap"
)

// readRequestHead reads from clientConn, relaying each read to destConn when
// it is not nil (record mode; nil at replay), until finalReq holds the
// request's whole header section, and returns that section and where the body
// starts in finalReq. The header section is complete when messageHead finds
// it, not at the first "\r\n\r\n": that can be empty lines in front of the
// request line, which are then dropped from finalReq
// (dropEmptyLinesBeforeRequest).
func (h *HTTP) readRequestHead(ctx context.Context, finalReq *[]byte, clientConn, destConn net.Conn) (head []byte, bodyStart int, err error) {
	head, bodyStart, _, ok := messageHead(*finalReq, false)
	if ok {
		h.Logger.Debug("this request has complete headers in the first chunk itself.")
	}

	for ; !ok; head, bodyStart, _, ok = messageHead(*finalReq, false) {
		h.Logger.Debug("couldn't get complete headers in first chunk so reading more chunks")
		reqHeader, err := pUtil.ReadBytes(ctx, h.Logger, clientConn)
		if err != nil {
			utils.LogError(h.Logger, nil, "failed to read the request message from the client")
			return nil, 0, err
		}
		// destConn is nil in case of test mode
		if destConn != nil {
			_, err = destConn.Write(reqHeader)
			if err != nil {
				if ctx.Err() != nil {
					return nil, 0, ctx.Err()
				}
				utils.LogError(h.Logger, nil, "failed to write request message to the destination server")
				return nil, 0, err
			}
		}

		*finalReq = append(*finalReq, reqHeader...)
	}
	return head, bodyStart - dropEmptyLinesBeforeRequest(finalReq), nil
}

func (h *HTTP) HandleChunkedRequests(ctx context.Context, finalReq *[]byte, clientConn, destConn net.Conn) error {
	head, bodyStart, err := h.readRequestHead(ctx, finalReq, clientConn, destConn)
	if err != nil {
		return err
	}

	// The framing headers are read from the header section only: a body line
	// such as a multipart part's "Content-Length: 10" is not a header.
	contentLengthHeader, transferEncodingHeader := parseHeaders(head)

	//Handle chunked requests
	if contentLengthHeader != "" {
		contentLength, err := strconv.Atoi(contentLengthHeader)
		if err != nil {
			utils.LogError(h.Logger, err, "failed to get the content-length header")
			return fmt.Errorf("failed to handle chunked request")
		}
		//Get the length of the body in the request.
		contentLength -= len(*finalReq) - bodyStart
		if contentLength > 0 {
			err := h.contentLengthRequest(ctx, finalReq, clientConn, destConn, contentLength)
			if err != nil {
				return err
			}
		}
	} else if transferEncodingHeader != "" {
		if strings.Contains(strings.ToLower(transferEncodingHeader), "chunked") {
			if err := h.chunkedRequest(ctx, finalReq, clientConn, destConn); err != nil {
				return err
			}
		}
	}
	return nil
}

// Handled chunked requests when content-length is given.
func (h *HTTP) contentLengthRequest(ctx context.Context, finalReq *[]byte, clientConn, destConn net.Conn, contentLength int) error {
	// Use a larger buffer (e.g., 32KB) for better performance than 1KB
	buf := make([]byte, 32*1024)
	// The deadlines below bound each body read; the connection outlives this
	// request (keep-alive), so leave no deadline behind for the next one.
	defer func() { _ = clientConn.SetReadDeadline(time.Time{}) }()

	for contentLength > 0 {
		// 1. Check if context is already done before trying to read
		if ctx.Err() != nil {
			return ctx.Err()
		}

		// 2. Refresh the deadline
		err := clientConn.SetReadDeadline(time.Now().Add(20 * time.Second))
		if err != nil {
			utils.LogError(h.Logger, err, "failed to set the read deadline for the client conn")
			return err
		}

		// 3. Read directly from connection
		// This blocks only until *some* data is available or error occurs.
		readBuf := buf
		if contentLength < len(buf) {
			readBuf = buf[:contentLength]
		}
		n, err := clientConn.Read(readBuf)

		if n > 0 {
			chunk := buf[:n]

			// Append to final request
			*finalReq = append(*finalReq, chunk...)
			contentLength -= n

			h.Logger.Debug("Read chunk", zap.Int("chunkSize", n), zap.Int("remaining", contentLength))

			// Write to destination
			if destConn != nil {
				_, wErr := destConn.Write(chunk)
				if wErr != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					utils.LogError(h.Logger, wErr, "failed to write request message to the destination server")
					return wErr
				}
			}
		}

		if err != nil {
			if err == io.EOF {
				// Client closed connection cleanly
				utils.LogError(h.Logger, nil, "conn closed by the user client")
				return err
			}

			// A timeout only bounds this read, so a cancelled ctx is noticed:
			// a client may pause mid-body, and the rest of the body follows.
			// Ending the request here answered (or relayed) it cut short and
			// read the rest of its body as the next request.
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				h.Logger.Debug("no request body bytes before the read deadline; reading on",
					zap.Int("remaining", contentLength), zap.Error(err))
				continue
			}

			// Check for Context Cancel (if Read failed due to context closure wrapped in net error)
			if ctx.Err() != nil {
				return ctx.Err()
			}

			utils.LogError(h.Logger, err, "failed to read the response message from the destination server")
			return err
		}
	}
	return nil
}

// chunkedRequest reads the rest of a chunked request body — finalReq already
// holds the headers and whatever body bytes arrived with them — until the body's
// chunked framing says it has ended, forwarding each piece to destConn when
// there is one (record mode; nil at replay).
//
// The end is found by framing the chunks (chunkedBody), never by looking for
// "0\r\n\r\n" at the end of a read: that missed a terminator split across
// reads, which is how Java's HttpURLConnection sends a chunked-streaming
// upload, and left replay waiting for bytes the app would never send.
func (h *HTTP) chunkedRequest(ctx context.Context, finalReq *[]byte, clientConn, destConn net.Conn) error {
	var body chunkedBody
	// The read deadline below bounds each read so a cancelled ctx is noticed;
	// the connection outlives this request, so leave none behind.
	defer func() { _ = clientConn.SetReadDeadline(time.Time{}) }()
	complete := func() (bool, error) {
		if destConn == nil {
			// Replay: nothing is relayed, and a body that cannot be framed
			// cannot be answered either.
			return body.complete(*finalReq)
		}
		// Record: the proxy is in the app's live path, so a peer that does
		// not frame its body by the RFC must still get it relayed.
		done, framingErr := body.completeOrLegacy(*finalReq)
		if framingErr != nil {
			h.Logger.Warn("chunked request body is not framed per RFC 9112; relaying it and taking a trailing \"0\\r\\n\\r\\n\" as its end, as before",
				zap.Error(framingErr))
		}
		return done, nil
	}
	for {
		done, err := complete()
		if err != nil {
			utils.LogError(h.Logger, err, "failed to frame the chunked request body")
			return err
		}
		if done {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		// A client may legitimately pause mid-body, so a timeout just reads on.
		if err := clientConn.SetReadDeadline(time.Now().Add(5 * time.Second)); err != nil {
			utils.LogError(h.Logger, err, "failed to set the read deadline for the client conn")
			return err
		}
		piece, err := pUtil.ReadBytes(ctx, h.Logger, clientConn)
		// ReadBytes hands back what it read before an error along with the
		// error; those bytes are part of the request and must be kept (dropping
		// them on a timeout lost chunks and the terminator).
		if len(piece) > 0 {
			*finalReq = append(*finalReq, piece...)
			// destConn is nil in case of test mode.
			if destConn != nil {
				if _, werr := destConn.Write(piece); werr != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					utils.LogError(h.Logger, nil, "failed to write request message to the destination server")
					return werr
				}
			}
		}
		if err != nil {
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				continue
			}
			if done, ferr := complete(); ferr == nil && done {
				return nil // the last bytes arrived together with the client closing
			}
			utils.LogError(h.Logger, nil, "failed to read the request message from the client")
			return err
		}
	}
}

// handleChunkedResponses relays and records the rest of a response whose first
// bytes (resp, also in finalResp) have been read. reqMethod is the method of the
// request it answers: the response to a HEAD has no body whatever its headers
// say.
func (h *HTTP) handleChunkedResponses(ctx context.Context, finalResp *[]byte, clientConn, destConn net.Conn, resp []byte, reqMethod string) error {
	var (
		head      []byte
		bodyStart int
		status    int
	)
	for {
		var ok bool
		if head, bodyStart, status, ok = messageHead(resp, true); ok {
			break // the final (non-1xx) response's header section is here
		}
		h.Logger.Debug("the response's header section is not complete yet; reading more")
		respHeader, err := pUtil.ReadBytes(ctx, h.Logger, destConn)
		if err != nil {
			if err == io.EOF {
				h.Logger.Debug("received EOF from the server")
				// if there is any buffer left before EOF, we must send it to the client and save this as mock
				if len(respHeader) != 0 {
					// write the response message to the user client
					_, err = clientConn.Write(resp)
					if err != nil {
						if ctx.Err() != nil {
							return ctx.Err()
						}
						utils.LogError(h.Logger, nil, "failed to write response message to the user client")
						return err
					}
					*finalResp = append(*finalResp, respHeader...)
				}
				return err
			}
			utils.LogError(h.Logger, nil, "failed to read the response message from the destination server")
			return err
		}
		// write the response message to the user client
		_, err = clientConn.Write(respHeader)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			utils.LogError(h.Logger, nil, "failed to write response message to the user client")
			return err
		}

		*finalResp = append(*finalResp, respHeader...)
		resp = append(resp, respHeader...)
	}

	if responseHasNoBody(reqMethod, status) {
		return nil // no body, whatever the headers say
	}
	// The framing headers of the final response, read from its header section
	// only (a body line is not a header).
	contentLengthHeader, transferEncodingHeader := parseHeaders(head)

	if contentLengthHeader != "" {
		contentLength, err := strconv.Atoi(contentLengthHeader)
		if err != nil {
			utils.LogError(h.Logger, err, "failed to get the content-length header")
			return fmt.Errorf("failed to handle chunked response")
		}
		contentLength -= len(resp) - bodyStart
		if contentLength > 0 {
			err := h.contentLengthResponse(ctx, finalResp, clientConn, destConn, contentLength)
			if err != nil {
				return err
			}
		}
	} else if strings.Contains(strings.ToLower(transferEncodingHeader), "chunked") {
		if err := h.chunkedResponse(ctx, finalResp, clientConn, destConn); err != nil {
			return err
		}
	}
	return nil
}

// chunkedResponse relays the rest of a chunked response from destConn to
// clientConn — finalResp already holds the status line, headers and whatever
// body bytes came with them — until the body's chunked framing says it has
// ended, or the server closes the connection.
//
// Like chunkedRequest it frames the chunks rather than looking for
// "0\r\n\r\n": the suffix test replaced a strict-equality test that hung on
// a terminator sharing a TLS record with body bytes (sap-demo-java /360), but
// it still hung on a terminator split across reads or followed by trailers,
// and ended a response early on chunk data that ends like a terminator.
func (h *HTTP) chunkedResponse(ctx context.Context, finalResp *[]byte, clientConn, destConn net.Conn) error {
	body := chunkedBody{response: true}
	for {
		// The proxy relays this response to the app as it reads it, so a
		// server that does not frame its body by the RFC must still have it
		// relayed (completeOrLegacy).
		done, framingErr := body.completeOrLegacy(*finalResp)
		if framingErr != nil {
			h.Logger.Warn("chunked response body is not framed per RFC 9112; relaying it and taking a trailing \"0\\r\\n\\r\\n\" or the server closing as its end, as before",
				zap.Error(framingErr))
		}
		if done {
			return nil
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		resp, err := pUtil.ReadBytes(ctx, h.Logger, destConn)
		if err != nil && err != io.EOF {
			utils.LogError(h.Logger, err, "failed to read the response message from the destination server")
			return err
		}
		if len(resp) > 0 {
			*finalResp = append(*finalResp, resp...)
			// write the response message to the user client
			if _, werr := clientConn.Write(resp); werr != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				utils.LogError(h.Logger, nil, "failed to write response message to the user client")
				return werr
			}
		}
		if err == io.EOF {
			// The server closed the connection: whatever it sent is the
			// response.
			h.Logger.Debug("received EOF from the destination server while reading a chunked response")
			return nil
		}
	}
}

// Handled chunked responses when content-length is given.
func (h *HTTP) contentLengthResponse(ctx context.Context, finalResp *[]byte, clientConn, destConn net.Conn, contentLength int) error {
	isEOF := false
	for contentLength > 0 {
		resp, err := pUtil.ReadBytes(ctx, h.Logger, destConn)
		if err != nil {
			if err == io.EOF {
				isEOF = true
				h.Logger.Debug("received EOF, conn closed by the destination server")
				if len(resp) == 0 {
					break
				}
			} else if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				h.Logger.Info("Stopped getting data from the conn", zap.Error(err))
				break
			} else {
				utils.LogError(h.Logger, nil, "failed to read the response message from the destination server")
				return err
			}
		}

		h.Logger.Debug("This is a chunk of response[content-length]: " + string(resp))
		*finalResp = append(*finalResp, resp...)
		contentLength -= len(resp)

		// write the response message to the user client
		_, err = clientConn.Write(resp)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			utils.LogError(h.Logger, nil, "failed to write response message to the user client")
			return err
		}

		if isEOF {
			break
		}
	}
	return nil
}
