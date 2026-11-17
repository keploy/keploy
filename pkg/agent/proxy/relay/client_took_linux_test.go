//go:build linux

package relay

import (
	"testing"

	"golang.org/x/sys/unix"
)

// This row models Linux's own count of the bytes written (countAt in
// client_took_linux.go): two questions, TCP_INFO and then SIOCOUTQ, which
// acknowledgements can move between. macOS asks one TCP_INFO, which answers
// both at one instant; its count has its own rows (client_took_darwin_test.go).

// countedSocket is a client socket whose two questions, TCP_INFO and
// SIOCOUTQ, are answered from a model of the kernel: total bytes written to
// it in all, and acked, how many of them the client's stack had acknowledged
// as each question was asked, in turn (the last repeats). SIOCOUTQ says what
// was written beyond that. asked counts the questions.
type countedSocket struct {
	total uint64
	acked []uint64
	asked int
}

func (c *countedSocket) at() uint64 {
	a := c.acked[min(c.asked, len(c.acked)-1)]
	c.asked++
	return a
}

func (c *countedSocket) socket() *tcpClientSocket {
	s := &tcpClientSocket{
		askInfo: func(_ uintptr, info *unix.TCPInfo) bool {
			*info = unix.TCPInfo{Bytes_acked: c.at()}
			return true
		},
		askUnsent: func(_ uintptr, n *int32) bool {
			*n = int32(c.total - c.at())
			return true
		},
	}
	s.count = s.countAt
	return s
}

// How many bytes written counts, from its two questions: the client's
// acknowledgements, and the bytes written beyond them. Acknowledgements can
// move between the two. While they hold, the sum is every byte written. When
// they move between the questions of one try, it asks again, and a try in
// which they held is exact. When they move on every try, it takes the sum
// with the later count, which is no less than the bytes written: a chunk is
// then judged from no earlier than its first byte (a chunk the client did
// not take is never teed). A question the socket cannot answer says so.
func TestWrittenCountsEveryByteWrittenToTheClientsSocket(t *testing.T) {
	t.Parallel()
	const total = 1000
	count := func(c *countedSocket) (uint64, bool) {
		s := c.socket()
		s.countAt(0)
		return s.wrote, s.wroteOK
	}
	t.Run("acknowledgements hold: acknowledged and not, one try", func(t *testing.T) {
		c := &countedSocket{total: total, acked: []uint64{400}}
		if n, ok := count(c); n != total || !ok || c.asked != 3 {
			t.Fatalf("written %d (ok %v) on %d questions, want %d on one try's 3", n, ok, c.asked, total)
		}
	})
	t.Run("they move once, then hold: the try they held is exact", func(t *testing.T) {
		// The first try: 400 acknowledged, 550 beyond the 450 acknowledged
		// as SIOCOUTQ is asked, 500 acknowledged after: 1050, too many.
		c := &countedSocket{total: total, acked: []uint64{400, 450, 500}}
		if n, ok := count(c); n != total || !ok || c.asked != 6 {
			t.Fatalf("written %d (ok %v) on %d questions, want %d on the second try", n, ok, c.asked, total)
		}
	})
	t.Run("they move on every try: no fewer than were written", func(t *testing.T) {
		acked := make([]uint64, 3*writtenTries)
		for i := range acked {
			acked[i] = uint64(10 * i)
		}
		c := &countedSocket{total: total, acked: acked}
		n, ok := count(c)
		if !ok || n < total || c.asked != 3*writtenTries {
			t.Fatalf("written %d (ok %v) on %d questions, want no fewer than %d after %d tries", n, ok, c.asked, total, writtenTries)
		}
	})
	t.Run("a question the socket cannot answer: it cannot say", func(t *testing.T) {
		for _, failing := range []string{"TCP_INFO", "SIOCOUTQ"} {
			s := (&countedSocket{total: total, acked: []uint64{400}}).socket()
			if failing == "TCP_INFO" {
				s.askInfo = func(uintptr, *unix.TCPInfo) bool { return false }
			} else {
				s.askUnsent = func(uintptr, *int32) bool { return false }
			}
			s.countAt(0)
			if s.wroteOK {
				t.Fatalf("%s failed: written says %d, want it to say it cannot say", failing, s.wrote)
			}
		}
	})
}

// A chunk written after the client's FIN, while the client has not yet
// acknowledged all of the one before it, is judged from where it starts, on
// the socket as the listener gave it: Linux's socket counts what its client
// acknowledged itself (tcpi_bytes_acked). macOS reads that count from the
// count of writes the proxy puts on every socket it accepts, and runs this
// case with it (TestAChunkBehindOneIsJudgedFromItsOwnFirstByteOnASocketCountedFromItsAccept);
// a macOS socket with no count from its accept is asked by its sequence
// numbers alone, which a closed window's probes leave unable to say, so the
// chunk is teed there (TestAResendingClientThatResetsHasWhatItNeverAcknowledgedRefused).
func TestAChunkBehindOneThatIsNotAcknowledgedYetIsJudgedFromItsOwnFirstByte(t *testing.T) {
	t.Parallel()
	judgedFromItsOwnFirstByte(t, noAcceptCount)
}
