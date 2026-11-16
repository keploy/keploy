package relay

import (
	"syscall"
	"testing"
)

// On macOS a reset that comes once the proxy's FIN is sent closes the
// connection with no error kept: the socket's TCP_INFO says it, closed with
// bytes the client's stack never acknowledged, and gives ECONNRESET once.
func TestXNUAResetAfterTheProxysFINIsReadFromItsTCPInfo(t *testing.T) {
	for _, c := range []struct {
		name string
		info darwinTCPInfo
		want error
	}{
		{"closed with bytes outstanding: reset", darwinTCPInfo{state: tcpsClosed, txunacked: 9}, syscall.ECONNRESET},
		{"closed with every byte acknowledged: its FIN's acknowledgement", darwinTCPInfo{state: tcpsClosed}, nil},
		{"last ack with bytes outstanding: not closed yet", darwinTCPInfo{state: tcpsLastAck, txunacked: 9}, nil},
		{"open with bytes outstanding", darwinTCPInfo{state: tcpsEstablished, txunacked: 9}, nil},
	} {
		t.Run(c.name, func(t *testing.T) {
			s := (&scriptedXNU{infos: []darwinTCPInfo{c.info}}).socket()
			if got := errnoOf(s.unresetError(0)); got != c.want {
				t.Fatalf("unresetError = %v, want %v", got, c.want)
			}
			// The kernel clears an error as it gives it: so does this one.
			if got := s.unresetError(0); got != 0 {
				t.Fatalf("unresetError asked again = %v, want none: it was given", syscall.Errno(got))
			}
		})
	}
}

// A socket that cannot answer gives no reset, whatever its buffer holds.
func TestXNUASocketThatCannotSayGivesNoReset(t *testing.T) {
	s := &tcpClientSocket{askInfo: func(_ uintptr, _ *[tcpInfoBuf]byte, info *darwinTCPInfo) bool {
		*info = darwinTCPInfo{state: tcpsClosed, txunacked: 9}
		return false
	}}
	if got := s.unresetError(0); got != 0 {
		t.Fatalf("unresetError = %v for a socket that could not say, want none", syscall.Errno(got))
	}
}

func errnoOf(n int) error {
	if n == 0 {
		return nil
	}
	return syscall.Errno(n)
}
