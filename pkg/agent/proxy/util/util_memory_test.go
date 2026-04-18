package util

import (
	"context"
	"errors"
	"io"
	"net"
	"testing"
	"time"

	"go.uber.org/zap"
)

func stubRecordingPaused(t *testing.T, paused bool) {
	t.Helper()
	prev := isRecordingPaused
	isRecordingPaused = func() bool { return paused }
	t.Cleanup(func() {
		isRecordingPaused = prev
	})
}

func TestReadBuffConnStopsWhenRecordingPaused(t *testing.T) {
	stubRecordingPaused(t, true)

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	bufferChannel := make(chan []byte, 1)
	errChannel := make(chan error, 1)
	done := make(chan struct{})

	go func() {
		ReadBuffConn(ctx, zap.NewNop(), clientConn, bufferChannel, errChannel, true)
		close(done)
	}()

	select {
	case err := <-errChannel:
		if !errors.Is(err, ErrRecordingPausedDueToMemoryPressure) {
			t.Fatalf("expected ErrRecordingPausedDueToMemoryPressure, got %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ReadBuffConn to stop on memory pressure")
	}

	select {
	case <-bufferChannel:
		t.Fatal("did not expect buffered data while recording is paused")
	default:
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ReadBuffConn goroutine to exit")
	}
}

func TestReadBuffConnIgnoresPauseForPassthroughReads(t *testing.T) {
	stubRecordingPaused(t, true)

	clientConn, serverConn := net.Pipe()
	defer clientConn.Close()
	defer serverConn.Close()

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	bufferChannel := make(chan []byte, 1)
	errChannel := make(chan error, 1)
	done := make(chan struct{})

	go func() {
		ReadBuffConn(ctx, zap.NewNop(), clientConn, bufferChannel, errChannel, false)
		close(done)
	}()

	payload := []byte("hello")
	go func() {
		_, _ = serverConn.Write(payload)
		_ = serverConn.Close()
	}()

	select {
	case buffer := <-bufferChannel:
		if string(buffer) != string(payload) {
			t.Fatalf("expected %q, got %q", payload, buffer)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for passthrough read to deliver data")
	}

	select {
	case err := <-errChannel:
		if errors.Is(err, ErrRecordingPausedDueToMemoryPressure) {
			t.Fatal("did not expect passthrough reads to stop on memory pressure")
		}
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, net.ErrClosed) {
			t.Fatalf("unexpected error from passthrough read: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for passthrough read goroutine to finish")
	}

	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for ReadBuffConn goroutine to exit")
	}
}

func TestReadBuffConnPauseSignalDoesNotBlockWithTwoReaders(t *testing.T) {
	stubRecordingPaused(t, true)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	errChannel := make(chan error, 2)
	done := make(chan struct{}, 2)

	for range 2 {
		clientConn, serverConn := net.Pipe()
		defer clientConn.Close()
		defer serverConn.Close()

		go func(conn net.Conn) {
			ReadBuffConn(ctx, zap.NewNop(), conn, make(chan []byte, 1), errChannel, true)
			done <- struct{}{}
		}(clientConn)
	}

	for range 2 {
		select {
		case err := <-errChannel:
			if !errors.Is(err, ErrRecordingPausedDueToMemoryPressure) {
				t.Fatalf("expected ErrRecordingPausedDueToMemoryPressure, got %v", err)
			}
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for pause errors from both readers")
		}
	}

	for range 2 {
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Fatal("timed out waiting for both ReadBuffConn goroutines to exit")
		}
	}
}
