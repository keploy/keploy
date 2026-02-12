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

func (h *HTTP) HandleChunkedRequests(ctx context.Context, finalReq *[]byte, clientConn, destConn net.Conn) error {

	if hasCompleteHeaders(*finalReq) {
		h.Logger.Debug("this request has complete headers in the first chunk itself.")
	}

	for !hasCompleteHeaders(*finalReq) {
		h.Logger.Debug("couldn't get complete headers in first chunk so reading more chunks")
		reqHeader, err := pUtil.ReadBytes(ctx, h.Logger, clientConn)
		if err != nil {
			utils.LogError(h.Logger, nil, "failed to read the request message from the client")
			return err
		}
		// destConn is nil in case of test mode
		if destConn != nil {
			_, err = destConn.Write(reqHeader)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				utils.LogError(h.Logger, nil, "failed to write request message to the destination server")
				return err
			}
		}

		*finalReq = append(*finalReq, reqHeader...)
	}

	lines := strings.Split(string(*finalReq), "\n")
	var contentLengthHeader, transferEncodingHeader string
	for _, line := range lines {
		line = strings.TrimRight(line, "\r")
		if line == "" {
			continue
		}

		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.ToLower(strings.TrimSpace(parts[0]))
		val := strings.TrimSpace(parts[1])

		switch key {
		case "content-length":
			contentLengthHeader = val
		case "transfer-encoding":
			transferEncodingHeader = val
		}
	}

	//Handle chunked requests
	if contentLengthHeader != "" {
		contentLength, err := strconv.Atoi(contentLengthHeader)
		if err != nil {
			utils.LogError(h.Logger, err, "failed to get the content-length header")
			return fmt.Errorf("failed to handle chunked request")
		}
		//Get the length of the body in the request.
		bodyLength := len(*finalReq) - strings.Index(string(*finalReq), "\r\n\r\n") - 4
		contentLength -= bodyLength
		if contentLength > 0 {
			err := h.contentLengthRequest(ctx, finalReq, clientConn, destConn, contentLength)
			if err != nil {
				return err
			}
		}
	} else if transferEncodingHeader != "" {
		if strings.Contains(strings.ToLower(transferEncodingHeader), "chunked") {
			if strings.HasSuffix(string(*finalReq), "0\r\n\r\n") {
				return nil
			}
			if err := h.chunkedRequest(ctx, finalReq, clientConn, destConn, transferEncodingHeader); err != nil {
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

			// Check for Timeout
			if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
				h.Logger.Info("Stopped getting data from the conn (Timeout)", zap.Error(err))
				break
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

// Handled chunked requests when transfer-encoding is given.
func (h *HTTP) chunkedRequest(ctx context.Context, finalReq *[]byte, clientConn, destConn net.Conn, _ string) error {

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			//TODO: we have to implement a way to read the buffer chunk wise according to the chunk size (chunk size comes in hexadecimal)
			// because it can happen that some chunks come after 5 seconds.
			err := clientConn.SetReadDeadline(time.Now().Add(5 * time.Second))
			if err != nil {
				utils.LogError(h.Logger, err, "failed to set the read deadline for the client conn")
				return err
			}
			requestChunked, err := pUtil.ReadBytes(ctx, h.Logger, clientConn)
			if err != nil {
				if netErr, ok := err.(net.Error); ok && netErr.Timeout() {
					break
				}
				utils.LogError(h.Logger, nil, "failed to read the response message from the destination server")
				return err
			}

			*finalReq = append(*finalReq, requestChunked...)
			// destConn is nil in case of test mode.
			if destConn != nil {
				_, err = destConn.Write(requestChunked)
				if err != nil {
					if ctx.Err() != nil {
						return ctx.Err()
					}
					utils.LogError(h.Logger, nil, "failed to write request message to the destination server")
					return err
				}
			}

			//check if the initial request is completed
			if strings.HasSuffix(string(requestChunked), "0\r\n\r\n") {
				return nil
			}
		}
	}
}

func (h *HTTP) handleChunkedResponses(ctx context.Context, finalResp *[]byte, clientConn, destConn net.Conn, resp []byte) error {

	if hasCompleteHeaders(*finalResp) {
		h.Logger.Debug("this response has complete headers in the first chunk itself.")
	}

	for !hasCompleteHeaders(resp) {
		h.Logger.Debug("couldn't get complete headers in first chunk so reading more chunks")
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

	//Getting the content-length or the transfer-encoding header
	var contentLengthHeader, transferEncodingHeader string
	lines := strings.Split(string(resp), "\n")
	for _, line := range lines {
		line = strings.TrimRight(line, "\r") // remove trailing \r if present
		if line == "" {
			continue
		}

		// Split key: value
		parts := strings.SplitN(line, ":", 2)
		if len(parts) != 2 {
			continue
		}

		key := strings.ToLower(strings.TrimSpace(parts[0]))
		val := strings.TrimSpace(parts[1])

		switch key {
		case "content-length":
			contentLengthHeader = val
		case "transfer-encoding":
			transferEncodingHeader = val
		}
	}

	if contentLengthHeader != "" {
		contentLength, err := strconv.Atoi(contentLengthHeader)
		if err != nil {
			utils.LogError(h.Logger, err, "failed to get the content-length header")
			return fmt.Errorf("failed to handle chunked response")
		}
		bodyLength := len(resp) - strings.Index(string(resp), "\r\n\r\n") - 4
		contentLength -= bodyLength
		if contentLength > 0 {
			err := h.contentLengthResponse(ctx, finalResp, clientConn, destConn, contentLength)
			if err != nil {
				return err
			}
		}
	} else if transferEncodingHeader != "" {
		if strings.Contains(strings.ToLower(transferEncodingHeader), "chunked") {
			if strings.HasSuffix(string(*finalResp), "0\r\n\r\n") {
				return nil
			}
			if err := h.chunkedResponse(ctx, finalResp, clientConn, destConn); err != nil {
				return err
			}
		}
	}
	return nil
}

// Handles chunked responses when transfer-encoding is given.
func (h *HTTP) chunkedResponse(ctx context.Context, finalResp *[]byte, clientConn, destConn net.Conn) error {
	isEOF := false
ReadLoop:
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		default:
			resp, err := pUtil.ReadBytes(ctx, h.Logger, destConn)
			if err != nil {
				if err != io.EOF {
					utils.LogError(h.Logger, err, "failed to read the response message from the destination server")
					return err
				}
				isEOF = true
				h.Logger.Debug("received EOF", zap.Error(err))
				if len(resp) == 0 {
					h.Logger.Debug("exiting loop as response is complete")
					break ReadLoop
				}
			}

			*finalResp = append(*finalResp, resp...)
			// write the response message to the user client
			_, err = clientConn.Write(resp)
			if err != nil {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				utils.LogError(h.Logger, nil, "failed to write response message to the user client")
				return err
			}

			//In some cases need to write the response to the client
			// where there is some response before getting the true EOF
			if isEOF {
				break ReadLoop
			}

			if string(resp) == "0\r\n\r\n" {
				return nil
			}
		}
	}
	return nil
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
