package tls

import (
	"io"
	"sync"
	"sync/atomic"
)

// keyLogSink is the package-level fanout that receives every NSS
// keylog line stdlib crypto/tls writes during handshakes. Each
// tls.Config the proxy builds carries a pointer to this sink under
// KeyLogWriter; subscribers (typically the recorder's streaming HTTP
// consumer) register themselves via AddKeyLogSubscriber and receive
// a copy of every line for the duration of their subscription.
//
// Why a singleton: stdlib crypto/tls calls KeyLogWriter.Write from
// each handshake goroutine concurrently, and each tls.Config we
// hand out captures the writer at build time. A singleton avoids
// "subscribed too late, missed the early handshakes" footguns —
// when no subscribers exist yet the writes are simply discarded,
// and the moment one connects it starts seeing lines on the next
// handshake.
type keyLogSink struct {
	mu   sync.RWMutex
	subs []*keyLogSub
	next atomic.Int64
}

type keyLogSub struct {
	id int64
	w  io.Writer
	// mu serialises Write so that unsub() can mark closed and wait
	// for any in-flight Write to finish before returning. Without
	// this, a caller that passes a writer which panics on use-after-
	// close would race with the unsub that tears it down.
	mu     sync.Mutex
	closed bool
}

func (s *keyLogSink) Write(p []byte) (int, error) {
	s.mu.RLock()
	subs := s.subs
	s.mu.RUnlock()
	// Best-effort: per-subscriber errors are swallowed so a slow or
	// dead consumer cannot stall the TLS handshake goroutines that
	// drove this Write — every active TLS connection in the
	// recorded application calls into this path on every handshake.
	for _, sub := range subs {
		sub.mu.Lock()
		if !sub.closed {
			_, _ = sub.w.Write(p)
		}
		sub.mu.Unlock()
	}
	return len(p), nil
}

func (s *keyLogSink) add(w io.Writer) func() {
	sub := &keyLogSub{id: s.next.Add(1), w: w}
	s.mu.Lock()
	s.subs = append(s.subs, sub)
	s.mu.Unlock()
	return func() {
		// Mark closed while holding the subscriber mutex so no new
		// Write can start after we return. Any in-flight Write
		// finishes before we acquire the lock.
		sub.mu.Lock()
		sub.closed = true
		sub.mu.Unlock()

		s.mu.Lock()
		defer s.mu.Unlock()
		for i, elem := range s.subs {
			if elem.id == sub.id {
				s.subs = append(s.subs[:i], s.subs[i+1:]...)
				return
			}
		}
	}
}

var sink = &keyLogSink{}

// KeyLogWriter returns the package-level fanout sink. Always
// non-nil; safe to assign unconditionally to tls.Config.KeyLogWriter
// at every call site. With zero subscribers the sink discards
// writes — there is no on/off switch.
func KeyLogWriter() io.Writer { return sink }

// AddKeyLogSubscriber registers w to receive a copy of every NSS
// keylog line stdlib crypto/tls writes during handshakes. Returns
// the unsubscribe func; safe to call once.
//
// Each line ends in '\n' and is one of the labels documented at
// https://datatracker.ietf.org/doc/html/draft-ietf-tls-keylogfile —
// CLIENT_RANDOM (TLS 1.2), CLIENT_HANDSHAKE_TRAFFIC_SECRET /
// SERVER_HANDSHAKE_TRAFFIC_SECRET / CLIENT_TRAFFIC_SECRET_0 /
// SERVER_TRAFFIC_SECRET_0 (TLS 1.3), etc.
func AddKeyLogSubscriber(w io.Writer) func() { return sink.add(w) }
