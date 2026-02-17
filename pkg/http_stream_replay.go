// Package pkg provides utility functions for Keploy.
package pkg

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"go.keploy.io/server/v3/pkg/models"
	"go.uber.org/zap"
)

func readHTTPResponseBodyWithStreamSupport(logger *zap.Logger, httpResp *http.Response, expected models.HTTPResp, apiTimeout uint64) ([]byte, []models.HTTPStreamEvent, models.HTTPStreamType, error) {
	if httpResp == nil || httpResp.Body == nil {
		return nil, nil, "", nil
	}

	expectedStream := expected.StreamType != "" || len(expected.StreamEvents) > 0
	if !expectedStream {
		body, err := io.ReadAll(httpResp.Body)
		return body, nil, "", err
	}

	streamType := expected.StreamType
	if streamType == "" {
		if detected, ok := DetectHTTPStreamType(httpResp); ok {
			streamType = detected
		} else {
			streamType = models.HTTPStreamTypeHTTP
		}
	}

	timeout := time.Duration(apiTimeout) * time.Second
	switch streamType {
	case models.HTTPStreamTypeSSE:
		body, events, err := readSSEStreamBody(httpResp.Body, expected.StreamEvents, timeout)
		if err != nil {
			return body, events, streamType, err
		}
		return body, events, streamType, nil
	default:
		body, events, err := readHTTPChunkStreamBody(httpResp.Body, expected.StreamEvents, timeout)
		if err != nil {
			return body, events, streamType, err
		}
		return body, events, streamType, nil
	}
}

func readSSEStreamBody(bodyReader io.Reader, expected []models.HTTPStreamEvent, timeout time.Duration) ([]byte, []models.HTTPStreamEvent, error) {
	buf := make([]byte, 4096)
	payload := bytes.NewBuffer(nil)
	events := make([]models.HTTPStreamEvent, 0, max(1, len(expected)))
	pending := make([]byte, 0, 512)
	sequence := 0
	deadline := time.Now().Add(timeout)
	target := len(expected)

	for {
		n, err := bodyReader.Read(buf)
		now := time.Now()
		if n > 0 {
			chunk := buf[:n]
			payload.Write(chunk)
			pending = append(pending, chunk...)

			parsed, remaining := ExtractSSEEvents(pending)
			pending = remaining

			for _, evt := range parsed {
				sequence++
				events = append(events, models.HTTPStreamEvent{
					Sequence:  sequence,
					Data:      evt,
					Timestamp: now,
				})
				if target > 0 && len(events) >= target {
					return payload.Bytes(), events[:target], nil
				}
			}
		}

		if err != nil {
			if err == io.EOF {
				break
			}
			if isTimeoutError(err) {
				return payload.Bytes(), events, nil
			}
			return payload.Bytes(), events, err
		}

		if timeout > 0 && time.Now().After(deadline) {
			return payload.Bytes(), events, nil
		}
	}

	if len(pending) > 0 {
		evt := NormalizeSSEEventData(string(pending))
		if evt != "" {
			sequence++
			events = append(events, models.HTTPStreamEvent{
				Sequence:  sequence,
				Data:      evt,
				Timestamp: time.Now(),
			})
		}
	}

	if target > 0 && len(events) > target {
		return payload.Bytes(), events[:target], nil
	}

	return payload.Bytes(), events, nil
}

func readHTTPChunkStreamBody(bodyReader io.Reader, expected []models.HTTPStreamEvent, timeout time.Duration) ([]byte, []models.HTTPStreamEvent, error) {
	buf := make([]byte, 4096)
	payload := bytes.NewBuffer(nil)
	events := make([]models.HTTPStreamEvent, 0, max(1, len(expected)))
	pending := strings.Builder{}
	sequence := 0
	deadline := time.Now().Add(timeout)
	target := len(expected)

	for {
		n, err := bodyReader.Read(buf)
		now := time.Now()
		if n > 0 {
			chunk := string(buf[:n])
			payload.WriteString(chunk)

			if target == 0 {
				sequence++
				events = append(events, models.HTTPStreamEvent{
					Sequence:  sequence,
					Data:      chunk,
					Timestamp: now,
				})
			} else {
				pending.WriteString(chunk)
				for len(events) < target {
					exp := expected[len(events)].Data
					cur := pending.String()
					if cur == "" {
						break
					}

					// Match exact recorded event boundaries while allowing
					// replay chunk reads to arrive in smaller pieces.
					if strings.HasPrefix(cur, exp) {
						sequence++
						events = append(events, models.HTTPStreamEvent{
							Sequence:  sequence,
							Data:      exp,
							Timestamp: now,
						})
						rest := cur[len(exp):]
						pending.Reset()
						pending.WriteString(rest)
						if len(events) >= target {
							return payload.Bytes(), events[:target], nil
						}
						continue
					}

					if strings.HasPrefix(exp, cur) {
						// Need more bytes to finish this event.
						break
					}

					// Sequence mismatch. Emit what we currently have so matcher can show diff.
					sequence++
					events = append(events, models.HTTPStreamEvent{
						Sequence:  sequence,
						Data:      cur,
						Timestamp: now,
					})
					pending.Reset()
					break
				}
			}
		}

		if err != nil {
			if err == io.EOF {
				break
			}
			if isTimeoutError(err) {
				return payload.Bytes(), events, nil
			}
			return payload.Bytes(), events, err
		}

		if timeout > 0 && time.Now().After(deadline) {
			return payload.Bytes(), events, nil
		}
	}

	if pending.Len() > 0 {
		sequence++
		events = append(events, models.HTTPStreamEvent{
			Sequence:  sequence,
			Data:      pending.String(),
			Timestamp: time.Now(),
		})
	}

	if target > 0 && len(events) > target {
		return payload.Bytes(), events[:target], nil
	}

	return payload.Bytes(), events, nil
}

func isTimeoutError(err error) bool {
	if err == nil {
		return false
	}

	if errors.Is(err, context.DeadlineExceeded) {
		return true
	}

	var netErr net.Error
	if errors.As(err, &netErr) && netErr.Timeout() {
		return true
	}

	return strings.Contains(strings.ToLower(err.Error()), "timeout")
}
