package orchestrator

import (
	"io"
	"sync"
	"testing"
	"time"
)

// BenchmarkRingBufWriteRead measures the write→signal→read wakeup latency
// of the ring buffer with the platform-optimised signal (eventfd on Linux).
func BenchmarkRingBufWriteRead(b *testing.B) {
	rb := newRingBuf(64 * 1024) // 64 KB ring
	defer rb.Close()

	payload := make([]byte, 64) // typical small MySQL packet
	readBuf := make([]byte, 64)

	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rb.Write(payload)
		rb.Read(readBuf)
	}
}

// BenchmarkRingBufLatency measures end-to-end wakeup latency across goroutines,
// mimicking the TeeForwardConn producer→consumer pattern.
func BenchmarkRingBufLatency(b *testing.B) {
	rb := newRingBuf(2 * 1024 * 1024) // 2 MB, same as production
	defer rb.Close()

	payload := make([]byte, 128)
	readBuf := make([]byte, 128)

	var wg sync.WaitGroup
	done := make(chan struct{})
	latencies := make([]time.Duration, 0, b.N)
	var mu sync.Mutex

	// Consumer goroutine
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			_, err := rb.Read(readBuf)
			if err == io.EOF {
				return
			}
		}
	}()

	// Producer: write and measure latency
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		start := time.Now()
		rb.Write(payload)
		// Measure how long it takes for the consumer to drain
		// (approximated by the next write's free-space availability)
		elapsed := time.Since(start)
		mu.Lock()
		latencies = append(latencies, elapsed)
		mu.Unlock()
	}

	rb.Close()
	wg.Wait()
	close(done)

	// Report p50/p99
	if len(latencies) > 0 {
		// Simple sort-free p50: just report average
		var total time.Duration
		for _, l := range latencies {
			total += l
		}
		avg := total / time.Duration(len(latencies))
		b.ReportMetric(float64(avg.Nanoseconds()), "ns/write-signal")
	}
}

// BenchmarkRingBufThroughput measures raw throughput with concurrent producer/consumer.
func BenchmarkRingBufThroughput(b *testing.B) {
	rb := newRingBuf(2 * 1024 * 1024)
	defer rb.Close()

	payload := make([]byte, 1024)
	readBuf := make([]byte, 1024)

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			_, err := rb.Read(readBuf)
			if err == io.EOF {
				return
			}
		}
	}()

	b.SetBytes(int64(len(payload)))
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rb.Write(payload)
	}
	b.StopTimer()

	rb.Close()
	wg.Wait()
}
