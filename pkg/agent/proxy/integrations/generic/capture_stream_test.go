package generic

import (
	"context"
	"io"
	"testing"

	syncMock "go.keploy.io/server/v3/pkg/agent/proxy/syncMock"
	"go.uber.org/zap"
)

// answerAt is a destination that answers what it is sent at once, through
// the other direction's tee, before the write that sent it returns: the
// answer's copy outrunning the request's.
type answerAt struct {
	answer *captureTeeWriter
}

func (a answerAt) Write(p []byte) (int, error) {
	if _, err := a.answer.Write([]byte("-ERR denied\r\n")); err != nil {
		return 0, err
	}
	return len(p), nil
}

// The capture stream holds a request before its answer, however fast the
// answer comes: the request's tee queues it before forwarding it. The two
// directions had a channel each, and the request was queued after it was
// forwarded, so an answer that outran it was read first and the exchange
// was dropped (a generic recording lost it, about one in twenty on
// loopback).
func TestTheCaptureStreamHoldsARequestBeforeItsAnswer(t *testing.T) {
	stream := newCaptureStream(8, 2)
	answer := &captureTeeWriter{dest: io.Discard, stream: stream}
	request := &captureTeeWriter{dest: answerAt{answer}, stream: stream, fromClient: true}
	if _, err := request.Write([]byte("AUTH app\r\n")); err != nil {
		t.Fatal(err)
	}
	stream.ended()
	stream.ended()
	var got []bool
	for c := range stream.ch {
		got = append(got, c.fromClient)
	}
	if len(got) != 2 || !got[0] || got[1] {
		t.Fatalf("the stream holds chunks from the client %v, want [true false]: the request, then its answer", got)
	}
}

// A connection whose context ends while its last exchange is still queued
// (a short connection whose caller returns, and cancels, as soon as both
// copies end) keeps that exchange: the mock maker takes what the stream
// holds before its final flush.
func TestACancelKeepsTheQueuedLastExchange(t *testing.T) {
	for i := 0; i < 50; i++ {
		mgr := syncMock.New(zap.NewNop())
		ctx, cancel := context.WithCancel(syncMock.NewContext(context.Background(), mgr))
		stream := newCaptureStream(4, 1)
		stream.add(true, []byte("AUTH app\r\n"))
		stream.add(false, []byte("-ERR denied\r\n"))
		stream.ended()
		cancel()
		createGenericMocksAsync(ctx, zap.NewNop(), stream.ch)
		if _, _, added, _ := mgr.GetDropStats(); added != 1 {
			t.Fatalf("run %d: %d mocks made of the queued exchange, want 1", i, added)
		}
	}
}

// A chunk that does not fit stops the capture of both directions, once:
// later chunks of either are not queued, and the end that follows does not
// close the stream again.
func TestAFullStreamStopsBothDirections(t *testing.T) {
	stream := newCaptureStream(1, 2)
	stream.add(true, []byte("a"))
	stream.add(false, []byte("b"))
	stream.add(true, []byte("c"))
	stream.ended()
	stream.ended()
	var got []string
	for c := range stream.ch {
		got = append(got, string(c.buf))
	}
	if len(got) != 1 || got[0] != "a" {
		t.Fatalf("the stream holds %q after it filled, want only what fit", got)
	}
}
